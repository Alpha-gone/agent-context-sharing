package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

// isolationFixture는 한 그래프와 그 안에 넣어 둔 정점·간선 식별자다.
type isolationFixture struct {
	graphID    string
	sourceID   string
	eventA     string
	eventB     string
	relationID string
	// lifecycleID는 폐기와 복구가 상태를 바꿔도 다른 연산에 영향을 주지 않도록 따로 둔다.
	lifecycleID string
}

// identifiers는 응답에 새어 나오면 안 되는 이 그래프의 모든 식별자다.
func (fixture isolationFixture) identifiers() []string {
	return []string{fixture.graphID, fixture.sourceID, fixture.eventA, fixture.eventB, fixture.relationID, fixture.lifecycleID}
}

// isolationCase는 연산 하나를 격리 관점에서 부르는 방법이다.
//
// foreign이 nil이면 그 연산은 대상 그래프를 인자로 받지 않는다는 뜻이며, 그런 연산은
// 자기 그래프 호출의 응답에 상대 그래프가 섞이지 않는지로만 확인한다.
type isolationCase struct {
	operation string
	own       func(isolationFixture) map[string]any
	foreign   func(isolationFixture) map[string]any
	// createsGraph는 응답이 자기 그래프가 아니라 이 호출이 새로 만든 그래프를 담는
	// 연산임을 뜻한다. 그런 응답은 담긴 graph_id가 서로 같기만 하면 된다.
	createsGraph bool
}

// TestOperationIsolationIntegration은 `SDD.md`의 「격리 위반 테스트」를 그대로 수행한다.
//
// 서로 다른 두 그래프를 만들고 한쪽에만 등급이 있는 계정으로 연산 13종을 모두 부른다.
// 상대 그래프를 지정한 요청은 존재를 노출하지 않도록 not_found여야 하고, 자기 그래프
// 요청의 응답에는 상대 그래프의 식별자가 하나도 없어야 한다. 마지막으로 연산 목록과 이
// 표를 대조해, 연산을 새로 만들고 격리 확인을 빠뜨리면 테스트가 스스로 알리게 한다.
func TestOperationIsolationIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 격리 위반 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	database, err := store.New(t.Context(), databaseURL, graphName, nil, nil)
	if err != nil {
		t.Fatalf("저장소 준비: %v", err)
	}
	defer database.Close()
	actorID, strangerID := newHandlerID(t), newHandlerID(t)
	createHandlerAccount(t, database, actorID)
	createHandlerAccount(t, database, strangerID)

	// 흐름 조회도 대상이므로 검색 실행기를 함께 조립한다. 질의 임베딩은 실패하게 두었다.
	// 「작업 컨텍스트 흐름 조회」가 그 경우 의미 유사도 채널만 빼고 나머지 세 채널로
	// 진행하기로 확정했고, 격리를 확인할 대상은 그 세 채널이 돌려주는 정점과 간선이다.
	searcher, err := search.New(database, failingEmbedder{}, search.Config{
		Execution: search.ExecutionSequential, CandidateLimit: 10, FoldThreshold: 0.9, GraphStage: search.GraphStageGlobal,
	}, nil)
	if err != nil {
		t.Fatalf("검색 실행기 준비: %v", err)
	}
	call := NewHandlerWithSearch(database, plan.AccountPlans{}, searcher, nil)

	own := seedIsolationGraph(t, call, actorID, "격리 자기 그래프")
	foreign := seedIsolationGraph(t, call, strangerID, "격리 상대 그래프")

	cases := isolationCases()
	// 4단계. 연산 목록을 이 표와 대조한다. 목록은 tools/list가 내보내는 것과 같은 곳에서
	// 가져오므로, 연산을 새로 만들고 여기에 넣지 않으면 그 사실이 여기에서 드러난다.
	covered := make(map[string]struct{}, len(cases))
	for _, testCase := range cases {
		covered[testCase.operation] = struct{}{}
	}
	for _, definition := range toolDefinitions() {
		if _, found := covered[definition.Name]; !found {
			t.Errorf("연산 %s가 격리 위반 테스트에 없다", definition.Name)
		}
	}
	for name := range covered {
		if !hasToolDefinition(name) {
			t.Errorf("격리 위반 테스트의 %s가 연산 목록에 없다", name)
		}
	}

	for _, testCase := range cases {
		t.Run(testCase.operation, func(t *testing.T) {
			if testCase.foreign != nil {
				// 등급이 없는 그래프는 존재를 노출하지 않는다. 「요청 처리 순서」가
				// permission_denied가 아니라 not_found로 답하기로 확정했다.
				if _, callErr := call(t.Context(), actorID, testCase.operation, testCase.foreign(foreign)); !hasCode(callErr, "not_found") {
					t.Fatalf("상대 그래프 요청이 not_found로 처리되지 않았다: %v", callErr)
				}
			}
			result, err := call(t.Context(), actorID, testCase.operation, testCase.own(own))
			if err != nil {
				t.Fatalf("자기 그래프 요청: %v", err)
			}
			assertNoForeignIdentifiers(t, result, foreign)
			expected := own.graphID
			if testCase.createsGraph {
				expected = ""
			}
			assertGraphIDs(t, result, expected)
		})
	}

	// 상대 그래프의 정점을 자기 그래프의 근거로 가리키는 것도 격리를 넘는 요청이다.
	if _, callErr := call(t.Context(), actorID, "node_create", map[string]any{
		"graph_id": own.graphID, "layer": "derived", "body": "상대 그래프를 근거로 가리키는 파생",
		"created_by_agent": actorID.String(), "derivation_kind": "proposition", "evidence_state": "observation",
		"derived_from": []any{foreign.sourceID},
	}); !hasCode(callErr, "not_found") {
		t.Fatalf("상대 그래프 정점을 근거로 쓴 생성이 not_found로 처리되지 않았다: %v", callErr)
	}
}

