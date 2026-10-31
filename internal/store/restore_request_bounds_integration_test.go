package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestRequestGraphRestoreIdempotencyIntegration(t *testing.T) {
	for _, former := range []bool{false, true} {
		name := "creator"
		if former {
			name = "former_owner"
		}
		t.Run(name, func(t *testing.T) {
			database := newEmbeddingContractStore(t, "vector", 8)
			graph, requester, operator := operatorRestoreFixture(t, database, former)
			before, err := database.PendingRestoreRequests(t.Context())
			if err != nil || len(before) != 1 {
				t.Fatalf("initial requests=%+v: %v", before, err)
			}
			for range 3 {
				if err := database.RequestGraphRestore(t.Context(), graph, requester); err != nil {
					t.Fatal(err)
				}
			}
			after, err := database.PendingRestoreRequests(t.Context())
			if err != nil || len(after) != 1 || after[0] != before[0] {
				t.Fatalf("repeated requests=%+v want=%+v: %v", after, before, err)
			}
			var records int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log WHERE graph_id=$1 AND target_kind='restore_request'`, graph.String()).Scan(&records); err != nil || records != 1 {
				t.Fatalf("audit rows=%d: %v", records, err)
			}
			if err := database.RequestGraphRestore(t.Context(), graph, operator); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ineligible request=%v", err)
			}
			if former {
				var creator string
				if err := database.pool.QueryRow(t.Context(), `SELECT created_by FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&creator); err != nil {
					t.Fatal(err)
				}
				creatorID, err := model.ParseID(creator)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.RequestGraphRestore(t.Context(), graph, creatorID); err != nil {
					t.Fatal(err)
				}
				requests, err := database.PendingRestoreRequests(t.Context())
				if err != nil || len(requests) != 2 || requests[1].RequestedBy != creatorID {
					t.Fatalf("distinct former owner requests=%+v: %v", requests, err)
				}
			}
			if err := database.OperatorRestoreGraph(t.Context(), graph, operator); err != nil {
				t.Fatal(err)
			}
			if err := database.RequestGraphRestore(t.Context(), graph, requester); !errors.Is(err, ErrNotFound) {
				t.Fatalf("active graph request=%v", err)
			}
			// 합성 상태를 다음 자동 삭제 주기로 옮겨 이전 처리 기록이 새 요청을 막지 않는지 대조한다.
			claimExec(t, database, `UPDATE public.context_graph SET deleted_at=now(),grace_started_at=now(),grace_expires_at=now() WHERE graph_id=$1`, graph.String())
			if err := database.RequestGraphRestore(t.Context(), graph, requester); err != nil {
				t.Fatal(err)
			}
			if err := database.RequestGraphRestore(t.Context(), graph, requester); err != nil {
				t.Fatal(err)
			}
			pending, err := database.PendingRestoreRequests(t.Context())
			if err != nil || len(pending) != 1 || pending[0].RequestedBy != requester || !pending[0].RequestedAt.After(before[0].RequestedAt) {
				t.Fatalf("next cycle requests=%+v: %v", pending, err)
			}
		})
	}
}

func TestRequestGraphRestoreConcurrentIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	actor := newTestIDWithAccount(t, database)
	graph := createTestGraph(t, database, actor)
	claimExec(t, database, `UPDATE public.context_graph SET deleted_at=now(),grace_started_at=now(),grace_expires_at=now() WHERE graph_id=$1`, graph.String())
	var workers sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		workers.Go(func() { <-start; results <- database.RequestGraphRestore(t.Context(), graph, actor) })
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log WHERE graph_id=$1 AND target_kind='restore_request'`, graph.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent audit rows=%d: %v", count, err)
	}
}

func TestPendingRestoreRequestsLimitIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	actor := newTestIDWithAccount(t, database)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	var graphs []model.ID
	for i := range 101 {
		graph := createTestGraph(t, database, actor)
		graphs = append(graphs, graph)
		claimExec(t, database, `UPDATE public.context_graph SET deleted_at=$2,grace_started_at=$2,grace_expires_at=$2 WHERE graph_id=$1`, graph.String(), at)
		claimExec(t, database, `INSERT INTO public.web_audit_log (audit_id,target_kind,action,actor_account_id,occurred_at,graph_id,requester_account_id) VALUES ($1,'restore_request','request',$2,$3,$4,$2)`, newTestID(t).String(), actor.String(), at.Add(time.Duration(i)*time.Microsecond), graph.String())
		if i == 98 || i == 99 || i == 100 {
			pending, err := database.PendingRestoreRequests(t.Context())
			want := min(i+1, 100)
			if err != nil || len(pending) != want {
				t.Fatalf("total=%d returned=%d want=%d: %v", i+1, len(pending), want, err)
			}
			for j, request := range pending {
				if request.GraphID != graphs[j] {
					t.Fatalf("request %d graph=%s want=%s", j, request.GraphID, graphs[j])
				}
			}
		}
	}
	if err := database.OperatorRestoreGraph(t.Context(), graphs[0], actor); err != nil {
		t.Fatal(err)
	}
	pending, err := database.PendingRestoreRequests(t.Context())
	if err != nil || len(pending) != 100 || pending[0].GraphID != graphs[1] || pending[99].GraphID != graphs[100] {
		t.Fatalf("queue did not advance: count=%d: %v", len(pending), err)
	}
}
