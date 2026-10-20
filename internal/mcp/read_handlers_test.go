package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type readStub struct {
	graph     model.Graph
	node      model.Context
	limits    plan.Limits
	found     bool
	err       error
	calls     []string
	hop       func(context.Context, model.ID, []model.Context, int, string, []string, int) (model.HopResult, error)
	list      func(model.ID, model.GraphListFilter, string, int) ([]model.GraphListItem, string, error)
	relations func(model.ID, model.ID, []model.RelationState, []model.RelationType, string, int) ([]model.Relation, string, error)
	flow      func(context.Context, model.FlowInput) (model.ContextFlow, error)
}

func (s *readStub) For(model.ID) plan.Limits { return s.limits }
func (s *readStub) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	s.calls = append(s.calls, "grade")
	return model.GraphGradeViewer, s.found, s.err
}
func (s *readStub) Graph(context.Context, model.ID) (model.Graph, error) {
	s.calls = append(s.calls, "graph")
	return s.graph, nil
}
func (s *readStub) Context(context.Context, model.ID, model.ID) (model.Context, error) {
	s.calls = append(s.calls, "node")
	return s.node, nil
}
func (s *readStub) ListGraphs(_ context.Context, a model.ID, f model.GraphListFilter, c string, n int) ([]model.GraphListItem, string, error) {
	s.calls = append(s.calls, "list")
	return s.list(a, f, c, n)
}
func (s *readStub) ListContextRelations(_ context.Context, g, c model.ID, states []model.RelationState, types []model.RelationType, cursor string, n int) ([]model.Relation, string, error) {
	s.calls = append(s.calls, "relations")
	return s.relations(g, c, states, types, cursor, n)
}
func (s *readStub) SearchHopContextsFrom(ctx context.Context, g model.ID, start []model.Context, hops int, direction string, filters []string, n int) (model.HopResult, error) {
	s.calls = append(s.calls, "hop")
	return s.hop(ctx, g, start, hops, direction, filters, n)
}
func (s *readStub) Flow(ctx context.Context, input model.FlowInput) (model.ContextFlow, error) {
	s.calls = append(s.calls, "flow")
	return s.flow(ctx, input)
}
func readFixture(t *testing.T) (*readStub, Principal, map[string]ToolHandler) {
	t.Helper()
	account, _ := model.NewID()
	g, _ := model.NewID()
	n, _ := model.NewID()
	s := &readStub{graph: model.Graph{ID: g, Version: 9007199254740993}, node: model.Context{ID: n, GraphID: g, Layer: model.LayerEvent, Event: &model.EventAttributes{}}, limits: plan.Default(), found: true}
	s.hop = func(context.Context, model.ID, []model.Context, int, string, []string, int) (model.HopResult, error) {
		return model.HopResult{Contexts: []model.Context{s.node}, Distances: map[model.ID]int{n: 0}}, nil
	}
	s.list = func(model.ID, model.GraphListFilter, string, int) ([]model.GraphListItem, string, error) {
		return nil, "", nil
	}
	s.relations = func(model.ID, model.ID, []model.RelationState, []model.RelationType, string, int) ([]model.Relation, string, error) {
		return nil, "", nil
	}
	s.flow = func(context.Context, model.FlowInput) (model.ContextFlow, error) { return model.ContextFlow{}, nil }
	handlers, err := NewReadHandlers(Dependencies{Grades: s, Plans: s, Graphs: s, Nodes: s, Hops: s, Relations: s, Flow: s})
	if err != nil {
		t.Fatal(err)
	}
	return s, Principal{AccountID: account, ClientID: "verified-client"}, handlers
}
func readCall(t *testing.T, ctx context.Context, h map[string]ToolHandler, p Principal, tool string, args map[string]any, code string) map[string]any {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := h[tool](ctx, p, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: tool, Arguments: data}})
	if err != nil || result == nil {
		t.Fatal("읽기 결과", err)
	}
	text := result.Content[0].(*sdk.TextContent).Text
	var actual, structured map[string]any
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(text), &actual) != nil || json.Unmarshal(raw, &structured) != nil || !reflect.DeepEqual(actual, structured) {
		t.Fatal("text/structuredContent 불일치")
	}
	if code == "" {
		if result.IsError || actual["code"] != nil {
			t.Fatalf("성공 예상: %s", text)
		}
	} else if !result.IsError || actual["code"] != code {
		t.Fatalf("%s 예상: %s", code, text)
	}
	if strings.Contains(text, "private") {
		t.Fatal("내부 원문 노출")
	}
	return actual
}
func TestReadHandlersDefaultsAndInputForwarding(t *testing.T) {
	for _, tool := range []string{"graph_list", "graph_get", "node_get", "relation_list", "context_flow_get"} {
		t.Run(tool, func(t *testing.T) {
			s, p, h := readFixture(t)
			args := map[string]any{"graph_id": s.graph.ID.String(), "context_id": s.node.ID.String()}
			switch tool {
			case "graph_list":
				args = map[string]any{"name_filter": "%한글_", "grade_filter": []string{"viewer", "viewer"}}
				s.list = func(account model.ID, f model.GraphListFilter, cursor string, n int) ([]model.GraphListItem, string, error) {
					if account != p.AccountID || f.Name != "%한글_" || len(f.Grades) != 2 || cursor != "" || n != s.limits.GraphPage.Default {
						t.Fatal("목록 입력")
					}
					return nil, "", nil
				}
			case "graph_get":
				delete(args, "context_id")
			case "node_get":
				s.hop = func(_ context.Context, g model.ID, start []model.Context, hops int, direction string, filters []string, n int) (model.HopResult, error) {
					if g != s.graph.ID || len(start) != 1 || start[0].ID != s.node.ID || hops != 0 || direction != "both" || n != s.limits.MaxHopNodes {
						t.Fatal("홉 기본값")
					}
					return model.HopResult{Contexts: start, Distances: map[model.ID]int{s.node.ID: 0}}, nil
				}
			case "relation_list":
				s.relations = func(g, c model.ID, states []model.RelationState, types []model.RelationType, cursor string, n int) ([]model.Relation, string, error) {
					if g != s.graph.ID || c != s.node.ID || len(states) != 0 || len(types) != 0 || n != s.limits.RelationPage.Default {
						t.Fatal("관계 기본값")
					}
					return nil, "", nil
				}
			case "context_flow_get":
				delete(args, "context_id")
				args["work_context"] = "작업"
				s.flow = func(_ context.Context, in model.FlowInput) (model.ContextFlow, error) {
					if in.GraphID != s.graph.ID || in.WorkContext != "작업" || in.AsOf.Location() != time.UTC || time.Since(in.AsOf) > time.Second || in.Budget != s.limits.ContextBudget || in.MaxHops != s.limits.MaxHops || in.MaxHopNodes != s.limits.MaxHopNodes {
						t.Fatal("흐름 기본값")
					}
					return model.ContextFlow{}, nil
				}
			}
			value := readCall(t, t.Context(), h, p, tool, args, "")
			if tool == "graph_list" && len(value["graphs"].([]any)) != 0 {
				t.Fatal("빈 목록")
			}
			if tool == "graph_get" {
				result, err := h[tool](t.Context(), p, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: tool, Arguments: []byte(`{"graph_id":"` + s.graph.ID.String() + `"}`)}})
				if err != nil || !strings.Contains(result.Content[0].(*sdk.TextContent).Text, "9007199254740993") {
					t.Fatal("판 정밀도")
				}
			}
		})
	}
}
func TestReadHandlersRejectBeforeStorage(t *testing.T) {
	for _, mode := range []string{"bad UUID", "unknown field", "non UTC", "zero budget", "fractional hops", "too many filters", "missing principal", "graph page limit"} {
		t.Run(mode, func(t *testing.T) {
			s, p, h := readFixture(t)
			tool := "node_get"
			args := map[string]any{"graph_id": s.graph.ID.String(), "context_id": s.node.ID.String()}
			code := "invalid_argument"
			switch mode {
			case "bad UUID":
				args["graph_id"] = "bad"
			case "unknown field":
				args["extra"] = true
			case "non UTC":
				tool = "context_flow_get"
				args = map[string]any{"graph_id": s.graph.ID.String(), "work_context": "작업", "as_of": "2026-10-20T09:00:00+09:00"}
			case "zero budget":
				tool = "context_flow_get"
				args = map[string]any{"graph_id": s.graph.ID.String(), "work_context": "작업", "budget": 0}
			case "fractional hops":
				args["hops"] = 1.5
			case "too many filters":
				args["traversal_filter"] = []string{"causes", "causes", "causes", "causes", "causes", "causes", "causes", "causes"}
			case "missing principal":
				p.ClientID = ""
				code = "internal"
			case "graph page limit":
				tool = "graph_list"
				args = map[string]any{"page_size": s.limits.GraphPage.Maximum + 1}
				code = "limit_exceeded"
			}
			readCall(t, t.Context(), h, p, tool, args, code)
			if len(s.calls) != 0 {
				t.Fatalf("거부 후 저장소 호출: %v", s.calls)
			}
		})
	}
}
func TestReadHandlersGraphAndNodeIsolation(t *testing.T) {
	for _, mode := range []string{"no grade", "grade failure", "other graph", "deleted graph", "other node", "node graph", "deleted node", "non event", "hop scope", "dangling edge", "unknown edge", "missing distance", "wrong zero distance", "duplicate node", "relation scope"} {
		t.Run(mode, func(t *testing.T) {
			s, p, h := readFixture(t)
			tool := "node_get"
			args := map[string]any{"graph_id": s.graph.ID.String(), "context_id": s.node.ID.String()}
			code := "internal"
			other, _ := model.NewID()
			now := time.Now().UTC()
			switch mode {
			case "no grade":
				s.found = false
				code = "not_found"
			case "grade failure":
				s.err = &DependencyFailure{Cause: model.FieldError{Field: "private"}}
			case "other graph":
				s.graph.ID = other
			case "deleted graph":
				s.graph.DeletedAt = &now
				code = "not_found"
			case "other node":
				s.node.ID = other
			case "node graph":
				s.node.GraphID = other
			case "deleted node":
				s.node.DeletedAt = &now
				code = "not_found"
			case "non event":
				s.node.Layer = model.LayerSource
				tool = "relation_list"
				code = "invalid_argument"
			case "relation scope":
				tool = "relation_list"
				s.relations = func(model.ID, model.ID, []model.RelationState, []model.RelationType, string, int) ([]model.Relation, string, error) {
					return []model.Relation{{GraphID: other}}, "", nil
				}
			default:
				s.hop = func(context.Context, model.ID, []model.Context, int, string, []string, int) (model.HopResult, error) {
					r := model.HopResult{Contexts: []model.Context{s.node}, Distances: map[model.ID]int{s.node.ID: 0}}
					switch mode {
					case "hop scope":
						n := s.node
						n.GraphID = other
						r.Contexts = append(r.Contexts, n)
					case "dangling edge":
						r.Edges = []model.HopEdge{{FromID: s.node.ID, ToID: other, Kind: "causes"}}
					case "unknown edge":
						r.Edges = []model.HopEdge{{FromID: s.node.ID, ToID: s.node.ID, Kind: "private"}}
					case "missing distance":
						r.Distances = nil
					case "wrong zero distance":
						r.Distances[s.node.ID] = 1
					case "duplicate node":
						r.Contexts = append(r.Contexts, s.node)
					}
					return r, nil
				}
			}
			readCall(t, t.Context(), h, p, tool, args, code)
			if (mode == "no grade" || mode == "grade failure") && len(s.calls) != 1 {
				t.Fatal("권한 거부 후 본문 조회")
			}
		})
	}
}
func TestReadHandlersForwardExplicitQueriesAndPartialResults(t *testing.T) {
	s, p, h := readFixture(t)
	other, _ := model.NewID()
	node := s.node
	node.ID = other
	s.hop = func(_ context.Context, g model.ID, starts []model.Context, hops int, direction string, filter []string, n int) (model.HopResult, error) {
		if hops != 2 || direction != "in" || !reflect.DeepEqual(filter, []string{"causes", "derived_from"}) || n != s.limits.MaxHopNodes {
			t.Fatal("명시한 홉 입력")
		}
		return model.HopResult{Contexts: []model.Context{node, s.node}, Distances: map[model.ID]int{s.node.ID: 0, other: 1}, Edges: []model.HopEdge{{FromID: other, ToID: s.node.ID, Kind: "derived_from"}, {FromID: other, ToID: s.node.ID, Kind: "causes"}}, Truncated: true, Boundary: 2}, nil
	}
	args := map[string]any{"graph_id": s.graph.ID.String(), "context_id": s.node.ID.String(), "hops": 2, "direction": "in", "traversal_filter": []string{"causes", "derived_from"}}
	value := readCall(t, t.Context(), h, p, "node_get", args, "")
	if len(value["references"].([]any)) != 1 || len(value["relations"].([]any)) != 1 || value["contexts"].([]any)[0].(map[string]any)["distance"] != float64(0) || value["truncated"].(map[string]any)["hop_boundary"] != float64(2) {
		t.Fatal("홉 부분 결과")
	}
	cursor := base64.RawURLEncoding.EncodeToString([]byte(other.String()))
	s.relations = func(g, c model.ID, states []model.RelationState, types []model.RelationType, cur string, n int) ([]model.Relation, string, error) {
		if cur != cursor || n != 1 || !reflect.DeepEqual(states, []model.RelationState{model.RelationStateConfirmed}) || !reflect.DeepEqual(types, []model.RelationType{model.RelationTypeCauses}) {
			t.Fatal("관계 필터")
		}
		return []model.Relation{{ID: other, GraphID: g, FromContextID: c, ToContextID: other}}, cursor, nil
	}
	readCall(t, t.Context(), h, p, "relation_list", map[string]any{"graph_id": s.graph.ID.String(), "context_id": s.node.ID.String(), "cursor": cursor, "page_size": 1, "state_filter": []string{"confirmed"}, "type_filter": []string{"causes"}}, "")
	s.flow = func(_ context.Context, in model.FlowInput) (model.ContextFlow, error) {
		if in.Budget != 10 || in.Scope != "global" || in.AsOf.Format(time.RFC3339) != "2026-10-20T01:00:00Z" {
			t.Fatal("흐름 입력")
		}
		return model.ContextFlow{Contexts: []model.FlowContext{{Value: node, Rank: 2, EntryDistance: 1, FoldedCount: 2}, {Value: s.node, Rank: 1, OriginKinds: []model.OriginKind{model.OriginKindUserUtterance}, MatchedChannels: []string{"time"}}}, Edges: []model.HopEdge{{FromID: other, ToID: s.node.ID, Kind: "causes"}}, Budget: 10, BudgetUsed: 4, EntryPoints: []model.ID{s.node.ID}, Channels: map[string]model.FlowChannel{"semantic": {Failure: "embedding_unavailable"}}, Truncation: &model.FlowTruncation{Reason: "budget", Excluded: 3}, HopBoundary: new(0), Read: model.ReadStats{Connections: 4}}, nil
	}
	value = readCall(t, t.Context(), h, p, "context_flow_get", map[string]any{"graph_id": s.graph.ID.String(), "work_context": "작업", "scope": "global", "budget": 10, "as_of": "2026-10-20T01:00:00Z"}, "")
	if value["Read"] != nil || value["contexts"].([]any)[1].(map[string]any)["folded_count"] != float64(2) || value["truncated"].(map[string]any)["hop_boundary"] != float64(0) || value["channels"].(map[string]any)["semantic"].(map[string]any)["failure"] != "embedding_unavailable" {
		t.Fatal("흐름 메타데이터")
	}
}
func TestReadHandlersLimitsFailuresAndCancellation(t *testing.T) {
	for _, mode := range []string{"hop limit", "budget limit", "relation page limit", "hop failure", "flow failure", "flow scope", "cancel before", "cancel during"} {
		t.Run(mode, func(t *testing.T) {
			s, p, h := readFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tool := "node_get"
			args := map[string]any{"graph_id": s.graph.ID.String(), "context_id": s.node.ID.String()}
			code := "limit_exceeded"
			switch mode {
			case "hop limit":
				s.limits.MaxHops = 1
				args["hops"] = 2
			case "budget limit":
				tool = "context_flow_get"
				args = map[string]any{"graph_id": s.graph.ID.String(), "work_context": "작업", "budget": s.limits.ContextBudget + 1}
			case "relation page limit":
				tool = "relation_list"
				args["page_size"] = s.limits.RelationPage.Maximum + 1
			case "hop failure":
				code = "internal"
				s.hop = func(context.Context, model.ID, []model.Context, int, string, []string, int) (model.HopResult, error) {
					return model.HopResult{}, errors.New("private SQL")
				}
			case "flow failure":
				code = "internal"
				tool = "context_flow_get"
				args = map[string]any{"graph_id": s.graph.ID.String(), "work_context": "작업"}
				s.flow = func(context.Context, model.FlowInput) (model.ContextFlow, error) {
					return model.ContextFlow{}, errors.New("private provider")
				}
			case "flow scope":
				code = "internal"
				tool = "context_flow_get"
				args = map[string]any{"graph_id": s.graph.ID.String(), "work_context": "작업"}
				s.flow = func(context.Context, model.FlowInput) (model.ContextFlow, error) {
					n := s.node
					n.GraphID, _ = model.NewID()
					return model.ContextFlow{Contexts: []model.FlowContext{{Value: n}}}, nil
				}
			case "cancel before":
				cancel()
			case "cancel during":
				s.hop = func(context.Context, model.ID, []model.Context, int, string, []string, int) (model.HopResult, error) {
					cancel()
					return model.HopResult{}, nil
				}
			}
			if strings.HasPrefix(mode, "cancel") {
				raw, _ := json.Marshal(args)
				result, err := h[tool](ctx, p, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: tool, Arguments: raw}})
				if result != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("취소가 완료 결과로 변환", err)
				}
				if mode == "cancel before" && len(s.calls) != 0 {
					t.Fatal("취소 뒤 저장소 호출")
				}
			} else {
				readCall(t, ctx, h, p, tool, args, code)
			}
		})
	}
}

