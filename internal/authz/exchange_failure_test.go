package authz

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"agent_context_sharing/internal/store"
)

type exchangeFailureStore struct {
	*memoryStore
	lookupError, keyError, createKeyError, consumeError, revokeError error
}

func (s *exchangeFailureStore) AuthorizationCodeForExchange(ctx context.Context, hash string, now time.Time) (store.AuthorizationCode, error) {
	if s.lookupError != nil {
		return store.AuthorizationCode{}, s.lookupError
	}
	return s.memoryStore.AuthorizationCodeForExchange(ctx, hash, now)
}

func (s *exchangeFailureStore) ActiveSigningKey(ctx context.Context) (store.SigningKey, error) {
	if s.keyError != nil {
		return store.SigningKey{}, s.keyError
	}
	return s.memoryStore.ActiveSigningKey(ctx)
}

func (s *exchangeFailureStore) CreateSigningKey(ctx context.Context, key store.SigningKey) error {
	if s.createKeyError != nil {
		return s.createKeyError
	}
	return s.memoryStore.CreateSigningKey(ctx, key)
}

func (s *exchangeFailureStore) ConsumeAuthorizationCode(ctx context.Context, hash, tokenID string, now, expiresAt time.Time) (store.AuthorizationCode, error) {
	if s.consumeError != nil {
		return store.AuthorizationCode{}, s.consumeError
	}
	return s.memoryStore.ConsumeAuthorizationCode(ctx, hash, tokenID, now, expiresAt)
}

func (s *exchangeFailureStore) RevokeToken(ctx context.Context, tokenID string, expiresAt time.Time) error {
	if s.revokeError != nil {
		return s.revokeError
	}
	return s.memoryStore.RevokeToken(ctx, tokenID, expiresAt)
}

func TestTokenExchangeStorageFailuresAreServerErrors(t *testing.T) {
	for _, stage := range []string{"코드조회", "서명키조회", "서명키생성", "코드소비", "재사용폐기", "서명키형식"} {
		t.Run(stage, func(t *testing.T) {
			backend := &exchangeFailureStore{memoryStore: newMemoryStore()}
			service := testService(t, backend)
			verifier := testVerifier("storage-failure")
			code := issueCode(t, service, verifier)
			switch stage {
			case "코드조회":
				backend.lookupError = errStoreUnavailable
			case "서명키조회":
				backend.keyError = errStoreUnavailable
			case "서명키생성":
				backend.createKeyError = errStoreUnavailable
			case "코드소비":
				backend.consumeError = errStoreUnavailable
			case "재사용폐기":
				if response := postToken(t, service, tokenForm(code, verifier)); response.Code != http.StatusOK {
					t.Fatal("첫 교환 실패")
				}
				backend.revokeError = errStoreUnavailable
			case "서명키형식":
				backend.keys = []store.SigningKey{{ID: "broken", State: "active", PrivateKey: "not-json"}}
			}
			_, err := exchangeWithDPoP(t, service, code, "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource)
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("내부 장애가 ErrUnavailable이 아니다: %v", err)
			}
			if stage != "서명키형식" && !errors.Is(err, errStoreUnavailable) {
				t.Errorf("오류 원인이 보존되지 않았다: %v", err)
			}
			response := postToken(t, service, tokenForm(code, verifier))
			if response.Code != http.StatusInternalServerError || response.Body.String() != `{"error":"server_error"}` {
				t.Fatalf("내부 장애 응답 = %d %s", response.Code, response.Body.String())
			}
			if stage != "재사용폐기" {
				stored := backend.codes[digest(code)]
				if stored.ConsumedAt != nil {
					t.Fatal("실패한 교환이 코드를 소비했다")
				}
				backend.lookupError, backend.keyError, backend.createKeyError, backend.consumeError = nil, nil, nil, nil
				if stage == "서명키형식" {
					backend.keys = nil
				}
				if retry := postToken(t, service, tokenForm(code, verifier)); retry.Code != http.StatusOK {
					t.Fatalf("새 proof로 같은 코드 재시도 실패: %d", retry.Code)
				}
			}
		})
	}
}

func TestTokenExchangeInvalidGrantsRemainClientErrors(t *testing.T) {
	for _, stage := range []string{"조회없음", "소비없음", "만료", "잘못된PKCE", "재사용조회", "재사용소비"} {
		t.Run(stage, func(t *testing.T) {
			backend := &exchangeFailureStore{memoryStore: newMemoryStore()}
			service := testService(t, backend)
			verifier := testVerifier("invalid-grant")
			code := issueCode(t, service, verifier)
			var token Token
			switch stage {
			case "조회없음":
				backend.lookupError = fmt.Errorf("lookup: %w", store.ErrNotFound)
			case "소비없음":
				backend.consumeError = fmt.Errorf("consume: %w", store.ErrNotFound)
			case "만료":
				stored := backend.codes[digest(code)]
				stored.ExpiresAt = time.Now().Add(-time.Second)
				backend.codes[digest(code)] = stored
			case "잘못된PKCE":
				verifier = testVerifier("wrong")
			case "재사용조회", "재사용소비":
				var err error
				token, err = exchangeWithDPoP(t, service, code, "test-client", "http://127.0.0.1/callback", verifier, service.config.Resource)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := service.Verify(t.Context(), token.Raw, service.config.Resource); err != nil {
					t.Fatal(err)
				}
				if stage == "재사용소비" {
					stored := backend.codes[digest(code)]
					stored.ConsumedAt, stored.IssuedTokenExpiresAt, stored.IssuedTokenID = nil, nil, ""
					backend.codes[digest(code)] = stored
					backend.consumeError = fmt.Errorf("concurrent exchange: %w", store.CodeUsedError{TokenID: token.ID, TokenExpiresAt: token.ExpiresAt})
				}
			}
			response := postToken(t, service, tokenForm(code, verifier))
			var result struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusBadRequest || result.Error != "invalid_grant" {
				t.Fatalf("무효 코드 응답 = %d %s", response.Code, response.Body.String())
			}
			if token.ID != "" {
				if _, _, err := service.Verify(t.Context(), token.Raw, service.config.Resource); !errors.Is(err, ErrInvalidCredential) {
					t.Fatalf("재사용 토큰이 캐시에서 즉시 거부되지 않았다: %v", err)
				}
			}
		})
	}
}
