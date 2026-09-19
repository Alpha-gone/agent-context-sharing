package mcp

import (
	"context"
	"os"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

// idempotency는 `SDD.md`의 「연산 계약」이 연산마다 확정한 멱등성이다.
type idempotency string

const (
	// idempotent는 같은 요청을 다시 보내도 같은 결과를 돌려준다.
	idempotent idempotency = "멱등"
	// conditional은 expected_version이 있는 수정이라 재요청이 version_conflict로 거부된다.
	conditional idempotency = "조건부"
	// notIdempotent는 재요청이 새 대상을 만들거나 상태가 맞지 않아 거부된다.
	notIdempotent idempotency = "비멱등"
)

// contractCase는 「연산 계약」 표의 한 행이다.
type contractCase struct {
	operation string
	// required는 연산이 요구하는 최소 등급이다. 비어 있으면 인증된 계정이면 된다.
	required model.GraphGrade
	kind     idempotency
	// arguments는 연산을 부를 인자를 만든다.
	arguments func(isolationFixture) map[string]any
	// setup은 연산이 성립하는 상태를 먼저 만든다. 복구처럼 앞선 전이가 필요한 연산에만 둔다.
	setup func(t *testing.T, call CallFunc, accountID model.ID, fixture isolationFixture)
	// repeat은 재요청 결과를 확인한다. 비우면 kind가 정한 기본 확인을 쓴다.
	repeat func(t *testing.T, call CallFunc, accountID model.ID, fixture isolationFixture, first ToolResult)
}

// TestOperationContractIntegration은 「연산 계약」 표의 필요 등급과 멱등성을 실제 연산으로 고정한다.
//
// 표를 문서에만 두면 등급 판정이나 재요청 처리가 바뀌어도 드러나지 않는다. 열람자 등급의
// 계정으로 13종을 모두 불러 편집자 연산이 permission_denied인지 보고, 연산마다 같은 요청을
// 다시 보내 표가 정한 멱등성과 같은지 확인한다.
func TestOperationContractIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 연산 계약 테스트를 건너뛴다")
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
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("등급 부여 연결: %v", err)
	}
	defer connection.Close(t.Context())
	if _, err := connection.Exec(t.Context(), `SET search_path = ag_catalog, "$user", public`); err != nil {
		t.Fatalf("AGE 검색 경로 설정: %v", err)
	}

	ownerID, viewerID := newHandlerID(t), newHandlerID(t)
	createHandlerAccount(t, database, ownerID)
	createHandlerAccount(t, database, viewerID)
	searcher, err := search.New(database, failingEmbedder{}, search.Config{
		Execution: search.ExecutionSequential, CandidateLimit: 10, FoldThreshold: 0.9, GraphStage: search.GraphStageGlobal,
	}, nil)
	if err != nil {
		t.Fatalf("검색 실행기 준비: %v", err)
	}
	call := NewHandlerWithSearch(database, plan.AccountPlans{}, searcher, nil)

	cases := contractCases()
	covered := make(map[string]struct{}, len(cases))
	for _, testCase := range cases {
		covered[testCase.operation] = struct{}{}
	}
	for _, definition := range toolDefinitions() {
		if _, found := covered[definition.Name]; !found {
			t.Errorf("연산 %s가 연산 계약 테스트에 없다", definition.Name)
		}
	}

	for _, testCase := range cases {
		t.Run(testCase.operation, func(t *testing.T) {
			// 연산마다 그래프를 새로 만든다. 폐기와 복구처럼 상태를 바꾸는 연산이 다른
			// 연산의 재요청 결과를 바꾸지 않게 하려는 것이다.
			fixture := seedIsolationGraph(t, call, ownerID, "연산 계약 "+testCase.operation)
			grantHandlerGrade(t, connection, graphName, fixture.graphID, viewerID, "viewer")
			cleanupIndexTasks(t, connection, fixture.graphID)

			if testCase.setup != nil {
				testCase.setup(t, call, ownerID, fixture)
			}
			_, viewerErr := call(t.Context(), viewerID, testCase.operation, testCase.arguments(fixture))
			if testCase.required == model.GraphGradeEditor {
				if !hasCode(viewerErr, "permission_denied") {
					t.Fatalf("열람자 호출이 권한 거부로 처리되지 않았다: %v", viewerErr)
				}
			} else if viewerErr != nil {
				t.Fatalf("열람자가 부를 수 있어야 하는 연산이 실패했다: %v", viewerErr)
			}

			first, err := call(t.Context(), ownerID, testCase.operation, testCase.arguments(fixture))
			if err != nil {
				t.Fatalf("첫 호출: %v", err)
			}
			repeat := testCase.repeat
			if repeat == nil {
				repeat = defaultRepeat(testCase.kind)
			}
			repeat(t, call, ownerID, fixture, first)
		})
	}
}

