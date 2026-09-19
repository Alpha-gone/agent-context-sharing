package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// Account은 인증에 필요한 계정 식별자와 저장된 자격 증명이다.
type Account struct {
	ID           model.ID
	LoginID      string
	PasswordHash string
	CreatedAt    time.Time
}

// AuthorizationCode는 원문 없이 보관하는 인가 코드의 교환 정보다.
type AuthorizationCode struct {
	Hash                 string
	ClientID             string
	AccountID            model.ID
	RedirectURI          string
	CodeChallenge        string
	Resource             string
	IssuedAt             time.Time
	ExpiresAt            time.Time
	ConsumedAt           *time.Time
	IssuedTokenID        string
	IssuedTokenExpiresAt *time.Time
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

// CodeUsedError는 이미 소비된 인가 코드를 다시 제시했음을 나타낸다.
type CodeUsedError struct {
	TokenID        string
	IssuedAt       time.Time
	TokenExpiresAt time.Time
}

// Error는 인가 코드 재사용 오류를 만든다.
func (CodeUsedError) Error() string { return "인가 코드가 이미 사용됐다" }

// CreateAccount는 로그인 아이디가 중복되지 않는 계정 행을 만든다.
func (s *Store) CreateAccount(ctx context.Context, account Account) error {
	if !account.ID.IsV7() || account.LoginID == "" || account.PasswordHash == "" || account.CreatedAt.IsZero() {
		return fmt.Errorf("계정 생성 인자가 올바르지 않다")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO public.account (account_id, login_id, password_hash, created_at) VALUES ($1, $2, $3, $4)`, account.ID.String(), account.LoginID, account.PasswordHash, account.CreatedAt)
	if err != nil {
		return fmt.Errorf("계정 생성: %w", err)
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
		return Account{}, fmt.Errorf("계정 조회: %w", err)
	}
	parsed, err := model.ParseID(id)
	if err != nil {
		return Account{}, fmt.Errorf("저장된 계정 식별자: %w", err)
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
		return false, fmt.Errorf("비밀번호 해시 갱신: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

// CreateAuthorizationCode는 인가 코드 해시만 저장한다.
func (s *Store) CreateAuthorizationCode(ctx context.Context, code AuthorizationCode) error {
	if !validAuthorizationCode(code) {
		return fmt.Errorf("인가 코드 생성 인자가 올바르지 않다")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO public.authorization_code (code_hash, client_id, account_id, redirect_uri, code_challenge, resource, issued_at, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, code.Hash, code.ClientID, code.AccountID.String(), code.RedirectURI, code.CodeChallenge, code.Resource, code.IssuedAt, code.ExpiresAt)
	if err != nil {
		return fmt.Errorf("인가 코드 저장: %w", err)
	}
	return nil
}

// AuthorizationCodeForExchange는 아직 소비되지 않고 만료되지 않은 인가 코드를 읽는다.
// 이미 소비된 코드는 발급 토큰을 폐기할 수 있도록 CodeUsedError로 구분한다.
func (s *Store) AuthorizationCodeForExchange(ctx context.Context, hash string, now time.Time) (AuthorizationCode, error) {
	if hash == "" || now.IsZero() {
		return AuthorizationCode{}, fmt.Errorf("인가 코드 조회 인자가 올바르지 않다")
	}
	code, err := s.authorizationCode(ctx, `
		SELECT code_hash, client_id, account_id, redirect_uri, code_challenge, resource,
		       issued_at, expires_at, consumed_at, issued_token_id, issued_token_expires_at
		FROM public.authorization_code
		WHERE code_hash = $1`, hash)
	if errors.Is(err, ErrNotFound) {
		return AuthorizationCode{}, ErrNotFound
	}
	if err != nil {
		return AuthorizationCode{}, err
	}
	// 소비 여부를 만료보다 먼저 본다. 코드 수명은 60초이고 접근 토큰은 1시간이므로,
	// 만료를 먼저 보면 탈취한 코드를 60초 뒤에 다시 제시할 때 재사용이 아니라 없는
	// 코드로 판정되어 그 코드로 발급한 토큰이 폐기되지 않는다. 「인가 코드 흐름」이
	// 소비 행을 남겨 두는 이유가 재사용 감지와 폐기 대상 찾기다.
	if code.ConsumedAt != nil {
		return AuthorizationCode{}, codeUsedError(code)
	}
	if !code.ExpiresAt.After(now) {
		return AuthorizationCode{}, ErrNotFound
	}
	return code, nil
}

// ConsumeAuthorizationCode는 아직 소비되지 않고 만료되지 않은 코드만 조건부 갱신으로
// 소비하며, 그 교환에서 발급한 토큰 식별자와 만료 시각을 같은 문장에서 기록한다.
func (s *Store) ConsumeAuthorizationCode(ctx context.Context, hash, tokenID string, now, tokenExpiresAt time.Time) (AuthorizationCode, error) {
	if hash == "" || tokenID == "" || now.IsZero() || tokenExpiresAt.IsZero() {
		return AuthorizationCode{}, fmt.Errorf("인가 코드 소비 인자가 올바르지 않다")
	}
	code, err := s.authorizationCode(ctx, `
		UPDATE public.authorization_code
		SET consumed_at = $3, issued_token_id = $2, issued_token_expires_at = $4
		WHERE code_hash = $1 AND consumed_at IS NULL AND expires_at > $3
		RETURNING code_hash, client_id, account_id, redirect_uri, code_challenge, resource,
		          issued_at, expires_at, consumed_at, issued_token_id, issued_token_expires_at`, hash, tokenID, now, tokenExpiresAt)
	if err == nil {
		return code, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return AuthorizationCode{}, err
	}
	code, err = s.authorizationCode(ctx, `
		SELECT code_hash, client_id, account_id, redirect_uri, code_challenge, resource,
		       issued_at, expires_at, consumed_at, issued_token_id, issued_token_expires_at
		FROM public.authorization_code
		WHERE code_hash = $1`, hash)
	if errors.Is(err, ErrNotFound) {
		return AuthorizationCode{}, ErrNotFound
	}
	if err != nil {
		return AuthorizationCode{}, err
	}
	// 소비 여부를 만료보다 먼저 본다. 코드 수명은 60초이고 접근 토큰은 1시간이므로,
	// 만료를 먼저 보면 탈취한 코드를 60초 뒤에 다시 제시할 때 재사용이 아니라 없는
	// 코드로 판정되어 그 코드로 발급한 토큰이 폐기되지 않는다. 「인가 코드 흐름」이
	// 소비 행을 남겨 두는 이유가 재사용 감지와 폐기 대상 찾기다.
	if code.ConsumedAt != nil {
		return AuthorizationCode{}, codeUsedError(code)
	}
	if !code.ExpiresAt.After(now) {
		return AuthorizationCode{}, ErrNotFound
	}
	return AuthorizationCode{}, ErrNotFound
}

func (s *Store) authorizationCode(ctx context.Context, query string, args ...any) (AuthorizationCode, error) {
	var code AuthorizationCode
	var accountID string
	var issuedTokenID *string
	var issuedTokenExpiresAt *time.Time
	err := s.pool.QueryRow(ctx, query, args...).Scan(
		&code.Hash, &code.ClientID, &accountID, &code.RedirectURI, &code.CodeChallenge, &code.Resource,
		&code.IssuedAt, &code.ExpiresAt, &code.ConsumedAt, &issuedTokenID, &issuedTokenExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthorizationCode{}, ErrNotFound
	}
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("인가 코드 조회: %w", err)
	}
	parsed, err := model.ParseID(accountID)
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("저장된 인가 코드 계정 식별자: %w", err)
	}
	code.AccountID = parsed
	code.IssuedAt, code.ExpiresAt = code.IssuedAt.UTC(), code.ExpiresAt.UTC()
	if code.ConsumedAt != nil {
		consumedAt := code.ConsumedAt.UTC()
		code.ConsumedAt = &consumedAt
	}
	if issuedTokenID != nil {
		code.IssuedTokenID = *issuedTokenID
	}
	if issuedTokenExpiresAt != nil {
		expiresAt := issuedTokenExpiresAt.UTC()
		code.IssuedTokenExpiresAt = &expiresAt
	}
	return code, nil
}

func codeUsedError(code AuthorizationCode) CodeUsedError {
	used := CodeUsedError{TokenID: code.IssuedTokenID, IssuedAt: code.IssuedAt}
	if code.IssuedTokenExpiresAt != nil {
		used.TokenExpiresAt = *code.IssuedTokenExpiresAt
	}
	return used
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

func validAuthorizationCode(code AuthorizationCode) bool {
	return code.Hash != "" && code.ClientID != "" && code.AccountID.IsV7() && code.RedirectURI != "" && code.CodeChallenge != "" && code.Resource != "" && !code.IssuedAt.IsZero() && !code.ExpiresAt.IsZero() && code.ExpiresAt.After(code.IssuedAt)
}

func validSigningKey(key SigningKey) bool {
	return key.ID != "" && key.Algorithm != "" && key.PublicKey != "" && key.PrivateKey != "" && (key.State == "active" || key.State == "retired") && !key.CreatedAt.IsZero()
}
