package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestContentRevisionIncrementsOnSearchVisibleWritesIntegration은 「테이블 열 정의」가 정한
// 증가 지점마다 내용 판이 하나씩 오르고, 검색 상태를 바꾸지 않는 쓰기는 올리지 않는지 확인한다.
func TestContentRevisionIncrementsOnSearchVisibleWritesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	revision := func() int64 {
		t.Helper()
		var value int64
		err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&value)
		if err != nil {
			t.Fatalf("내용 판 조회: %v", err)
		}
		return value
	}
	expect := func(name string, delta int64, write func()) {
		t.Helper()
		before := revision()
		write()
		if got := revision() - before; got != delta {
			t.Fatalf("%s의 내용 판 증가 = %d, want %d", name, got, delta)
		}
	}
	if revision() != 1 {
		t.Fatalf("새 그래프의 내용 판 = %d, want 1", revision())
	}

	var source, first, second model.Context
	expect("원천 생성", 1, func() {
		var err error
		if source, err = database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "api://revision/"+graphID.String()), nil); err != nil {
			t.Fatalf("원천 생성: %v", err)
		}
	})
	expect("사건 생성", 2, func() {
		var err error
		if first, err = database.CreateContext(t.Context(), graphID, testEventContext(t, graphID, actorID, source.ID), nil); err != nil {
			t.Fatalf("첫 사건 생성: %v", err)
		}
		if second, err = database.CreateContext(t.Context(), graphID, testEventContext(t, graphID, actorID, source.ID), nil); err != nil {
			t.Fatalf("두 번째 사건 생성: %v", err)
		}
	})
	expect("임베딩 공개", 1, func() {
		result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, scopeTestProcessor(embeddingDimension(t, database)))
		if err != nil || !result.Succeeded {
			t.Fatalf("색인 처리 = %+v, %v", result, err)
		}
	})
	expect("그래프 이름 변경", 0, func() {
		if _, err := database.UpdateGraph(t.Context(), graphID, 1, "renamed", ""); err != nil {
			t.Fatalf("그래프 갱신: %v", err)
		}
	})
	var relation model.Relation
	expect("관계 확정", 1, func() {
		var err error
		if relation, err = database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, first.ID, second.ID), nil); err != nil {
			t.Fatalf("관계 확정: %v", err)
		}
	})
	expect("같은 관계 재확정", 0, func() {
		if _, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, first.ID, second.ID), nil); err != nil {
			t.Fatalf("관계 재확정: %v", err)
		}
	})
	expect("관계 폐기", 1, func() {
		if _, err := database.DiscardRelation(t.Context(), graphID, relation.ID, nil); err != nil {
			t.Fatalf("관계 폐기: %v", err)
		}
	})
	expect("폐기 관계 재확정", 1, func() {
		if _, err := database.ConfirmRelation(t.Context(), graphID, confirmable(t, graphID, model.RelationTypePrecedes, first.ID, second.ID), nil); err != nil {
			t.Fatalf("폐기 관계 재확정: %v", err)
		}
	})
	expect("컨텍스트 폐기와 복구", 2, func() {
		for _, deleted := range []bool{true, false} {
			if _, err := database.SetContextDeleted(t.Context(), graphID, second.ID, actorID, deleted, 0); err != nil {
				t.Fatalf("폐기 %v: %v", deleted, err)
			}
		}
	})
	t.Cleanup(func() {
		// 원천 색인이 남긴 나머지 대기 작업이 다른 검사의 확보를 가로채지 않게 지운다.
		_, _ = database.pool.Exec(t.Context(), `DELETE FROM public.index_task WHERE graph_id = $1`, graphID.String())
	})
}

// TestReadSnapshotHidesLaterCommitsIntegration은 스냅숏을 실은 채널 읽기, 홉 확장과 출처
// 조립이 스냅숏 뒤에 커밋된 쓰기를 보지 않고, 스냅숏이 없는 읽기는 본다는 것을 확인한다.
func TestReadSnapshotHidesLaterCommitsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	before, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "api://snapshot/before/"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}

	snapshot, err := database.BeginReadSnapshot(t.Context())
	if err != nil {
		t.Fatalf("스냅숏 시작: %v", err)
	}
	// 검사가 도중에 실패해도 조정 연결을 풀에 돌려준다. 돌려주지 않으면 저장소 정리가
	// 그 연결을 기다리며 멈춘다.
	closed := false
	t.Cleanup(func() {
		if !closed {
			snapshot.Close(context.WithoutCancel(t.Context()))
		}
	})
	later, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{before.ID})
	if err != nil {
		t.Fatalf("스냅숏 뒤 파생 생성: %v", err)
	}
	scoped := snapshot.Context(t.Context())
	asOf := time.Now().UTC()

	timeCount := func(inSnapshot bool) int {
		t.Helper()
		ctx := t.Context()
		if inSnapshot {
			ctx = scoped
		}
		candidates, err := database.TimeCandidates(ctx, graphID, asOf, 10)
		if err != nil {
			t.Fatalf("시간 후보: %v", err)
		}
		return len(candidates)
	}
	if got := timeCount(true); got != 1 {
		t.Fatalf("스냅숏 안 시간 후보 = %d, want 1", got)
	}
	if got := timeCount(false); got != 2 {
		t.Fatalf("스냅숏 밖 시간 후보 = %d, want 2", got)
	}
	hop, err := database.HopContextsFrom(scoped, graphID, []model.Context{before}, 1, "both", nil, 0)
	if err != nil {
		t.Fatalf("스냅숏 안 홉 확장: %v", err)
	}
	if _, found := hop.Distances[later.ID]; found || len(hop.Contexts) != 1 {
		t.Fatalf("스냅숏 안 홉 확장이 뒤 커밋을 봤다: %v", hop.Distances)
	}
	// 스냅숏 안에서는 뒤에 만든 파생이 없으므로 출처 조립이 누락으로 거부한다.
	var missing missingContextError
	if _, err := database.ContextOriginKinds(scoped, graphID, []model.ID{later.ID}); !errors.As(err, &missing) {
		t.Fatalf("스냅숏 안 출처 조립이 뒤 커밋을 봤다: %v", err)
	}
	origins, err := database.ContextOriginKinds(scoped, graphID, []model.ID{before.ID})
	if err != nil || len(origins[before.ID]) != 1 {
		t.Fatalf("스냅숏 안 출처 조립 = %v, %v", origins, err)
	}
	stats := snapshot.Close(t.Context())
	closed = true
	// 조정 연결 하나와 스냅숏 안 읽기 넷이다. 스냅숏 밖 시간 후보는 세지 않는다.
	if stats.Connections != 5 || stats.PeakConnections != 2 || stats.Hold <= 0 {
		t.Fatalf("스냅숏 자원 = %+v", stats)
	}
}
