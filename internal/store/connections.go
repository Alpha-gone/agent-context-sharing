package store

import (
	"context"
	"errors"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

const connectionPageSize = 50

// OAuthGrantSummary에는 본인 연결 화면에 필요한 상태만 담는다. 자격·해시는 조회하지 않는다.
type OAuthGrantSummary struct {
	ID                                                            model.ID
	ClientID                                                      string
	AuthorizedAt, LastRotatedAt, AbsoluteExpiresAt, IdleExpiresAt time.Time
	RevokedAt                                                     *time.Time
}

// AgentSummary는 폐기하지 않은 등록의 공개 식별 정보다. 등록 키는 포함하지 않는다.
type AgentSummary struct {
	ID        model.ID
	ClientID  string
	CreatedAt time.Time
}

// ConnectionPage는 계열의 유한 페이지와 계정별 상한 이내의 활성 등록 목록이다.
type ConnectionPage struct {
	Grants []OAuthGrantSummary
	Agents []AgentSummary
	Next   model.ID
}

// ListConnections는 계정 결합을 각 조회에 적용하고 자격을 제외한 유한 목록을 반환한다.
func (s *Store) ListConnections(ctx context.Context, account, before model.ID) (ConnectionPage, error) {
	if err := ctx.Err(); err != nil {
		return ConnectionPage{}, err
	}
	if !account.IsV7() || (before != (model.ID{}) && !before.IsV7()) {
		return ConnectionPage{}, model.ErrNotFound
	}
	var cursor any
	if before.IsV7() {
		cursor = before.String()
	}
	rows, err := s.pool.Query(ctx, `SELECT grant_id::text,client_id,authorized_at,last_rotated_at,absolute_expires_at,idle_expires_at,revoked_at
		FROM public.oauth_grant WHERE account_id=$1 AND ($2::uuid IS NULL OR grant_id<$2)
		ORDER BY grant_id DESC LIMIT $3`, account.String(), cursor, connectionPageSize+1)
	if err != nil {
		return ConnectionPage{}, storageError("connections_grant_list", err)
	}
	var page ConnectionPage
	for rows.Next() {
		var item OAuthGrantSummary
		var rawID string
		if err := rows.Scan(&rawID, &item.ClientID, &item.AuthorizedAt, &item.LastRotatedAt, &item.AbsoluteExpiresAt, &item.IdleExpiresAt, &item.RevokedAt); err != nil {
			rows.Close()
			return ConnectionPage{}, storageError("connections_grant_scan", err)
		}
		item.ID, err = model.ParseID(rawID)
		if err != nil {
			rows.Close()
			return ConnectionPage{}, storageError("connections_grant_identifier", err)
		}
		page.Grants = append(page.Grants, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ConnectionPage{}, storageError("connections_grant_rows", err)
	}
	if len(page.Grants) > connectionPageSize {
		page.Grants = page.Grants[:connectionPageSize]
		page.Next = page.Grants[len(page.Grants)-1].ID
	}
	rows, err = s.pool.Query(ctx, `SELECT agent_id::text,client_id,created_at FROM public.agent_registration
		WHERE account_id=$1 AND revoked_at IS NULL ORDER BY agent_id DESC LIMIT $2`, account.String(), activeAgentLimit+1)
	if err != nil {
		return ConnectionPage{}, storageError("connections_agent_list", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item AgentSummary
		var rawID string
		if err := rows.Scan(&rawID, &item.ClientID, &item.CreatedAt); err != nil {
			return ConnectionPage{}, storageError("connections_agent_scan", err)
		}
		item.ID, err = model.ParseID(rawID)
		if err != nil {
			return ConnectionPage{}, storageError("connections_agent_identifier", err)
		}
		page.Agents = append(page.Agents, item)
	}
	if err := rows.Err(); err != nil {
		return ConnectionPage{}, storageError("connections_agent_rows", err)
	}
	if len(page.Agents) > activeAgentLimit {
		return ConnectionPage{}, storageError("connections_agent_limit", model.ErrInvalidState)
	}
	if err := ctx.Err(); err != nil {
		return ConnectionPage{}, err
	}
	return page, nil
}

// RevokeOAuthGrant는 회전과 같은 계열 행을 잠그며 다른 소유자의 대상과 없음을 구분하지 않는다.
func (s *Store) RevokeOAuthGrant(ctx context.Context, account, grant model.ID) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !account.IsV7() || !grant.IsV7() {
		return model.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storageError("connections_revoke_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			resultErr = storageError("connections_revoke_rollback", errors.Join(resultErr, err))
		}
	}()
	var found string
	err = tx.QueryRow(ctx, `SELECT grant_id::text FROM public.oauth_grant WHERE grant_id=$1 AND account_id=$2 FOR UPDATE`, grant.String(), account.String()).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrNotFound
	}
	if err != nil {
		return storageError("connections_grant_lock", err)
	}
	_, err = tx.Exec(ctx, `UPDATE public.oauth_grant SET revoked_at=$2,revocation_reason='owner_revoked' WHERE grant_id=$1 AND revoked_at IS NULL`, grant.String(), time.Now().UTC())
	if err != nil {
		return storageError("connections_revoke_update", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return storageError("connections_revoke_commit", err)
	}
	return ctx.Err()
}
