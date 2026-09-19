package mcp

import (
	"errors"
	"os"
	"testing"
	"time"

	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

// TestRelationListUsesRelationPageIntegration은 relation_list가 그래프 목록이 아니라 관계
// 목록의 페이지 크기 플랜을 쓰는지 확인한다. 값을 잘못 읽으면 relation_page 설정이
// 무시되고 한도 초과 응답의 한도 이름도 graph_page로 나간다.
func TestRelationListUsesRelationPageIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 관계 페이지 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	database, err := store.New(t.Context(), databaseURL, graphName, nil, nil, "")
	if err != nil {
		t.Fatalf("저장소 준비: %v", err)
	}
	defer database.Close()

	ownerID := newHandlerID(t)
	createHandlerAccount(t, database, ownerID)

	// 관계 목록만 좁히고 그래프 목록은 기본값으로 둔다. 두 값을 구분해 읽는지 본다.
	plans, err := plan.ParseAccountPlans(`{"` + ownerID.String() + `": {"relation_page": {"default": 5, "maximum": 10}}}`)
	if err != nil {
		t.Fatalf("계정 플랜 해석: %v", err)
	}
	call := NewHandler(database, plans, nil)

	created, err := call(t.Context(), ownerID, "graph_create", map[string]any{"name": "관계 페이지 그래프"})
	if err != nil {
		t.Fatalf("그래프 생성: %v", err)
	}
	graphID := structured(t, created)["graph_id"].(string)
	agentID := newHandlerID(t)
	node, err := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "created_by_agent": agentID.String(), "layer": "source", "body": "관계 페이지 본문",
		"source_channel": "api", "locator": "https://example.test/relation-page-" + graphID,
		"occurred_at": time.Now().UTC().Format(time.RFC3339), "origin_kind": "external_content",
	})
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	contextID := structured(t, node)["context_id"].(string)

	// 관계 목록 상한을 넘는 요청은 relation_page 이름으로 거부되어야 한다.
	_, err = call(t.Context(), ownerID, "relation_list", map[string]any{
		"graph_id": graphID, "context_id": contextID, "page_size": float64(150),
	})
	domain, ok := errors.AsType[*Error](err)
	if !ok || domain.Code != "limit_exceeded" {
		t.Fatalf("상한 초과 응답 = %#v, want limit_exceeded", err)
	}
	if name, _ := domain.Data["limit"].(string); name != "relation_page" {
		t.Fatalf("한도 이름 = %q, want relation_page", name)
	}

	// 상한 안의 요청은 통과한다.
	if _, err := call(t.Context(), ownerID, "relation_list", map[string]any{
		"graph_id": graphID, "context_id": contextID, "page_size": float64(10),
	}); err != nil {
		t.Fatalf("상한 안 요청: %v", err)
	}
}

// TestRelationOperationsDoNotConsumeWriteRateIntegration은 관계 확정과 폐기가 요청 빈도
// 한도를 소비하지 않는지 확인한다. `TBD-AGENT_CONTEXT-062`가 적용 대상을 컨텍스트를
// 만들거나 고치는 연산으로 한정했고, 관계 연산은 컨텍스트를 만들지도 고치지도 않는다.
func TestRelationOperationsDoNotConsumeWriteRateIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 요청 빈도 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	database, err := store.New(t.Context(), databaseURL, graphName, nil, nil, "")
	if err != nil {
		t.Fatalf("저장소 준비: %v", err)
	}
	defer database.Close()

	ownerID := newHandlerID(t)
	createHandlerAccount(t, database, ownerID)
	// 쓰기 한도를 1로 두면 대상이 아닌 연산이 소비하는지 바로 드러난다.
	plans, err := plan.ParseAccountPlans(`{"` + ownerID.String() + `": {"writes_per_minute": 1}}`)
	if err != nil {
		t.Fatalf("계정 플랜 해석: %v", err)
	}
	call := NewHandler(database, plans, nil)

	created, err := call(t.Context(), ownerID, "graph_create", map[string]any{"name": "빈도 한도 그래프"})
	if err != nil {
		t.Fatalf("그래프 생성: %v", err)
	}
	graphID := structured(t, created)["graph_id"].(string)
	agentID := newHandlerID(t)

	// 관계 목록 조회와 폐기 시도는 한도를 쓰지 않아야 한다. 없는 관계라 not_found가 난다.
	_, err = call(t.Context(), ownerID, "relation_discard", map[string]any{
		"graph_id": graphID, "relation_id": newHandlerID(t).String(), "created_by_agent": agentID.String(),
	})
	if domain, ok := errors.AsType[*Error](err); !ok || domain.Code == "limit_exceeded" {
		t.Fatalf("관계 폐기 응답 = %v; 한도를 소비하면 안 된다", err)
	}

	// 컨텍스트 생성은 대상이므로 한 번은 통과한다.
	source := map[string]any{
		"graph_id": graphID, "created_by_agent": agentID.String(), "layer": "source", "body": "빈도 한도 본문",
		"source_channel": "api", "locator": "https://example.test/write-rate-" + graphID,
		"occurred_at": time.Now().UTC().Format(time.RFC3339), "origin_kind": "external_content",
	}
	if _, err := call(t.Context(), ownerID, "node_create", source); err != nil {
		t.Fatalf("첫 원천 생성: %v", err)
	}
	// 두 번째는 한도를 넘는다.
	second := map[string]any{}
	for key, value := range source {
		second[key] = value
	}
	second["locator"] = "https://example.test/write-rate-2-" + graphID
	_, err = call(t.Context(), ownerID, "node_create", second)
	domain, ok := errors.AsType[*Error](err)
	if !ok || domain.Code != "limit_exceeded" {
		t.Fatalf("두 번째 생성 응답 = %v, want limit_exceeded", err)
	}
	if name, _ := domain.Data["limit"].(string); name != "writes_per_minute" {
		t.Fatalf("한도 이름 = %q, want writes_per_minute", name)
	}
}
