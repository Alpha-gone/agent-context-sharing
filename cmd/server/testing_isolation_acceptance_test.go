package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type test04Embedder struct{}

func (test04Embedder) Embed(context.Context, string) ([]float64, error) {
	vector := make([]float64, 768)
	vector[0] = 1
	return vector, nil
}
func (test04Embedder) ModelID() string { return "test04:vector:768" }

func test04Handlers(t *testing.T, database *store.Store) map[string]mcp.ToolHandler {
	t.Helper()
	flow, err := search.New(database, test04Embedder{}, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 20, SemanticThreshold: 0.6, FoldThreshold: 0.95, GraphStage: search.GraphStageRelations, Consistency: search.ConsistencySnapshot}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := newMCPReadHandlers(database, plan.AccountPlans{}, flow)
	if err != nil {
		t.Fatal(err)
	}
	writes, err := newMCPWriteHandlers(database, plan.AccountPlans{})
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(handlers, writes)
	handlers["agent_register"], err = mcp.NewAgentHandler(database)
	if err != nil {
		t.Fatal(err)
	}
	return handlers
}

// 실제 tools/list와 같은 목록·입력 검증을 통과시킨다. Bearer 경계는 별도 TLS 인수에서 검사한다.
func test04Transport(t *testing.T, handlers map[string]mcp.ToolHandler, principal mcp.Principal) func(string, map[string]any) map[string]any {
	t.Helper()
	handler, err := mcp.NewHandler(mcp.HTTPConfig{AllowedOrigins: []string{"https://allowed.test"}, Handlers: handlers, Protect: func(next mcp.AuthenticatedHandler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next(w, r, principal) })
	}})
	if err != nil {
		t.Fatal(err)
	}
	return func(tool string, args map[string]any) map[string]any {
		t.Helper()
		method := "tools/call"
		params := map[string]any{"_meta": map[string]any{sdk.MetaKeyProtocolVersion: "2026-07-28", sdk.MetaKeyClientCapabilities: map[string]any{}}}
		if tool == "" {
			method = "tools/list"
		} else {
			params["name"], params["arguments"] = tool, args
		}
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "test04", "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequestWithContext(t.Context(), "POST", "https://server.test/mcp", bytes.NewReader(data))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("MCP-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", method)
		if tool != "" {
			request.Header.Set("Mcp-Name", tool)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var envelope map[string]any
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &envelope) != nil || envelope["error"] != nil {
			t.Fatalf("MCP 전송 실패: %d %s", response.Code, response.Body.String())
		}
		return envelope["result"].(map[string]any)
	}
}

func test04Foreign(t *testing.T, f *mcpWriteFixture) *mcpWriteFixture {
	t.Helper()
	other := *f
	account, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.CreateAccount(t.Context(), store.Account{ID: account, LoginID: "test04_foreign", PasswordHash: "fixture_hash", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	other.principal.AccountID = account
	other.agent, err = f.database.RegisterAgent(t.Context(), account, "first", "registration_key_1")
	if err != nil {
		t.Fatal(err)
	}
	// 같은 이름과 같은 원천 자연 키도 계정/그래프 사이에서 합쳐지면 안 된다.
	other.graph = other.id(t, "graph_create", map[string]any{"name": "그래프", "description": "외부 비공개 설명"}, "graph", "graph_id")
	args := other.args("node_create")
	args["layer"], args["body"], args["origin_kind"], args["occurred_at"] = "source", "외부 비공개 원천", "user_utterance", "2026-10-20T12:00:00Z"
	args["source_ref"] = map[string]any{"channel": "file", "locator": "file:///mcp06/first"}
	other.source = other.id(t, "node_create", args, "node", "context_id")
	args = other.args("node_create")
	args["layer"], args["body"], args["derived_from"], args["derivation_kind"], args["evidence_state"] = "derived", "외부 비공개 파생", []string{other.source.String()}, "proposition", "observation"
	other.derived = other.id(t, "node_create", args, "node", "context_id")
	args = other.args("node_create")
	args["layer"], args["body"], args["member_refs"], args["event_start"], args["event_end"] = "event", "외부 비공개 사건", []string{other.source.String()}, "2026-10-20T11:00:00Z", "2026-10-20T13:00:00Z"
	other.firstEvent = other.id(t, "node_create", args, "node", "context_id")
	event := other.args("node_create")
	event["layer"], event["body"], event["member_refs"], event["event_start"], event["event_end"] = "event", "외부 비공개 둘째 사건", []string{other.source.String()}, "2026-10-20T11:00:00Z", "2026-10-20T13:00:00Z"
	other.secondEvent = other.id(t, "node_create", event, "node", "context_id")
	args = other.args("relation_confirm")
	args["relation_type"], args["from_context_id"], args["to_context_id"] = "relates_to", other.firstEvent.String(), other.secondEvent.String()
	other.call(t, "relation_confirm", args, "")
	return &other
}

func test04GraphFingerprint(t *testing.T, f *mcpWriteFixture, graph model.ID) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"context_graph", "graph_grant", "operation_log", "context_revision", "index_task", "context_embedding"} {
		var hash string
		if err := f.pool.QueryRow(t.Context(), `SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY row_to_json(t)::text),'')) FROM public.`+table+` t WHERE graph_id=$1`, graph.String()).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, hash)
	}
	for _, table := range []string{`"Context"`, `_ag_label_edge`} {
		var hash string
		if err := f.pool.QueryRow(t.Context(), `SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY row_to_json(t)::text),'')) FROM agent_context.`+table+` t WHERE properties::text::jsonb->>'graph_id'=$1`, graph.String()).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, hash)
	}
	return strings.Join(parts, ":")
}

