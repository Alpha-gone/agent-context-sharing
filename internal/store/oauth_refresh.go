package store

import (
	"context"
	"errors"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// OAuthGrantBinding은 공유 계열에서 읽은 불변 인가 결합이다.
type OAuthGrantBinding struct {
	ID, AccountID             model.ID
	ClientID, Resource, Scope string
}

// OAuthRefreshExchange는 원문을 보관하지 않는 갱신 요청이다. 빈 Scope는 기존 범위를 유지한다.
type OAuthRefreshExchange struct {
	Hash                      [32]byte
	ClientID, Resource, Scope string
}

// RotateOAuthRefreshToken은 계열→토큰 순서로 잠그고 소비·후속 발급·활동 시각을 함께 커밋한다.
// issue는 추가 DB 연결을 확보하지 않는다. 재사용 폐기는 오류 반환 전에 커밋한다.
func (s *Store) RotateOAuthRefreshToken(ctx context.Context, input OAuthRefreshExchange, clock func() time.Time, issue func(OAuthGrantBinding, time.Time) ([32]byte, error)) (resultErr error) {
	if input.ClientID == "" || input.Resource == "" || clock == nil || issue == nil {
		return ErrInvalidOAuthGrant
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storageError("oauth_refresh_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			resultErr = storageError("oauth_refresh_rollback", errors.Join(resultErr, err))
		}
	}()
	var grantID string
	err = tx.QueryRow(ctx, `SELECT grant_id::text FROM public.oauth_refresh_token WHERE token_hash=$1`, input.Hash[:]).Scan(&grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidOAuthGrant
	}
	if err != nil {
		return storageError("oauth_refresh_lookup", err)
	}
	var binding OAuthGrantBinding
	var accountID string
	var authorized, rotated, absolute, idle, retain time.Time
	var revoked *time.Time
	err = tx.QueryRow(ctx, `SELECT account_id::text,client_id,resource,scope,authorized_at,last_rotated_at,absolute_expires_at,idle_expires_at,retain_until,revoked_at
		FROM public.oauth_grant WHERE grant_id=$1 FOR UPDATE`, grantID).Scan(&accountID, &binding.ClientID, &binding.Resource, &binding.Scope, &authorized, &rotated, &absolute, &idle, &retain, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidOAuthGrant
	}
	if err != nil {
		return storageError("oauth_refresh_grant_lock", err)
	}
	var issued, expires time.Time
	var consumed *time.Time
	err = tx.QueryRow(ctx, `SELECT issued_at,expires_at,consumed_at FROM public.oauth_refresh_token WHERE token_hash=$1 AND grant_id=$2 FOR UPDATE`, input.Hash[:], grantID).Scan(&issued, &expires, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidOAuthGrant
	}
	if err != nil {
		return storageError("oauth_refresh_token_lock", err)
	}
	if binding.ClientID != input.ClientID || binding.Resource != input.Resource || binding.Scope != "agent-context" || (input.Scope != "" && input.Scope != binding.Scope) {
		return &OAuthRejection{Reason: "binding_violation"}
	}
	binding.ID, err = model.ParseID(grantID)
	if err != nil {
		return storageError("oauth_refresh_grant_identifier", err)
	}
	binding.AccountID, err = model.ParseID(accountID)
	if err != nil {
		return storageError("oauth_refresh_account_identifier", err)
	}
	reject := func(reason string) error { return &OAuthRejection{Reason: reason, Binding: binding} }
	if revoked != nil {
		return reject("family_revoked")
	}
	now := clock().UTC().Truncate(time.Second)
	if consumed != nil {
		if _, err := tx.Exec(ctx, `UPDATE public.oauth_grant SET revoked_at=$2,revocation_reason='refresh_token_reuse' WHERE grant_id=$1`, grantID, now); err != nil {
			return storageError("oauth_refresh_reuse_revoke", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return storageError("oauth_refresh_reuse_commit", err)
		}
		return reject("reuse_detected")
	}
	// 현재 시각은 두 잠금을 모두 얻은 뒤 읽는다. 정리 지연이나 잘못된 저장 수명이 유효성을 늘리지 않는다.
	if now.Before(authorized) || now.Before(rotated) || now.Before(issued) || issued.Before(authorized) || rotated.Before(authorized) ||
		!absolute.Equal(authorized.Add(30*24*time.Hour)) || !idle.Equal(rotated.Add(7*24*time.Hour)) ||
		!expires.Equal(minTime(absolute, idle)) {
		return ErrInvalidOAuthGrant
	}
	if !now.Before(absolute) {
		return reject("expired")
	}
	if !now.Before(idle) {
		return reject("inactive_expired")
	}
	if !now.Before(expires) {
		return reject("expired")
	}
	successor, err := issue(binding, now)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	idle = now.Add(7 * 24 * time.Hour)
	_, err = tx.Exec(ctx, `INSERT INTO public.oauth_refresh_token (token_hash,grant_id,issued_at,expires_at) VALUES ($1,$2,$3,$4)`, successor[:], grantID, now, minTime(absolute, idle))
	if err != nil {
		return storageError("oauth_refresh_successor_create", err)
	}
	_, err = tx.Exec(ctx, `UPDATE public.oauth_refresh_token SET consumed_at=$2,successor_token_hash=$3 WHERE token_hash=$1`, input.Hash[:], now, successor[:])
	if err != nil {
		return storageError("oauth_refresh_consume", err)
	}
	if boundary := absolute.Add(time.Hour); boundary.After(retain) {
		retain = boundary
	}
	if boundary := now.Add(time.Hour); boundary.After(retain) {
		retain = boundary
	}
	_, err = tx.Exec(ctx, `UPDATE public.oauth_grant SET last_rotated_at=$2,idle_expires_at=$3,retain_until=$4 WHERE grant_id=$1`, grantID, now, idle, retain)
	if err != nil {
		return storageError("oauth_refresh_grant_update", err)
	}
	return storageError("oauth_refresh_commit", tx.Commit(ctx))
}
