package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
)

var _ mcp.WriteExecutor = (*Store)(nil)

func writeReplayCheck(context.Context) error { return nil }

func TestMCP05EightWriteReplayDatabase(t *testing.T) {
	for _, tool := range lifeWriteTools {
		t.Run(tool, func(t *testing.T) {
			database, second, _ := newAgentContractDatabase(t)
			write := lifeAcceptanceFixture(t, database, tool)
			normalized := []byte(`{"tool":"` + tool + `","arguments":{}}`)
			runs, checks := 0, 0
			check := func(context.Context) error { checks++; return nil }
			apply := func(ctx context.Context) ([]byte, error) {
				runs++
				if err := write.run(ctx); err != nil {
					return nil, err
				}
				return []byte(`{"applied":true,"tool":"` + tool + `"}`), nil
			}
			before := lifeAcceptanceFingerprint(t, database)
			first, err := database.ExecuteWrite(t.Context(), write.actor, "request_key_00001", tool, normalized, check, apply)
			if err != nil {
				t.Fatal(err)
			}
			after := lifeAcceptanceFingerprint(t, database)
			if before == after {
				t.Fatal("업무 저장 누락")
			}
			var received, expires time.Time
			var storedFingerprint []byte
			if err := database.pool.QueryRow(t.Context(), `SELECT received_at,expires_at,request_fingerprint FROM public.idempotency_record WHERE actor_account_id=$1 AND idempotency_key=$2`, write.actor.String(), "request_key_00001").Scan(&received, &expires, &storedFingerprint); err != nil {
				t.Fatal(err)
			}
			fingerprint := sha256.Sum256(normalized)
			if expires.Sub(received) != 24*time.Hour || !bytes.Equal(storedFingerprint, fingerprint[:]) {
				t.Fatal("수명·지문 저장")
			}
			replayed, err := second.ExecuteWrite(t.Context(), write.actor, "request_key_00001", tool, normalized, check, apply)
			if err != nil || !bytes.Equal(first, replayed) || runs != 1 || checks != 2 || after != lifeAcceptanceFingerprint(t, database) {
				t.Fatalf("재생 중복 적용: run=%d check=%d err=%v", runs, checks, err)
			}
		})
	}
}