func test04CheckGraphIDs(t *testing.T, value any, graph string) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for field, item := range value {
			if field == "graph_id" && item != graph {
				t.Fatalf("응답 graph_id 불일치: %v, 기대 %s", item, graph)
			}
			test04CheckGraphIDs(t, item, graph)
		}
	case []any:
		for _, item := range value {
			test04CheckGraphIDs(t, item, graph)
		}
	}
}

func TestTEST04CatalogIsolationIntegration(t *testing.T) {
	catalogFixture := newMCPWriteFixture(t)
	list := test04Transport(t, test04Handlers(t, catalogFixture.database), catalogFixture.principal)("", nil)
	tools := list["tools"].([]any)
	if len(tools) != 14 {
		t.Fatalf("14종 도구 대응을 갱신해야 한다: %d", len(tools))
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)["name"].(string)
		t.Run(tool, func(t *testing.T) {
			f := newMCPWriteFixture(t)
			foreign := test04Foreign(t, f)
			args := f.args(tool)
			switch tool {
			case "agent_register":
				args = map[string]any{"registration_key": "registration_key_1"}
			case "graph_create":
				args = map[string]any{"name": "그래프", "request_id": "test04_shared_request"}
			case "graph_list":
				args = map[string]any{"name_filter": "그래프"}
			case "graph_get":
			case "graph_update":
				args["expected_version"], args["description"] = 1, "내 그래프 수정"
			case "node_create":
				args["layer"], args["body"], args["derived_from"], args["derivation_kind"], args["evidence_state"] = "derived", "내 새 파생", []string{f.source.String()}, "proposition", "observation"
			case "node_get":
				args = map[string]any{"graph_id": f.graph.String(), "context_id": f.source.String(), "hops": 2, "direction": "both"}
			case "node_update":
				args["context_id"], args["expected_version"], args["body"] = f.derived.String(), 1, "내 파생 수정"
			case "node_discard", "node_restore":
				args["context_id"] = f.derived.String()
				if tool == "node_restore" {
					f.call(t, "node_discard", args, "")
				}
			case "relation_confirm", "relation_discard", "relation_list":
				args["relation_type"], args["from_context_id"], args["to_context_id"] = "precedes", f.firstEvent.String(), f.secondEvent.String()
				if tool != "relation_confirm" {
					id := f.id(t, "relation_confirm", args, "relation", "relation_id")
					args = f.args(tool)
					if tool == "relation_discard" {
						args["relation_id"] = id.String()
					} else {
						args = map[string]any{"graph_id": f.graph.String(), "context_id": f.firstEvent.String()}
					}
				}
			case "context_flow_get":
				args = map[string]any{"graph_id": f.graph.String(), "work_context": "원천 사건", "scope": "local"}
				for _, graph := range []model.ID{f.graph, foreign.graph} {
					for range 10 {
						result, err := f.database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, store.IndexTask) store.IndexTaskResult {
							vector, _ := (test04Embedder{}).Embed(t.Context(), "")
							return store.IndexTaskResult{Embedding: vector, ModelID: (test04Embedder{}).ModelID()}
						})
						if err != nil || result.Found && !result.Succeeded {
							t.Fatal("양쪽 그래프 색인", err)
						}
						if !result.Found {
							break
						}
					}
				}
			default:
				t.Fatalf("격리 시험이 없는 새 도구: %s", tool)
			}
			first := test04Transport(t, test04Handlers(t, f.database), f.principal)
			secondStore, err := store.New(t.Context(), f.pool.Config().ConnString(), "agent_context", nil, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(secondStore.Close)
			second := test04Transport(t, test04Handlers(t, secondStore), f.principal)
			before := test04GraphFingerprint(t, f, foreign.graph)
			if _, scoped := args["graph_id"]; scoped {
				var previous map[string]any
				missing, err := model.NewID()
				if err != nil {
					t.Fatal(err)
				}
				for _, graph := range []model.ID{foreign.graph, missing} {
					attempt := maps.Clone(args)
					attempt["graph_id"] = graph.String()
					result := second(tool, attempt)
					value := result["structuredContent"].(map[string]any)
					if result["isError"] != true || value["code"] != "not_found" {
						t.Fatalf("다른 그래프와 부재를 구별함: %v", result)
					}
					if previous != nil && !reflect.DeepEqual(previous, value) {
						t.Fatal("다른 그래프와 부재의 오류 정보가 다르다")
					}
					previous = value
				}
			}
			result := first(tool, args)
			if result["isError"] == true {
				t.Fatalf("자기 범위 호출 실패: %v", result)
			}
			value := result["structuredContent"].(map[string]any)
			graph := f.graph.String()
			if tool == "graph_create" {
				graph = value["graph"].(map[string]any)["graph_id"].(string)
			}
			test04CheckGraphIDs(t, value, graph)
			switch tool {
			case "graph_list":
				if len(value["graphs"].([]any)) != 1 {
					t.Fatal("소유 그래프 목록이 비거나 다른 계정을 포함한다")
				}
			case "node_get":
				if len(value["contexts"].([]any)) < 2 || len(value["references"].([]any)) == 0 {
					t.Fatal("정점·참조 표본 누락")
				}
			case "relation_list":
				if len(value["relations"].([]any)) != 1 {
					t.Fatal("간선 표본 누락")
				}
			case "context_flow_get":
				semantic := value["channels"].(map[string]any)["semantic"].(map[string]any)
				if len(value["contexts"].([]any)) == 0 || semantic["failure"] != "" || semantic["candidates"].(float64) == 0 {
					t.Fatalf("실제 벡터 검색을 포함한 표본 누락: %v", value)
				}
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{foreign.graph.String(), foreign.source.String(), foreign.derived.String(), foreign.firstEvent.String(), foreign.secondEvent.String(), "외부 비공개"} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatal("다른 그래프의 응답 노출")
				}
			}
			if before != test04GraphFingerprint(t, f, foreign.graph) {
				t.Fatal("다른 그래프의 정점·간선·저장량·감사·큐·벡터를 변경했다")
			}
			if tool == "agent_register" {
				if result["structuredContent"].(map[string]any)["agent_id"] != f.agent.String() {
					t.Fatal("같은 계정/client/key 등록 재생 실패")
				}
				for _, principal := range []mcp.Principal{foreign.principal, {AccountID: f.principal.AccountID, ClientID: "second"}} {
					other := test04Transport(t, test04Handlers(t, secondStore), principal)(tool, args)
					id := other["structuredContent"].(map[string]any)["agent_id"]
					if id == f.agent.String() || other["isError"] == true {
						t.Fatal("등록이 계정/client 경계를 공유했다")
					}
				}
			} else if tool == "graph_create" {
				replay := second(tool, args)
				if !reflect.DeepEqual(result, replay) {
					t.Fatal("다중 인스턴스 멱등성 결과 불일치")
				}
			}
		})
	}
}

