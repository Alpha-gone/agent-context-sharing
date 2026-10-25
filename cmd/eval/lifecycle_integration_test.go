package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func lifecycleInput(t *testing.T) contextSet {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return contextSet{Version: "eval03-" + id.String(), Contexts: []contextSpec{
		{Key: "source", Layer: "source", Body: "독립 표본의 원천이다.", Source: &sourceSpec{Channel: "conversation", Locator: "urn:eval03:source", OccurredAt: time.Now().UTC().Add(-time.Hour), OriginKind: "user_utterance"}},
		{Key: "derived", Layer: "derived", Body: "원천에 근거한 독립 파생이다.", Derived: &derivedSpec{Kind: "proposition", DerivedFrom: []string{"source"}, EvidenceState: "observation"}},
	}}
}

func requireEvaluationDeleted(t *testing.T, connection *pgx.Conn, version string) {
	t.Helper()
	var graphs, active int
	if err := connection.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM public.context_graph WHERE name = $1`, "eval "+version).Scan(&graphs, &active); err != nil {
		t.Fatal(err)
	}
	if graphs == 0 || active != 0 {
		t.Fatalf("평가 그래프 정리 누락: graphs=%d active=%d", graphs, active)
	}
}

func TestEvaluationGraphLifecycleIntegration(t *testing.T) {
	for _, outcome := range []string{"success", "measurement_failure", "cancellation", "silent_cancellation", "index_failure", "load_failure", "relation_failure"} {
		t.Run(outcome, func(t *testing.T) {
			database, worker, connection := evalDatabase(t, nil)
			set := lifecycleInput(t)
			if outcome == "index_failure" {
				worker = failingEvalWorker(t, database, connection, 400, 0)
			}
			if outcome == "load_failure" {
				set.Contexts[1].Derived.DerivedFrom = []string{"missing"}
			}
			if outcome == "relation_failure" {
				set.Relations = []relationSpec{{Type: "invalid", From: "source", To: "derived"}}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			marker := errors.New("measurement failed")
			called := false
			err := withEvaluationGraph(ctx, database, worker, set, 8, func(graph loadedGraph) error {
				called = true
				if err := database.CheckGraphIndexReady(ctx, graph.GraphID, worker.ModelID()); err != nil {
					t.Fatalf("미준비 상태에서 측정을 시작했다: %v", err)
				}
				switch outcome {
				case "measurement_failure":
					return marker
				case "cancellation":
					cancel()
					return ctx.Err()
				case "silent_cancellation":
					cancel()
				}
				return nil
			})
			if outcome == "success" && (err != nil || !called) {
				t.Fatalf("정상 수명: %v, measured=%v", err, called)
			}
			if outcome != "success" && err == nil {
				t.Fatal("실패를 성공으로 처리했다")
			}
			if strings.HasSuffix(outcome, "cancellation") && !errors.Is(err, context.Canceled) {
				t.Fatalf("취소 원인 유실: %v", err)
			}
			if outcome == "measurement_failure" && !errors.Is(err, marker) {
				t.Fatalf("측정 오류 유실: %v", err)
			}
			if (outcome == "index_failure" || outcome == "load_failure" || outcome == "relation_failure") && called {
				t.Fatal("준비 실패 뒤 측정이 실행됐다")
			}
			requireEvaluationDeleted(t, connection, set.Version)
		})
	}
}

func TestEvaluationGraphCleanupFailureJoinsCauseIntegration(t *testing.T) {
	for _, failure := range []string{"success", "measurement", "index", "load"} {
		t.Run(failure, func(t *testing.T) {
			database, worker, connection := evalDatabase(t, nil)
			set := lifecycleInput(t)
			// 거부 trigger를 제거한 다음에만 시험 자원을 다시 정리한다.
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
				defer cancel()
				if _, err := connection.Exec(ctx, `UPDATE public.context_graph SET deleted_at = now() WHERE name = $1`, "eval "+set.Version); err != nil {
					t.Error(err)
				}
			})
			rejectEvalCleanup(t, connection)
			if failure == "index" {
				worker = failingEvalWorker(t, database, connection, 400, 0)
			}
			if failure == "load" {
				set.Contexts[1].Derived.DerivedFrom = []string{"missing"}
			}
			marker := errors.New("original measurement error")
			err := withEvaluationGraph(t.Context(), database, worker, set, 8, func(loadedGraph) error {
				if failure == "measurement" {
					return marker
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "issue85 cleanup rejected") {
				t.Fatalf("정리 실패가 전파되지 않았다: %v", err)
			}
			if failure == "measurement" && !errors.Is(err, marker) {
				t.Fatalf("원래 오류 유실: %v", err)
			}
			if failure == "index" && !strings.Contains(err.Error(), "색인 미완료") {
				t.Fatalf("준비 오류 유실: %v", err)
			}
			if failure == "load" && !strings.Contains(err.Error(), "참조가 순환") {
				t.Fatalf("적재 오류 유실: %v", err)
			}
		})
	}
}

func TestEvaluationIndependentGraphsAndFiniteIndexingIntegration(t *testing.T) {
	database, worker, connection := evalDatabase(t, nil)
	foreignSet := lifecycleInput(t)
	foreign, err := loadGraph(t.Context(), database, foreignSet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dropGraph(t.Context(), database, foreign); err != nil {
			t.Error(err)
		}
	})
	seen := map[model.ID]bool{}
	for sample := range 2 {
		set := lifecycleInput(t)
		for repeat := range 2 {
			for condition := range 2 {
				err := withEvaluationGraph(t.Context(), database, worker, set, 2, func(graph loadedGraph) error {
					if seen[graph.GraphID] || graph.GraphID == foreign.GraphID {
						t.Fatal("조건·표본·회차가 그래프를 재사용했다")
					}
					seen[graph.GraphID] = true
					return nil
				})
				if err != nil {
					t.Fatalf("표본 %d 회차 %d 조건 %d: %v", sample, repeat, condition, err)
				}
				requireEvaluationDeleted(t, connection, set.Version)
			}
		}
	}
	// 마지막 허용 회차 직후의 준비는 성공하고, 부족한 상한은 측정을 차단한다.
	set := lifecycleInput(t)
	if err := withEvaluationGraph(t.Context(), database, worker, set, 1, func(loadedGraph) error {
		t.Fatal("처리 상한을 넘긴 그래프에서 측정을 시작했다")
		return nil
	}); err == nil || !strings.Contains(err.Error(), "상한 1회") || !strings.Contains(err.Error(), "pending=1") {
		t.Fatalf("유한 상한: %v", err)
	}
	requireEvaluationDeleted(t, connection, set.Version)
	var pending, attempts, vectors int
	if err := connection.QueryRow(t.Context(), `SELECT count(*), COALESCE(sum(attempts), 0), (SELECT count(*) FROM public.context_embedding WHERE graph_id = $1) FROM public.index_task WHERE graph_id = $1 AND state = 'pending'`, foreign.GraphID.String()).Scan(&pending, &attempts, &vectors); err != nil {
		t.Fatal(err)
	}
	if pending != 2 || attempts != 0 || vectors != 0 {
		t.Fatalf("다른 그래프의 큐·임베딩 변경: pending=%d attempts=%d vectors=%d", pending, attempts, vectors)
	}
}

func TestEvaluationPreparedGroupsIntegration(t *testing.T) {
	data, err := prepareSupplement()
	if err != nil {
		t.Fatal(err)
	}
	if err := data.finish(); err != nil {
		t.Fatal(err)
	}
	database, worker, connection := evalDatabase(t, nil)
	for _, group := range data.Memory.Groups {
		t.Run(group.Code, func(t *testing.T) {
			group.Contexts.Version = fmt.Sprintf("eval03-%s-%d", group.Code, time.Now().UnixNano())
			err := withEvaluationGraph(t.Context(), database, worker, group.Contexts, len(group.Contexts.Contexts), func(graph loadedGraph) error {
				if len(graph.Keys) != len(group.Contexts.Contexts) {
					t.Fatal("준비된 기억 그룹의 컨텍스트 수가 다르다")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			requireEvaluationDeleted(t, connection, group.Contexts.Version)
		})
	}
}
