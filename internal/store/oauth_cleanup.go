package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CleanupOAuthState는 만료한 미소비 코드와 보존이 끝난 계열의 자식·계열을 정리한다.
// 소비 코드·갱신 해시는 자체 만료가 지나도 계열의 재사용 탐지 근거로 유지한다.
func (s *Store) CleanupOAuthState(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, fmt.Errorf("OAuth 정리 기준 시각이 없다")
	}
	result, err := s.pool.Exec(ctx, `DELETE FROM public.oauth_authorization_code
		WHERE consumed_at IS NULL AND expires_at <= $1`, now.UTC())
	if err != nil {
		return 0, storageError("oauth_cleanup_codes", err)
	}
	count := int(result.RowsAffected())
	var cursor string
	for {
		var after any
		if cursor != "" {
			after = cursor
		}
		rows, err := s.pool.Query(ctx, `SELECT grant_id::text FROM public.oauth_grant
			WHERE retain_until <= $1 AND ($2::uuid IS NULL OR grant_id > $2::uuid)
			ORDER BY grant_id LIMIT 128`, now.UTC(), after)
		if err != nil {
			return count, storageError("oauth_cleanup_candidates", err)
		}
		ids := make([]string, 0, 128)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return count, storageError("oauth_cleanup_scan", err)
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return count, storageError("oauth_cleanup_scan", err)
		}
		if len(ids) == 0 {
			return count, nil
		}
		for _, id := range ids {
			removed, err := s.cleanupOAuthGrant(ctx, id, now.UTC())
			count += removed
			if err != nil {
				return count, err
			}
		}
		cursor = ids[len(ids)-1]
	}
}

func (s *Store) cleanupOAuthGrant(ctx context.Context, id string, now time.Time) (_ int, resultErr error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, storageError("oauth_cleanup_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			resultErr = storageError("oauth_cleanup_rollback", errors.Join(resultErr, err))
		}
	}()
	var locked string
	err = tx.QueryRow(ctx, `SELECT grant_id::text FROM public.oauth_grant WHERE grant_id=$1
		AND GREATEST(retain_until, absolute_expires_at + interval '1 hour', last_rotated_at + interval '1 hour') <= $2
		FOR UPDATE`, id, now).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, storageError("oauth_cleanup_lock", err)
	}
	count := 0
	// 인증 요청과 같이 계열을 먼저 잠그며 FK 자식은 같은 트랜잭션에서 지운다.
	for _, query := range []string{
		`DELETE FROM public.oauth_authorization_code WHERE grant_id=$1`,
		`DELETE FROM public.oauth_refresh_token WHERE grant_id=$1`,
		`DELETE FROM public.oauth_grant WHERE grant_id=$1`,
	} {
		result, err := tx.Exec(ctx, query, id)
		if err != nil {
			return 0, storageError("oauth_cleanup_delete", err)
		}
		count += int(result.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, storageError("oauth_cleanup_commit", err)
	}
	return count, nil
}
