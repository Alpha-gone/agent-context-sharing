package authz

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOAuthCodeExchangeIntegration(t *testing.T) {
	firstStore, secondStore, pool := newAuthenticationContractDatabase(t)
	config := oauthTestConfig()
	config.AuthorizationCodes = firstStore
	first, err := New(firstStore, config)
	if err != nil {
		t.Fatal(err)
	}
	config.AuthorizationCodes = secondStore
	second, err := New(secondStore, config)
	if err != nil {
		t.Fatal(err)
	}
	account, err := first.Register(t.Context(), "oauth_account", "correct password")
	if err != nil {
		t.Fatal("시험 계정 준비 실패")
	}
	var clock atomic.Int64
	now := time.Now().UTC().Truncate(time.Second)
	clock.Store(now.Unix())
	first.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	second.now = first.now
	issue := func(t *testing.T) string {
		t.Helper()
		raw, err := first.IssueAuthorizationCode(t.Context(), account, validAuthorizationRequest())
		if err != nil {
			t.Fatal("인가 코드 발급 실패")
		}
		return raw
	}
	exchange := func(service *Service, raw string) (OAuthTokens, error) {
		return service.ExchangeAuthorizationCode(t.Context(), raw, "first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", oauthVerifier)
	}
	assertUnused := func(t *testing.T, raw string) {
		t.Helper()
		hash := sha256.Sum256([]byte(raw))
		var consumed bool
		if err := pool.QueryRow(t.Context(), `SELECT consumed_at IS NOT NULL FROM public.oauth_authorization_code WHERE code_hash=$1`, hash[:]).Scan(&consumed); err != nil || consumed {
			t.Fatal("실패한 교환이 코드를 소비했습니다")
		}
	}
	t.Run("binding failures preserve code", func(t *testing.T) {
		raw := issue(t)
		for _, test := range []struct{ name, client, callback, resource, verifier string }{
			{"wrong verifier", "first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", strings.Repeat("a", 43)},
			{"other registered client", "native", "http://127.0.0.1:4000/callback?mode=fixed", "https://resource.test/mcp", oauthVerifier},
			{"wrong callback", "first", "http://127.0.0.1:49153/callback", "https://resource.test/mcp", oauthVerifier},
			{"wrong resource", "first", "http://127.0.0.1:49152/callback", "https://other.test/mcp", oauthVerifier},
		} {
			t.Run(test.name, func(t *testing.T) {
				tokens, err := second.ExchangeAuthorizationCode(t.Context(), raw, test.client, test.callback, test.resource, test.verifier)
				failure, ok := errors.AsType[*OAuthError](err)
				if !ok || failure.Code != "invalid_grant" || tokens.AccessToken != "" || tokens.RefreshToken != "" {
					t.Fatal("잘못된 결합으로 토큰 발급")
				}
				assertUnused(t, raw)
			})
		}
	})
	t.Run("expiry at sixty seconds", func(t *testing.T) {
		raw := issue(t)
		clock.Add(60)
		defer clock.Store(now.Unix())
		if tokens, err := exchange(second, raw); err == nil || tokens.AccessToken != "" {
			t.Fatal("60초 만료 경계 허용")
		}
		assertUnused(t, raw)
	})
	t.Run("expiry after row lock wait", func(t *testing.T) {
		raw := issue(t)
		hash := sha256.Sum256([]byte(raw))
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal("잠금 시험 트랜잭션 준비 실패")
		}
		defer tx.Rollback(context.WithoutCancel(t.Context()))
		if _, err := tx.Exec(t.Context(), `SELECT code_hash FROM public.oauth_authorization_code WHERE code_hash=$1 FOR UPDATE`, hash[:]); err != nil {
			t.Fatal("코드 행 잠금 실패")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := second.ExchangeAuthorizationCode(ctx, raw, "first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", oauthVerifier)
			result <- err
		}()
		// 실제 Lock 대기를 관찰한 뒤에만 시계를 옮겨 요청 시작 시각의 만료 판정을 배제한다.
		for {
			var waiting bool
			err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%oauth_authorization_code%FOR UPDATE%')`).Scan(&waiting)
			if err != nil {
				t.Fatal("교환의 실제 행 잠금 대기를 확인하지 못했습니다")
			}
			if waiting {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("교환이 행 잠금 대기에 도달하지 않았습니다")
			case <-time.After(10 * time.Millisecond):
			}
		}
		clock.Add(60)
		defer clock.Store(now.Unix())
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal("코드 행 잠금 해제 실패")
		}
		select {
		case err := <-result:
			if failure, ok := errors.AsType[*OAuthError](err); !ok || failure.Code != "invalid_grant" {
				t.Fatal("행 잠금 해제 후 만료를 재판정하지 않았습니다")
			}
		case <-ctx.Done():
			t.Fatal("만료 교환이 반환하지 않았습니다")
		}
		assertUnused(t, raw)
	})
	t.Run("successful issuance and retained reuse", func(t *testing.T) {
		raw := issue(t)
		tokens, err := exchange(second, raw)
		if err != nil {
			t.Fatal("최초 교환 실패", oauthFailureKind(err))
		}
		if tokens.TokenType != "Bearer" || tokens.ExpiresIn != 3600 || tokens.Scope != "agent-context" {
			t.Fatal("토큰 외피 불일치")
		}
		refresh, err := base64.RawURLEncoding.Strict().DecodeString(tokens.RefreshToken)
		if err != nil || len(refresh) != 32 {
			t.Fatal("갱신 난수 형식 불일치")
		}
		set, err := first.JWKS(t.Context())
		if err != nil {
			t.Fatal("공개 키 준비 실패")
		}
		parsed, err := jwt.ParseSigned(tokens.AccessToken, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatal("접근 JWT 형식 불일치")
		}
		var claims jwt.Claims
		var binding struct {
			Scope  string `json:"scope"`
			Client string `json:"client_id"`
			Grant  string `json:"grant_id"`
		}
		if len(parsed.Headers) != 1 || len(set.Key(parsed.Headers[0].KeyID)) != 1 || parsed.Claims(set.Key(parsed.Headers[0].KeyID)[0].Key, &claims, &binding) != nil {
			t.Fatal("접근 JWT 독립 서명 검증 실패")
		}
		grant, err := model.ParseID(binding.Grant)
		if err != nil || claims.Issuer != "https://issuer.test" || claims.Subject != account.String() || len(claims.Audience) != 1 || claims.Audience[0] != "https://resource.test/mcp" || claims.ID == "" || claims.IssuedAt == nil || claims.Expiry == nil || claims.Expiry.Time().Sub(claims.IssuedAt.Time()) != time.Hour || binding.Client != "first" || binding.Scope != "agent-context" {
			t.Fatal("접근 클레임 결합 불일치")
		}
		if _, _, err := first.Verify(t.Context(), tokens.AccessToken, "web"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("접근 토큰을 웹 세션으로 허용")
		}
		hash := sha256.Sum256([]byte(tokens.RefreshToken))
		var storedGrant string
		var authorized, rotated, absolute, idle, retain, refreshExpiry time.Time
		if err := pool.QueryRow(t.Context(), `SELECT g.grant_id::text,g.authorized_at,g.last_rotated_at,g.absolute_expires_at,g.idle_expires_at,g.retain_until,r.expires_at FROM public.oauth_grant g JOIN public.oauth_refresh_token r USING(grant_id) WHERE r.token_hash=$1`, hash[:]).Scan(&storedGrant, &authorized, &rotated, &absolute, &idle, &retain, &refreshExpiry); err != nil {
			t.Fatal("갱신 해시·계열 결합 누락")
		}
		if storedGrant != grant.String() || !authorized.Equal(now) || !rotated.Equal(now) || !absolute.Equal(now.Add(30*24*time.Hour)) || !idle.Equal(now.Add(7*24*time.Hour)) || !retain.Equal(absolute.Add(time.Hour)) || !refreshExpiry.Equal(idle) {
			t.Fatal("최초 갱신 계열 수명 불일치")
		}
		_, err = second.ExchangeAuthorizationCode(t.Context(), raw, "first", "http://127.0.0.1:49152/callback", "https://resource.test/mcp", strings.Repeat("a", 43))
		if err == nil {
			t.Fatal("잘못된 verifier 재사용 허용")
		}
		var revoked bool
		if err := pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM public.oauth_grant WHERE grant_id=$1`, grant.String()).Scan(&revoked); err != nil || revoked {
			t.Fatal("잘못된 verifier로 계열 폐기")
		}
		clock.Add(61)
		defer clock.Store(now.Unix())
		if tokens, err := exchange(first, raw); err == nil || tokens.AccessToken != "" {
			t.Fatal("만료 후 소비 코드 재사용 허용")
		}
		if err := pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL AND revocation_reason='authorization_code_reuse' FROM public.oauth_grant WHERE grant_id=$1`, grant.String()).Scan(&revoked); err != nil || !revoked {
			t.Fatal("오류 반환으로 재사용 폐기가 롤백됨")
		}
		restarted, err := New(secondStore, config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := exchange(restarted, raw); err == nil {
			t.Fatal("재시작이 소비 기록을 잃음")
		}
	})
	t.Run("concurrent exchanges create one revoked family", func(t *testing.T) {
		raw := issue(t)
		start := make(chan struct{})
		results := make(chan error, 8)
		var workers sync.WaitGroup
		for index := range 8 {
			workers.Go(func() {
				<-start
				service := first
				if index%2 != 0 {
					service = second
				}
				_, err := exchange(service, raw)
				results <- err
			})
		}
		close(start)
		workers.Wait()
		close(results)
		success := 0
		for err := range results {
			if err == nil {
				success++
			} else if failure, ok := errors.AsType[*OAuthError](err); !ok || failure.Code != "invalid_grant" {
				t.Fatal("경합 오류가 자격 거부가 아님")
			}
		}
		if success != 1 {
			t.Fatalf("최초 교환 성공 횟수=%d", success)
		}
		hash := sha256.Sum256([]byte(raw))
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.oauth_authorization_code c JOIN public.oauth_grant g USING(grant_id) JOIN public.oauth_refresh_token r USING(grant_id) WHERE c.code_hash=$1 AND g.revoked_at IS NOT NULL`, hash[:]).Scan(&count); err != nil || count != 1 {
			t.Fatal("경합 후 계열·첫 갱신·폐기가 하나로 수렴하지 않음")
		}
	})
	t.Run("signing callback rollback and cancellation", func(t *testing.T) {
		for _, cancelled := range []bool{false, true} {
			raw := issue(t)
			hash := sha256.Sum256([]byte(raw))
			ctx, cancel := context.WithCancel(t.Context())
			err := firstStore.ExchangeOAuthAuthorizationCode(ctx, store.OAuthCodeExchange{Hash: hash, ClientID: "first", RedirectURI: "http://127.0.0.1:49152/callback", Resource: "https://resource.test/mcp", Verifier: oauthVerifier}, first.now, func(store.OAuthAuthorizationCode, time.Time) (store.OAuthInitialGrant, error) {
				if cancelled {
					cancel()
					id, _ := model.NewID()
					return store.OAuthInitialGrant{ID: id}, nil
				}
				return store.OAuthInitialGrant{}, errors.New("synthetic signing failure")
			})
			cancel()
			if err == nil {
				t.Fatal("서명 실패·취소를 커밋")
			}
			assertUnused(t, raw)
			if _, err := exchange(second, raw); err != nil {
				t.Fatal("롤백된 코드의 정상 재교환 실패")
			}
		}
	})
	t.Run("database failure rolls back family and code", func(t *testing.T) {
		raw := issue(t)
		before := oauthCounts(t, pool)
		if _, err := pool.Exec(t.Context(), `CREATE FUNCTION public.fail_oauth_refresh() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic storage failure'; END $$; CREATE TRIGGER fail_oauth_refresh BEFORE INSERT ON public.oauth_refresh_token FOR EACH ROW EXECUTE FUNCTION public.fail_oauth_refresh()`); err != nil {
			t.Fatal("장애 주입 준비 실패")
		}
		tokens, err := exchange(first, raw)
		if !errors.Is(err, ErrUnavailable) || tokens.AccessToken != "" || tokens.RefreshToken != "" || strings.Contains(err.Error(), "synthetic") {
			t.Fatal("저장 실패가 자격·원문·토큰을 반환")
		}
		assertUnused(t, raw)
		if oauthCounts(t, pool) != before {
			t.Fatal("저장 실패가 일부 계열을 남김")
		}
		if _, err := pool.Exec(t.Context(), `DROP TRIGGER fail_oauth_refresh ON public.oauth_refresh_token; DROP FUNCTION public.fail_oauth_refresh()`); err != nil {
			t.Fatal("장애 주입 정리 실패")
		}
		if _, err := exchange(second, raw); err != nil {
			t.Fatal("저장 실패 뒤 코드 복구 실패")
		}
	})
	t.Run("signing key failure does not consume", func(t *testing.T) {
		raw := issue(t)
		key, err := firstStore.ActiveSigningKey(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var public jose.JSONWebKey
		if json.Unmarshal([]byte(key.PublicKey), &public) != nil {
			t.Fatal("공개 키 해석 실패")
		}
		config.AuthorizationCodes = firstStore
		broken, err := New(&oauthBrokenKeyStore{authStore: firstStore}, config)
		if err != nil {
			t.Fatal(err)
		}
		if tokens, err := exchange(broken, raw); !errors.Is(err, ErrUnavailable) || tokens.AccessToken != "" {
			t.Fatal("서명 키 장애 분류 불일치")
		}
		assertUnused(t, raw)
	})
}

type oauthBrokenKeyStore struct{ authStore }

func (s *oauthBrokenKeyStore) ActiveSigningKey(ctx context.Context) (store.SigningKey, error) {
	key, err := s.authStore.ActiveSigningKey(ctx)
	key.PrivateKey = "broken"
	return key, err
}

func oauthCounts(t *testing.T, pool *pgxpool.Pool) [2]int {
	t.Helper()
	var result [2]int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public.oauth_grant),(SELECT count(*) FROM public.oauth_refresh_token)`).Scan(&result[0], &result[1]); err != nil {
		t.Fatal("계열 개수 조회 실패")
	}
	return result
}

func oauthFailureKind(err error) string {
	if failure, ok := errors.AsType[*pgconn.PgError](err); ok {
		return "postgres:" + failure.Code + ":" + failure.ColumnName + ":" + failure.ConstraintName
	}
	if failure, ok := errors.AsType[*store.StorageError](err); ok {
		return failure.Error()
	}
	if err != nil {
		return err.Error()
	}
	return ""
}
