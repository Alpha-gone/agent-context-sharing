package store

import (
	"reflect"
	"sync"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestStartupIndexRegistrationPreservesExistingWorkIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		t.Run(typ, func(t *testing.T) {
			database := newEmbeddingContractStore(t, typ, 8)
			actor := newTestIDWithAccount(t, database)
			graph := createTestGraph(t, database, actor)
			source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://startup-preserve"), nil)
			if err != nil {
				t.Fatal(err)
			}
			current := "current:" + typ
			reindexVector(t, database, source, current)
			nodes := make([]model.Context, 7)
			for i := range nodes {
				nodes[i], err = database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
				if err != nil {
					t.Fatal(err)
				}
				if i != 4 { // 4는 벡터도 작업도 누락된 대상이다.
					modelID := "previous"
					if i == 5 {
						modelID = current
					}
					reindexVector(t, database, nodes[i], modelID)
				}
			}
			if _, err := database.DiscardContext(t.Context(), graph, nodes[6].ID, registrationOperation(nodes[6], OperationDiscard), WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			keep := []string{nodes[0].ID.String(), nodes[1].ID.String(), nodes[2].ID.String()}
			claimExec(t, database, `DELETE FROM public.index_task WHERE graph_id=$1 AND NOT (context_id=ANY($2::uuid[]))`, graph.String(), keep)
			claimExec(t, database, `UPDATE public.index_task SET state='failed',attempts=5,last_error='permanent' WHERE context_id=$1`, nodes[0].ID.String())
			claimExec(t, database, `UPDATE public.index_task SET attempts=2,last_error='retry',next_attempt_at=statement_timestamp()+interval '10 minutes' WHERE context_id=$1`, nodes[1].ID.String())
			claimExec(t, database, `UPDATE public.index_task SET correlation_id=$2 WHERE context_id=ANY($1::uuid[])`, keep, newTestID(t).String())
			task, claim, found, err := database.claimIndexTask(t.Context(), graph)
			if err != nil || !found || task.ContextID != nodes[2].ID {
				t.Fatalf("처리 중 리스 준비: %+v/%t/%v", task, found, err)
			}
			snapshot := func() []string {
				t.Helper()
				values := make([]string, len(keep))
				for i, id := range keep {
					if err := database.pool.QueryRow(t.Context(), `SELECT row_to_json(task)::text FROM public.index_task AS task WHERE context_id=$1`, id).Scan(&values[i]); err != nil {
						t.Fatal(err)
					}
				}
				return values
			}
			before := snapshot()
			var afterFirst string
			for restart := range 3 {
				results := make(chan error, 2)
				var starts sync.WaitGroup
				for range 2 {
					starts.Go(func() { results <- database.ReindexOutdatedEmbeddings(t.Context(), current) })
				}
				starts.Wait()
				close(results)
				for err := range results {
					if err != nil {
						t.Fatal(err)
					}
				}
				if after := snapshot(); !reflect.DeepEqual(after, before) {
					t.Fatalf("기동 %d가 failed·백오프·리스 또는 확보 세대를 바꿨다: %v -> %v", restart, before, after)
				}
				for _, node := range []model.Context{nodes[3], nodes[4]} {
					if count := indexTaskCount(t, database, node.ID); count != 1 {
						t.Fatalf("누락 작업 수=%d, want 1", count)
					}
				}
				for _, node := range []model.Context{source, nodes[5], nodes[6]} {
					if count := indexTaskCount(t, database, node.ID); count != 0 {
						t.Fatalf("현재 모델 또는 폐기 대상이 재등록됐다: %s/%d", node.ID, count)
					}
				}
				after := registrationStateFingerprint(t, database)
				if restart == 0 {
					afterFirst = after
				} else if after != afterFirst {
					t.Fatal("반복 기동이 보충한 작업이나 벡터를 다시 바꿨다")
				}
			}
			stored, err := database.storeIndexResult(t.Context(), task, claim, IndexTaskResult{Embedding: []float64{1, 0, 0, 0, 0, 0, 0, 0}, ModelID: current})
			if err != nil || !stored || indexTaskCount(t, database, nodes[2].ID) != 0 {
				t.Fatalf("기동 전 확보분의 결과 공개: %t/%v", stored, err)
			}
			afterCompletion := registrationStateFingerprint(t, database)
			if err := database.ReindexOutdatedEmbeddings(t.Context(), current); err != nil || registrationStateFingerprint(t, database) != afterCompletion {
				t.Fatalf("영구 실패가 남은 기동이 완료된 컨텍스트를 재등록했다: %v", err)
			}
			// 명시적 요청은 기존 재등록 계약대로 실패·백오프를 초기화한다.
			if err := database.ReindexGraph(t.Context(), graph); err != nil {
				t.Fatal(err)
			}
			for _, node := range nodes[:2] {
				task := registrationTaskFor(t, database, node.ID)
				if task.attempts != 0 || task.state != "pending" || task.lastError != nil || !task.enqueued.Equal(task.next) {
					t.Fatalf("명시적 재색인의 초기화 누락: %+v", task)
				}
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
