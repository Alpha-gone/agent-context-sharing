package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"github.com/jackc/pgx/v5"
)

const ingestCredentialPrefix = "acs_ing_"

// IngestCredential는 원문과 해시를 제외한 연결 관리 정보다.
type IngestCredential struct {
	ID                    model.ID
	Label                 string
	IssuedAt              time.Time
	LastUsedAt, RevokedAt *time.Time
}

// IssueIngestCredential은 자격과 결합 등록을 하나의 트랜잭션에서 발급한다.
func (s *Store) IssueIngestCredential(ctx context.Context, account model.ID, label string) (token string, resultErr error) {
	if !account.IsV7() || !utf8.ValidString(label) || utf8.RuneCountInString(label) < 1 || utf8.RuneCountInString(label) > 64 {
		return "", model.FieldError{Field: "label", Message: "수집 자격 이름을 확인해 주십시오."}
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", storageError("ingest_random", err)
	}
	token = ingestCredentialPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(token))
	credential, err := model.NewID()
	if err != nil {
		return "", err
	}
	agent, err := model.NewID()
	if err != nil {
		return "", err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", storageError("ingest_issue_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			token, resultErr = "", storageError("ingest_issue_rollback", errors.Join(resultErr, err))
		}
	}()
	if err := lockAgentAccount(ctx, tx, account); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	var count int64
	err = tx.QueryRow(ctx, `SELECT count(*) FROM public.ingest_credential WHERE account_id=$1 AND revoked_at IS NULL AND issued_at + interval '1 year' > $2::timestamptz AND GREATEST(last_used_at, issued_at) + interval '30 days' > $2::timestamptz`, account.String(), now).Scan(&count)
	if err != nil {
		return "", storageError("ingest_credential_count", err)
	}
	if err := plan.CheckIncrease("ingest_credentials_per_account", count, 1, 20); err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `SELECT count(*) FROM public.agent_registration WHERE account_id=$1 AND revoked_at IS NULL`, account.String()).Scan(&count)
	if err != nil {
		return "", storageError("ingest_agent_count", err)
	}
	if err := plan.CheckIncrease("agent_registrations_per_account", count, 1, activeAgentLimit); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.ingest_credential (credential_id,token_hash,account_id,label,issued_at) VALUES ($1,$2,$3,$4,$5)`, credential.String(), hash[:], account.String(), label, now)
	if err != nil {
		return "", storageError("ingest_credential_insert", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.agent_registration (agent_id,account_id,ingest_credential_id,registration_key,created_at) VALUES ($1,$2,$3,$4,$5)`, agent.String(), account.String(), credential.String(), strings.ReplaceAll(credential.String(), "-", ""), now)
	if err != nil {
		return "", storageError("ingest_agent_insert", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", storageError("ingest_issue_commit", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeIngestCredential은 본인 자격과 결합 등록을 함께 폐기한다.
func (s *Store) RevokeIngestCredential(ctx context.Context, account, credential model.ID) (resultErr error) {
	if !account.IsV7() || !credential.IsV7() {
		return model.ErrNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return storageError("ingest_revoke_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			resultErr = storageError("ingest_revoke_rollback", errors.Join(resultErr, err))
		}
	}()
	if err := lockAgentAccount(ctx, tx, account); err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := tx.Exec(ctx, `UPDATE public.ingest_credential SET revoked_at=COALESCE(revoked_at,$3) WHERE credential_id=$1 AND account_id=$2`, credential.String(), account.String(), now)
	if err != nil {
		return storageError("ingest_revoke", err)
	}
	if result.RowsAffected() != 1 {
		return model.ErrNotFound
	}
	result, err = tx.Exec(ctx, `UPDATE public.agent_registration SET revoked_at=COALESCE(revoked_at,$3) WHERE ingest_credential_id=$1 AND account_id=$2`, credential.String(), account.String(), now)
	if err != nil {
		return storageError("ingest_agent_revoke", err)
	}
	if result.RowsAffected() != 1 {
		return storageError("ingest_agent_revoke", model.ErrInvalidState)
	}
	if err := tx.Commit(ctx); err != nil {
		return storageError("ingest_revoke_commit", err)
	}
	return ctx.Err()
}

// ValidateIngestCredential은 원문 형식과 현재 만료·폐기 상태를 확인한다.
// 인증 성공 시 마지막 사용 시각은 최대 한 시간에 한 번 갱신한다.
func (s *Store) ValidateIngestCredential(ctx context.Context, token string) (account, agent model.ID, resultErr error) {
	raw, ok := strings.CutPrefix(token, ingestCredentialPrefix)
	if !ok || len(raw) != 43 {
		return account, agent, model.ErrNotFound
	}
	secret, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != raw {
		return account, agent, model.ErrNotFound
	}
	hash := sha256.Sum256([]byte(token))
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return account, agent, storageError("ingest_validate_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			account, agent, resultErr = model.ID{}, model.ID{}, storageError("ingest_validate_rollback", errors.Join(resultErr, err))
		}
	}()
	now := time.Now().UTC()
	var accountID, agentID string
	var update bool
	err = tx.QueryRow(ctx, `SELECT c.account_id::text,a.agent_id::text,GREATEST(c.last_used_at,c.issued_at) <= $2::timestamptz - interval '1 hour'
 FROM public.ingest_credential c JOIN public.agent_registration a ON a.ingest_credential_id=c.credential_id AND a.account_id=c.account_id AND a.revoked_at IS NULL
 WHERE c.token_hash=$1 AND c.revoked_at IS NULL AND c.issued_at + interval '1 year' > $2::timestamptz AND GREATEST(c.last_used_at,c.issued_at) + interval '30 days' > $2::timestamptz
 FOR UPDATE OF c`, hash[:], now).Scan(&accountID, &agentID, &update)
	if errors.Is(err, pgx.ErrNoRows) {
		return account, agent, model.ErrNotFound
	}
	if err != nil {
		return account, agent, storageError("ingest_validate", err)
	}
	if update {
		if _, err := tx.Exec(ctx, `UPDATE public.ingest_credential SET last_used_at=$2 WHERE token_hash=$1`, hash[:], now); err != nil {
			return account, agent, storageError("ingest_last_used", err)
		}
	}
	account, err = model.ParseID(accountID)
	if err != nil {
		return model.ID{}, model.ID{}, storageError("ingest_account_decode", err)
	}
	agent, err = model.ParseID(agentID)
	if err != nil {
		return model.ID{}, model.ID{}, storageError("ingest_agent_decode", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ID{}, model.ID{}, storageError("ingest_validate_commit", err)
	}
	return account, agent, ctx.Err()
}

// ListIngestCredentials는 계정의 자격 메타데이터만 반환한다.
func (s *Store) ListIngestCredentials(ctx context.Context, account model.ID) ([]IngestCredential, error) {
	if !account.IsV7() {
		return nil, model.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT credential_id::text,label,issued_at,last_used_at,revoked_at FROM public.ingest_credential WHERE account_id=$1 ORDER BY issued_at DESC,credential_id DESC`, account.String())
	if err != nil {
		return nil, storageError("ingest_list", err)
	}
	defer rows.Close()
	var items []IngestCredential
	for rows.Next() {
		var item IngestCredential
		var rawID string
		if err := rows.Scan(&rawID, &item.Label, &item.IssuedAt, &item.LastUsedAt, &item.RevokedAt); err != nil {
			return nil, storageError("ingest_list_scan", err)
		}
		item.ID, err = model.ParseID(rawID)
		if err != nil {
			return nil, storageError("ingest_list_id", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, storageError("ingest_list_rows", err)
	}
	return items, ctx.Err()
}
