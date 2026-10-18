package authz

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestStoredPublicSigningKeyBoundary(t *testing.T) {
	for _, mode := range []string{"private in public", "symmetric", "malformed", "wrong kid", "row algorithm", "jwk algorithm", "wrong use", "wrong curve"} {
		t.Run(mode, func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			token, err := service.WebSession(t.Context(), id, "web")
			if err != nil {
				t.Fatal("합성 세션 준비에 실패했습니다")
			}
			item := backend.keys[0]
			var public jose.JSONWebKey
			if err := json.Unmarshal([]byte(item.PublicKey), &public); err != nil {
				t.Fatal("합성 공개 키 준비에 실패했습니다")
			}
			switch mode {
			case "private in public":
				item.PublicKey = item.PrivateKey
			case "symmetric":
				public.Key = []byte(strings.Repeat("synthetic", 8))
			case "malformed":
				item.PublicKey = `{"kty":"synthetic-sensitive-marker"}`
			case "wrong kid":
				public.KeyID = "different"
			case "row algorithm":
				item.Algorithm = string(jose.HS256)
			case "jwk algorithm":
				public.Algorithm = string(jose.HS256)
			case "wrong use":
				public.Use = "enc"
			case "wrong curve":
				private, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				public.Key = private.Public()
			}
			if mode != "private in public" && mode != "malformed" {
				item.PublicKey = marshalSyntheticKey(t, public)
			}
			backend.keys[0] = item
			event := &droppedDiagnostic{}
			service.SetDiagnostics(event)
			set, err := service.JWKS(t.Context())
			if !errors.Is(err, ErrUnavailable) || len(set.Keys) != 0 || strings.Contains(err.Error(), "synthetic-sensitive-marker") {
				t.Fatal("잘못된 공개 키를 공개하거나 키 장애의 원문을 노출했습니다")
			}
			if _, _, err := service.Verify(t.Context(), token.Raw, "web"); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalidCredential) {
				t.Fatal("저장된 키 장애를 자격 불일치로 처리했습니다")
			}
			if event.reason != "authentication_unavailable" || event.result != "failed" || len(service.cache.keys) != 0 || !service.cache.keysLoadedAt.IsZero() {
				t.Fatal("잘못된 키가 캐시에 남거나 진단이 고정된 장애 코드가 아닙니다")
			}
			handler, err := NewHandler(service)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.JWKS(response, httptest.NewRequest(http.MethodGet, "/jwks.json", nil))
			if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != `{"error":"server_error"}` || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("키 장애의 HTTP 응답이 고정된 비공개 오류가 아닙니다")
			}
		})
	}
}

func TestStoredPrivateSigningKeyBoundary(t *testing.T) {
	for _, mode := range []string{"public in private", "malformed", "wrong kid", "wrong algorithm", "wrong pair"} {
		t.Run(mode, func(t *testing.T) {
			item, err := newSigningKey()
			if err != nil {
				t.Fatal(err)
			}
			var private jose.JSONWebKey
			if err := json.Unmarshal([]byte(item.PrivateKey), &private); err != nil {
				t.Fatal("합성 개인 키 준비에 실패했습니다")
			}
			switch mode {
			case "public in private":
				private = private.Public()
			case "malformed":
				item.PrivateKey = `{"kty":"synthetic-sensitive-marker"}`
			case "wrong kid":
				private.KeyID = "different"
			case "wrong algorithm":
				private.Algorithm = string(jose.HS256)
			case "wrong pair":
				key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				private.Key = key
			}
			if mode != "malformed" {
				item.PrivateKey = marshalSyntheticKey(t, private)
			}
			backend := newMemoryStore()
			backend.keys = []store.SigningKey{item}
			service := testService(t, backend)
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			token, err := service.WebSession(t.Context(), id, "web")
			if !errors.Is(err, ErrUnavailable) || token.Raw != "" || strings.Contains(err.Error(), "synthetic-sensitive-marker") {
				t.Fatal("잘못된 개인 키로 발급하거나 키 원문을 오류에 노출했습니다")
			}
		})
	}
}

func TestConcurrentKnownKeyLoadsShareOneQuery(t *testing.T) {
	item, err := newSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	backend := &blockedKeyStore{memoryStore: newMemoryStore(), started: make(chan struct{}), release: make(chan struct{})}
	backend.keys = []store.SigningKey{item}
	release := sync.OnceFunc(func() { close(backend.release) })
	t.Cleanup(release)
	cache := newVerificationCache()
	results := make(chan error, 40)
	var workers sync.WaitGroup
	for range 40 {
		workers.Go(func() {
			key, err := cache.publicKey(t.Context(), backend, item.ID)
			if err == nil && (!key.IsPublic() || key.KeyID != item.ID) {
				err = errors.New("공개 키의 결합이 다릅니다")
			}
			results <- err
		})
	}
	awaitSigningKeyLoad(t, backend.started)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.publicKey(ctx, backend, item.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("대기 요청의 취소를 전파하지 않았습니다")
	}
	release()
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("병행 공개 키 조회에 실패했습니다")
		}
	}
	if backend.reads.Load() != 1 || len(cache.keys) != 1 {
		t.Fatal("병행 공개 키 조회가 하나로 합쳐지지 않았습니다")
	}
}

func TestExpiredRevocationCacheFailureAndRecovery(t *testing.T) {
	backend := &failingStore{memoryStore: newMemoryStore()}
	service := testService(t, backend)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(t.Context(), id, "web")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); err != nil {
		t.Fatal("캐시 준비에 실패했습니다")
	}
	service.cache.loadedAt = time.Now().Add(-revocationTTL)
	backend.revocationFails = true
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); !errors.Is(err, ErrUnavailable) || !errors.Is(err, errStoreUnavailable) {
		t.Fatal("오래된 폐기 목록으로 장애 중 인증을 허용했습니다")
	}
	backend.revocationFails = false
	if err := backend.RevokeToken(t.Context(), token.ID, token.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("장애 회복 뒤 공유 폐기를 다시 읽지 않았습니다")
	}
}

func marshalSyntheticKey(t *testing.T, key jose.JSONWebKey) string {
	t.Helper()
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal("합성 키 JSON 구성에 실패했습니다")
	}
	return string(encoded)
}

func awaitSigningKeyLoad(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("서명 키 조회가 시작되지 않았습니다")
	}
}

func TestJWTSignatureAlgorithmAllowlist(t *testing.T) {
	for _, algorithm := range []jose.SignatureAlgorithm{jose.HS256, jose.ES384} {
		t.Run(string(algorithm), func(t *testing.T) {
			backend := &countingStore{memoryStore: newMemoryStore()}
			service := testService(t, backend)
			item, err := service.activeKey(t.Context())
			if err != nil {
				t.Fatal("서명 키 준비에 실패했습니다")
			}
			var signingKey any = []byte(item.PublicKey)
			if algorithm == jose.ES384 {
				key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				signingKey = key
			}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: algorithm, Key: signingKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", item.ID))
			if err != nil {
				t.Fatal("금지 알고리즘 합성 서명기 준비에 실패했습니다")
			}
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: service.config.Issuer, Subject: id.String(), Audience: jwt.Audience{"web"}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)), ID: "synthetic-token"}).Serialize()
			if err != nil {
				t.Fatal("금지 알고리즘 합성 토큰 생성에 실패했습니다")
			}
			if _, _, err := service.Verify(t.Context(), raw, "web"); !errors.Is(err, ErrInvalidCredential) || backend.signingKeyReads.Load() != 0 {
				t.Fatal("ES256 이외의 알고리즘을 키 조회 전에 거부하지 않았습니다")
			}
		})
	}
}
