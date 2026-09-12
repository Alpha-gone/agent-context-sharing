package mcp

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

// TestHandlerIntegration은 실제 AGE에서 그래프·노드 CRUD 처리기의 등급, 멱등성과 채널 경계를 확인한다.
func TestHandlerIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 MCP 처리기 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	database, err := store.New(t.Context(), databaseURL, graphName)
	if err != nil {
		t.Fatalf("저장소 준비: %v", err)
	}
	defer database.Close()
	pool, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("등급 부여 연결: %v", err)
	}
	defer pool.Close(t.Context())
	if _, err := pool.Exec(t.Context(), `SET search_path = ag_catalog, "$user", public`); err != nil {
		t.Fatalf("AGE 검색 경로 설정: %v", err)
	}

	ownerID, viewerID := newHandlerID(t), newHandlerID(t)
	createHandlerAccount(t, database, ownerID)
	createHandlerAccount(t, database, viewerID)
	call := NewHandler(database, plan.AccountPlans{}, nil)

	created, err := call(t.Context(), ownerID, "graph_create", map[string]any{"name": "handler 통합 그래프"})
	if err != nil {
		t.Fatalf("그래프 생성: %v", err)
	}
	graphID := structured(t, created)["graph_id"].(string)
	grantHandlerGrade(t, pool, graphName, graphID, viewerID, "viewer")

	if _, err := call(t.Context(), ownerID, "graph_get", map[string]any{"graph_id": graphID}); err != nil {
		t.Fatalf("그래프 조회: %v", err)
	}
	if _, err := call(t.Context(), ownerID, "graph_update", map[string]any{"graph_id": graphID, "expected_version": float64(1), "name": "갱신된 그래프"}); err != nil {
		t.Fatalf("그래프 갱신: %v", err)
	}
	if _, callErr := call(t.Context(), ownerID, "graph_update", map[string]any{"graph_id": graphID, "expected_version": float64(1), "name": "다시 갱신"}); !hasCode(callErr, "version_conflict") {
		t.Fatalf("이전 판 번호 갱신이 버전 충돌로 처리되지 않았다: %v", callErr)
	}
	if _, callErr := call(t.Context(), viewerID, "graph_update", map[string]any{"graph_id": graphID, "expected_version": float64(2), "name": "열람자 갱신"}); !hasCode(callErr, "permission_denied") {
		t.Fatalf("열람자 갱신이 권한 거부로 처리되지 않았다: %v", callErr)
	}
	if _, err := call(t.Context(), ownerID, "graph_get", map[string]any{"graph_id": newHandlerID(t).String()}); !hasCode(err, "not_found") {
		t.Fatalf("없는 그래프 조회가 not found로 처리되지 않았다: %v", err)
	}
	listed, err := call(t.Context(), ownerID, "graph_list", map[string]any{})
	if err != nil {
		t.Fatalf("그래프 목록: %v", err)
	}
	if graphs := structured(t, listed)["graphs"].([]any); len(graphs) == 0 {
		t.Fatal("생성한 그래프가 목록에 없다")
	}

	locator := "https://example.test/handler-" + newHandlerID(t).String()
	sourceArguments := func() map[string]any {
		return map[string]any{
			"graph_id": graphID, "layer": "source", "body": "원천 본문", "created_by_agent": ownerID.String(),
			"source_channel": "api", "locator": locator, "occurred_at": time.Now().UTC().Format(time.RFC3339), "origin_kind": "external_content",
		}
	}
	createdSource, err := call(t.Context(), ownerID, "node_create", sourceArguments())
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	sourceID := structured(t, createdSource)["context_id"].(string)
	sourceValue := structured(t, createdSource)
	sourceRef, ok := sourceValue["source_ref"].(map[string]any)
	if !ok || sourceRef["channel"] != "api" || sourceRef["locator"] != locator {
		t.Fatalf("원천 응답에 source_ref가 없다: %#v", sourceValue["source_ref"])
	}
	if sourceValue["occurred_at"] == nil || sourceValue["origin_kind"] != "external_content" {
		t.Fatalf("원천 응답에 계층별 속성이 없다: occurred_at=%#v origin_kind=%#v", sourceValue["occurred_at"], sourceValue["origin_kind"])
	}
	duplicated, err := call(t.Context(), ownerID, "node_create", sourceArguments())
	if err != nil {
		t.Fatalf("중복 원천 생성: %v", err)
	}
	if structured(t, duplicated)["context_id"].(string) != sourceID {
		t.Fatal("같은 source_ref가 기존 원천을 반환하지 않았다")
	}
	if _, callErr := call(t.Context(), viewerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "source", "body": "열람자 원천", "created_by_agent": viewerID.String(),
		"source_channel": "api", "locator": locator + "/viewer", "occurred_at": time.Now().UTC().Format(time.RFC3339), "origin_kind": "external_content",
	}); !hasCode(callErr, "permission_denied") {
		t.Fatalf("열람자 노드 생성이 권한 거부로 처리되지 않았다: %v", callErr)
	}

	createdDerived, err := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "derived", "body": "파생 본문", "created_by_agent": ownerID.String(),
		"derivation_kind": "proposition", "evidence_state": "observation", "derived_from": []any{sourceID},
	})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}
	derivedValue := structured(t, createdDerived)
	derivedID := derivedValue["context_id"].(string)
	// 절단은 오류가 아니라 성공 응답의 부분 상태이며, SRS의 「정상 응답의 부분 상태」가
	// 표시 이름을 result_truncated로, 홉 조회가 함께 실을 값을 잘린 홉 경계로 확정했다.
	narrowPlans, err := plan.ParseAccountPlans(fmt.Sprintf(`{%q:{"max_hop_nodes":1}}`, ownerID.String()))
	if err != nil {
		t.Fatalf("홉 노드 상한 플랜 해석: %v", err)
	}
	narrowed, err := NewHandler(database, narrowPlans, nil)(t.Context(), ownerID, "node_get", map[string]any{
		"graph_id": graphID, "context_id": sourceID, "hops": float64(1),
	})
	if err != nil {
		t.Fatalf("절단 조회: %v", err)
	}
	truncation, ok := structured(t, narrowed)["result_truncated"].(map[string]any)
	if !ok {
		t.Fatalf("절단이 result_truncated로 표시되지 않았다: %#v", structured(t, narrowed))
	}
	if truncation["truncated_hop"] != 1 {
		t.Fatalf("잘린 홉 경계 = %#v, want 1", truncation["truncated_hop"])
	}

	fetched, err := call(t.Context(), viewerID, "node_get", map[string]any{"graph_id": graphID, "context_id": derivedID, "hops": float64(0)})
	if err != nil {
		t.Fatalf("노드 조회: %v", err)
	}
	contexts, ok := structured(t, fetched)["contexts"].([]any)
	if !ok || len(contexts) != 1 {
		t.Fatalf("0홉 조회가 단일 노드를 반환하지 않았다: %#v", structured(t, fetched)["contexts"])
	}
	derivedView, ok := contexts[0].(map[string]any)
	if !ok {
		t.Fatalf("노드 조회의 컨텍스트 항목이 객체가 아니다: %#v", contexts[0])
	}
	if derivedFrom, ok := derivedView["derived_from"].([]string); !ok || len(derivedFrom) != 1 || derivedFrom[0] != sourceID {
		t.Fatalf("파생 응답에 derived_from이 없다: %#v", derivedView["derived_from"])
	}
	if derivedView["derivation_kind"] != "proposition" || derivedView["evidence_state"] != "observation" || derivedView["evidence_invalidated"] != false {
		t.Fatalf("파생 응답에 계층별 속성이 없다: %#v", derivedView)
	}
	updated, err := call(t.Context(), ownerID, "node_update", map[string]any{
		"graph_id": graphID, "context_id": derivedID, "expected_version": float64(1), "created_by_agent": ownerID.String(), "body": "갱신된 파생 본문",
	})
	if err != nil {
		t.Fatalf("파생 갱신: %v", err)
	}
	if structured(t, updated)["version"].(int64) != 2 {
		t.Fatal("파생 갱신이 판 번호를 증가시키지 않았다")
	}

	discarded, err := call(t.Context(), ownerID, "node_discard", map[string]any{"graph_id": graphID, "context_id": derivedID, "created_by_agent": ownerID.String()})
	if err != nil {
		t.Fatalf("파생 폐기: %v", err)
	}
	if _, ok := structured(t, discarded)["deleted_at"]; !ok {
		t.Fatal("폐기한 파생에 deleted_at이 없다")
	}
	if _, callErr := call(t.Context(), ownerID, "node_discard", map[string]any{"graph_id": graphID, "context_id": derivedID, "created_by_agent": ownerID.String()}); !hasCode(callErr, "invalid_argument") {
		t.Fatalf("재폐기가 invalid_argument로 처리되지 않았다: %v", callErr)
	}
	restored, err := call(t.Context(), ownerID, "node_restore", map[string]any{"graph_id": graphID, "context_id": derivedID, "created_by_agent": ownerID.String()})
	if err != nil {
		t.Fatalf("파생 복구: %v", err)
	}
	if _, stillDeleted := structured(t, restored)["deleted_at"]; stillDeleted {
		t.Fatal("복구한 파생에 deleted_at이 남아 있다")
	}
	occurredAt, ok := sourceValue["occurred_at"].(time.Time)
	if !ok {
		t.Fatalf("원천 발생 시각이 time.Time이 아니다: %#v", sourceValue["occurred_at"])
	}
	createdEvent, err := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "event", "body": "사건 본문", "created_by_agent": ownerID.String(),
		"member_refs": []any{sourceID}, "start": occurredAt.Add(-time.Minute).Format(time.RFC3339), "end": occurredAt.Add(time.Minute).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("사건 생성: %v", err)
	}
	eventValue := structured(t, createdEvent)
	if members, ok := eventValue["member_refs"].([]string); !ok || len(members) != 1 || members[0] != sourceID {
		t.Fatalf("사건 응답에 member_refs가 없다: %#v", eventValue["member_refs"])
	}
	if eventValue["event_start"] == nil || eventValue["event_end"] == nil {
		t.Fatalf("사건 응답에 시간 범위가 없다: %#v", eventValue)
	}
	if _, callErr := call(t.Context(), ownerID, "node_restore", map[string]any{"graph_id": graphID, "context_id": derivedID, "created_by_agent": ownerID.String()}); !hasCode(callErr, "invalid_argument") {
		t.Fatalf("활성 노드 복구가 invalid_argument로 처리되지 않았다: %v", callErr)
	}

	createdWebDeleted, err := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "derived", "body": "웹 삭제 파생", "created_by_agent": ownerID.String(),
		"derivation_kind": "proposition", "evidence_state": "observation", "derived_from": []any{sourceID},
	})
	if err != nil {
		t.Fatalf("웹 삭제 대상 파생 생성: %v", err)
	}
	webDeletedID := structured(t, createdWebDeleted)["context_id"].(string)
	webDeleteContext(t, pool, graphName, graphID, webDeletedID)
	_, restoreRejected := call(t.Context(), ownerID, "node_restore", map[string]any{"graph_id": graphID, "context_id": webDeletedID, "created_by_agent": ownerID.String()})
	domain, ok := errors.AsType[*Error](restoreRejected)
	if !ok || domain.Code != "not_supported" || domain.Data["alternative_channel"] != "web" {
		t.Fatalf("웹 삭제분 복구가 웹 대체 채널로 안내되지 않았다: %v", restoreRejected)
	}
	// 「감사 범위」가 거부된 연산도 기록 대상으로 확정했으므로 채널 경계 거부도 남아야 한다.
	if reasons := rejectedReasons(t, pool, webDeletedID); !slices.Contains(reasons, "not_supported") {
		t.Fatalf("웹 삭제분 복구 거부가 기록되지 않았다: %v", reasons)
	}
	if _, err := call(t.Context(), ownerID, "node_discard", map[string]any{"graph_id": graphID, "context_id": sourceID, "created_by_agent": ownerID.String()}); err != nil {
		t.Fatalf("근거 원천 폐기: %v", err)
	}
	invalidatedView, err := call(t.Context(), ownerID, "node_get", map[string]any{"graph_id": graphID, "context_id": derivedID, "hops": float64(0)})
	if err != nil {
		t.Fatalf("무효 표시 확인 조회: %v", err)
	}
	invalidated, ok := structured(t, invalidatedView)["contexts"].([]any)[0].(map[string]any)
	if !ok || invalidated["evidence_invalidated"] != true {
		t.Fatalf("근거 폐기 뒤 파생의 무효 표시가 응답에 없다: %#v", invalidated)
	}
}