func TestTEST04ManipulatedInputsIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	foreign := test04Foreign(t, f)
	call := test04Transport(t, test04Handlers(t, f.database), f.principal)
	before := test04GraphFingerprint(t, f, foreign.graph)
	for _, sample := range []struct {
		tool, field string
		value       any
	}{
		{"graph_list", "cursor", "' OR true //"},
		{"graph_list", "grade_filter", []string{"owner') OR true --"}},
		{"node_get", "graph_id", "' OR true //"},
		{"node_get", "context_id", "00000000-0000-4000-8000-000000000000"},
		{"node_get", "traversal_filter", []string{"HAS_MEMBER] RETURN node //"}},
		{"node_get", "direction", "both'] RETURN node //"},
		{"relation_list", "cursor", "%%%"},
		{"relation_list", "state_filter", []string{"confirmed'] RETURN edge //"}},
		{"relation_list", "type_filter", []string{"precedes'] RETURN edge //"}},
	} {
		t.Run(sample.tool+"/"+sample.field, func(t *testing.T) {
			args := map[string]any{}
			if sample.tool != "graph_list" {
				args["graph_id"] = f.graph.String()
				args["context_id"] = f.firstEvent.String()
			}
			args[sample.field] = sample.value
			result := call(sample.tool, args)
			if result["isError"] != true || result["structuredContent"].(map[string]any)["code"] != "invalid_argument" {
				t.Fatalf("조작 입력을 허용했다: %v", result)
			}
		})
	}
	for _, tool := range []string{"node_get", "node_update", "node_discard", "node_restore", "relation_list"} {
		t.Run(tool+"/foreign_target", func(t *testing.T) {
			args := f.args(tool)
			if tool == "node_get" || tool == "relation_list" {
				args = map[string]any{"graph_id": f.graph.String()}
			}
			if tool == "node_update" {
				args["expected_version"], args["body"] = 1, "경계 우회"
			}
			missing, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			var previous any
			for _, target := range []model.ID{foreign.derived, missing} {
				args["context_id"] = target.String()
				result := call(tool, args)
				value := result["structuredContent"].(map[string]any)
				if result["isError"] != true || value["code"] != "not_found" || previous != nil && !reflect.DeepEqual(previous, value) {
					t.Fatalf("상대 대상과 부재가 다르다: %v", result)
				}
				previous = value
			}
		})
	}
	if before != test04GraphFingerprint(t, f, foreign.graph) {
		t.Fatal("조작 요청이 다른 그래프를 변경했다")
	}
}