func TestReadHandlersRequireDependenciesWithoutEffects(t *testing.T) {
	s, _, _ := readFixture(t)
	d := Dependencies{Grades: s, Plans: s, Graphs: s, Nodes: s, Hops: s, Relations: s, Flow: s}
	for _, remove := range []func(*Dependencies){func(d *Dependencies) { d.Grades = nil }, func(d *Dependencies) { d.Plans = nil }, func(d *Dependencies) { d.Graphs = nil }, func(d *Dependencies) { d.Nodes = nil }, func(d *Dependencies) { d.Hops = nil }, func(d *Dependencies) { d.Relations = nil }, func(d *Dependencies) { d.Flow = nil }} {
		missing := d
		remove(&missing)
		if h, err := NewReadHandlers(missing); h != nil || err == nil {
			t.Fatal("누락된 의존성 허용")
		}
	}
	if len(s.calls) != 0 {
		t.Fatal("조립 중 저장소 호출")
	}
}

// 보호 인증은 대역이며 다섯 읽기 처리기와 고정 SDK의 실제 외피를 검증한다.
func TestReadHandlersSDKCompleteResults(t *testing.T) {
	s, principal, handlers := readFixture(t)
	for _, tool := range toolDefinitions() {
		if handlers[tool.Name] == nil {
			handlers[tool.Name] = func(context.Context, Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				t.Error("읽기 표본 밖의 도구 호출")
				return nil, errors.New("시험 범위 밖")
			}
		}
	}
	handler, err := NewHandler(HTTPConfig{AllowedOrigins: []string{"https://allowed.test"}, Handlers: handlers, Protect: func(next AuthenticatedHandler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next(w, r, principal) })
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	for _, tool := range []string{"graph_list", "graph_get", "node_get", "relation_list", "context_flow_get"} {
		t.Run(tool, func(t *testing.T) {
			body := sdkProbeRequest("tools/call")
			params := body["params"].(map[string]any)
			args := map[string]any{}
			if tool != "graph_list" {
				args["graph_id"] = s.graph.ID.String()
			}
			if tool == "node_get" || tool == "relation_list" {
				args["context_id"] = s.node.ID.String()
			}
			if tool == "context_flow_get" {
				args["work_context"] = "작업"
			}
			params["name"], params["arguments"] = tool, args
			response := exchangeSDKProbe(t, server, "POST", body, map[string]string{"Mcp-Name": tool})
			wire := decodeSDKProbe(t, response)
			result, ok := wire["result"].(map[string]any)
			if !ok || response.Status != http.StatusOK || result["resultType"] != "complete" || result["isError"] == true || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("완료 외피: %s", response.Body)
			}
			var text map[string]any
			content := result["content"].([]any)[0].(map[string]any)["text"].(string)
			if json.Unmarshal([]byte(content), &text) != nil || !reflect.DeepEqual(text, result["structuredContent"]) {
				t.Fatal("SDK text/structuredContent 불일치")
			}
		})
	}
}
