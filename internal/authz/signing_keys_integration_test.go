package authz

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSharedSigningKeyContractIntegration(t *testing.T) {
	firstStore, secondStore := newAuthenticationContractStores(t)
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	first := testService(t, &initialKeyBarrierStore{authStore: firstStore, arrived: arrived, release: release})
	second := testService(t, &initialKeyBarrierStore{authStore: secondStore, arrived: arrived, release: release})
	t.Run("concurrent initialization", func(t *testing.T) {
		type result struct {
			set jose.JSONWebKeySet
			err error
		}
		results := make(chan result, 2)
		var workers sync.WaitGroup
		t.Cleanup(func() { unblock(); workers.Wait() })
		for _, service := range []*Service{first, second} {
			workers.Go(func() { set, err := service.JWKS(t.Context()); results <- result{set, err} })
		}
		awaitSigningKeyLoad(t, arrived)
		awaitSigningKeyLoad(t, arrived)
		unblock()
		workers.Wait()
		close(results)
		var keyID string
		for result := range results {
			if result.err != nil || len(result.set.Keys) != 1 || !result.set.Keys[0].IsPublic() {
				t.Fatal("두 인스턴스의 초기 JWKS 준비에 실패했습니다")
			}
			if keyID != "" && keyID != result.set.Keys[0].KeyID {
				t.Fatal("두 인스턴스가 다른 활성 키를 사용합니다")
			}
			keyID = result.set.Keys[0].KeyID
		}
		keys, err := firstStore.SigningKeys(t.Context())
		if err != nil || len(keys) != 1 || keys[0].ID != keyID || keys[0].State != "active" {
			t.Fatal("동시 초기화가 활성 키 하나로 수렴하지 않았습니다")
		}
		duplicate, err := newSigningKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := secondStore.CreateSigningKey(t.Context(), duplicate); !errors.Is(err, store.ErrActiveSigningKeyExists) {
			t.Fatal("공유 DB의 활성 키 유일 제약을 확인하지 못했습니다")
		}
	})
	// 하위 시험 실패 뒤에도 막힌 초기화 요청을 남기지 않는다.
	unblock()
	if t.Failed() {
		return
	}
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	before, err := first.WebSession(t.Context(), id, "web")
	if err != nil {
		t.Fatal("첫 인스턴스의 서명에 실패했습니다")
	}
	t.Run("cross instance signatures", func(t *testing.T) {
		other, err := second.WebSession(t.Context(), id, "web")
		if err != nil {
			t.Fatal("두 번째 인스턴스의 서명에 실패했습니다")
		}
		assertSharedToken(t, second, before, id)
		assertSharedToken(t, first, other, id)
	})
	t.Run("rotation and restart retain old keys", func(t *testing.T) {
		if err := first.RotateSigningKey(t.Context()); err != nil {
			t.Fatal("공유 키 회전에 실패했습니다")
		}
		after, err := first.WebSession(t.Context(), id, "web")
		if err != nil {
			t.Fatal("회전 후 서명에 실패했습니다")
		}
		// 다른 인스턴스의 회전은 기존 1초 부재 재조회 제한 뒤 반영한다.
		second.cache.mu.Lock()
		second.cache.keysLoadedAt = time.Now().Add(-keyMissTTL)
		second.cache.mu.Unlock()
		assertSharedToken(t, second, after, id)
		assertSharedToken(t, second, before, id)
		assertSharedToken(t, first, before, id)
		restarted := testService(t, secondStore)
		assertSharedToken(t, restarted, before, id)
		assertSharedToken(t, restarted, after, id)
		set, err := restarted.JWKS(t.Context())
		if err != nil || len(set.Keys) != 2 {
			t.Fatal("회전 전후 공개 키를 보존하지 않았습니다")
		}
		encoded, err := json.Marshal(set)
		if err != nil || strings.Contains(string(encoded), `"d":`) || strings.Contains(string(encoded), `"k":`) {
			t.Fatal("JWKS가 개인 키 또는 대칭 키를 포함합니다")
		}
		parsed, err := jwt.ParseSigned(after.Raw, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil || len(parsed.Headers) != 1 {
			t.Fatal("실제 서명이 ES256이 아닙니다")
		}
		var independentlyVerified bool
		for _, key := range set.Keys {
			if !key.IsPublic() || !key.Valid() || key.Algorithm != string(jose.ES256) || key.Use != "sig" {
				t.Fatal("공개 키의 알고리즘·용도 계약이 다릅니다")
			}
			if key.KeyID == parsed.Headers[0].KeyID {
				var claims jwt.Claims
				independentlyVerified = parsed.Claims(key.Key, &claims) == nil && claims.Subject == id.String()
			}
		}
		if !independentlyVerified {
			t.Fatal("공개된 JWKS로 실제 서명을 독립 대조하지 못했습니다")
		}
		keys, err := firstStore.SigningKeys(t.Context())
		var active, retired int
		for _, key := range keys {
			if key.State == "active" {
				active++
			} else if key.State == "retired" {
				retired++
			}
		}
		if err != nil || active != 1 || retired != 1 {
			t.Fatal("회전 뒤 공유 DB의 활성·은퇴 상태가 다릅니다")
		}
	})
	t.Run("canceled rotation preserves key", func(t *testing.T) {
		previous, err := firstStore.ActiveSigningKey(t.Context())
		if err != nil {
			t.Fatal("현재 활성 키를 읽지 못했습니다")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := second.RotateSigningKey(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("회전 취소를 전파하지 않았습니다")
		}
		current, err := secondStore.ActiveSigningKey(t.Context())
		if err != nil || previous.ID != current.ID {
			t.Fatal("취소한 회전이 활성 키를 바꿨습니다")
		}
	})
}

type initialKeyBarrierStore struct {
	authStore
	arrived chan<- struct{}
	release <-chan struct{}
	seen    atomic.Bool
}

func (source *initialKeyBarrierStore) ActiveSigningKey(ctx context.Context) (store.SigningKey, error) {
	key, err := source.authStore.ActiveSigningKey(ctx)
	if source.seen.CompareAndSwap(false, true) && errors.Is(err, store.ErrNotFound) {
		source.arrived <- struct{}{}
		select {
		case <-source.release:
		case <-ctx.Done():
			return store.SigningKey{}, ctx.Err()
		}
	}
	return key, err
}

func assertSharedToken(t *testing.T, service *Service, token Token, expected model.ID) {
	t.Helper()
	if got, _, err := service.Verify(t.Context(), token.Raw, "web"); err != nil || got != expected {
		t.Fatal("공유 키를 사용한 서명 검증에 실패했습니다")
	}
}

// 시험별 DB에 정식 마이그레이션을 적용하고 공유 DB의 서명 키는 변경하지 않는다.
func newAuthenticationContractStores(t *testing.T) (*store.Store, *store.Store) {
	t.Helper()
	first, second, _ := newAuthenticationContractDatabase(t)
	return first, second
}

func newAuthenticationContractDatabase(t *testing.T) (*store.Store, *store.Store, *pgxpool.Pool) {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있습니다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 공유 키 DB 시험을 건너뜁니다")
	}
	config, err := pgxpool.ParseConfig(address)
	if err != nil {
		t.Fatal("시험 DB 연결 설정을 해석하지 못했습니다")
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("시험 DB 관리 연결을 준비하지 못했습니다")
	}
	t.Cleanup(admin.Close)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "auth_contract_" + strings.ReplaceAll(id.String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal("시험 전용 DB를 만들지 못했습니다(DB 생성 권한 필요)")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Error("시험 전용 DB를 정리하지 못했습니다")
		}
	})
	isolated, err := url.Parse(address)
	if err != nil || (isolated.Scheme != "postgres" && isolated.Scheme != "postgresql") {
		t.Fatal("TEST_DATABASE_URL은 PostgreSQL 연결 URL이어야 합니다")
	}
	isolated.Path, isolated.RawPath = "/"+name, ""
	// dbname·database 옵션이 새 경로를 덮어쓰지 않도록 제거한다.
	queryOptions := isolated.Query()
	queryOptions.Del("dbname")
	queryOptions.Del("database")
	isolated.RawQuery = queryOptions.Encode()
	isolatedAddress := isolated.String()
	pool, err := migrate.NewPool(t.Context(), isolatedAddress)
	if err != nil {
		t.Fatal("시험 전용 마이그레이션 연결을 준비하지 못했습니다")
	}
	t.Cleanup(pool.Close)
	var connectedDatabase string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&connectedDatabase); err != nil || connectedDatabase != name {
		t.Fatal("실제 연결이 시험 전용 DB가 아니므로 스키마 준비를 중단합니다")
	}
	loaded, err := migrate.Load(migrations.FS, ".", migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if err := migrate.Apply(t.Context(), pool, migration); err != nil {
			t.Fatal(err)
		}
	}
	makeStore := func() *store.Store {
		backend, err := store.New(t.Context(), isolatedAddress, "agent_context", nil, nil, "")
		if err != nil {
			t.Fatal("시험 전용 저장소를 준비하지 못했습니다")
		}
		t.Cleanup(backend.Close)
		return backend
	}
	return makeStore(), makeStore(), pool
}
