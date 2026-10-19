package authz

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type refreshSnapshot struct {
	Consumed, Revoked                                bool
	Successor                                        string
	Rotated, Absolute, Idle, Retain, Issued, Expires time.Time
	Count                                            int
}

func readRefreshSnapshot(t *testing.T, pool *pgxpool.Pool, raw string) refreshSnapshot {
	t.Helper()
	hash := sha256.Sum256([]byte(raw))
	var state refreshSnapshot
	err := pool.QueryRow(t.Context(), `SELECT t.consumed_at IS NOT NULL,g.revoked_at IS NOT NULL,COALESCE(encode(t.successor_token_hash,'hex'),''),g.last_rotated_at,g.absolute_expires_at,g.idle_expires_at,g.retain_until,t.issued_at,t.expires_at,(SELECT count(*) FROM public.oauth_refresh_token)
		FROM public.oauth_refresh_token t JOIN public.oauth_grant g USING(grant_id) WHERE t.token_hash=$1`, hash[:]).Scan(&state.Consumed, &state.Revoked, &state.Successor, &state.Rotated, &state.Absolute, &state.Idle, &state.Retain, &state.Issued, &state.Expires, &state.Count)
	if err != nil {
		t.Fatal("회전 상태 조회 실패")
	}
	return state
}

func waitRefreshLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE $1)`, "%FROM public."+table+"%FOR UPDATE%").Scan(&waiting); err != nil {
			t.Fatal("실제 회전 잠금 대기 확인 실패")
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("회전이 잠금 대기에 도달하지 않음")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestOAuthRefreshDatabaseIntegration(t *testing.T) {
	firstStore, secondStore, pool := newAuthenticationContractDatabase(t)
	config := oauthTestConfig()
	config.AuthorizationCodes, config.AccessTokens, config.RefreshTokens = firstStore, firstStore, firstStore
	first, err := New(firstStore, config)
	if err != nil {
		t.Fatal(err)
	}
	config.AuthorizationCodes, config.AccessTokens, config.RefreshTokens = secondStore, secondStore, secondStore
	second, err := New(secondStore, config)
	if err != nil {
		t.Fatal(err)
	}
	account, err := first.Register(t.Context(), "refresh_account", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(base.Unix())
	first.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	second.now = first.now
	issue := func(t *testing.T) OAuthTokens {
		t.Helper()
		clock.Store(base.Unix())
		code, err := first.IssueAuthorizationCode(t.Context(), account, validAuthorizationRequest())
		if err != nil {
			t.Fatal("초기 코드 준비 실패")
		}
		tokens, err := first.ExchangeAuthorizationCode(t.Context(), code, "first", "http://127.0.0.1:49152/callback", config.OAuth.Resource, oauthVerifier)
		if err != nil {
			t.Fatal("초기 갱신 자격 준비 실패")
		}
		return tokens
	}
	refresh := func(service *Service, raw string) (OAuthTokens, error) {
		return service.Refresh(t.Context(), raw, "first", config.OAuth.Resource, "")
	}
	invalid := func(t *testing.T, tokens OAuthTokens, err error) {
		t.Helper()
		failure, ok := errors.AsType[*OAuthError](err)
		if !ok || failure.Code != "invalid_grant" || tokens != (OAuthTokens{}) {
			t.Fatal("회전 거부·자격 비노출 실패")
		}
	}
	t.Run("rotation preserves binding and absolute lifetime", func(t *testing.T) {
		initial := issue(t)
		principal, err := first.VerifyBearer(t.Context(), initial.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		clock.Add(600)
		rotated, err := refresh(second, initial.RefreshToken)
		if err != nil {
			t.Fatal("정상 회전 실패")
		}
		next, err := first.VerifyBearer(t.Context(), rotated.AccessToken)
		if err != nil || next != principal {
			t.Fatal("다른 인스턴스의 회전 결합 실패")
		}
		if _, err := second.VerifyBearer(t.Context(), initial.AccessToken); err != nil {
			t.Fatal("회전이 미만료 접근 토큰 폐기")
		}
		old, new := readRefreshSnapshot(t, pool, initial.RefreshToken), readRefreshSnapshot(t, pool, rotated.RefreshToken)
		hash := sha256.Sum256([]byte(rotated.RefreshToken))
		var linked bool
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM public.oauth_refresh_token WHERE successor_token_hash=$1 AND consumed_at=$2)`, hash[:], first.now()).Scan(&linked); err != nil || !linked {
			t.Fatal("후속 해시·소비 시각 누락")
		}
		if !old.Consumed || old.Revoked || new.Consumed || !new.Rotated.Equal(first.now()) || !new.Absolute.Equal(base.Add(30*24*time.Hour)) || !new.Idle.Equal(first.now().Add(7*24*time.Hour)) || !new.Retain.Equal(base.Add(30*24*time.Hour+time.Hour)) || !new.Expires.Equal(new.Idle) || !new.Issued.Equal(first.now()) || rotated.RefreshToken == initial.RefreshToken {
			t.Fatal("회전 수명·보존 상태 불일치")
		}
		before := new
		if _, err := first.Authenticate(t.Context(), "refresh_account", "correct password"); err != nil {
			t.Fatal(err)
		}
		if _, err := first.WebSession(t.Context(), account, "web"); err != nil {
			t.Fatal(err)
		}
		if before != readRefreshSnapshot(t, pool, rotated.RefreshToken) {
			t.Fatal("로그인·웹 세션이 갱신 활동 연장")
		}
	})
	t.Run("binding failures preserve usable credential", func(t *testing.T) {
		initial := issue(t)
		before := readRefreshSnapshot(t, pool, initial.RefreshToken)
		for _, input := range []struct{ client, resource, scope string }{{"native", config.OAuth.Resource, ""}, {"first", "https://other.test/mcp", ""}, {"unknown", config.OAuth.Resource, ""}, {"first", config.OAuth.Resource, "agent-context admin"}} {
			if tokens, err := second.Refresh(t.Context(), initial.RefreshToken, input.client, input.resource, input.scope); err == nil || tokens != (OAuthTokens{}) {
				t.Fatal("결합 위반 허용")
			}
			if before != readRefreshSnapshot(t, pool, initial.RefreshToken) {
				t.Fatal("실패가 계열·소비 변경")
			}
		}
		result, err := refresh(second, strings.Repeat("A", 43))
		invalid(t, result, err)
		if _, err := refresh(second, initial.RefreshToken); err != nil {
			t.Fatal("정상 후속 회전 실패")
		}
	})
	for _, boundary := range []string{"idle exact", "idle after", "absolute exact"} {
		t.Run(boundary, func(t *testing.T) {
			tokens := issue(t)
			if boundary == "absolute exact" {
				for _, day := range []int64{6, 12, 18, 24, 29} {
					clock.Store(base.Add(time.Duration(day) * 24 * time.Hour).Unix())
					var err error
					tokens, err = refresh(second, tokens.RefreshToken)
					if err != nil {
						t.Fatal("절대 만료 전 회전 실패")
					}
				}
				clock.Store(base.Add(29*24*time.Hour + 23*time.Hour + 30*time.Minute).Unix())
				var err error
				tokens, err = refresh(first, tokens.RefreshToken)
				if err != nil {
					t.Fatal(err)
				}
				state := readRefreshSnapshot(t, pool, tokens.RefreshToken)
				if !state.Expires.Equal(base.Add(30*24*time.Hour)) || !state.Retain.Equal(base.Add(30*24*time.Hour+time.Hour)) {
					t.Fatal("절대 만료·보존 연장")
				}
				clock.Store(base.Add(30 * 24 * time.Hour).Unix())
			} else {
				clock.Store(base.Add(7 * 24 * time.Hour).Unix())
				if boundary == "idle after" {
					clock.Add(1)
				}
			}
			before := readRefreshSnapshot(t, pool, tokens.RefreshToken)
			result, err := refresh(first, tokens.RefreshToken)
			invalid(t, result, err)
			if before != readRefreshSnapshot(t, pool, tokens.RefreshToken) {
				t.Fatal("만료 요청이 소비·수명 변경")
			}
			if boundary == "absolute exact" {
				if _, err := second.VerifyBearer(t.Context(), tokens.AccessToken); err != nil {
					t.Fatal("갱신 만료가 유효 접근 토큰 차단")
				}
			}
		})
	}
	t.Run("response loss reuse revokes across instances and restart", func(t *testing.T) {
		initial := issue(t)
		rotated, err := refresh(first, initial.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		result, err := refresh(second, initial.RefreshToken)
		invalid(t, result, err)
		restarted, err := New(secondStore, config)
		if err != nil {
			t.Fatal(err)
		}
		restarted.now = first.now
		for _, service := range []*Service{first, second, restarted} {
			result, err := refresh(service, rotated.RefreshToken)
			invalid(t, result, err)
			if _, err := service.VerifyBearer(t.Context(), rotated.AccessToken); !errors.Is(err, ErrInvalidCredential) {
				t.Fatal("재사용 계열의 접근 허용")
			}
		}
		if !readRefreshSnapshot(t, pool, initial.RefreshToken).Revoked {
			t.Fatal("재사용 폐기 롤백")
		}
	})
	t.Run("expired consumed credential still detects reuse", func(t *testing.T) {
		initial := issue(t)
		if _, err := refresh(first, initial.RefreshToken); err != nil {
			t.Fatal(err)
		}
		clock.Store(base.Add(31 * 24 * time.Hour).Unix())
		result, err := refresh(second, initial.RefreshToken)
		invalid(t, result, err)
		if !readRefreshSnapshot(t, pool, initial.RefreshToken).Revoked {
			t.Fatal("보존 중인 소비 기록의 만료 후 재사용 누락")
		}
	})
	t.Run("eight concurrent exchanges have one success then family revocation", func(t *testing.T) {
		initial := issue(t)
		before := readRefreshSnapshot(t, pool, initial.RefreshToken)
		start := make(chan struct{})
		type result struct {
			tokens OAuthTokens
			err    error
		}
		results := make(chan result, 8)
		var workers sync.WaitGroup
		for i := range 8 {
			workers.Go(func() {
				<-start
				tokens, err := refresh([]*Service{first, second}[i%2], initial.RefreshToken)
				results <- result{tokens, err}
			})
		}
		close(start)
		workers.Wait()
		close(results)
		success := 0
		var issued OAuthTokens
		for result := range results {
			if result.err == nil {
				success++
				issued = result.tokens
			} else {
				invalid(t, result.tokens, result.err)
			}
		}
		after := readRefreshSnapshot(t, pool, initial.RefreshToken)
		if success != 1 || !after.Consumed || !after.Revoked || after.Count != before.Count+1 {
			t.Fatal("동시 회전 단일 소비·재사용 폐기 실패")
		}
		if _, err := second.VerifyBearer(t.Context(), issued.AccessToken); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("경합 후 폐기 접근 허용")
		}
	})
	for _, mode := range []string{"signing failure", "cancel after issue"} {
		t.Run(mode, func(t *testing.T) {
			initial := issue(t)
			before := readRefreshSnapshot(t, pool, initial.RefreshToken)
			hash := sha256.Sum256([]byte(initial.RefreshToken))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := firstStore.RotateOAuthRefreshToken(ctx, store.OAuthRefreshExchange{Hash: hash, ClientID: "first", Resource: config.OAuth.Resource}, first.now, func(store.OAuthGrantBinding, time.Time) ([32]byte, error) {
				if mode == "cancel after issue" {
					cancel()
					return sha256.Sum256([]byte("synthetic-next")), nil
				}
				return [32]byte{}, errors.New("synthetic signing failure")
			})
			if err == nil || before != readRefreshSnapshot(t, pool, initial.RefreshToken) {
				t.Fatal("서명·취소 실패의 부분 커밋")
			}
			if _, err := refresh(second, initial.RefreshToken); err != nil {
				t.Fatal("실패 뒤 이전 자격 복구 실패")
			}
		})
	}
	for _, stage := range []string{"insert successor", "consume old", "update grant", "commit"} {
		t.Run("storage rollback/"+stage, func(t *testing.T) {
			initial := issue(t)
			before := readRefreshSnapshot(t, pool, initial.RefreshToken)
			table, event := "oauth_refresh_token", "BEFORE INSERT"
			if stage == "consume old" {
				event = "BEFORE UPDATE"
			}
			if stage == "update grant" || stage == "commit" {
				table, event = "oauth_grant", "BEFORE UPDATE"
			}
			trigger := "CREATE TRIGGER refresh_fault " + event + " ON public." + table + " FOR EACH ROW EXECUTE FUNCTION public.refresh_fault()"
			if stage == "commit" {
				trigger = "CREATE CONSTRAINT TRIGGER refresh_fault AFTER UPDATE ON public.oauth_grant DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.refresh_fault()"
			}
			if _, err := pool.Exec(t.Context(), `CREATE FUNCTION public.refresh_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic storage secret'; END $$; `+trigger); err != nil {
				t.Fatal("실패 주입 준비 실패")
			}
			cleaned := false
			cleanup := func() {
				if !cleaned {
					if _, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP TRIGGER refresh_fault ON public."+table+"; DROP FUNCTION public.refresh_fault()"); err != nil {
						t.Error("실패 주입 정리 실패")
					}
					cleaned = true
				}
			}
			defer cleanup()
			result, err := refresh(first, initial.RefreshToken)
			if !errors.Is(err, ErrUnavailable) || result != (OAuthTokens{}) || strings.Contains(err.Error(), "synthetic storage secret") || before != readRefreshSnapshot(t, pool, initial.RefreshToken) {
				t.Fatal("저장·커밋 실패의 원자성·비노출 실패")
			}
			cleanup()
			if _, err := refresh(second, initial.RefreshToken); err != nil {
				t.Fatal("저장 실패 뒤 이전 자격 회복 실패")
			}
		})
	}
	t.Run("reuse revocation commit failure is unavailable and retryable", func(t *testing.T) {
		initial := issue(t)
		rotated, err := refresh(first, initial.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `CREATE FUNCTION public.reuse_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic revocation secret'; END $$; CREATE CONSTRAINT TRIGGER reuse_fault AFTER UPDATE ON public.oauth_grant DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.reuse_fault()`); err != nil {
			t.Fatal("폐기 커밋 장애 준비 실패")
		}
		cleaned := false
		cleanup := func() {
			if !cleaned {
				if _, err := pool.Exec(context.WithoutCancel(t.Context()), `DROP TRIGGER reuse_fault ON public.oauth_grant; DROP FUNCTION public.reuse_fault()`); err != nil {
					t.Error("폐기 장애 정리 실패")
				}
				cleaned = true
			}
		}
		defer cleanup()
		result, err := refresh(second, initial.RefreshToken)
		if !errors.Is(err, ErrUnavailable) || result != (OAuthTokens{}) || readRefreshSnapshot(t, pool, initial.RefreshToken).Revoked {
			t.Fatal("실패한 폐기 커밋을 성공·자격 오류로 표시")
		}
		if _, err := second.VerifyBearer(t.Context(), rotated.AccessToken); err != nil {
			t.Fatal("실패한 폐기가 접근 차단")
		}
		cleanup()
		result, err = refresh(second, initial.RefreshToken)
		invalid(t, result, err)
		if _, err := first.VerifyBearer(t.Context(), rotated.AccessToken); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("재시도 폐기 커밋 미반영")
		}
	})
	t.Run("key failure preserves previous credential", func(t *testing.T) {
		initial := issue(t)
		before := readRefreshSnapshot(t, pool, initial.RefreshToken)
		broken, err := New(&oauthBrokenKeyStore{authStore: firstStore}, config)
		if err != nil {
			t.Fatal(err)
		}
		broken.now = first.now
		result, err := refresh(broken, initial.RefreshToken)
		if !errors.Is(err, ErrUnavailable) || result != (OAuthTokens{}) || before != readRefreshSnapshot(t, pool, initial.RefreshToken) {
			t.Fatal("서명 키 장애가 자격 소비")
		}
		if _, err := refresh(second, initial.RefreshToken); err != nil {
			t.Fatal("서명 키 장애 뒤 자격 복구 실패")
		}
	})
	t.Run("retain boundary never shrinks", func(t *testing.T) {
		initial := issue(t)
		hash := sha256.Sum256([]byte(initial.RefreshToken))
		later := base.Add(40 * 24 * time.Hour)
		if _, err := pool.Exec(t.Context(), `UPDATE public.oauth_grant SET retain_until=$2 WHERE grant_id=(SELECT grant_id FROM public.oauth_refresh_token WHERE token_hash=$1)`, hash[:], later); err != nil {
			t.Fatal(err)
		}
		rotated, err := refresh(second, initial.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		if !readRefreshSnapshot(t, pool, rotated.RefreshToken).Retain.Equal(later) {
			t.Fatal("기존 보존 경계 축소")
		}
	})
	t.Run("revocation committed while waiting is rechecked", func(t *testing.T) {
		initial := issue(t)
		hash := sha256.Sum256([]byte(initial.RefreshToken))
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.WithoutCancel(t.Context()))
		if _, err := tx.Exec(t.Context(), `UPDATE public.oauth_grant SET revoked_at=$2,revocation_reason='synthetic_revoke' WHERE grant_id=(SELECT grant_id FROM public.oauth_refresh_token WHERE token_hash=$1)`, hash[:], first.now()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := second.Refresh(ctx, initial.RefreshToken, "first", config.OAuth.Resource, "")
			done <- err
		}()
		waitRefreshLock(t, ctx, pool, "oauth_grant")
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			invalid(t, OAuthTokens{}, err)
		case <-ctx.Done():
			t.Fatal("폐기 커밋 뒤 반환 실패")
		}
		if state := readRefreshSnapshot(t, pool, initial.RefreshToken); state.Consumed || !state.Revoked {
			t.Fatal("대기 중 폐기 자격 소비")
		}
	})
	for _, table := range []string{"oauth_grant", "oauth_refresh_token"} {
		t.Run("expiry after lock/"+table, func(t *testing.T) {
			initial := issue(t)
			before := readRefreshSnapshot(t, pool, initial.RefreshToken)
			hash := sha256.Sum256([]byte(initial.RefreshToken))
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(t.Context()))
			query := `SELECT token_hash FROM public.oauth_refresh_token WHERE token_hash=$1 FOR UPDATE`
			if table == "oauth_grant" {
				query = `SELECT grant_id FROM public.oauth_grant WHERE grant_id=(SELECT grant_id FROM public.oauth_refresh_token WHERE token_hash=$1) FOR UPDATE`
			}
			if _, err := tx.Exec(t.Context(), query, hash[:]); err != nil {
				t.Fatal("만료 시험 잠금 준비 실패")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := second.Refresh(ctx, initial.RefreshToken, "first", config.OAuth.Resource, "")
				done <- err
			}()
			waitRefreshLock(t, ctx, pool, table)
			clock.Store(base.Add(7 * 24 * time.Hour).Unix())
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				invalid(t, OAuthTokens{}, err)
			case <-ctx.Done():
				t.Fatal("잠금 해제 뒤 반환 실패")
			}
			if before != readRefreshSnapshot(t, pool, initial.RefreshToken) {
				t.Fatal("잠금 대기 중 만료 자격 소비")
			}
		})
	}
	t.Run("cancel lock wait releases transaction", func(t *testing.T) {
		initial := issue(t)
		before := readRefreshSnapshot(t, pool, initial.RefreshToken)
		hash := sha256.Sum256([]byte(initial.RefreshToken))
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.WithoutCancel(t.Context()))
		if _, err := tx.Exec(t.Context(), `SELECT grant_id FROM public.oauth_grant WHERE grant_id=(SELECT grant_id FROM public.oauth_refresh_token WHERE token_hash=$1) FOR UPDATE`, hash[:]); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		done := make(chan error, 1)
		go func() {
			_, err := second.Refresh(ctx, initial.RefreshToken, "first", config.OAuth.Resource, "")
			done <- err
		}()
		waitRefreshLock(t, waitCtx, pool, "oauth_grant")
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("잠금 취소 원인 손실")
			}
		case <-waitCtx.Done():
			t.Fatal("취소 뒤 트랜잭션 미반환")
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if before != readRefreshSnapshot(t, pool, initial.RefreshToken) {
			t.Fatal("취소 회전의 부분 소비")
		}
		if _, err := refresh(first, initial.RefreshToken); err != nil {
			t.Fatal("취소 뒤 잠금·연결 회복 실패")
		}
	})
}