func TestMCP05KeyScopeAndExpiryDatabase(t *testing.T) {
	database, second, _ := newAgentContractDatabase(t)
	account := newTestID(t)
	runs := 0
	apply := func(context.Context) ([]byte, error) { runs++; return []byte(fmt.Sprintf(`{"run":%d}`, runs)), nil }
	normalized := []byte(`{"tool":"graph_create","arguments":{"name":"first"}}`)
	first, err := database.ExecuteWrite(t.Context(), account, "request_key_00001", "graph_create", normalized, writeReplayCheck, apply)
	if err != nil {
		t.Fatal(err)
	}
	var received, expiry time.Time
	if err := database.pool.QueryRow(t.Context(), `SELECT received_at,expires_at FROM public.idempotency_record WHERE actor_account_id=$1`, account.String()).Scan(&received, &expiry); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"same", "other input", "other tool", "expired"} {
		t.Run(mode, func(t *testing.T) {
			tool, input := "graph_create", normalized
			if mode == "other input" {
				input = []byte(`{"name":"other"}`)
			}
			if mode == "other tool" {
				tool = "graph_update"
			}
			if mode == "expired" {
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.idempotency_record SET expires_at=clock_timestamp()-interval '1 microsecond' WHERE actor_account_id=$1`, account.String()); err != nil {
					t.Fatal(err)
				}
				input = []byte(`{"name":"new after expiration"}`)
			}
			value, err := second.ExecuteWrite(t.Context(), account, "request_key_00001", tool, input, writeReplayCheck, apply)
			if mode == "other input" || mode == "other tool" {
				field, ok := errors.AsType[model.FieldError](err)
				if !ok || field.Field != "request_id" || !errors.Is(err, ErrIdempotencyConflict) || value != nil {
					t.Fatal("키 혼용을 거부하지 않음", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "same" {
				if !bytes.Equal(first, value) {
					t.Fatal("최초 결과 불일치")
				}
				var a, b time.Time
				if err := database.pool.QueryRow(t.Context(), `SELECT received_at,expires_at FROM public.idempotency_record WHERE actor_account_id=$1`, account.String()).Scan(&a, &b); err != nil || a != received || b != expiry {
					t.Fatal("재생 수명 연장")
				}
			}
			if mode == "expired" && bytes.Equal(first, value) {
				t.Fatal("만료 결과 재생")
			}
		})
	}
	if runs != 2 {
		t.Fatal("실행 수", runs)
	}
	for _, binding := range []struct {
		account model.ID
		key     string
	}{{account, "Request_key_00001"}, {newTestID(t), "request_key_00001"}} {
		if _, err := database.ExecuteWrite(t.Context(), binding.account, binding.key, "graph_create", normalized, writeReplayCheck, apply); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := database.ExecuteWrite(t.Context(), account, "", "graph_create", normalized, writeReplayCheck, apply); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.idempotency_record`).Scan(&count); err != nil || count != 3 || runs != 6 {
		t.Fatal("계정·대소문자·키 생략 범위", count, runs, err)
	}
	// 기존 UUID 행은 지문·완료 결과를 변경하지 않고 같은 경계로 읽는다.
	key := newTestID(t)
	fp := sha256.Sum256(normalized)
	legacy, err := database.ReplayIdempotent(t.Context(), IdempotencyRequest{AccountID: account, Key: key, ToolName: "graph_create", Fingerprint: fp}, apply)
	if err != nil {
		t.Fatal(err)
	}
	value, err := second.ExecuteWrite(t.Context(), account, key.String(), "graph_create", normalized, writeReplayCheck, apply)
	if err != nil || !bytes.Equal(legacy, value) || runs != 7 {
		t.Fatal("기존 UUID 결과 보존", err)
	}
}

func TestMCP05ConcurrentSameKeyDatabase(t *testing.T) {
	first, second, _ := newAgentContractDatabase(t)
	account := newTestID(t)
	var runs, checks atomic.Int64
	results := make(chan []byte, 16)
	failures := make(chan error, 16)
	var group sync.WaitGroup
	for i := range 16 {
		group.Go(func() {
			database := first
			if i%2 != 0 {
				database = second
			}
			value, err := database.ExecuteWrite(t.Context(), account, "concurrent_key_1", "graph_create", []byte(`{"name":"same"}`), func(context.Context) error { checks.Add(1); return nil }, func(context.Context) ([]byte, error) { runs.Add(1); return []byte(`{"id":"stable","ok":true}`), nil })
			results <- value
			failures <- err
		})
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var initial []byte
	for value := range results {
		if initial == nil {
			initial = value
		}
		if !bytes.Equal(initial, value) {
			t.Fatal("동시 결과 불일치")
		}
	}
	if runs.Load() != 1 || checks.Load() != 16 {
		t.Fatal("동시 실행·현재 검사 수", runs.Load(), checks.Load())
	}
}

func TestMCP05FailureRollbackDatabase(t *testing.T) {
	for _, mode := range []string{"apply failure", "result store", "commit", "cancel", "empty", "null", "array", "invalid JSON", "empty object", "protocol envelope", "credential", "nested"} {
		t.Run(mode, func(t *testing.T) {
			database, _, _ := newAgentContractDatabase(t)
			write := lifeAcceptanceFixture(t, database, "node_create")
			remove := func() {}
			if mode == "result store" || mode == "commit" {
				remove = lifeFailIdempotencyResult(t, database, write.actor, mode == "commit")
			}
			before := lifeAcceptanceFingerprint(t, database)
			cause := errors.New("synthetic private application error")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			runs := 0
			result, err := database.ExecuteWrite(ctx, write.actor, "failure_key_0001", "node_create", []byte(`{"body":"value"}`), writeReplayCheck, func(ctx context.Context) ([]byte, error) {
				runs++
				if err := write.run(ctx); err != nil {
					return nil, err
				}
				switch mode {
				case "apply failure":
					return nil, cause
				case "cancel":
					cancel(cause)
				case "empty":
					return nil, nil
				case "null":
					return []byte(`null`), nil
				case "array":
					return []byte(`[]`), nil
				case "invalid JSON":
					return []byte(`{"ok":`), nil
				case "empty object":
					return []byte(`{}`), nil
				case "protocol envelope":
					return []byte(`{"jsonrpc":"2.0","result":{}}`), nil
				case "credential":
					return []byte(`{"access_token":"synthetic-private-token"}`), nil
				case "nested":
					return database.ExecuteWrite(ctx, write.actor, "failure_key_0002", "node_create", []byte(`{}`), writeReplayCheck, func(context.Context) ([]byte, error) { return []byte(`{"ok":true}`), nil })
				}
				return []byte(`{"ok":true}`), nil
			})
			if err == nil || result != nil || runs != 1 || lifeAcceptanceFingerprint(t, database) != before {
				t.Fatal("실패가 부분 적용/예약을 남김", mode, err)
			}
			if mode == "apply failure" && (!errors.Is(err, cause) || strings.Contains(err.Error(), "synthetic private")) {
				t.Fatal("오류 원인·비노출")
			}
			if mode == "cancel" && (!errors.Is(err, cause) || !errors.Is(err, context.Canceled)) {
				t.Fatal("취소 원인 누락")
			}
			remove()
			if _, err := database.ExecuteWrite(t.Context(), write.actor, "failure_key_0001", "node_create", []byte(`{"body":"value"}`), writeReplayCheck, func(ctx context.Context) ([]byte, error) { return []byte(`{"ok":true}`), write.run(ctx) }); err != nil {
				t.Fatal("같은 판·한도·키 재시도 실패", err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestMCP05DeterministicRejectionDatabase(t *testing.T) {
	database, second, _ := newAgentContractDatabase(t)
	graph, actor, source := boundaryGraph(t, database)
	stored := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	next := stored
	next.Body = "충돌 요청"
	next.Version += 2
	runs := 0
	before := registrationStateFingerprint(t, database)
	apply := func(ctx context.Context) ([]byte, error) {
		runs++
		_, err := database.UpdateContextWithOperation(ctx, graph, stored.Version+1, next, registrationOperation(stored, OperationUpdate), WriteLimits{})
		if _, ok := errors.AsType[model.VersionConflictError](err); !ok {
			return nil, fmt.Errorf("기대 판 충돌이 아니다: %w", err)
		}
		op := registrationOperation(stored, OperationUpdate)
		if err := database.RecordRejectedOperation(ctx, *op, "version_conflict"); err != nil {
			return nil, err
		}
		return []byte(`{"error":{"code":"version_conflict","current_version":1}}`), nil
	}
	first, err := database.ExecuteWrite(t.Context(), actor, "rejection_key_001", "node_update", []byte(`{"expected_version":2}`), writeReplayCheck, apply)
	if err != nil {
		t.Fatal(err)
	}
	if before == registrationStateFingerprint(t, database) {
		t.Fatal("거부 감사 누락")
	}
	after := lifeAcceptanceFingerprint(t, database)
	secondResult, err := second.ExecuteWrite(t.Context(), actor, "rejection_key_001", "node_update", []byte(`{"expected_version":2}`), writeReplayCheck, apply)
	if err != nil || !bytes.Equal(first, secondResult) || runs != 1 || after != lifeAcceptanceFingerprint(t, database) {
		t.Fatal("결정적 거부 재적용", err)
	}
}

func TestMCP05PostCommitCancellationDatabase(t *testing.T) {
	database, second, _ := newAgentContractDatabase(t)
	write := lifeAcceptanceFixture(t, database, "node_create")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runs := 0
	apply := func(ctx context.Context) ([]byte, error) {
		runs++
		if err := write.run(ctx); err != nil {
			return nil, err
		}
		if !deferAfterWriteCommit(ctx, cancel) {
			t.Fatal("바깥 커밋 후 동작 등록 실패")
		}
		return []byte(`{"committed":true}`), nil
	}
	result, err := database.ExecuteWrite(ctx, write.actor, "postcommit_key_1", "node_create", []byte(`{}`), writeReplayCheck, apply)
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatal("커밋 뒤 취소 전달", err)
	}
	after := lifeAcceptanceFingerprint(t, database)
	value, err := second.ExecuteWrite(t.Context(), write.actor, "postcommit_key_1", "node_create", []byte(`{}`), writeReplayCheck, apply)
	if err != nil || !bytes.Contains(value, []byte("true")) || runs != 1 || after != lifeAcceptanceFingerprint(t, database) {
		t.Fatal("커밋된 결과 유실/중복 적용", err)
	}
}

func waitMCP05Lock(t *testing.T, database *Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("실제 자문 잠금 대기를 관찰하지 못함")
}

func TestMCP05CurrentGuardAfterLockDatabase(t *testing.T) {
	for _, mode := range []string{"revoked agent", "lost grade", "wait cancelled"} {
		t.Run(mode, func(t *testing.T) {
			first, second, _ := newAgentContractDatabase(t)
			account := newTestID(t)
			createTestAccount(t, first, account)
			agent, err := first.RegisterAgent(t.Context(), account, "verified-client", "registration_key_1")
			if err != nil {
				t.Fatal(err)
			}
			runner, err := mcp.NewWriteRunner(first, first)
			if err != nil {
				t.Fatal(err)
			}
			otherRunner, err := mcp.NewWriteRunner(second, second)
			if err != nil {
				t.Fatal(err)
			}
			args := []byte(fmt.Sprintf(`{"graph_id":"%s","context_id":"%s","agent_id":"%s","request_id":"waiting_key_0001"}`, newTestID(t).String(), newTestID(t).String(), agent.String()))
			principal := mcp.Principal{AccountID: account, ClientID: "verified-client"}
			entered, release := make(chan struct{}), make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			defer releaseOnce()
			var allowed atomic.Bool
			allowed.Store(true)
			var applies atomic.Int64
			check := func(context.Context) error {
				if !allowed.Load() {
					return model.ErrNotFound
				}
				return nil
			}
			initial := make(chan error, 1)
			go func() {
				_, err := runner.Execute(t.Context(), principal, "node_discard", args, check, func(context.Context) ([]byte, error) {
					applies.Add(1)
					close(entered)
					<-release
					return []byte(`{"ok":true}`), nil
				})
				initial <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("최초 처리 진입 실패")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waiting := make(chan error, 1)
			go func() {
				_, err := otherRunner.Execute(ctx, principal, "node_discard", args, check, func(context.Context) ([]byte, error) { applies.Add(1); return []byte(`{"ok":true}`), nil })
				waiting <- err
			}()
			waitMCP05Lock(t, first)
			switch mode {
			case "revoked agent":
				if err := first.RevokeAgent(t.Context(), account, agent); err != nil {
					t.Fatal(err)
				}
			case "lost grade":
				allowed.Store(false)
			case "wait cancelled":
				cancel()
			}
			releaseOnce()
			if err := <-initial; err != nil {
				t.Fatal(err)
			}
			err = <-waiting
			if mode == "revoked agent" {
				requireAgentField(t, err, "agent_id")
			} else if mode == "lost grade" && !errors.Is(err, model.ErrNotFound) {
				t.Fatal("대기 중 권한 상실 허용", err)
			} else if mode == "wait cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("잠금 취소 누락", err)
			}
			if applies.Load() != 1 {
				t.Fatal("대기 재생이 업무를 다시 적용")
			}
			assertConnectionRecovery(t, first, nil, 0)
			assertConnectionRecovery(t, second, nil, 0)
		})
	}
}

func TestMCP05InvalidBoundary(t *testing.T) {
	var database Store
	account := newTestID(t)
	for _, mode := range []string{"account", "tool", "key", "check", "apply", "normalized", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			actor, key, tool, input := account, "valid_key_000001", "graph_create", []byte(`{}`)
			check, apply := writeReplayCheck, func(context.Context) ([]byte, error) { t.Fatal("잘못된 입력의 업무 호출"); return nil, nil }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "account":
				actor = model.ID{}
			case "tool":
				tool = "graph_list"
			case "key":
				key = "short"
			case "check":
				check = nil
			case "apply":
				apply = nil
			case "normalized":
				input = []byte(`{`)
			case "cancel":
				cancel()
			}
			if value, err := database.ExecuteWrite(ctx, actor, key, tool, input, check, apply); err == nil || value != nil {
				t.Fatal("입력 선행 거부 실패")
			}
		})
	}
}

func TestMCP05KeylessBoundary(t *testing.T) {
	// 키 생략 자체는 예약·풀을 사용하지 않는다. 실제 업무 트랜잭션은 apply가 소유한다.
	var database Store
	account := newTestID(t)
	cause := errors.New("synthetic private failure")
	for _, mode := range []string{"complete", "incomplete", "failure", "cancel", "check failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			value, err := database.ExecuteWrite(ctx, account, "", "graph_create", []byte(`{}`), func(context.Context) error {
				if mode == "check failure" {
					return model.ErrNotFound
				}
				return nil
			}, func(context.Context) ([]byte, error) {
				calls++
				switch mode {
				case "incomplete":
					return nil, nil
				case "failure":
					return nil, cause
				case "cancel":
					cancel()
				}
				return []byte(`{"ok":true}`), nil
			})
			if mode == "complete" {
				if err != nil || value == nil || calls != 1 {
					t.Fatal("키 없는 실행 실패", err)
				}
				return
			}
			if err == nil || value != nil {
				t.Fatal("미완료 결과 수락")
			}
			if mode == "failure" && (!errors.Is(err, cause) || strings.Contains(err.Error(), "synthetic private")) {
				t.Fatal("오류 원인·비노출")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("취소 누락")
			}
			if mode == "check failure" && calls != 0 {
				t.Fatal("현재 거부 뒤 업무 실행")
			}
		})
	}
}

func TestMCP05ExpiryAfterLockDatabase(t *testing.T) {
	first, second, _ := newAgentContractDatabase(t)
	account := newTestID(t)
	var runs atomic.Int64
	apply := func(context.Context) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"run":%d}`, runs.Add(1))), nil
	}
	const key = "expiry_lock_0001"
	input := []byte(`{"name":"same"}`)
	if _, err := first.ExecuteWrite(t.Context(), account, key, "graph_create", input, writeReplayCheck, apply); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	owner, waiter := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := first.ExecuteWrite(t.Context(), account, key, "graph_create", input, func(context.Context) error {
			close(entered)
			<-release
			return nil
		}, apply)
		owner <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("잠금 소유자 진입 실패")
	}
	go func() {
		_, err := second.ExecuteWrite(t.Context(), account, key, "graph_create", input, writeReplayCheck, apply)
		waiter <- err
	}()
	waitMCP05Lock(t, first)
	if _, err := first.pool.Exec(t.Context(), `UPDATE public.idempotency_record SET expires_at=clock_timestamp()-interval '1 microsecond' WHERE actor_account_id=$1 AND idempotency_key=$2`, account.String(), key); err != nil {
		t.Fatal(err)
	}
	finish()
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 2 {
		t.Fatal("잠금 전 만료 상태 재사용 또는 중복 재실행", runs.Load())
	}
}
