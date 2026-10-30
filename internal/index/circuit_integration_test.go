package index

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/diagnostic"
)

func TestProviderCircuitPreservesQueueIntegration(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			database, pool := newIndexContractDatabase(t)
			first, _ := readinessGraph(t, database, pool)
			second, _ := readinessGraph(t, database, pool)
			third, _ := readinessGraph(t, database, pool)
			var calls atomic.Int64
			var providerStatus atomic.Int64
			providerStatus.Store(int64(status))
			worker := readinessWorker(t, database, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if code := int(providerStatus.Load()); code != 200 {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(code)
					return
				}
				readinessResponse(t, w, 128)
			})
			now := time.Now()
			worker.providerNow = func() time.Time { return now }
			sink := &obs05ProviderEvents{}
			source, _ := diagnostic.NewSource("provider_circuit")
			reporter, _ := diagnostic.NewReporter(diagnostic.Index, sink, source)
			worker.SetDiagnostics(reporter)
			if found, err := worker.RunOnceInGraph(t.Context(), first); err != nil || !found {
				t.Fatal("첫 작업 실패 반영", found, err)
			}
			// 작업별 백오프가 아니라 제공자 차단 때문에 확보하지 않는지 대조한다.
			if _, err := pool.Exec(t.Context(), `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second'`); err != nil {
				t.Fatal(err)
			}
			snapshot := func() string {
				t.Helper()
				var value string
				if err := pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(task) ORDER BY task_id)::text FROM public.index_task task`).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			for range 10 {
				if found, err := worker.RunOnceInGraph(t.Context(), second); err != nil || found {
					t.Fatal("차단 중 다른 그래프 확보", found, err)
				}
				if found, err := worker.RunOnce(t.Context()); err != nil || found {
					t.Fatal("차단 중 전역 확보", found, err)
				}
			}
			if calls.Load() != 1 || before != snapshot() {
				t.Fatal("차단 대기가 제공자 호출·큐 세대·예산을 소진했다")
			}
			if worker.providerCircuit.retryAt.Before(now.Add(2 * time.Minute)) {
				t.Fatal("제공자 Retry-After를 반영하지 않았다")
			}
			now = worker.providerCircuit.retryAt
			if found, err := worker.RunOnceInGraph(t.Context(), second); err != nil || found {
				t.Fatal("실패한 탐침이 작업 확보", found, err)
			}
			if calls.Load() != 2 || before != snapshot() {
				t.Fatal("실패 탐침이 큐를 변경했다")
			}
			providerStatus.Store(200)
			now = worker.providerCircuit.retryAt
			if found, err := worker.RunOnceInGraph(t.Context(), second); err != nil || !found {
				t.Fatal("탐침 회복 후 기록 재개 실패", found, err)
			}
			if calls.Load() != 4 || !worker.providerCircuit.retryAt.IsZero() {
				t.Fatal("탐침과 단일 작업의 회복 호출 경계")
			}
			var remaining, attempts int
			var state string
			if err := pool.QueryRow(t.Context(), `SELECT state,attempts FROM public.index_task WHERE graph_id=$1`, first.String()).Scan(&state, &attempts); err != nil {
				t.Fatal(err)
			}
			want := "pending"
			if status == 401 || status == 403 {
				want = "failed"
			}
			if state != want || attempts != 1 {
				t.Fatal("최초 작업의 재시도 계약 변경", state, attempts)
			}
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.index_task WHERE graph_id=$1 AND attempts=0`, third.String()).Scan(&remaining); err != nil || remaining != 1 {
				t.Fatal("대기 작업 예산 손실", remaining, err)
			}
			if database.CheckGraphIndexReady(t.Context(), second, worker.ModelID()) != nil {
				t.Fatal("회복한 작업의 색인 미공개")
			}
			blocked, resumed := 0, 0
			for _, event := range sink.events {
				if event.Validate() != nil {
					t.Fatal("차단 진단 계약 위반", event)
				}
				if event.ReasonCode == "provider_blocked" {
					blocked++
				}
				if event.ReasonCode == "provider_resumed" {
					resumed++
				}
			}
			if blocked != 2 || resumed != 1 {
				t.Fatal("차단·탐침 실패·해제 진단 누락", blocked, resumed)
			}
		})
	}
}
