package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// ErrInvalidOAuthGrant는 자격 원문이나 실패 필드를 노출하지 않는 코드 거부다.
var ErrInvalidOAuthGrant = errors.New("invalid_grant")

var errOAuthCodeConsumed = errors.New("authorization_code_consumed_during_lock")

// OAuthAuthorizationCode는 원문 대신 해시와 검증한 인가 결합만 저장한다.
type OAuthAuthorizationCode struct {
	Hash                                              [32]byte
	AccountID                                         model.ID
	ClientID, RedirectURI, Resource, Scope, Challenge string
	IssuedAt, ExpiresAt                               time.Time
}

// OAuthCodeExchange는 등록 검증을 마친 요청이며 verifier는 저장하지 않는다.
type OAuthCodeExchange struct {
	Hash                                      [32]byte
	ClientID, RedirectURI, Resource, Verifier string
}

// OAuthInitialGrant는 같은 트랜잭션에서 생성할 계열과 첫 갱신 자격의 해시다.
type OAuthInitialGrant struct {
	ID          model.ID
	RefreshHash [32]byte
}

// CreateOAuthAuthorizationCode는 60초·단일 scope의 미소비 코드를 저장한다.
func (s *Store) CreateOAuthAuthorizationCode(ctx context.Context, code OAuthAuthorizationCode) error {
	if !code.AccountID.IsV7() || code.ClientID == "" || code.RedirectURI == "" || code.Resource == "" || code.Scope != "agent-context" || code.Challenge == "" || code.IssuedAt.IsZero() || code.ExpiresAt.Sub(code.IssuedAt) != time.Minute {
		return ErrInvalidOAuthGrant
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO public.oauth_authorization_code
		(code_hash, account_id, client_id, redirect_uri, resource, scope, code_challenge, issued_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, code.Hash[:], code.AccountID.String(), code.ClientID, code.RedirectURI, code.Resource, code.Scope, code.Challenge, code.IssuedAt, code.ExpiresAt)
	return storageError("oauth_code_create", err)
}

// ExchangeOAuthAuthorizationCode는 코드 소비·서명·계열·첫 갱신 해시를 원자적으로 반영한다.
// issue는 DB 접근 없이 서명한다. 재사용은 폐기를 커밋한 뒤 invalid_grant를 반환한다.
func (s *Store) ExchangeOAuthAuthorizationCode(ctx context.Context, input OAuthCodeExchange, clock func() time.Time, issue func(OAuthAuthorizationCode, time.Time) (OAuthInitialGrant, error)) error {
	if input.ClientID == "" || input.RedirectURI == "" || input.Resource == "" || input.Verifier == "" || clock == nil || issue == nil {
		return ErrInvalidOAuthGrant
	}
	// 미소비 행의 잠금을 기다리는 동안 최초 소비가 커밋됐다면 트랜잭션을 열어
	// 다시 계열→코드 순서로 잠근다. 소비 뒤 grant_id는 불변이므로 재시도는 한 번이다.
	for range 2 {
		err := s.exchangeOAuthAuthorizationCode(ctx, input, clock, issue)
		_, storageFailure := errors.AsType[*StorageError](err)
		if !errors.Is(err, errOAuthCodeConsumed) || storageFailure {
			return err
		}
	}
	return ErrInvalidOAuthGrant
}

func (s *Store) exchangeOAuthAuthorizationCode(ctx context.Context, input OAuthCodeExchange, clock func() time.Time, issue func(OAuthAuthorizationCode, time.Time) (OAuthInitialGrant, error)) (resultErr error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storageError("oauth_code_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			resultErr = storageError("oauth_code_rollback", errors.Join(resultErr, err))
		}
	}()
	var observedGrant *string
	err = tx.QueryRow(ctx, `SELECT grant_id::text FROM public.oauth_authorization_code WHERE code_hash=$1`, input.Hash[:]).Scan(&observedGrant)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidOAuthGrant
	}
	if err != nil {
		return storageError("oauth_code_lookup", err)
	}
	if observedGrant != nil {
		var id string
		if err := tx.QueryRow(ctx, `SELECT grant_id::text FROM public.oauth_grant WHERE grant_id=$1 FOR UPDATE`, *observedGrant).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInvalidOAuthGrant
			}
			return storageError("oauth_grant_lock", err)
		}
	}
	var code OAuthAuthorizationCode
	var accountID string
	var consumedAt *time.Time
	var grantID *string
	err = tx.QueryRow(ctx, `SELECT account_id::text, client_id, redirect_uri, resource, scope, code_challenge, issued_at, expires_at, consumed_at, grant_id::text
		FROM public.oauth_authorization_code WHERE code_hash=$1 FOR UPDATE`, input.Hash[:]).Scan(&accountID, &code.ClientID, &code.RedirectURI, &code.Resource, &code.Scope, &code.Challenge, &code.IssuedAt, &code.ExpiresAt, &consumedAt, &grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidOAuthGrant
	}
	if err != nil {
		return storageError("oauth_code_lock", err)
	}
	if observedGrant == nil && grantID != nil {
		return errOAuthCodeConsumed
	}
	challengeHash := sha256.Sum256([]byte(input.Verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])
	if code.ClientID != input.ClientID || code.RedirectURI != input.RedirectURI || code.Resource != input.Resource || code.Scope != "agent-context" || subtle.ConstantTimeCompare([]byte(code.Challenge), []byte(challenge)) != 1 {
		return ErrInvalidOAuthGrant
	}
	now := clock().UTC().Truncate(time.Second)
	if consumedAt != nil {
		if _, err := tx.Exec(ctx, `UPDATE public.oauth_grant SET revoked_at=COALESCE(revoked_at,$2), revocation_reason=COALESCE(revocation_reason,'authorization_code_reuse') WHERE grant_id=$1`, *grantID, now); err != nil {
			return storageError("oauth_code_reuse_revoke", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return storageError("oauth_code_reuse_commit", err)
		}
		return ErrInvalidOAuthGrant
	}
	if now.Before(code.IssuedAt) || !now.Before(code.ExpiresAt) || code.ExpiresAt.Sub(code.IssuedAt) != time.Minute {
		return ErrInvalidOAuthGrant
	}
	code.Hash = input.Hash
	code.AccountID, err = model.ParseID(accountID)
	if err != nil {
		return storageError("oauth_code_account", err)
	}
	initial, err := issue(code, now)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !initial.ID.IsV7() {
		return ErrInvalidOAuthGrant
	}
	absolute, idle := code.IssuedAt.Add(30*24*time.Hour), code.IssuedAt.Add(7*24*time.Hour)
	retain := absolute.Add(time.Hour)
	if accessExpiry := now.Add(time.Hour); accessExpiry.After(retain) {
		retain = accessExpiry
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.oauth_grant
		(grant_id,account_id,client_id,resource,scope,authorized_at,last_rotated_at,absolute_expires_at,idle_expires_at,retain_until)
		VALUES ($1,$2,$3,$4,$5,$6,$6,$7,$8,$9)`, initial.ID.String(), accountID, code.ClientID, code.Resource, code.Scope, code.IssuedAt, absolute, idle, retain)
	if err != nil {
		return storageError("oauth_grant_create", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.oauth_refresh_token (token_hash,grant_id,issued_at,expires_at) VALUES ($1,$2,$3,$4)`, initial.RefreshHash[:], initial.ID.String(), now, minTime(absolute, idle))
	if err != nil {
		return storageError("oauth_refresh_create", err)
	}
	_, err = tx.Exec(ctx, `UPDATE public.oauth_authorization_code SET consumed_at=$2,grant_id=$3 WHERE code_hash=$1`, input.Hash[:], now, initial.ID.String())
	if err != nil {
		return storageError("oauth_code_consume", err)
	}
	return storageError("oauth_code_commit", tx.Commit(ctx))
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
