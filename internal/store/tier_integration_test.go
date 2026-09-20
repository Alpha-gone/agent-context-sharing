package store

import (
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestMoveColdEmbeddingsAndReadBackIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)

	active, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, accountID, "https://example.test/tier-active-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("활성 계층 이동 대상 생성: %v", err)
	}
	discarded, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, accountID, "https://example.test/tier-discarded-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("폐기 계층 이동 대상 생성: %v", err)
	}
	readyIndexTasks(t, database, active.ID, discarded.ID)
	processor := scopeTestProcessor(embeddingDimension(t, database))
	for {
		result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, processor)
		if err != nil {
			t.Fatalf("계층 이동 대상 색인: %v", err)
		}
		if !result.Found {
			break
		}
	}
	if _, err := database.DiscardContext(t.Context(), graphID, discarded.ID, nil, WriteLimits{}); err != nil {
		t.Fatalf("컨텍스트 폐기: %v", err)
	}

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET last_accessed_at = $2 WHERE graph_id = $1`, graphID.String(), now.AddDate(0, 0, -31)); err != nil {
		t.Fatalf("이전 접근 시각 준비: %v", err)
	}
	moved, err := database.MoveColdEmbeddings(t.Context(), now, func(id model.ID) int {
		if id == accountID {
			return 30
		}
		return 0
	})
	if err != nil || moved < 2 {
		t.Fatalf("콜드 계층 이동 = %d, %v", moved, err)
	}
	if tier := embeddingTier(t, database, active.ID); tier != embeddingTierCold {
		t.Fatalf("미사용 활성 임베딩 계층 = %q", tier)
	}
	if tier := embeddingTier(t, database, discarded.ID); tier != embeddingTierCold {
		t.Fatalf("폐기 임베딩 계층 = %q", tier)
	}

	read, err := database.Context(t.Context(), graphID, active.ID)
	if err != nil || read.ID != active.ID {
		t.Fatalf("콜드 활성 컨텍스트 되읽기 = %#v, %v", read, err)
	}
	if tier := embeddingTier(t, database, active.ID); tier != embeddingTierHot {
		t.Fatalf("되읽은 활성 임베딩 계층 = %q", tier)
	}
	if tier := embeddingTier(t, database, discarded.ID); tier != embeddingTierCold {
		t.Fatalf("폐기 임베딩이 되읽기 없이 승격됐다: %q", tier)
	}

	var table string
	if err := database.pool.QueryRow(t.Context(), `SELECT tableoid::regclass::text FROM public.context_embedding WHERE context_id = $1`, active.ID.String()).Scan(&table); err != nil {
		t.Fatalf("활성 임베딩 파티션 조회: %v", err)
	}
	if table != "context_embedding_hot" && table != "public.context_embedding_hot" {
		t.Fatalf("되읽은 활성 임베딩 파티션 = %q", table)
	}
}

func TestMoveColdEmbeddingsDefaultKeepsInactiveActiveIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	graphID := createTestGraph(t, database, accountID)
	active, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, accountID, "https://example.test/tier-default-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("기본 계층 이동 대상 생성: %v", err)
	}
	readyIndexTasks(t, database, active.ID)
	result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, scopeTestProcessor(embeddingDimension(t, database)))
	if err != nil || !result.Succeeded {
		t.Fatalf("기본 계층 이동 대상 색인 = %#v, %v", result, err)
	}
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET last_accessed_at = now() - interval '10 years' WHERE context_id = $1`, active.ID.String()); err != nil {
		t.Fatalf("오래된 접근 시각 준비: %v", err)
	}
	moved, err := database.MoveColdEmbeddings(t.Context(), time.Now().UTC(), func(model.ID) int { return 0 })
	if err != nil {
		t.Fatalf("기본 플랜 계층 이동 = %d, %v", moved, err)
	}
	if tier := embeddingTier(t, database, active.ID); tier != embeddingTierHot {
		t.Fatalf("이동하지 않음 기본값의 임베딩 계층 = %q", tier)
	}

	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at = $2 WHERE graph_id = $1`, graphID.String(), time.Now().UTC()); err != nil {
		t.Fatalf("접근 차단 그래프 준비: %v", err)
	}
	if _, err := database.MoveColdEmbeddings(t.Context(), time.Now().UTC(), func(model.ID) int { return 0 }); err != nil {
		t.Fatalf("접근 차단 그래프 계층 이동: %v", err)
	}
	if tier := embeddingTier(t, database, active.ID); tier != embeddingTierCold {
		t.Fatalf("접근 차단 그래프의 임베딩 계층 = %q", tier)
	}
}

func embeddingTier(t *testing.T, database *Store, contextID model.ID) string {
	t.Helper()
	var tier string
	if err := database.pool.QueryRow(t.Context(), `SELECT storage_tier FROM public.context_embedding WHERE context_id = $1`, contextID.String()).Scan(&tier); err != nil {
		t.Fatalf("임베딩 계층 조회: %v", err)
	}
	return tier
}