// createHandlerAccount는 처리기 통합 테스트에 필요한 계정 행을 만든다.
func createHandlerAccount(t *testing.T, database *store.Store, accountID model.ID) {
	t.Helper()
	loginID := "mcp_" + strings.ReplaceAll(accountID.String(), "-", "")[5:]
	if err := database.CreateAccount(t.Context(), store.Account{ID: accountID, LoginID: loginID, PasswordHash: "test-password-hash", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("테스트 계정 생성: %v", err)
	}
}

// grantHandlerGrade는 처리기 등급 검사에 필요한 직접 등급을 만든다.
func grantHandlerGrade(t *testing.T, pool *pgx.Conn, graphName, graphID string, accountID model.ID, grade string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO public.graph_grant (graph_id, subject_type, subject_id, grade)
		VALUES ($1::uuid, 'account', $2::uuid, $3)`, graphID, accountID.String(), grade); err != nil {
		t.Fatalf("테스트 등급 부여: %v", err)
	}
}

// webDeleteContext는 폐기 연산 기록 없이 웹 직접 삭제 상태만 만든다.
func webDeleteContext(t *testing.T, pool *pgx.Conn, graphName, graphID, contextID string) {
	t.Helper()
	deletedAt := time.Now().UTC().Format(time.RFC3339Nano)
	graphLiteral := strings.ReplaceAll(graphName, "'", "''")
	cypher := fmt.Sprintf("MATCH (node:Context) WHERE node.context_id = %q AND node.graph_id = %q SET node += {deleted_at: %q} RETURN node",
		contextID, graphID, deletedAt)
	statement := "SELECT * FROM ag_catalog.cypher('" + graphLiteral + "', $$" + cypher + "$$) AS (node agtype)"
	if _, err := pool.Exec(t.Context(), statement); err != nil {
		t.Fatalf("웹 삭제 상태 표시: %v", err)
	}
}

// newHandlerID는 처리기 통합 테스트에 쓰는 UUIDv7 식별자를 만든다.
func newHandlerID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("UUIDv7 생성: %v", err)
	}
	return id
}

// structured는 도구 결과에서 구조화 응답을 꺼낸다.
func structured(t *testing.T, toolResult ToolResult) map[string]any {
	t.Helper()
	value, ok := toolResult.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("구조화 응답이 객체가 아니다: %#v", toolResult.StructuredContent)
	}
	return value
}

// hasCode는 도메인 오류의 MCP 코드를 판별한다.
func hasCode(callErr error, code string) bool {
	domain, ok := errors.AsType[*Error](callErr)
	return ok && domain.Code == code
}

// rejectedReasons는 한 컨텍스트에 남은 거부 기록의 사유를 모은다.
func rejectedReasons(t *testing.T, pool *pgx.Conn, contextID string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT reject_reason FROM public.operation_log WHERE context_id = $1 AND result = 'rejected'`, contextID)
	if err != nil {
		t.Fatalf("거부 기록 조회: %v", err)
	}
	defer rows.Close()
	reasons := make([]string, 0)
	for rows.Next() {
		var reason *string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("거부 기록 행 해석: %v", err)
		}
		if reason != nil {
			reasons = append(reasons, *reason)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("거부 기록 행 읽기: %v", err)
	}
	return reasons
}
