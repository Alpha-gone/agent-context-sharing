package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
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
	// 요청 경로에서 자동 후보 제안이 실제로 도는지 확인해야 하므로 구성을 켜고 만든다.
	database, err := store.New(t.Context(), databaseURL, graphName, &store.RelationProposalConfig{
		AdjacencyWindow: time.Hour, SimilarityThreshold: 0.8, Limit: 10,
	}, nil, "")
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

	// 전송 결과를 받지 못한 재시도도 업무 변경과 적용 기록을 한 번만 남기고 최초 결과를
	// 돌려줘야 한다. 같은 키를 다른 요청에 보내면 두 번째 쓰기는 시작하지 않는다.
	idempotencyKey, err := model.NewID()
	if err != nil {
		t.Fatalf("멱등성 키 생성: %v", err)
	}
	idempotencyArguments := sourceArguments()
	idempotencyArguments["body"] = "멱등성 원천 본문"
	idempotencyArguments["locator"] = locator + "/idempotency"
	fingerprint, err := requestFingerprint("node_create", idempotencyArguments)
	if err != nil {
		t.Fatalf("멱등성 요청 지문: %v", err)
	}
	idempotencyContext := context.WithValue(t.Context(), idempotencyContextKey{}, IdempotencyRequest{KeyHeader: idempotencyHeader(idempotencyKey), Fingerprint: fingerprint})
	firstCreate, err := call(idempotencyContext, ownerID, "node_create", idempotencyArguments)
	if err != nil {
		t.Fatalf("멱등성 생성: %v", err)
	}
	replayedCreate, err := call(idempotencyContext, ownerID, "node_create", idempotencyArguments)
	if err != nil {
		t.Fatalf("멱등성 생성 재생: %v", err)
	}
	idempotencySourceID := structured(t, firstCreate)["context_id"].(string)
	if idempotencySourceID != structured(t, replayedCreate)["context_id"] {
		t.Fatalf("재생 결과가 최초 결과와 다르다: first=%#v replay=%#v", structured(t, firstCreate), structured(t, replayedCreate))
	}
	var addRecordCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM public.operation_log
		WHERE graph_id = $1 AND context_id = $2 AND operation_kind = 'add' AND result = 'applied'`, graphID, idempotencySourceID).Scan(&addRecordCount); err != nil {
		t.Fatalf("멱등성 생성 기록 수 조회: %v", err)
	}
	if addRecordCount != 1 {
		t.Fatalf("멱등성 생성 적용 기록 수 = %d, want 1", addRecordCount)
	}
	differentArguments := map[string]any{"graph_id": graphID, "layer": "source", "body": "다른 요청", "created_by_agent": ownerID.String(), "source_channel": "api", "locator": locator + "/different", "occurred_at": time.Now().UTC().Format(time.RFC3339), "origin_kind": "external_content"}
	differentFingerprint, err := requestFingerprint("node_create", differentArguments)
	if err != nil {
		t.Fatalf("다른 멱등성 요청 지문: %v", err)
	}
	differentContext := context.WithValue(t.Context(), idempotencyContextKey{}, IdempotencyRequest{KeyHeader: idempotencyHeader(idempotencyKey), Fingerprint: differentFingerprint})
	if _, err := call(differentContext, ownerID, "node_create", differentArguments); !hasCode(err, "invalid_argument") {
		t.Fatalf("같은 키의 다른 요청 오류 = %v, want invalid_argument", err)
	}

	concurrentKey, err := model.NewID()
	if err != nil {
		t.Fatalf("동시 멱등성 키 생성: %v", err)
	}
	concurrentArguments := sourceArguments()
	concurrentArguments["body"] = "동시 멱등성 원천 본문"
	concurrentArguments["locator"] = locator + "/concurrent"
	concurrentFingerprint, err := requestFingerprint("node_create", concurrentArguments)
	if err != nil {
		t.Fatalf("동시 멱등성 요청 지문: %v", err)
	}
	concurrentContext := context.WithValue(t.Context(), idempotencyContextKey{}, IdempotencyRequest{KeyHeader: idempotencyHeader(concurrentKey), Fingerprint: concurrentFingerprint})
	type concurrentResult struct {
		result ToolResult
		err    error
	}
	concurrentResults := make(chan concurrentResult, 2)
	var calls sync.WaitGroup
	for range 2 {
		calls.Go(func() {
			result, callErr := call(concurrentContext, ownerID, "node_create", concurrentArguments)
			concurrentResults <- concurrentResult{result: result, err: callErr}
		})
	}
	calls.Wait()
	close(concurrentResults)
	var concurrentIDs []string
	for result := range concurrentResults {
		if result.err != nil {
			t.Fatalf("동시 멱등성 생성: %v", result.err)
		}
		concurrentIDs = append(concurrentIDs, structured(t, result.result)["context_id"].(string))
	}
	if len(concurrentIDs) != 2 || concurrentIDs[0] != concurrentIDs[1] {
		t.Fatalf("동시 재생 결과가 다르다: %#v", concurrentIDs)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM public.operation_log
		WHERE graph_id = $1 AND context_id = $2 AND operation_kind = 'add' AND result = 'applied'`, graphID, concurrentIDs[0]).Scan(&addRecordCount); err != nil {
		t.Fatalf("동시 멱등성 생성 기록 수 조회: %v", err)
	}
	if addRecordCount != 1 {
		t.Fatalf("동시 멱등성 생성 적용 기록 수 = %d, want 1", addRecordCount)
	}

	// 트랜잭션 밖 사전 검사가 낡은 저장량을 읽어 통과하면, 한도 초과는 정점과 색인 작업을
	// 쓴 뒤에야 드러난다. 이 거부를 완료 결과로 확정할 때 앞선 업무 변경은 함께 커밋되면
	// 안 된다.
	limitedSource := sourceArguments()
	limitedSource["body"] = "한도 경합 원천 본문"
	limitedSource["locator"] = locator + "/limit-race"
	var storedChars int64
	if err := pool.QueryRow(t.Context(), `SELECT stored_chars FROM public.context_graph WHERE graph_id = $1`, graphID).Scan(&storedChars); err != nil {
		t.Fatalf("그래프 저장량 조회: %v", err)
	}
	limitedPlans, err := plan.ParseAccountPlans(fmt.Sprintf(`{%q: {"stored_characters_per_graph": %d}}`, ownerID.String(), storedChars+int64(len([]rune(limitedSource["body"].(string))))-1))
	if err != nil {
		t.Fatalf("한도 플랜 해석: %v", err)
	}
	limitedCall := NewHandler(staleStoredCharsOperations{database}, limitedPlans, nil)
	contextsBefore := countGraphContexts(t, pool, graphName, graphID)
	limitedKey := newHandlerID(t)
	limitedFingerprint, err := requestFingerprint("node_create", limitedSource)
	if err != nil {
		t.Fatalf("한도 경합 요청 지문: %v", err)
	}
	limitedContext := context.WithValue(t.Context(), idempotencyContextKey{}, IdempotencyRequest{KeyHeader: idempotencyHeader(limitedKey), Fingerprint: limitedFingerprint})
	for attempt := range 2 {
		limited, err := limitedCall(limitedContext, ownerID, "node_create", limitedSource)
		if err != nil || !limited.IsError || structured(t, limited)["code"] != "limit_exceeded" {
			t.Fatalf("한도 경합 %d번째 결과 = %#v, err = %v, want limit_exceeded", attempt+1, limited, err)
		}
	}
	if contextsAfter := countGraphContexts(t, pool, graphName, graphID); contextsAfter != contextsBefore {
		t.Fatalf("한도 초과로 거부한 생성이 정점을 남겼다: before=%d after=%d", contextsBefore, contextsAfter)
	}

	// 키 형식은 권한 확인 뒤의 7단계에서 검증하므로 권한 거부가 형식 오류에 가려지지 않는다.
	updateArguments := map[string]any{"graph_id": graphID, "expected_version": float64(2), "name": "키 형식 검사"}
	malformedContext := context.WithValue(t.Context(), idempotencyContextKey{}, IdempotencyRequest{KeyHeader: []string{"bad-key"}})
	if _, err := call(malformedContext, viewerID, "graph_update", updateArguments); !hasCode(err, "permission_denied") {
		t.Fatalf("열람자의 잘못된 키 갱신 오류 = %v, want permission_denied", err)
	}
	if _, err := call(malformedContext, ownerID, "graph_update", updateArguments); !hasCode(err, "invalid_argument") {
		t.Fatalf("잘못된 키 갱신 오류 = %v, want invalid_argument", err)
	}

	// graph_create 재생도 저장 결과의 그래프에 지금 등급이 있는지 다시 확인한다.
	createArguments := map[string]any{"name": "멱등성 재생 권한 그래프"}
	createFingerprint, err := requestFingerprint("graph_create", createArguments)
	if err != nil {
		t.Fatalf("그래프 생성 요청 지문: %v", err)
	}
	createContext := context.WithValue(t.Context(), idempotencyContextKey{}, IdempotencyRequest{KeyHeader: idempotencyHeader(newHandlerID(t)), Fingerprint: createFingerprint})
	createdByViewer, err := call(createContext, viewerID, "graph_create", createArguments)
	if err != nil {
		t.Fatalf("멱등성 그래프 생성: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM public.graph_grant WHERE graph_id = $1 AND subject_id = $2`, structured(t, createdByViewer)["graph_id"], viewerID.String()); err != nil {
		t.Fatalf("그래프 등급 회수: %v", err)
	}
	if _, err := call(createContext, viewerID, "graph_create", createArguments); !hasCode(err, "not_found") {
		t.Fatalf("등급을 잃은 뒤 그래프 생성 재생 오류 = %v, want not_found", err)
	}

	rollbackKey, err := model.NewID()
	if err != nil {
		t.Fatalf("롤백 멱등성 키 생성: %v", err)
	}
	rollbackRequest := store.IdempotencyRequest{AccountID: ownerID, Key: rollbackKey, ToolName: "graph_create", Fingerprint: concurrentFingerprint}
	if _, err := database.ReplayIdempotent(t.Context(), rollbackRequest, func(context.Context) ([]byte, error) {
		return nil, errors.New("의도한 업무 롤백")
	}); err == nil {
		t.Fatal("롤백되는 멱등성 업무가 성공했다")
	}
	var rollbackRecordCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.idempotency_record WHERE actor_account_id = $1 AND idempotency_key = $2`, ownerID.String(), rollbackKey.String()).Scan(&rollbackRecordCount); err != nil {
		t.Fatalf("롤백 멱등성 기록 수 조회: %v", err)
	}
	if rollbackRecordCount != 0 {
		t.Fatalf("롤백된 업무의 멱등성 기록 수 = %d, want 0", rollbackRecordCount)
	}

	expiredKey, err := model.NewID()
	if err != nil {
		t.Fatalf("만료 멱등성 키 생성: %v", err)
	}
	expiredRequest := store.IdempotencyRequest{AccountID: ownerID, Key: expiredKey, ToolName: "graph_create", Fingerprint: concurrentFingerprint}
	firstExpiryResult, err := database.ReplayIdempotent(t.Context(), expiredRequest, func(context.Context) ([]byte, error) {
		return []byte(`{"marker":1}`), nil
	})
	if err != nil || string(firstExpiryResult) != `{"marker":1}` {
		t.Fatalf("만료 검증 최초 결과 = %s, err = %v", firstExpiryResult, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE public.idempotency_record SET expires_at = now() - interval '1 second' WHERE actor_account_id = $1 AND idempotency_key = $2`, ownerID.String(), expiredKey.String()); err != nil {
		t.Fatalf("멱등성 결과 만료: %v", err)
	}
	secondExpiryResult, err := database.ReplayIdempotent(t.Context(), expiredRequest, func(context.Context) ([]byte, error) {
		return []byte(`{"marker":2}`), nil
	})
	if err != nil || string(secondExpiryResult) != `{"marker":2}` {
		t.Fatalf("만료 뒤 새 결과 = %s, err = %v", secondExpiryResult, err)
	}
	duplicated, err := call(t.Context(), ownerID, "node_create", sourceArguments())
	if err != nil {
		t.Fatalf("중복 원천 생성: %v", err)
	}
	if structured(t, duplicated)["context_id"].(string) != sourceID {
		t.Fatal("같은 source_ref가 기존 원천을 반환하지 않았다")
	}
	// 거부된 생성의 기록은 대상을 비운다. 요청이 배정한 식별자는 저장되지 않으므로 남기면
	// 기록이 존재하지 않는 컨텍스트를 가리킨다. 근거는 「기록 항목」이다.
	// 없는 근거를 가리켜 저장 계층에서 거부시킨다. 처리기 검증에서 막히면 기록 자체가 없다.
	if _, callErr := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "derived", "body": "없는 근거를 가리키는 파생", "created_by_agent": ownerID.String(),
		"derivation_kind": "proposition", "evidence_state": "observation",
		"derived_from": []any{newHandlerID(t).String()},
	}); callErr == nil {
		t.Fatal("없는 근거를 가리킨 파생 생성이 거부되지 않았다")
	}
	if dangling := danglingRejectedTargets(t, pool, graphID); dangling != 0 {
		t.Fatalf("거부 기록이 없는 대상을 가리킨다: %d개", dangling)
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
	kept, err := call(t.Context(), ownerID, "node_update", map[string]any{
		"graph_id": graphID, "context_id": derivedID, "expected_version": float64(structured(t, restored)["version"].(int64)),
		"created_by_agent": ownerID.String(), "management_action": "keep",
	})
	if err != nil {
		t.Fatalf("파생 유지 판단: %v", err)
	}
	if structured(t, kept)["version"] != structured(t, restored)["version"] {
		t.Fatal("유지 판단이 컨텍스트 판 번호를 바꿨다")
	}
	dependent, err := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "derived", "body": "이전 판을 근거로 한 파생", "created_by_agent": ownerID.String(),
		"derivation_kind": "proposition", "evidence_state": "observation", "derived_from": []any{derivedID},
	})
	if err != nil {
		t.Fatalf("대체 전 파생 생성: %v", err)
	}
	dependentID := structured(t, dependent)["context_id"].(string)
	if _, err := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "derived", "body": "대체 파생", "created_by_agent": ownerID.String(),
		"derivation_kind": "proposition", "evidence_state": "observation", "derived_from": []any{sourceID}, "supersedes_context_id": derivedID,
	}); err != nil {
		t.Fatalf("파생 대체: %v", err)
	}
	dependentView, err := call(t.Context(), ownerID, "node_get", map[string]any{"graph_id": graphID, "context_id": dependentID, "hops": float64(0)})
	if err != nil {
		t.Fatalf("대체 근거 무효 표시 조회: %v", err)
	}
	if dependentContext := structured(t, dependentView)["contexts"].([]any)[0].(map[string]any); dependentContext["evidence_invalidated"] != true {
		t.Fatalf("파생 대체가 직접 의존 파생을 무효 표시하지 않았다: %#v", dependentContext)
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
	eventID := eventValue["context_id"].(string)
	nextSource, err := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "source", "body": "다음 사건 원천", "created_by_agent": ownerID.String(),
		"source_channel": "api", "locator": locator + "/next", "occurred_at": occurredAt.Add(2 * time.Minute).Format(time.RFC3339), "origin_kind": "external_content",
	})
	if err != nil {
		t.Fatalf("다음 사건 원천 생성: %v", err)
	}
	nextSourceID := structured(t, nextSource)["context_id"].(string)
	createdNextEvent, err := call(t.Context(), ownerID, "node_create", map[string]any{
		"graph_id": graphID, "layer": "event", "body": "다음 사건 본문", "created_by_agent": ownerID.String(),
		"member_refs": []any{nextSourceID}, "start": occurredAt.Add(2 * time.Minute).Format(time.RFC3339), "end": occurredAt.Add(3 * time.Minute).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("두 번째 사건 생성: %v", err)
	}
	nextEventID := structured(t, createdNextEvent)["context_id"].(string)

	// 두 사건의 간격이 인접 임계값 안이므로 요청 경로의 저장이 끝난 뒤 시스템 제안이 남아야
	// 한다. 저장소 단위 테스트와 달리 여기에서는 MCP 요청이 그 경로를 실제로 밟는지 본다.
	proposedList, err := call(t.Context(), ownerID, "relation_list", map[string]any{
		"graph_id": graphID, "context_id": nextEventID, "state_filter": []any{"proposed"},
	})
	if err != nil {
		t.Fatalf("시스템 후보 조회: %v", err)
	}
	if !hasSystemProposal(structured(t, proposedList), "precedes", eventID, nextEventID) {
		t.Fatalf("요청 경로의 사건 저장이 시스템 후보를 만들지 않았다: %#v", structured(t, proposedList)["relations"])
	}
	confirmed, err := call(t.Context(), ownerID, "relation_confirm", map[string]any{
		"graph_id": graphID, "relation_type": "relates_to", "from_context_id": nextEventID, "to_context_id": eventID, "created_by_agent": ownerID.String(),
	})
	if err != nil {
		t.Fatalf("관계 확정: %v", err)
	}
	confirmedValue := structured(t, confirmed)
	if confirmedValue["from_context_id"] != eventID || confirmedValue["to_context_id"] != nextEventID || confirmedValue["state"] != "confirmed" {
		t.Fatalf("대칭 관계가 정규화되어 확정되지 않았다: %#v", confirmedValue)
	}
	relationID := confirmedValue["relation_id"].(string)
	// 자동 후보가 함께 붙으므로 확정 상태로 좁혀야 이 단언이 확정 관계만 본다.
	listedRelations, err := call(t.Context(), ownerID, "relation_list", map[string]any{
		"graph_id": graphID, "context_id": eventID, "state_filter": []any{"confirmed"},
	})
	if err != nil || len(structured(t, listedRelations)["relations"].([]any)) != 1 {
		t.Fatalf("사건 관계 목록 조회: result=%#v, err=%v", structured(t, listedRelations), err)
	}
	if _, err := call(t.Context(), ownerID, "relation_discard", map[string]any{"graph_id": graphID, "relation_id": relationID, "created_by_agent": ownerID.String()}); err != nil {
		t.Fatalf("관계 폐기: %v", err)
	}
	if _, callErr := call(t.Context(), ownerID, "relation_discard", map[string]any{"graph_id": graphID, "relation_id": relationID, "created_by_agent": ownerID.String()}); !hasCode(callErr, "invalid_argument") {
		t.Fatalf("관계 재폐기가 invalid_argument로 처리되지 않았다: %v", callErr)
	}
	reconfirmed, err := call(t.Context(), ownerID, "relation_confirm", map[string]any{
		"graph_id": graphID, "relation_type": "relates_to", "from_context_id": eventID, "to_context_id": nextEventID, "created_by_agent": ownerID.String(),
	})
	if err != nil || structured(t, reconfirmed)["relation_id"] != relationID || structured(t, reconfirmed)["state"] != "confirmed" {
		t.Fatalf("폐기 관계 재확정: result=%#v, err=%v", structured(t, reconfirmed), err)
	}
	if _, err := call(t.Context(), ownerID, "relation_confirm", map[string]any{
		"graph_id": graphID, "relation_type": "precedes", "from_context_id": eventID, "to_context_id": nextEventID, "created_by_agent": ownerID.String(),
	}); err != nil {
		t.Fatalf("시간 관계 확정: %v", err)
	}
	if _, callErr := call(t.Context(), ownerID, "relation_confirm", map[string]any{
		"graph_id": graphID, "relation_type": "precedes", "from_context_id": nextEventID, "to_context_id": eventID, "created_by_agent": ownerID.String(),
	}); !hasCode(callErr, "invalid_argument") {
		t.Fatalf("순환 시간 관계가 invalid_argument로 거부되지 않았다: %v", callErr)
	}
	if _, err := call(t.Context(), ownerID, "node_discard", map[string]any{"graph_id": graphID, "context_id": nextEventID, "created_by_agent": ownerID.String()}); err != nil {
		t.Fatalf("관계가 붙은 사건 폐기: %v", err)
	}
	afterEventDiscard, err := call(t.Context(), ownerID, "relation_list", map[string]any{"graph_id": graphID, "context_id": eventID, "state_filter": []any{"discarded"}})
	if err != nil || len(structured(t, afterEventDiscard)["relations"].([]any)) != 2 {
		t.Fatalf("사건 폐기 뒤 관계 정리: result=%#v, err=%v", structured(t, afterEventDiscard), err)
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

// staleStoredCharsOperations는 트랜잭션 밖 사전 검사가 동시 쓰기 전의 저장량을 읽은
// 경합을 재현한다. 실제 한도 강제는 저장소의 쓰기 트랜잭션만 맡게 된다.
type staleStoredCharsOperations struct{ *store.Store }

func (operations staleStoredCharsOperations) Graph(ctx context.Context, graphID model.ID) (model.Graph, error) {
	graph, err := operations.Store.Graph(ctx, graphID)
	graph.StoredChars = 0
	return graph, err
}

// idempotencyHeader는 키를 Structured Fields String 헤더 값으로 만든다.
func idempotencyHeader(key model.ID) []string {
	return []string{`"` + key.String() + `"`}
}

// countGraphContexts는 폐기 여부와 무관하게 그래프의 컨텍스트 정점 수를 센다.
func countGraphContexts(t *testing.T, pool *pgx.Conn, graphName, graphID string) int {
	t.Helper()
	graphLiteral := strings.ReplaceAll(graphName, "'", "''")
	cypher := fmt.Sprintf("MATCH (node:Context) WHERE node.graph_id = %q RETURN count(node)", graphID)
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count::text::int FROM ag_catalog.cypher('"+graphLiteral+"', $$"+cypher+"$$) AS (count agtype)").Scan(&count); err != nil {
		t.Fatalf("컨텍스트 정점 수 조회: %v", err)
	}
	return count
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

// hasSystemProposal은 관계 목록 응답에 지정한 정체성의 시스템 제안이 있는지 확인한다.
func hasSystemProposal(listed map[string]any, relationType, fromID, toID string) bool {
	relations, ok := listed["relations"].([]any)
	if !ok {
		return false
	}
	for _, item := range relations {
		relation, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if relation["relation_type"] == relationType && relation["from_context_id"] == fromID &&
			relation["to_context_id"] == toID && relation["state"] == "proposed" && relation["proposed_by"] == "system" {
			return true
		}
	}
	return false
}

// danglingRejectedTargets는 대상 열이 채워졌지만 그 대상이 실재하지 않는 거부 기록을 센다.
func danglingRejectedTargets(t *testing.T, pool *pgx.Conn, graphID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM public.operation_log log
		WHERE log.graph_id = $1 AND log.result = 'rejected'
		  AND log.context_id IS NULL AND log.relation_id IS NULL
		  AND log.target_version IS NOT NULL`, graphID).Scan(&count); err != nil {
		t.Fatalf("거부 기록 대상 조회: %v", err)
	}
	var withTarget int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM public.operation_log log
		WHERE log.graph_id = $1 AND log.result = 'rejected' AND log.context_id IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM public.operation_log applied
			WHERE applied.context_id = log.context_id AND applied.result = 'applied')`,
		graphID).Scan(&withTarget); err != nil {
		t.Fatalf("거부 기록 대상 실재 확인: %v", err)
	}
	return count + withTarget
}