// isolationCases는 연산 13종의 호출 방법을 한곳에 둔다.
func isolationCases() []isolationCase {
	lifecycle := func(fixture isolationFixture) map[string]any {
		return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.lifecycleID, "created_by_agent": isolationAgent}
	}
	return []isolationCase{
		{
			operation: "graph_list",
			// 대상 그래프를 인자로 받지 않으므로 상대 그래프 호출이 없다. 등급이 없는
			// 그래프가 목록에 섞이지 않는지는 자기 호출의 응답으로 본다.
			own: func(isolationFixture) map[string]any { return map[string]any{} },
		},
		{
			operation: "graph_create",
			own: func(isolationFixture) map[string]any {
				return map[string]any{"name": "격리 테스트가 만든 그래프"}
			},
			createsGraph: true,
		},
		{
			operation: "graph_get",
			own:       func(fixture isolationFixture) map[string]any { return map[string]any{"graph_id": fixture.graphID} },
			foreign:   func(fixture isolationFixture) map[string]any { return map[string]any{"graph_id": fixture.graphID} },
		},
		{
			operation: "graph_update",
			own: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "expected_version": float64(1), "name": "격리 테스트 갱신"}
			},
			foreign: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "expected_version": float64(1), "name": "상대 그래프 갱신"}
			},
		},
		{
			operation: "node_create",
			own:       func(fixture isolationFixture) map[string]any { return isolationSource(fixture.graphID) },
			foreign:   func(fixture isolationFixture) map[string]any { return isolationSource(fixture.graphID) },
		},
		{
			operation: "node_get",
			own: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.eventA, "hops": float64(2)}
			},
			foreign: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.eventA, "hops": float64(2)}
			},
		},
		{
			operation: "node_update",
			own: func(fixture isolationFixture) map[string]any {
				return map[string]any{
					"graph_id": fixture.graphID, "context_id": fixture.eventA, "expected_version": float64(1),
					"created_by_agent": isolationAgent, "management_action": "keep", "judgment_input": "격리 테스트 유지",
				}
			},
			foreign: func(fixture isolationFixture) map[string]any {
				return map[string]any{
					"graph_id": fixture.graphID, "context_id": fixture.eventA, "expected_version": float64(1),
					"created_by_agent": isolationAgent, "management_action": "keep", "judgment_input": "상대 그래프 유지",
				}
			},
		},
		// 폐기와 복구는 같은 정점을 쓰므로 순서가 있다. 표의 순서가 곧 실행 순서다.
		{operation: "node_discard", own: lifecycle, foreign: lifecycle},
		{operation: "node_restore", own: lifecycle, foreign: lifecycle},
		{
			operation: "relation_list",
			own: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.eventA}
			},
			foreign: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.eventA}
			},
		},
		{
			operation: "relation_confirm",
			own: func(fixture isolationFixture) map[string]any {
				return isolationRelation(fixture, "causes")
			},
			foreign: func(fixture isolationFixture) map[string]any {
				return isolationRelation(fixture, "causes")
			},
		},
		{
			operation: "relation_discard",
			own: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "relation_id": fixture.relationID, "created_by_agent": isolationAgent}
			},
			foreign: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "relation_id": fixture.relationID, "created_by_agent": isolationAgent}
			},
		},
		{
			operation: "context_flow_get",
			own: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "work_context": "격리 흐름 조회 본문", "scope": "global"}
			},
			foreign: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "work_context": "격리 흐름 조회 본문", "scope": "global"}
			},
		},
	}
}

