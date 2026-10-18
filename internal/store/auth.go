package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"agent_context_sharing/internal/model"
)

// ErrLoginIDExists는 계정 로그인 아이디의 유일 제약 위반만 나타낸다.
var ErrLoginIDExists = errors.New("login_id_exists")

// Account은 인증에 필요한 계정 식별자와 저장된 자격 증명이다.
type Account struct {
	ID           model.ID
	LoginID      string
	PasswordHash string
	CreatedAt    time.Time
}

// SigningKey는 데이터베이스에 보관하는 서명 키의 직렬화된 형태다.
type SigningKey struct {
	ID         string
	Algorithm  string
	PublicKey  string
	PrivateKey string
	State      string
	CreatedAt  time.Time
}

// CreateAccount는 로그인 아이디가 중복되지 않는 계정 행을 만든다.
func (s *Store) CreateAccount(ctx context.Context, account Account) error {
	if !account.ID.IsV7() || account.LoginID == "" || account.PasswordHash == "" || account.CreatedAt.IsZero() {
		return fmt.Errorf("계정 생성 인자가 올바르지 않다")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO public.account (account_id, login_id, password_hash, created_at) VALUES ($1, $2, $3, $4)`, account.ID.String(), account.LoginID, account.PasswordHash, account.CreatedAt)
	if failure, ok := errors.AsType[*pgconn.PgError](err); ok && failure.Code == "23505" && failure.SchemaName == "public" && failure.TableName == "account" && failure.ConstraintName == "account_login_id_idx" {
		return ErrLoginIDExists
	}
	if err != nil {
		return storageError("account_create", err)
	}
	return nil
}

// AccountByLoginID는 자격 증명 대조를 위해 계정을 읽는다.
func (s *Store) AccountByLoginID(ctx context.Context, loginID string) (Account, error) {
	if loginID == "" {
		return Account{}, fmt.Errorf("계정 조회 인자가 올바르지 않다")
	}
	var account Account
	var id string
	err := s.pool.QueryRow(ctx, `SELECT account_id, login_id, password_hash, created_at FROM public.account WHERE login_id = $1`, loginID).Scan(&id, &account.LoginID, &account.PasswordHash, &account.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, storageError("account_lookup", err)
	}
	parsed, err := model.ParseID(id)
	if err != nil {
		return Account{}, storageError("account_identifier", err)
	}
	account.ID, account.CreatedAt = parsed, account.CreatedAt.UTC()
	return account, nil
}

// UpdatePasswordHashIfMatches는 읽은 기존 해시가 아직 같을 때만 비밀번호 해시를 바꾼다.
// false, nil은 다른 로그인 요청 또는 이후 비밀번호 변경이 먼저 반영됐음을 뜻한다.
func (s *Store) UpdatePasswordHashIfMatches(ctx context.Context, accountID model.ID, oldHash, newHash string) (bool, error) {
	if !accountID.IsV7() || oldHash == "" || newHash == "" {
		return false, fmt.Errorf("비밀번호 해시 갱신 인자가 올바르지 않다")
	}
	result, err := s.pool.Exec(ctx, `UPDATE public.account SET password_hash = $3 WHERE account_id = $1 AND password_hash = $2`, accountID.String(), oldHash, newHash)
	if err != nil {
		return false, storageError("password_hash_update", err)
	}
	return result.RowsAffected() == 1, nil
}

// RevokeToken은 만료 시각까지 토큰 식별자를 폐기 목록에 둔다.
func (s *Store) RevokeToken(ctx context.Context, tokenID string, expiresAt time.Time) error {
	if tokenID == "" || expiresAt.IsZero() {
		return fmt.Errorf("토큰 폐기 인자가 올바르지 않다")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO public.revoked_token (token_id, expires_at) VALUES ($1,$2) ON CONFLICT (token_id) DO UPDATE SET expires_at = EXCLUDED.expires_at`, tokenID, expiresAt)
	if err != nil {
		return fmt.Errorf("토큰 폐기: %w", err)
	}
	return nil
}

// IsTokenRevoked는 아직 만료되지 않은 폐기 목록 항목을 확인한다.
// RevokedTokenIDs는 아직 만료되지 않은 폐기 토큰 식별자를 모두 읽는다.
//
// 「토큰 검증」이 폐기 목록을 요청마다 가져오지 않기로 했으므로 접근 계층이 이 목록을
// 메모리에 두고 쓴다. 폐기 행은 토큰 만료까지만 남으므로 목록의 크기가 제한된다.
func (s *Store) RevokedTokenIDs(ctx context.Context, now time.Time) ([]string, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("폐기 목록 조회 시각이 비어 있다")
	}
	rows, err := s.pool.Query(ctx, `SELECT token_id FROM public.revoked_token WHERE expires_at > $1`, now)
	if err != nil {
		return nil, fmt.Errorf("폐기 목록 조회: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("폐기 목록 행 해석: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("폐기 목록 행 읽기: %w", err)
	}
	return ids, nil
}