// contractCases는 「연산 계약」 표 13행을 그대로 옮긴다.
func contractCases() []contractCase {
	return []contractCase{
		{
			operation: "graph_list", required: model.GraphGradeViewer, kind: idempotent,
			arguments: func(isolationFixture) map[string]any { return map[string]any{} },
		},
		{
			operation: "graph_create", required: "", kind: notIdempotent,
			arguments: func(isolationFixture) map[string]any { return map[string]any{"name": "연산 계약 그래프"} },
			repeat: func(t *testing.T, call CallFunc, accountID model.ID, _ isolationFixture, first ToolResult) {
				// 자연 키가 없으므로 같은 이름의 재요청은 다른 그래프를 만든다.
				second, err := call(t.Context(), accountID, "graph_create", map[string]any{"name": "연산 계약 그래프"})
				if err != nil {
					t.Fatalf("재요청: %v", err)
				}
				if structured(t, first)["graph_id"] == structured(t, second)["graph_id"] {
					t.Fatal("비멱등 연산의 재요청이 같은 그래프를 돌려줬다")
				}
			},
		},
		{
			operation: "graph_get", required: model.GraphGradeViewer, kind: idempotent,
			arguments: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID}
			},
			repeat: repeatReturnsSame("graph_id"),
		},
		{
			operation: "graph_update", required: model.GraphGradeEditor, kind: conditional,
			arguments: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "expected_version": float64(1), "name": "연산 계약 갱신"}
			},
		},
		{
			operation: "node_create", required: model.GraphGradeEditor, kind: conditional,
			// 「연산 계약」이 조건부로 둔 근거가 계층에 따라 갈리는 것이다. 원천은
			// source_ref가 자연 키라 같은 요청이 기존 정점을 돌려주므로 여기에서 그쪽을 본다.
			arguments: contractSource,
			repeat:    repeatReturnsSame("context_id"),
		},
		{
			operation: "node_get", required: model.GraphGradeViewer, kind: idempotent,
			arguments: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.eventA, "hops": float64(1)}
			},
		},
		{
			operation: "node_update", required: model.GraphGradeEditor, kind: conditional,
			arguments: func(fixture isolationFixture) map[string]any {
				// 유지 연산은 판 번호를 올리지 않아 조건부 멱등성의 대상이 아니다.
				// 「연산 계약」이 말하는 조건부는 expected_version이 걸리는 실제 수정이다.
				return map[string]any{
					"graph_id": fixture.graphID, "context_id": fixture.eventA, "expected_version": float64(1),
					"created_by_agent": isolationAgent, "management_action": "update",
					"judgment_input": "연산 계약 수정", "body": "연산 계약이 바꾼 사건 본문",
				}
			},
		},
		{
			operation: "node_discard", required: model.GraphGradeEditor, kind: notIdempotent,
			arguments: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.lifecycleID, "created_by_agent": isolationAgent}
			},
		},
		{
			operation: "node_restore", required: model.GraphGradeEditor, kind: notIdempotent,
			// 복구는 폐기된 정점에만 성립하므로 먼저 폐기해 둔다.
			setup: func(t *testing.T, call CallFunc, accountID model.ID, fixture isolationFixture) {
				t.Helper()
				if _, err := call(t.Context(), accountID, "node_discard", map[string]any{
					"graph_id": fixture.graphID, "context_id": fixture.lifecycleID, "created_by_agent": isolationAgent,
				}); err != nil {
					t.Fatalf("복구 대상 폐기: %v", err)
				}
			},
			arguments: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.lifecycleID, "created_by_agent": isolationAgent}
			},
		},
		{
			operation: "relation_list", required: model.GraphGradeViewer, kind: idempotent,
			arguments: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "context_id": fixture.eventA}
			},
		},
		{
			operation: "relation_confirm", required: model.GraphGradeEditor, kind: idempotent,
			arguments: func(fixture isolationFixture) map[string]any { return isolationRelation(fixture, "causes") },
			repeat:    repeatReturnsSame("relation_id"),
		},
		{
			operation: "relation_discard", required: model.GraphGradeEditor, kind: notIdempotent,
			arguments: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "relation_id": fixture.relationID, "created_by_agent": isolationAgent}
			},
		},
		{
			operation: "context_flow_get", required: model.GraphGradeViewer, kind: idempotent,
			arguments: func(fixture isolationFixture) map[string]any {
				return map[string]any{"graph_id": fixture.graphID, "work_context": "연산 계약 흐름", "scope": "local"}
			},
		},
	}
}