// isolationAgent는 등급 판정에 쓰이지 않는 행위 에이전트 식별자다.
// 「권한 상속」이 created_by_agent를 판정에 쓰지 않기로 확정했으므로 고정 값이면 된다.
const isolationAgent = "018f0000-0000-7000-8000-000000000001"

// seedIsolationGraph는 한 계정의 그래프에 원천·사건 둘·확정 관계·수명주기 대상을 넣는다.
func seedIsolationGraph(t *testing.T, call CallFunc, accountID model.ID, name string) isolationFixture {
	t.Helper()
	created, err := call(t.Context(), accountID, "graph_create", map[string]any{"name": name})
	if err != nil {
		t.Fatalf("격리 그래프 생성: %v", err)
	}
	fixture := isolationFixture{graphID: structured(t, created)["graph_id"].(string)}
	fixture.sourceID = createIsolationNode(t, call, accountID, isolationSource(fixture.graphID))
	fixture.lifecycleID = createIsolationNode(t, call, accountID, isolationSource(fixture.graphID))
	fixture.eventA = createIsolationNode(t, call, accountID, isolationEvent(fixture.graphID, fixture.sourceID, "격리 사건 앞"))
	fixture.eventB = createIsolationNode(t, call, accountID, isolationEvent(fixture.graphID, fixture.sourceID, "격리 사건 뒤"))

	confirmed, err := call(t.Context(), accountID, "relation_confirm", isolationRelation(fixture, "precedes"))
	if err != nil {
		t.Fatalf("격리 관계 확정: %v", err)
	}
	relationID, ok := structured(t, confirmed)["relation_id"].(string)
	if !ok {
		t.Fatalf("확정 응답에 relation_id가 없다: %#v", structured(t, confirmed))
	}
	fixture.relationID = relationID
	return fixture
}

// createIsolationNode는 정점 하나를 만들고 식별자를 돌려준다.
func createIsolationNode(t *testing.T, call CallFunc, accountID model.ID, arguments map[string]any) string {
	t.Helper()
	created, err := call(t.Context(), accountID, "node_create", arguments)
	if err != nil {
		t.Fatalf("격리 정점 생성: %v", err)
	}
	return structured(t, created)["context_id"].(string)
}

func isolationSource(graphID string) map[string]any {
	id, err := model.NewID()
	if err != nil {
		panic("격리 테스트 locator 식별자: " + err.Error())
	}
	return map[string]any{
		"graph_id": graphID, "layer": "source", "body": "격리 원천 본문", "created_by_agent": isolationAgent,
		"source_channel": "api", "locator": "https://example.test/isolation-" + id.String(),
		"occurred_at": time.Now().UTC().Format(time.RFC3339), "origin_kind": "external_content",
	}
}

