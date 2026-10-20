package store

import (
	"context"
	"errors"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"github.com/jackc/pgx/v5"
)

const activeAgentLimit = 128

func invalidAgent() error {
	return model.FieldError{Field: "agent_id", Message: "등록된 행위자를 확인해 주십시오."}
}

func invalidRegistration() error {
	return model.FieldError{Field: "registration_key", Message: "등록 키를 확인해 주십시오."}
}

// lockAgentAccount는 등록·폐기의 상한 판정을 계정 전체에서 직렬화한다.
func lockAgentAccount(ctx context.Context, tx pgx.Tx, account model.ID) error {
	var found string
	err := tx.QueryRow(ctx, `SELECT account_id::text FROM public.account WHERE account_id=$1 FOR UPDATE`, account.String()).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrNotFound
	}
	return storageError("agent_account_lock", err)
}

// RegisterAgent는 현재 인증 계정·검증 client에 등록 키를 결합한다.
// 활성 재조회는 128개 상한에서도 허용하고 폐기 키를 새 ID로 복구하지 않는다.
func (s *Store) RegisterAgent(ctx context.Context, account model.ID, clientID, key string) (result model.ID, resultErr error) {
	if err := ctx.Err(); err != nil {
		return model.ID{}, err
	}
	if !account.IsV7() || clientID == "" {
		return model.ID{}, invalidRegistration()
	}
	if err := model.ValidateRegistrationKey(key); err != nil {
		return model.ID{}, err
	}
	// 각 명령의 새 스냅숏으로 잠금 대기 중 커밋된 등록·폐기와 현재 개수를 함께 본다.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.ID{}, storageError("agent_register_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			result, resultErr = model.ID{}, storageError("agent_register_rollback", errors.Join(resultErr, err))
		}
	}()
	if err := lockAgentAccount(ctx, tx, account); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.ID{}, invalidRegistration()
		}
		return model.ID{}, err
	}
	var rawID string
	var revoked *time.Time
	err = tx.QueryRow(ctx, `SELECT agent_id::text,revoked_at FROM public.agent_registration WHERE account_id=$1 AND client_id=$2 AND registration_key=$3`, account.String(), clientID, key).Scan(&rawID, &revoked)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return model.ID{}, storageError("agent_registration_lookup", err)
	}
	if err == nil && revoked != nil {
		return model.ID{}, invalidRegistration()
	}
	if errors.Is(err, pgx.ErrNoRows) {
		var count int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.agent_registration WHERE account_id=$1 AND revoked_at IS NULL`, account.String()).Scan(&count); err != nil {
			return model.ID{}, storageError("agent_registration_count", err)
		}
		if err := plan.CheckIncrease("agent_registrations_per_account", count, 1, activeAgentLimit); err != nil {
			return model.ID{}, err
		}
		id, err := model.NewID()
		if err != nil {
			return model.ID{}, storageError("agent_identifier_create", err)
		}
		err = tx.QueryRow(ctx, `INSERT INTO public.agent_registration (agent_id,account_id,client_id,registration_key,created_at)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT (account_id,client_id,registration_key)
			DO UPDATE SET registration_key=EXCLUDED.registration_key WHERE agent_registration.revoked_at IS NULL RETURNING agent_id::text`, id.String(), account.String(), clientID, key, time.Now().UTC()).Scan(&rawID)
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ID{}, invalidRegistration()
		}
		if err != nil {
			return model.ID{}, storageError("agent_registration_create", err)
		}
	}
	id, err := model.ParseID(rawID)
	if err != nil {
		return model.ID{}, storageError("agent_identifier_decode", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ID{}, storageError("agent_register_commit", err)
	}
	if err := ctx.Err(); err != nil {
		return model.ID{}, err
	}
	return id, nil
}

// ValidateAgent는 UUID만으로 신뢰하지 않고 매번 현재 활성 계정/client 결합을 조회한다.
func (s *Store) ValidateAgent(ctx context.Context, account model.ID, clientID string, agent model.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !account.IsV7() || clientID == "" || !agent.IsV7() {
		return invalidAgent()
	}
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.agent_registration WHERE agent_id=$1 AND account_id=$2 AND client_id=$3 AND revoked_at IS NULL)`, agent.String(), account.String(), clientID).Scan(&allowed)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err != nil {
		return storageError("agent_binding_lookup", err)
	}
	if !allowed {
		return invalidAgent()
	}
	return nil
}

// RevokeAgent는 웹 세션에서 확인한 소유 계정만 받는다. 반복 폐기는 같은 상태를 유지한다.
// 다른 계정·없는 ID는 같은 오류이며 등록 행·과거 기여·감사를 삭제하거나 재작성하지 않는다.
func (s *Store) RevokeAgent(ctx context.Context, account, agent model.ID) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !account.IsV7() || !agent.IsV7() {
		return invalidAgent()
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return storageError("agent_revoke_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			resultErr = storageError("agent_revoke_rollback", errors.Join(resultErr, err))
		}
	}()
	if err := lockAgentAccount(ctx, tx, account); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return invalidAgent()
		}
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE public.agent_registration SET revoked_at=COALESCE(revoked_at,$3) WHERE agent_id=$1 AND account_id=$2`, agent.String(), account.String(), time.Now().UTC())
	if err != nil {
		return storageError("agent_revoke_update", err)
	}
	if result.RowsAffected() != 1 {
		return invalidAgent()
	}
	if err := tx.Commit(ctx); err != nil {
		return storageError("agent_revoke_commit", err)
	}
	return ctx.Err()
}