// defaultRepeat은 「연산 계약」의 멱등성 값이 뜻하는 기본 재요청 확인을 고른다.
func defaultRepeat(kind idempotency) func(*testing.T, CallFunc, model.ID, isolationFixture, ToolResult) {
	switch kind {
	case conditional:
		return repeatConflicts
	case notIdempotent:
		return repeatRejected
	default:
		return repeatSucceeds
	}
}

// repeatSucceeds는 읽기 전용 멱등 연산의 재요청이 그대로 성공하는지 본다.
func repeatSucceeds(t *testing.T, call CallFunc, accountID model.ID, fixture isolationFixture, _ ToolResult) {
	t.Helper()
	operation := currentOperation(t)
	if _, err := call(t.Context(), accountID, operation, contractArguments(t, operation, fixture)); err != nil {
		t.Fatalf("멱등 연산 재요청: %v", err)
	}
}

// repeatReturnsSame은 재요청이 같은 대상을 돌려주는지 본다.
func repeatReturnsSame(field string) func(*testing.T, CallFunc, model.ID, isolationFixture, ToolResult) {
	return func(t *testing.T, call CallFunc, accountID model.ID, fixture isolationFixture, first ToolResult) {
		t.Helper()
		operation := currentOperation(t)
		second, err := call(t.Context(), accountID, operation, contractArguments(t, operation, fixture))
		if err != nil {
			t.Fatalf("멱등 연산 재요청: %v", err)
		}
		if structured(t, first)[field] != structured(t, second)[field] {
			t.Fatalf("멱등 연산의 재요청이 다른 %s를 돌려줬다", field)
		}
	}
}

// repeatConflicts는 조건부 연산의 같은 판 번호 재요청이 거부되는지 본다.
func repeatConflicts(t *testing.T, call CallFunc, accountID model.ID, fixture isolationFixture, _ ToolResult) {
	t.Helper()
	operation := currentOperation(t)
	_, err := call(t.Context(), accountID, operation, contractArguments(t, operation, fixture))
	if !hasCode(err, "version_conflict") {
		t.Fatalf("조건부 연산의 재요청이 판 번호 충돌로 거부되지 않았다: %v", err)
	}
}

// repeatRejected는 비멱등 상태 전이의 재요청이 거부되는지 본다.
func repeatRejected(t *testing.T, call CallFunc, accountID model.ID, fixture isolationFixture, _ ToolResult) {
	t.Helper()
	operation := currentOperation(t)
	if _, err := call(t.Context(), accountID, operation, contractArguments(t, operation, fixture)); err == nil {
		t.Fatal("비멱등 상태 전이의 재요청이 거부되지 않았다")
	}
}

// currentOperation은 하위 테스트 이름에서 연산 이름을 꺼낸다.
func currentOperation(t *testing.T) string {
	t.Helper()
	name := t.Name()
	for index := len(name) - 1; index >= 0; index-- {
		if name[index] == '/' {
			return name[index+1:]
		}
	}
	return name
}

// contractArguments는 같은 연산의 인자를 다시 만든다.
func contractArguments(t *testing.T, operation string, fixture isolationFixture) map[string]any {
	t.Helper()
	for _, testCase := range contractCases() {
		if testCase.operation == operation {
			return testCase.arguments(fixture)
		}
	}
	t.Fatalf("연산 %s의 인자를 찾지 못했다", operation)
	return nil
}

// contractSource는 그래프마다 고정된 원천 인자를 만든다.
// locator가 그래프 식별자에서 나오므로 같은 그래프의 재요청이 같은 자연 키를 쓴다.
func contractSource(fixture isolationFixture) map[string]any {
	return map[string]any{
		"graph_id": fixture.graphID, "layer": "source", "body": "연산 계약 원천 본문",
		"created_by_agent": isolationAgent, "source_channel": "api",
		"locator":     "https://example.test/contract-" + fixture.graphID,
		"occurred_at": "2026-09-19T00:00:00Z", "origin_kind": "external_content",
	}
}

// cleanupIndexTasks는 테스트가 만든 정점의 색인 대기 작업을 지운다.
//
// 개발 데이터베이스를 공유하므로 남겨 두면 색인 작업 확보 순서를 보는 다른 테스트가
// 이 행들을 먼저 집는다. 색인 작업자를 돌리지 않는 테스트의 행은 처리될 일도 없다.
func cleanupIndexTasks(t *testing.T, connection *pgx.Conn, graphID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := connection.Exec(ctx, `DELETE FROM public.index_task WHERE graph_id = $1`, graphID); err != nil {
			t.Errorf("색인 작업 정리: %v", err)
		}
	})
}