func isolationEvent(graphID, memberID, body string) map[string]any {
	now := time.Now().UTC()
	return map[string]any{
		"graph_id": graphID, "layer": "event", "body": body, "created_by_agent": isolationAgent,
		"member_refs": []any{memberID},
		"start":       now.Add(-time.Hour).Format(time.RFC3339), "end": now.Format(time.RFC3339),
	}
}

func isolationRelation(fixture isolationFixture, relationType string) map[string]any {
	return map[string]any{
		"graph_id": fixture.graphID, "relation_type": relationType,
		"from_context_id": fixture.eventA, "to_context_id": fixture.eventB, "created_by_agent": isolationAgent,
	}
}

// assertNoForeignIdentifiers는 응답 어디에도 상대 그래프의 식별자가 없는지 본다.
// 필드를 하나씩 세지 않고 직렬화한 응답 전체를 보는 이유는, 연산마다 다른 응답 모양에서
// 새 필드가 생겨도 이 확인이 그대로 유효하기 때문이다.
func assertNoForeignIdentifiers(t *testing.T, result ToolResult, foreign isolationFixture) {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("응답 직렬화: %v", err)
	}
	for _, identifier := range foreign.identifiers() {
		if identifier != "" && strings.Contains(string(encoded), identifier) {
			t.Fatalf("응답에 상대 그래프의 식별자 %s가 있다: %s", identifier, encoded)
		}
	}
}

// assertGraphIDs는 응답에 담긴 모든 graph_id가 요청한 그래프인지 본다.
// graphID가 비어 있으면 응답이 새로 만든 그래프를 담는 경우이므로 서로 같은지만 본다.
func assertGraphIDs(t *testing.T, result ToolResult, graphID string) {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("응답 직렬화: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("응답 해석: %v", err)
	}
	for _, found := range collectGraphIDs(decoded) {
		if graphID == "" {
			graphID = found
		}
		// graph_list는 접근 가능한 그래프를 모두 담으므로 다른 그래프가 함께 나올 수 있다.
		// 상대 그래프가 섞이지 않는 것은 assertNoForeignIdentifiers가 본다.
		if found != graphID && !isolationMultiGraphResult(result) {
			t.Fatalf("응답에 요청하지 않은 graph_id %s가 있다: %s", found, encoded)
		}
	}
}

// isolationMultiGraphResult는 여러 그래프를 담는 것이 정상인 응답인지 알려 준다.
func isolationMultiGraphResult(result ToolResult) bool {
	value, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return false
	}
	_, listed := value["graphs"]
	return listed
}

// collectGraphIDs는 중첩된 응답에서 graph_id 값을 모두 모은다.
func collectGraphIDs(value any) []string {
	switch typed := value.(type) {
	case map[string]any:
		found := make([]string, 0)
		for key, nested := range typed {
			if key == "graph_id" {
				if text, ok := nested.(string); ok {
					found = append(found, text)
					continue
				}
			}
			found = append(found, collectGraphIDs(nested)...)
		}
		return found
	case []any:
		found := make([]string, 0)
		for _, nested := range typed {
			found = append(found, collectGraphIDs(nested)...)
		}
		return found
	default:
		return nil
	}
}

// hasToolDefinition은 이름이 연산 목록에 있는지 알려 준다.
func hasToolDefinition(name string) bool {
	for _, definition := range toolDefinitions() {
		if definition.Name == name {
			return true
		}
	}
	return false
}

// failingEmbedder는 질의 임베딩만 실패시켜 의미 유사도 채널을 뺀다.
type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, string) ([]float64, error) {
	return nil, fmt.Errorf("격리 테스트는 질의 임베딩을 쓰지 않는다")
}

func (failingEmbedder) ModelID() string { return "isolation-test" }
