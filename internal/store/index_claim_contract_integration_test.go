package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

func newIndexClaimFixture(t *testing.T, count int) (*Store, model.ID, []model.Context) {
	t.Helper()
	database := newEmbeddingContractStore(t, "vector", 8)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://claim/"+graph.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []model.Context{source}
	for range count - 1 {
		node, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	return database, graph, nodes
}

func claimExec(t *testing.T, database *Store, sql string, args ...any) {
	t.Helper()
	if _, err := database.pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func requireIndexClaim(t *testing.T, database *Store, graph model.ID, want model.ID) (IndexTask, indexTaskClaim) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	task, claim, found, err := database.claimIndexTask(ctx, graph)
	if err != nil || !found || task.ContextID != want || task.GraphID != graph {
		t.Fatalf("확보=%+v, found=%t, want=%s: %v", task, found, want, err)
	}
	state := registrationTaskFor(t, database, want)
	if state.id != task.ID.String() || !state.enqueued.Equal(claim.enqueuedAt) || !state.next.Equal(claim.leasedUntil) || state.state != "pending" {
		t.Fatalf("확보 세대와 커밋된 작업 불일치: %+v, %+v", claim, state)
	}
	return task, claim
}

func TestIndexClaimOrderAndLeaseIntegration(t *testing.T) {
	database, graph, nodes := newIndexClaimFixture(t, 5)
	correlation := newTestID(t)
	// 힙 삽입 순서와 반대로 식별자를 배치해 동률에서 task_id 정렬을 검증한다.
	lower, higher := newTestID(t), newTestID(t)
	if lower.Compare(higher) > 0 {
		lower, higher = higher, lower
	}
	claimExec(t, database, `UPDATE public.index_task SET task_id=$2 WHERE context_id=$1`, nodes[0].ID.String(), higher.String())
	claimExec(t, database, `UPDATE public.index_task SET task_id=$2 WHERE context_id=$1`, nodes[1].ID.String(), lower.String())
	for _, node := range nodes {
		claimExec(t, database, `UPDATE public.index_task SET enqueued_at='2000-01-01', next_attempt_at='2000-01-01',
			attempts=2, last_error='previous', correlation_id=$2 WHERE context_id=$1`, node.ID.String(), correlation.String())
	}
	claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()+interval '1 hour' WHERE context_id=$1`, nodes[2].ID.String())
	claimExec(t, database, `UPDATE public.index_task SET state='failed', enqueued_at='1990-01-01' WHERE context_id=$1`, nodes[3].ID.String())
	claimExec(t, database, `UPDATE public.index_task SET enqueued_at='2001-01-01' WHERE context_id=$1`, nodes[4].ID.String())
	deferred, failed := registrationTaskFor(t, database, nodes[2].ID), registrationTaskFor(t, database, nodes[3].ID)
	// 같은 DB 문장의 시각과 대조해 호스트 시계로 계산한 리스를 검출한다.
	claimExec(t, database, `CREATE TABLE public.claim_clock_sample(task_id uuid, marked_at timestamptz);
		CREATE FUNCTION public.claim_clock_sample() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO public.claim_clock_sample VALUES (NEW.task_id, statement_timestamp()); RETURN NEW; END $$;
		CREATE TRIGGER claim_clock_sample BEFORE UPDATE ON public.index_task FOR EACH ROW EXECUTE FUNCTION public.claim_clock_sample()`)
	tied := slices.Clone(nodes[:2])
	taskIDs := make(map[model.ID]string)
	for _, node := range tied {
		taskIDs[node.ID] = registrationTaskFor(t, database, node.ID).id
	}
	slices.SortFunc(tied, func(a, b model.Context) int {
		return cmp.Compare(taskIDs[a.ID], taskIDs[b.ID])
	})
	order := append(tied, nodes[4])
	var first indexTaskClaim
	for i, node := range order {
		before := registrationTaskFor(t, database, node.ID)
		task, claim := requireIndexClaim(t, database, graph, node.ID)
		if i == 0 {
			first = claim
		}
		var marked time.Time
		if err := database.pool.QueryRow(t.Context(), `SELECT marked_at FROM public.claim_clock_sample WHERE task_id=$1`, task.ID.String()).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if claim.leasedUntil.Sub(marked) != 2*time.Minute || task.Attempts != 2 || task.CorrelationID != correlation.String() {
			t.Fatalf("리스·메타데이터=%+v, DB 시각=%v, 작업=%+v", claim, marked, task)
		}
		after := registrationTaskFor(t, database, node.ID)
		after.next = before.next
		if !reflect.DeepEqual(after, before) {
			t.Fatal("확보가 리스 이외 필드를 바꿨다")
		}
	}
	before := registrationStateFingerprint(t, database)
	if _, _, found, err := database.claimIndexTask(t.Context(), graph); err != nil || found || registrationStateFingerprint(t, database) != before {
		t.Fatalf("유효 리스·미래 재시도·failed를 확보했다: %t, %v", found, err)
	}
	claimExec(t, database, `DROP TRIGGER claim_clock_sample ON public.index_task`)
	// 작업자가 결과를 반환하지 않고 중단된 상태에서, 자기 작업의 리스만 만료시킨다.
	claimExec(t, database, `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second' WHERE context_id=$1`, order[0].ID.String())
	_, reclaimed := requireIndexClaim(t, database, graph, order[0].ID)
	if !reclaimed.enqueuedAt.Equal(first.enqueuedAt) || !reclaimed.leasedUntil.After(first.leasedUntil) {
		t.Fatalf("재확보 세대=%+v, 이전=%+v", reclaimed, first)
	}
	if !reflect.DeepEqual(deferred, registrationTaskFor(t, database, nodes[2].ID)) || !reflect.DeepEqual(failed, registrationTaskFor(t, database, nodes[3].ID)) {
		t.Fatal("확보하지 않은 작업이 바뀌었다")
	}
	t.Log("DB 문장 시각 대비 120초 리스, 등록 시각·식별자 순서, 중단 후 재확보 확인")
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIndexClaimSkipsLockedIntegration(t *testing.T) {
	database, graph, nodes := newIndexClaimFixture(t, 2)
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), tx)
	var locked string
	if err := tx.QueryRow(t.Context(), `SELECT context_id::text FROM public.index_task WHERE graph_id=$1
		ORDER BY enqueued_at, task_id LIMIT 1 FOR UPDATE`, graph.String()).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	lockedID, err := model.ParseID(locked)
	if err != nil {
		t.Fatal(err)
	}
	other := nodes[0].ID
	if other == lockedID {
		other = nodes[1].ID
	}
	before := registrationTaskFor(t, database, lockedID)
	requireIndexClaim(t, database, graph, other)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, found, err := database.claimIndexTask(ctx, graph); err != nil || found {
		t.Fatalf("잠긴 선두와 유효 리스 건너뛰기=%t: %v", found, err)
	}
	if !reflect.DeepEqual(before, registrationTaskFor(t, database, lockedID)) {
		t.Fatal("잠긴 작업을 변경했다")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireIndexClaim(t, database, graph, lockedID)
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIndexClaimConcurrentIntegration(t *testing.T) {
	database, graph, nodes := newIndexClaimFixture(t, 12)
	configureTierPool(t, database, nil)
	type outcome struct {
		task  IndexTask
		found bool
		err   error
	}
	results := make(chan outcome, 24)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range cap(results) {
		workers.Go(func() {
			<-start
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			task, _, found, err := database.claimIndexTask(ctx, graph)
			results <- outcome{task, found, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	seen := make(map[string]bool)
	empty := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if !result.found {
			empty++
			continue
		}
		if seen[result.task.ID.String()] || result.task.GraphID != graph {
			t.Fatalf("중복·범위 외 확보=%+v", result.task)
		}
		seen[result.task.ID.String()] = true
	}
	if len(seen) != len(nodes) || empty != cap(results)-len(nodes) {
		t.Fatalf("고유 확보=%d, 빈 회차=%d", len(seen), empty)
	}
	for _, node := range nodes {
		state := registrationTaskFor(t, database, node.ID)
		if !seen[state.id] || state.attempts != 0 || state.state != "pending" {
			t.Fatalf("확보 후 상태=%+v", state)
		}
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIndexClaimScopeIntegration(t *testing.T) {
	database, graph, _ := newIndexClaimFixture(t, 2)
	other := createIndexScopeGraph(t, database, newTestIDWithAccount(t, database), "claim-other")
	claimExec(t, database, `UPDATE public.index_task SET enqueued_at='1990-01-01', next_attempt_at='1990-01-01', attempts=3, last_error='other' WHERE graph_id=$1`, other.String())
	var before, after string
	fingerprint := `SELECT md5(jsonb_agg(to_jsonb(task) ORDER BY task_id)::text) FROM public.index_task task WHERE graph_id=$1`
	if err := database.pool.QueryRow(t.Context(), fingerprint, other.String()).Scan(&before); err != nil {
		t.Fatal(err)
	}
	processed := 0
	for {
		result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, claimProcessor)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Found {
			break
		}
		if !result.Succeeded || result.Task.GraphID != graph {
			t.Fatalf("그래프 한정 처리=%+v", result)
		}
		processed++
	}
	if err := database.pool.QueryRow(t.Context(), fingerprint, other.String()).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if processed != 2 || before != after {
		t.Fatalf("처리=%d, 다른 그래프 상태 변경=%t", processed, before != after)
	}
	unchanged := registrationStateFingerprint(t, database)
	for _, id := range []model.ID{{}, {0: 1}} {
		if _, err := database.ProcessNextIndexTaskInGraph(t.Context(), id, claimProcessor); err == nil {
			t.Fatal("잘못된 그래프를 받았다")
		}
	}
	if _, err := database.ProcessNextIndexTask(t.Context(), nil); err == nil {
		t.Fatal("빈 제공자를 받았다")
	}
	if _, err := database.ProcessNextIndexTaskInGraph(t.Context(), other, nil); err == nil {
		t.Fatal("범위 확보가 빈 제공자를 받았다")
	}
	if result, err := database.ProcessNextIndexTaskInGraph(t.Context(), newTestID(t), claimProcessor); err != nil || result.Found {
		t.Fatalf("없는 그래프=%+v: %v", result, err)
	}
	if registrationStateFingerprint(t, database) != unchanged {
		t.Fatal("무효·빈 확보가 저장 상태를 바꿨다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func newTestIDWithAccount(t *testing.T, database *Store) model.ID {
	t.Helper()
	id := newTestID(t)
	createTestAccount(t, database, id)
	return id
}

func claimProcessor(context.Context, IndexTask) IndexTaskResult {
	return IndexTaskResult{Embedding: []float64{1, 0, 0, 0, 0, 0, 0, 0}, ModelID: "idx02-fixture"}
}

func TestIndexClaimGlobalIntegration(t *testing.T) {
	// 운영 진입점은 새 DB 전체를 소유한 시험에서만 호출한다.
	database, first, nodes := newIndexClaimFixture(t, 2)
	second := createIndexScopeGraph(t, database, newTestIDWithAccount(t, database), "claim-global")
	claimExec(t, database, `UPDATE public.index_task SET enqueued_at='2001-01-01', next_attempt_at='2000-01-01' WHERE graph_id=$1`, second.String())
	claimExec(t, database, `UPDATE public.index_task SET enqueued_at='2000-01-01', next_attempt_at='2000-01-01' WHERE context_id=$1`, nodes[0].ID.String())
	claimExec(t, database, `UPDATE public.index_task SET enqueued_at='2002-01-01', next_attempt_at='2000-01-01' WHERE context_id=$1`, nodes[1].ID.String())
	for _, graph := range []model.ID{first, second, second, first} {
		result, err := database.ProcessNextIndexTask(t.Context(), claimProcessor)
		if err != nil || !result.Succeeded || result.Task.GraphID != graph {
			t.Fatalf("전체 그래프 처리=%+v, want=%s: %v", result, graph, err)
		}
	}
	if result, err := database.ProcessNextIndexTask(t.Context(), claimProcessor); err != nil || result.Found {
		t.Fatalf("빈 큐=%+v: %v", result, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIndexClaimProviderOutsideTransactionIntegration(t *testing.T) {
	database, graph, nodes := newIndexClaimFixture(t, 2)
	configureTierPool(t, database, nil)
	target := nodes[1]
	readyIndexTasks(t, database, graph, target.ID)
	entered, release := make(chan IndexTask, 1), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	type outcome struct {
		result IndexProcessResult
		err    error
	}
	done := make(chan outcome, 1)
	finished := false
	t.Cleanup(func() {
		unblock()
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("제공자 작업자 종료 실패")
			}
		}
	})
	go func() {
		result, err := database.ProcessNextIndexTaskInGraph(ctx, graph, func(ctx context.Context, task IndexTask) IndexTaskResult {
			entered <- task
			select {
			case <-release:
				return claimProcessor(ctx, task)
			case <-ctx.Done():
				return IndexTaskResult{Failure: "canceled", Retryable: true}
			}
		})
		done <- outcome{result, err}
	}()
	select {
	case task := <-entered:
		if task.ContextID != target.ID || task.Body != target.Body {
			t.Fatalf("제공자 입력=%+v", task)
		}
	case <-ctx.Done():
		t.Fatal("제공자 호출 시작 실패")
	}
	// 제공자가 응답하지 않는 동안에도 풀 점유 0·행 잠금 해제를 직접 확인한다.
	assertConnectionRecovery(t, database, nil, 0)
	tx, err := database.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), tx)
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM public.index_task WHERE context_id=$1 FOR UPDATE NOWAIT`, target.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := database.claimIndexTask(t.Context(), graph); err != nil || found {
		t.Fatalf("제공자 대기 중 유효 리스를 확보했다: %t, %v", found, err)
	}
	next := target
	next.Body += " 제공자 대기 중 저장"
	next.Version++
	saveCtx, saveCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer saveCancel()
	if _, err := database.UpdateContext(saveCtx, graph, target.Version, next); err != nil {
		t.Fatalf("제공자 대기 중 본문 저장=%v", err)
	}
	unblock()
	select {
	case result := <-done:
		finished = true
		if result.err != nil || !result.result.Found || result.result.Succeeded {
			t.Fatalf("대기 해제 후 무효 확보=%+v: %v", result.result, result.err)
		}
	case <-ctx.Done():
		t.Fatal("작업자 완료 실패")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestIndexClaimRollbackIntegration(t *testing.T) {
	for _, mode := range []string{"invalid_id", "update_error", "commit_error", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			database, graph, nodes := newIndexClaimFixture(t, 1)
			original := registrationTaskFor(t, database, nodes[0].ID).id
			if mode == "invalid_id" {
				claimExec(t, database, `UPDATE public.index_task SET task_id='00000000-0000-4000-8000-000000000001'`)
			} else {
				body := "RAISE EXCEPTION 'claim fixture' USING ERRCODE='P0001';"
				trigger := "CREATE TRIGGER claim_fault BEFORE UPDATE ON public.index_task FOR EACH ROW EXECUTE FUNCTION public.claim_fault()"
				if mode == "deadline" {
					claimExec(t, database, `CREATE SEQUENCE public.claim_entered`)
					body = "PERFORM nextval('public.claim_entered'); PERFORM pg_sleep(30);"
				}
				if mode == "commit_error" {
					trigger = "CREATE CONSTRAINT TRIGGER claim_fault AFTER UPDATE ON public.index_task DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.claim_fault()"
				}
				claimExec(t, database, fmt.Sprintf(`CREATE FUNCTION public.claim_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN %s RETURN NEW; END $$; %s`, body, trigger))
			}
			before := registrationStateFingerprint(t, database)
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			_, _, found, err := database.claimIndexTask(ctx, graph)
			if err == nil || found {
				t.Fatalf("실패해야 할 확보=%t: %v", found, err)
			}
			if mode == "deadline" {
				var entered bool
				if queryErr := database.pool.QueryRow(t.Context(), `SELECT is_called FROM public.claim_entered`).Scan(&entered); queryErr != nil || !entered || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("DB 대기·취소=%t: %v, %v", entered, err, queryErr)
				}
			} else if mode != "invalid_id" {
				if cause, ok := errors.AsType[*pgconn.PgError](err); !ok || cause.Code != "P0001" {
					t.Fatalf("DB 원인 누락=%v", err)
				}
			}
			if registrationStateFingerprint(t, database) != before {
				t.Fatal("확보 실패가 저장 상태를 바꿨다")
			}
			assertConnectionRecovery(t, database, nil, 0)
			if mode == "invalid_id" {
				claimExec(t, database, `UPDATE public.index_task SET task_id=$1`, original)
			} else {
				claimExec(t, database, `DROP TRIGGER claim_fault ON public.index_task`)
			}
			requireIndexClaim(t, database, graph, nodes[0].ID)
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