func (s *Store) IsTokenRevoked(ctx context.Context, tokenID string, now time.Time) (bool, error) {
	if tokenID == "" || now.IsZero() {
		return false, fmt.Errorf("토큰 폐기 조회 인자가 올바르지 않다")
	}
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.revoked_token WHERE token_id = $1 AND expires_at > $2)`, tokenID, now).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("토큰 폐기 조회: %w", err)
	}
	return found, nil
}

// ActiveSigningKey는 현재 토큰 발급에 쓸 활성 키를 읽는다.
func (s *Store) ActiveSigningKey(ctx context.Context) (SigningKey, error) {
	return s.signingKey(ctx, `SELECT key_id, algorithm, public_key, private_key, state, created_at FROM public.signing_key WHERE state = 'active' ORDER BY created_at DESC LIMIT 1`)
}

// SigningKey는 JWT 헤더의 kid에 맞는 검증 키 하나를 읽는다.
func (s *Store) SigningKey(ctx context.Context, keyID string) (SigningKey, error) {
	if keyID == "" {
		return SigningKey{}, ErrNotFound
	}
	return s.signingKey(ctx, `SELECT key_id, algorithm, public_key, private_key, state, created_at FROM public.signing_key WHERE key_id = $1`, keyID)
}

// SigningKeys는 JWKS 공개와 이전 토큰 검증에 필요한 모든 키를 읽는다.
func (s *Store) SigningKeys(ctx context.Context) ([]SigningKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT key_id, algorithm, public_key, private_key, state, created_at FROM public.signing_key ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("서명 키 목록 조회: %w", err)
	}
	defer rows.Close()
	var keys []SigningKey
	for rows.Next() {
		var key SigningKey
		if err := rows.Scan(&key.ID, &key.Algorithm, &key.PublicKey, &key.PrivateKey, &key.State, &key.CreatedAt); err != nil {
			return nil, fmt.Errorf("서명 키 해석: %w", err)
		}
		key.CreatedAt = key.CreatedAt.UTC()
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("서명 키 행 읽기: %w", err)
	}
	return keys, nil
}

// CreateSigningKey는 새 키를 활성 또는 은퇴 상태로 저장한다.
func (s *Store) CreateSigningKey(ctx context.Context, key SigningKey) error {
	if !validSigningKey(key) {
		return fmt.Errorf("서명 키 생성 인자가 올바르지 않다")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO public.signing_key (key_id, algorithm, public_key, private_key, state, created_at) VALUES ($1,$2,$3,$4,$5,$6)`, key.ID, key.Algorithm, key.PublicKey, key.PrivateKey, key.State, key.CreatedAt)
	if isUniqueViolation(err) && key.State == "active" {
		return ErrActiveSigningKeyExists
	}
	if err != nil {
		return fmt.Errorf("서명 키 저장: %w", err)
	}
	return nil
}

// RotateSigningKey는 현재 활성 키를 은퇴시키고 새 키를 하나의 트랜잭션에서 활성화한다.
func (s *Store) RotateSigningKey(ctx context.Context, key SigningKey) error {
	if !validSigningKey(key) || key.State != "active" {
		return fmt.Errorf("서명 키 회전 인자가 올바르지 않다")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("서명 키 회전 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE public.signing_key SET state = 'retired' WHERE state = 'active'`); err != nil {
		return fmt.Errorf("기존 서명 키 은퇴: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.signing_key (key_id, algorithm, public_key, private_key, state, created_at) VALUES ($1,$2,$3,$4,'active',$5)`, key.ID, key.Algorithm, key.PublicKey, key.PrivateKey, key.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return ErrActiveSigningKeyExists
		}
		return fmt.Errorf("새 서명 키 저장: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("서명 키 회전 커밋: %w", err)
	}
	return nil
}

func (s *Store) signingKey(ctx context.Context, query string, args ...any) (SigningKey, error) {
	var key SigningKey
	err := s.pool.QueryRow(ctx, query, args...).Scan(&key.ID, &key.Algorithm, &key.PublicKey, &key.PrivateKey, &key.State, &key.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SigningKey{}, ErrNotFound
	}
	if err != nil {
		return SigningKey{}, fmt.Errorf("서명 키 조회: %w", err)
	}
	key.CreatedAt = key.CreatedAt.UTC()
	return key, nil
}

func validSigningKey(key SigningKey) bool {
	return key.ID != "" && key.Algorithm != "" && key.PublicKey != "" && key.PrivateKey != "" && (key.State == "active" || key.State == "retired") && !key.CreatedAt.IsZero()
}
