package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

func TestWriteRateInputBoundary(t *testing.T) {
	database := &Store{}
	actor, graph := newTestID(t), newTestID(t)
	source := testSourceContext(t, graph, actor, "api://perm06/input")
	derived := testDerivedContext(t, graph, actor)
	for _, id := range []model.ID{{}, {6: 0x40, 8: 0x80}} {
		limits := WriteLimits{ActorID: id, WritesPerMinute: 1}
		for name, run := range map[string]func() error{
			"create": func() error {
				_, err := database.CreateContextWithOperation(t.Context(), graph, source, nil, nil, limits)
				return err
			},
			"update": func() error {
				_, err := database.UpdateContextWithOperation(t.Context(), graph, 1, derived, nil, limits)
				return err
			},
			"supersede": func() error {
				_, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, derived, []model.ID{source.ID}, newTestID(t), nil, limits)
				return err
			},
		} {
			t.Run(name+"/"+id.String(), func(t *testing.T) {
				field, ok := errors.AsType[model.FieldError](run())
				if !ok || field.Field != "actor_account_id" {
					t.Fatalf("요청 계정 선검사=%+v", field)
				}
			})
		}
	}
	for _, input := range []struct {
		id    model.ID
		at    time.Time
		limit int
	}{
		{actor, time.Time{}, 1}, {model.ID{}, time.Now(), 1}, {actor, time.Now(), -1},
	} {
		if _, _, err := database.TryIncrementRequestRate(t.Context(), input.id, input.at, input.limit); err == nil {
			t.Fatal("잘못된 빈도 입력 통과")
		}
	}
	if count, accepted, err := database.TryIncrementRequestRate(t.Context(), actor, time.Now(), 0); err != nil || !accepted || count != 0 {
		t.Fatalf("무제한의 DB 없는 경계=%d/%t/%v", count, accepted, err)
	}
}

func TestWriteRateFixedWindowsIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	account := newTestID(t)
	start := time.Date(2028, 2, 29, 23, 59, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		at       time.Time
		limit    int
		count    int
		accepted bool
	}{
		{"처음", start, 2, 1, true},
		{"같은 창 다른 시간대", start.Add(59 * time.Second).In(time.FixedZone("KST", 9*60*60)), 2, 2, true},
		{"경계 직전 거부", start.Add(time.Minute - time.Nanosecond), 2, 2, false},
		{"상향 즉시", start.Add(59 * time.Second), 3, 3, true},
		{"하향 기존 수 유지", start.Add(59 * time.Second), 1, 3, false},
		{"무제한 기존 행 미갱신", start.Add(59 * time.Second), 0, 0, true},
		{"다시 제한", start.Add(59 * time.Second), 3, 3, false},
		{"윤일 다음 달 경계", start.Add(time.Minute), 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count, accepted, err := database.TryIncrementRequestRate(t.Context(), account, tc.at, tc.limit)
			if err != nil || count != tc.count || accepted != tc.accepted {
				t.Fatalf("빈도=%d/%t/%v, want %d/%t", count, accepted, err, tc.count, tc.accepted)
			}
		})
	}
	other := newTestID(t)
	if count, ok, err := database.TryIncrementRequestRate(t.Context(), other, start, 1); err != nil || !ok || count != 1 {
		t.Fatalf("계정 격리=%d/%t/%v", count, ok, err)
	}
	if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.request_rate VALUES ($1,$2,$3)`, account.String(), start.Add(2*time.Minute), int64(math.MaxInt64-1)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false} {
		count, ok, err := tryIncrementRequestRate(t.Context(), database.pool, account, start.Add(2*time.Minute), math.MaxInt64)
		if err != nil || ok != want || count != math.MaxInt64 {
			t.Fatalf("bigint 경계=%d/%t/%v", count, ok, err)
		}
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestWriteRateContextScopeIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	contributor, actor := newTestID(t), newTestID(t)
	createTestAccount(t, database, contributor)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, contributor)
	limits := WriteLimits{ActorID: actor, WritesPerMinute: 10}
	source, err := database.CreateContextWithOperation(t.Context(), graph, testSourceContext(t, graph, contributor, "api://perm06/scope/"+graph.String()), nil, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateContextWithOperation(t.Context(), graph, testDerivedContext(t, graph, contributor), []model.ID{source.ID}, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	next := node
	next.Version++
	next.Body += "수정"
	node, err = database.UpdateContextWithOperation(t.Context(), graph, node.Version, next, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	replacement := testDerivedContext(t, graph, contributor)
	if _, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, replacement, []model.ID{source.ID}, node.ID, nil, limits); err != nil {
		t.Fatal(err)
	}
	if got := currentRateCount(t, database, actor); got != 4 {
		t.Fatalf("생성·수정·대체=%d, want 4", got)
	}
	if got := currentRateCount(t, database, contributor); got != 0 {
		t.Fatalf("기여 계정을 요청 계정으로 셌다: %d", got)
	}
	seedSaturatedWriteWindows(t, database, actor, 10)
	before := currentRateCount(t, database, actor)
	duplicate := source
	duplicate.ID = newTestID(t)
	if found, err := database.CreateContextWithOperation(t.Context(), graph, duplicate, nil, nil, limits); err != nil || found.ID != source.ID {
		t.Fatalf("소진 뒤 기존 원천=%s/%v", found.ID, err)
	}
	if _, err := database.Context(t.Context(), graph, source.ID); err != nil {
		t.Fatal(err)
	}
	newGraph := model.Graph{ID: newTestID(t), Name: "빈도 제외", CreatedBy: actor, CreatedAt: nowUTC(), LastActivityAt: nowUTC(), Version: 1}
	if _, err := database.CreateGraphWithOwner(t.Context(), newGraph, limits); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DiscardContext(t.Context(), graph, source.ID, testMCPDiscardOperation(source), limits); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RestoreContext(t.Context(), graph, source.ID, nil, limits); err != nil {
		t.Fatal(err)
	}
	value := testSourceContext(t, graph, contributor, "api://perm06/unlimited/"+graph.String())
	if _, err := database.CreateContextWithOperation(t.Context(), graph, value, nil, nil, WriteLimits{ActorID: actor}); err != nil {
		t.Fatal(err)
	}
	if got := currentRateCount(t, database, actor); got != before {
		t.Fatalf("조회·그래프·폐기·복구·중복·무제한이 빈도를 바꿨다: %d/%d", got, before)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestWriteRateAtomicReplayIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	limits := WriteLimits{ActorID: actor, WritesPerMinute: 100}
	source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://perm06/seed/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"version", "storage", "audit", "outer_rollback", "result_json"} {
		t.Run(failure, func(t *testing.T) {
			before := planStateFingerprint(t, database, graph)
			value := testSourceContext(t, graph, actor, "api://perm06/rollback/"+newTestID(t).String())
			innerSaved := false
			request := IdempotencyRequest{AccountID: actor, Key: newTestID(t), ToolName: "node_create"}
			switch failure {
			case "version":
				next := node
				next.Version++
				next.Body += "충돌"
				_, err = database.UpdateContextWithOperation(t.Context(), graph, node.Version+7, next, nil, limits)
			case "storage":
				limited := limits
				limited.StoredCharsPerGraph = 1
				_, err = database.CreateContextWithOperation(t.Context(), graph, value, nil, nil, limited)
			case "audit":
				operation := registrationOperation(value, OperationAdd)
				operation.AccountID = model.ID{}
				_, err = database.CreateContextWithOperation(t.Context(), graph, value, nil, operation, limits)
			default:
				_, err = database.ReplayIdempotent(t.Context(), request, func(ctx context.Context) ([]byte, error) {
					if _, err := database.CreateContextWithOperation(ctx, graph, value, nil, registrationOperation(value, OperationAdd), limits); err != nil {
						return nil, err
					}
					innerSaved = true
					if failure == "outer_rollback" {
						return nil, errors.New("synthetic outer failure")
					}
					return []byte(`invalid json`), nil
				})
			}
			if err == nil {
				t.Fatal("의도한 거부가 성공했다")
			}
			if failure == "outer_rollback" || failure == "result_json" {
				if !innerSaved {
					t.Fatalf("업무 저장 뒤 실패 지점에 도달하지 않았다: %v", err)
				}
				var exists bool
				if err := database.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM public.idempotency_record WHERE actor_account_id=$1 AND idempotency_key=$2)`, actor.String(), request.Key.String()).Scan(&exists); err != nil || exists {
					t.Fatalf("실패한 멱등성 예약이 남았다: %t/%v", exists, err)
				}
			}
			if before != planStateFingerprint(t, database, graph) || currentRateCount(t, database, actor) != 0 {
				t.Fatal("거부 뒤 본문·누계·색인·감사·빈도가 남았다")
			}
		})
	}
	request := IdempotencyRequest{AccountID: actor, Key: newTestID(t), ToolName: "node_create"}
	value := testSourceContext(t, graph, actor, "api://perm06/replay/"+graph.String())
	runs := 0
	write := func(ctx context.Context) ([]byte, error) {
		runs++
		_, err := database.CreateContextWithOperation(ctx, graph, value, nil, registrationOperation(value, OperationAdd), limits)
		return []byte(`{"saved":true}`), err
	}
	if _, err := database.ReplayIdempotent(t.Context(), request, write); err != nil {
		t.Fatal(err)
	}
	if got := currentRateCount(t, database, actor); got != 1 {
		t.Fatalf("최초 멱등 쓰기 빈도=%d", got)
	}
	before := planStateFingerprint(t, database, graph)
	seedSaturatedWriteWindows(t, database, actor, 100)
	count := currentRateCount(t, database, actor)
	for range 3 {
		if _, err := database.ReplayIdempotent(t.Context(), request, write); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 1 || currentRateCount(t, database, actor) != count || before != planStateFingerprint(t, database, graph) {
		t.Fatal("한도 소진 뒤 재생이 업무나 빈도를 다시 적용했다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestWriteRateConcurrentKeysIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	seedSaturatedWriteWindows(t, database, actor, 2)
	limits := WriteLimits{ActorID: actor, WritesPerMinute: 3}
	before := currentRateCount(t, database, actor)
	// 제한 직전의 같은 카운터 행을 두 실제 연결이 겹쳐 기다리게 한다.
	blocker, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.WithoutCancel(t.Context()))
	if _, err := blocker.Exec(t.Context(), `SELECT 1 FROM public.request_rate WHERE account_id=$1 FOR UPDATE`, actor.String()); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		value := testSourceContext(t, graph, actor, "api://perm06/parallel/"+newTestID(t).String())
		request := IdempotencyRequest{AccountID: actor, Key: newTestID(t), ToolName: "node_create"}
		group.Go(func() {
			_, err := database.ReplayIdempotent(t.Context(), request, func(ctx context.Context) ([]byte, error) {
				_, err := database.CreateContextWithOperation(ctx, graph, value, nil, registrationOperation(value, OperationAdd), limits)
				return []byte(`{"saved":true}`), err
			})
			results <- err
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	waiters := 0
	for time.Now().Before(deadline) {
		if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%INSERT INTO public.request_rate%'`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	group.Wait()
	close(results)
	if waiters != 2 {
		t.Fatalf("실제 카운터 잠금 대기=%d, want 2", waiters)
	}
	successes, denied := 0, 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		limit, ok := errors.AsType[plan.LimitError](err)
		if !ok || limit.Name != "writes_per_minute" || limit.Current != 3 || limit.Allowed != 3 {
			t.Fatalf("빈도 거부=%v", err)
		}
		denied++
	}
	if successes != 1 || denied != 1 || currentRateCount(t, database, actor) != before+1 {
		t.Fatalf("병행 빈도 결과=%d/%d", successes, denied)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestWriteRateCancellationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	seedSaturatedWriteWindows(t, database, actor, 0)
	before := planStateFingerprint(t, database, graph)
	blocker, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.WithoutCancel(t.Context()))
	if _, err := blocker.Exec(t.Context(), `SELECT 1 FROM public.request_rate WHERE account_id=$1 FOR UPDATE`, actor.String()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	value := testSourceContext(t, graph, actor, "api://perm06/cancel/"+graph.String())
	done := make(chan error, 1)
	go func() {
		_, err := database.CreateContextWithOperation(ctx, graph, value, nil, registrationOperation(value, OperationAdd), WriteLimits{ActorID: actor, WritesPerMinute: 1})
		done <- err
	}()
	waiters := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%INSERT INTO public.request_rate%'`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("카운터 잠금 대기 취소=%v", err)
	}
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if waiters != 1 || before != planStateFingerprint(t, database, graph) || currentRateCount(t, database, actor) != 0 {
		t.Fatalf("취소 뒤 상태 또는 대기 증거가 다르다: waiters=%d", waiters)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

// 분 경계에 시험이 걸쳐도 포화 조건이 유지되게 자기 계정의 인접 창만 준비한다.
func seedSaturatedWriteWindows(t *testing.T, database *Store, actor model.ID, count int64) {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Minute)
	for minute := range 3 {
		if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.request_rate VALUES ($1,$2,$3) ON CONFLICT (account_id,window_started_at) DO UPDATE SET count=EXCLUDED.count`, actor.String(), start.Add(time.Duration(minute)*time.Minute), count); err != nil {
			t.Fatal(fmt.Errorf("자기 계정 창 준비: %w", err))
		}
	}
}
