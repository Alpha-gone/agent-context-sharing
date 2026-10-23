package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

func TestOPS04OAuthCleanupIntegration(t *testing.T) {
	_, pool, address := newServerContractDatabase(t)
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("pool_max_conns", "4")
	parsed.RawQuery = query.Encode()
	database, err := store.New(t.Context(), parsed.String(), "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	other, err := store.New(t.Context(), parsed.String(), "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	now := time.Now().UTC()
	seed := func(retain, absolute time.Time, children bool) string {
		t.Helper()
		id, _ := model.NewID()
		account, _ := model.NewID()
		authorized := absolute.Add(-30 * 24 * time.Hour)
		_, err := pool.Exec(t.Context(), `INSERT INTO public.oauth_grant
		(grant_id,account_id,client_id,resource,scope,authorized_at,last_rotated_at,absolute_expires_at,idle_expires_at,retain_until)
		VALUES ($1,$2,'ops04','https://service.test/mcp','agent-context',$3,$3,$4,$3::timestamptz + interval '7 days',$5)`, id.String(), account.String(), authorized, absolute, retain)
		if err != nil {
			t.Fatal(err)
		}
		if children {
			hash := sha256.Sum256([]byte(id.String()))
			_, err = pool.Exec(t.Context(), `INSERT INTO public.oauth_authorization_code
			(code_hash,account_id,client_id,redirect_uri,resource,scope,code_challenge,issued_at,expires_at,consumed_at,grant_id)
			VALUES ($1,$2,'ops04','https://callback.test','https://service.test/mcp','agent-context','challenge',$3,$3::timestamptz+interval '60 seconds',$3,$4)`, hash[:], account.String(), authorized, id.String())
			if err != nil {
				t.Fatal(err)
			}
			_, err = pool.Exec(t.Context(), `INSERT INTO public.oauth_refresh_token(token_hash,grant_id,issued_at,expires_at,consumed_at,successor_token_hash)
			VALUES ($1,$2,$3,$3::timestamptz+interval '7 days',$3,$1)`, hash[:], id.String(), authorized)
			if err != nil {
				t.Fatal(err)
			}
		}
		return id.String()
	}
	exists := func(id string) bool {
		t.Helper()
		var found bool
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM public.oauth_grant WHERE grant_id=$1)`, id).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	old := now.Add(-48 * time.Hour)
	expired := seed(now.Add(-time.Hour), old, true)
	retained := seed(now.Add(time.Hour), old, true)
	protected := seed(now.Add(-time.Hour), now.Add(time.Hour), true)
	hash := sha256.Sum256([]byte("expired-unconsumed"))
	account, _ := model.NewID()
	_, err = pool.Exec(t.Context(), `INSERT INTO public.oauth_authorization_code
	(code_hash,account_id,client_id,redirect_uri,resource,scope,code_challenge,issued_at,expires_at)
	VALUES ($1,$2,'ops04','https://callback.test','https://service.test/mcp','agent-context','challenge',$3,$3::timestamptz+interval '60 seconds')`, hash[:], account.String(), old)
	if err != nil {
		t.Fatal(err)
	}
	count, err := database.CleanupOAuthState(t.Context(), now)
	if err != nil || count != 4 || exists(expired) || !exists(retained) || !exists(protected) {
		t.Fatal("OAuth 정리와 보존 경계", count, err)
	}
	var children int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public.oauth_authorization_code WHERE grant_id=$1)+(SELECT count(*) FROM public.oauth_refresh_token WHERE grant_id=$1)`, retained).Scan(&children); err != nil || children != 2 {
		t.Fatal("만료한 소비 근거 보존", children, err)
	}

	t.Run("lock recheck", func(t *testing.T) {
		id := seed(now.Add(-time.Hour), old, true)
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(t.Context(), `SELECT grant_id FROM public.oauth_grant WHERE grant_id=$1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { _, err := database.CleanupOAuthState(t.Context(), now); result <- err }()
		deadline := time.Now().Add(5 * time.Second)
		waiting := false
		for time.Now().Before(deadline) {
			if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%GREATEST(retain_until%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("계열 잠금 대기를 관찰하지 못했습니다")
		}
		if _, err := tx.Exec(t.Context(), `UPDATE public.oauth_grant SET retain_until=$2 WHERE grant_id=$1`, id, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err != nil || !exists(id) {
			t.Fatal("계열 잠금 뒤 보존 재검사", err)
		}
	})
	t.Run("rollback and retry", func(t *testing.T) {
		id := seed(now.Add(-time.Hour), old, true)
		if _, err := pool.Exec(t.Context(), `CREATE TABLE public.ops04_blocker(grant_id uuid REFERENCES public.oauth_grant(grant_id)); INSERT INTO public.ops04_blocker VALUES ('`+id+`')`); err != nil {
			t.Fatal(err)
		}
		if _, err := database.CleanupOAuthState(t.Context(), now); err == nil {
			t.Fatal("계열 삭제 실패를 무시했습니다")
		}
		if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public.oauth_authorization_code WHERE grant_id=$1)+(SELECT count(*) FROM public.oauth_refresh_token WHERE grant_id=$1)`, id).Scan(&children); err != nil || children != 2 || !exists(id) {
			t.Fatal("자식 삭제 롤백", children, err)
		}
		if _, err := pool.Exec(t.Context(), `DROP TABLE public.ops04_blocker`); err != nil {
			t.Fatal(err)
		}
		if count, err := database.CleanupOAuthState(t.Context(), now); err != nil || count != 3 || exists(id) {
			t.Fatal("실패 다음 회차", count, err)
		}
	})
	t.Run("bounded pages", func(t *testing.T) {
		for range 130 {
			seed(now.Add(-time.Hour), old, false)
		}
		if count, err := database.CleanupOAuthState(t.Context(), now); err != nil || count != 130 {
			t.Fatal("128개 후보 페이지 경계", count, err)
		}
	})
	t.Run("cancel blocked cleanup", func(t *testing.T) {
		id := seed(now.Add(-time.Hour), old, true)
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(t.Context(), `SELECT grant_id FROM public.oauth_grant WHERE grant_id=$1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, held, err := database.WithTryAdvisoryLock(ctx, oauthCleanupLockKey, func(ctx context.Context) (int, error) { return database.CleanupOAuthState(ctx, now) })
		if !held || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("실제 계열 정리의 잠금 대기 취소", held, err)
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !exists(id) {
			t.Fatal("취소한 계열 정리가 반영됐습니다")
		}
		ctx2, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		count, held, err := database.WithTryAdvisoryLock(ctx2, oauthCleanupLockKey, func(ctx context.Context) (int, error) { return database.CleanupOAuthState(ctx, now) })
		if err != nil || !held || count != 3 || exists(id) {
			t.Fatal("실제 취소 뒤 연결·실행권 재사용", count, held, err)
		}
	})
	t.Run("two instances and cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			_, _, err := database.WithTryAdvisoryLock(ctx, oauthCleanupLockKey, func(ctx context.Context) (int, error) { close(started); <-ctx.Done(); return 0, ctx.Err() })
			result <- err
		}()
		<-started
		if _, acquired, err := other.WithTryAdvisoryLock(t.Context(), oauthCleanupLockKey, func(context.Context) (int, error) { t.Error("중복 회차 실행"); return 0, nil }); err != nil || acquired {
			t.Fatal("다중 인스턴스 실행권", acquired, err)
		}
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatal("취소 원인", err)
		}
		if _, acquired, err := other.WithTryAdvisoryLock(t.Context(), oauthCleanupLockKey, func(ctx context.Context) (int, error) { return other.CleanupOAuthState(ctx, now) }); err != nil || !acquired {
			t.Fatal("취소 뒤 실행권 반환", acquired, err)
		}
	})
	worker, err := newPeriodicWorker(database, plan.AccountPlans{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }
	results := make(chan string, 8)
	worker.diagnostics = ops04Observation(func(_ context.Context, _, _, result, _ string, _ bool, fields diagnostic.Fields) bool {
		if result != "completed" {
			results <- "failed"
		} else {
			results <- fields.System.Job
		}
		return true
	})
	worker.Start(t.Context())
	jobs := make(map[string]bool)
	for range 8 {
		select {
		case job := <-results:
			jobs[job] = true
		case <-time.After(5 * time.Second):
			t.Fatal("최소 풀의 여덟 기동 회차가 끝나지 않았습니다")
		}
	}
	if err := worker.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 8 || jobs["failed"] || !jobs["oauth_cleanup"] {
		t.Fatal("여덟 주기 작업의 첫 회차", jobs)
	}
}

func TestOPS04PeriodicFailureAndFollowingRound(t *testing.T) {
	var logs bytes.Buffer
	worker := &periodicWorker{store: &fakePeriodicStore{acquired: true}, logger: slog.New(slog.NewTextHandler(&logs, nil)), done: make(chan struct{})}
	var outcomes []string
	var processed []uint64
	var jobs []string
	worker.diagnostics = ops04Observation(func(ctx context.Context, _, _, result, _ string, _ bool, fields diagnostic.Fields) bool {
		outcomes = append(outcomes, result)
		processed = append(processed, fields.System.Processed)
		jobs = append(jobs, diagnostic.TraceFromContext(ctx).JobID())
		return true
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int64
	worker.tasks = []periodicTask{{name: "oauth_cleanup", interval: 5 * time.Millisecond, lockKey: oauthCleanupLockKey, run: func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			return 2, errors.New("private credential marker")
		}
		cancel()
		return 3, nil
	}}}
	worker.Start(ctx)
	select {
	case <-worker.done:
	case <-time.After(time.Second):
		t.Fatal("실패 뒤 다음 회차가 실행되지 않았습니다")
	}
	if err := worker.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !strings.Contains(logs.String(), "status=failed") || !strings.Contains(logs.String(), "status=completed") || strings.Contains(logs.String(), "credential marker") {
		t.Fatal("회차 결과·건수·민감 원인 제외", logs.String())
	}
	if len(outcomes) != 2 || outcomes[0] != "failed" || outcomes[1] != "completed" || processed[0] != 2 || processed[1] != 3 || jobs[0] == "" || jobs[0] == jobs[1] {
		t.Fatal("회차 결과·처리 건수·독립 작업 ID", outcomes, processed, jobs)
	}
}

type ops04Observation func(context.Context, string, string, string, string, bool, diagnostic.Fields) bool

func (observe ops04Observation) RecordFields(ctx context.Context, level, stage, result, reason string, verified bool, fields diagnostic.Fields) bool {
	return observe(ctx, level, stage, result, reason, verified, fields)
}
