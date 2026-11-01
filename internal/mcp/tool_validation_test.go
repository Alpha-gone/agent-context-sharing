package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agent_context_sharing/internal/model"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func countedCatalog(t *testing.T, count *atomic.Int64) *toolCatalog {
	t.Helper()
	catalog := mustCatalog(t)
	for i := range catalog.definitions {
		validate := catalog.definitions[i].validate
		catalog.definitions[i].validate = func(raw []byte) (map[string]any, error) {
			count.Add(1)
			return validate(raw)
		}
	}
	return catalog
}

type validationGrades struct{ *readStub }

func (s validationGrades) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	return model.GraphGradeEditor, true, nil
}

// 실제 처리기를 통과하는 재생에서도 검증·권한·행위자 결합을 생략하지 않는다.
func TestToolValidationOnceHTTPAndDirect(t *testing.T) {
	var count, writes, bindings atomic.Int64
	s, principal, _ := readFixture(t)
	s.graph.ID, _ = model.ParseID(catalogID)
	s.node.ID, s.node.GraphID = s.graph.ID, s.graph.ID
	s.hop = func(context.Context, model.ID, []model.Context, int, string, []string, int) (model.HopResult, error) {
		return model.HopResult{Contexts: []model.Context{s.node}, Distances: map[model.ID]int{s.node.ID: 0}}, nil
	}
	registry := agentRegistryStub{
		register: func(context.Context, model.ID, string, string) (model.ID, error) { return s.graph.ID, nil },
		validate: func(context.Context, model.ID, string, model.ID) error { bindings.Add(1); return nil },
	}
	executor := writeExecutorFunc(func(ctx context.Context, _ model.ID, key, tool string, normalized []byte, check func(context.Context) error, _ func(context.Context) ([]byte, error)) ([]byte, error) {
		writes.Add(1)
		if key != "request_key_00001" || bytes.Contains(normalized, []byte("request_id")) {
			t.Error("재생 키/지문 경계")
		}
		if err := check(ctx); err != nil {
			return nil, err
		}
		return []byte(`{"replayed":true}`), nil
	})
	d := Dependencies{Grades: validationGrades{s}, Plans: s, Graphs: s, Nodes: s, Hops: s, Relations: s, Flow: s}
	handlers := newReadHandlers(d, countedCatalog(t, &count))
	runner := &WriteRunner{executor: executor, agents: registry, catalog: countedCatalog(t, &count)}
	maps.Copy(handlers, newWriteHandlers(d, runner))
	handlers["agent_register"] = newAgentHandler(registry, countedCatalog(t, &count))
	catalog := countedCatalog(t, &count)
	sdkHandlers := map[string]sdk.ToolHandler{}
	for name, handler := range handlers {
		sdkHandlers[name] = func(ctx context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return handler(ctx, principal, request)
		}
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "validation-probe", Version: "1"}, &sdk.ServerOptions{SupportedProtocolVersions: []string{protocolRevision}})
	if err := catalog.register(server, sdkHandlers, nil); err != nil {
		t.Fatal(err)
	}
	probe := httptest.NewServer(newSDKTransport(server, sdk.DefaultMaxRequestBodyBytes))
	t.Cleanup(probe.Close)
	for _, definition := range catalog.definitions {
		name := definition.tool.Name
		t.Run(name, func(t *testing.T) {
			args := catalogSample(name)
			switch name {
			case "graph_create", "graph_update", "node_create", "node_update", "node_discard", "node_restore", "relation_confirm", "relation_discard":
				args["request_id"] = "request_key_00001"
			}
			for _, direct := range []bool{false, true} {
				before := count.Load()
				if direct {
					raw, err := json.Marshal(args)
					if err != nil {
						t.Fatal(err)
					}
					result, err := handlers[name](t.Context(), principal, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: name, Arguments: raw}})
					if err != nil || result == nil || result.IsError {
						t.Fatalf("직접 호출: %v %v", result, err)
					}
				} else {
					body := sdkProbeRequest("tools/call")
					params := body["params"].(map[string]any)
					params["name"], params["arguments"] = name, args
					response := exchangeSDKProbe(t, probe, http.MethodPost, body, map[string]string{"Mcp-Name": name})
					result := decodeSDKProbe(t, response)["result"].(map[string]any)
					if result["isError"] == true {
						t.Fatalf("HTTP 호출: %s", response.Body)
					}
				}
				if got := count.Load() - before; got != 1 {
					t.Fatalf("direct=%v 스키마 검증=%d, want 1", direct, got)
				}
			}
			args["unexpected"] = "private-input"
			raw, _ := json.Marshal(args)
			before := count.Load()
			result, err := handlers[name](t.Context(), principal, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: name, Arguments: raw}})
			if err != nil || !result.IsError || count.Load()-before != 1 {
				t.Fatalf("직접 호출 검증 누락: %v %v", result, err)
			}
			body := sdkProbeRequest("tools/call")
			params := body["params"].(map[string]any)
			params["name"], params["arguments"] = name, args
			before = count.Load()
			response := exchangeSDKProbe(t, probe, http.MethodPost, body, map[string]string{"Mcp-Name": name})
			failure := decodeSDKProbe(t, response)["result"].(map[string]any)
			content := failure["structuredContent"].(map[string]any)
			if failure["isError"] != true || content["code"] != "invalid_argument" || content["field"] != "unexpected" || count.Load()-before != 1 || strings.Contains(string(response.Body), "private-input") {
				t.Fatalf("HTTP 검증 경계: %s", response.Body)
			}
		})
	}
	if writes.Load() != 16 || bindings.Load() != 12 {
		t.Fatalf("쓰기/행위자 재검사: %d/%d", writes.Load(), bindings.Load())
	}
}

func TestValidatedArgumentsRequestScope(t *testing.T) {
	for _, mode := range []string{"same", "other_request", "changed_raw", "changed_tool", "no_context"} {
		t.Run(mode, func(t *testing.T) {
			var count atomic.Int64
			catalog := countedCatalog(t, &count)
			request := &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: "graph_list", Arguments: []byte(`{}`)}}
			values, err := catalog.validate("graph_list", request.Params.Arguments)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), validatedToolCallKey{}, validatedToolCall{request: request, tool: "graph_list", raw: string(request.Params.Arguments), values: values})
			switch mode {
			case "other_request":
				request = &sdk.CallToolRequest{Params: request.Params}
			case "changed_raw":
				request.Params.Arguments = []byte(`{"unexpected":true}`)
			case "changed_tool":
				request.Params.Name = "graph_get"
			case "no_context":
				ctx = t.Context()
			}
			_, err = catalog.arguments(ctx, request)
			want := int64(2)
			if mode == "same" {
				want = 1
			}
			if count.Load() != want || (err != nil) != (mode == "changed_raw" || mode == "changed_tool") {
				t.Fatalf("검증 범위 count=%d err=%v", count.Load(), err)
			}
		})
	}
}

func TestValidatedWriteFingerprintAndRequestID(t *testing.T) {
	var count atomic.Int64
	var fingerprints [][]byte
	principal := Principal{AccountID: argumentID(map[string]any{"id": catalogID}, "id"), ClientID: "verified-client"}
	runner := &WriteRunner{catalog: countedCatalog(t, &count), agents: agentRegistryStub{validate: func(context.Context, model.ID, string, model.ID) error { return nil }}}
	runner.executor = writeExecutorFunc(func(ctx context.Context, _ model.ID, key, tool string, raw []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error) {
		if key != "request_key_00001" {
			t.Fatal("요청 키 손실")
		}
		fingerprints = append(fingerprints, bytes.Clone(raw))
		if err := check(ctx); err != nil {
			return nil, err
		}
		return apply(ctx)
	})
	args := eventCatalogSample()
	args["body"] = strings.Repeat("한", 8000)
	refs := make([]string, 1000)
	for i := range refs {
		refs[i] = strings.ToUpper(strings.ReplaceAll(catalogID, "-", ""))
	}
	args["member_refs"], args["request_id"] = refs, "request_key_00001"
	raw, _ := json.Marshal(args)
	check := func(context.Context) error { return nil }
	apply := func(context.Context) ([]byte, error) { return []byte(`{}`), nil }
	if _, err := runner.Execute(t.Context(), principal, "node_create", raw, check, apply); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatal("직접 실행 검증 횟수", count.Load())
	}
	values, err := runner.catalog.validate("node_create", raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runner.executeValidated(t.Context(), principal, "node_create", values, check, func(ctx context.Context) ([]byte, error) {
		if values["request_id"] != "request_key_00001" || values["member_refs"].([]any)[0] != catalogID {
			t.Fatal("업무 인자의 키/UUID 보존")
		}
		return apply(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 || len(fingerprints) != 2 || !bytes.Equal(fingerprints[0], fingerprints[1]) || bytes.Contains(fingerprints[1], []byte("request_id")) {
		t.Fatal("지문/검증 횟수 변경")
	}
}

func TestValidationPanicRemainsInsideToolBoundary(t *testing.T) {
	catalog := mustCatalog(t)
	var attempts atomic.Int64
	for i := range catalog.definitions {
		if catalog.definitions[i].tool.Name != "graph_list" {
			continue
		}
		validate := catalog.definitions[i].validate
		catalog.definitions[i].validate = func(raw []byte) (map[string]any, error) {
			if attempts.Add(1) == 1 {
				panic("private-validator-panic")
			}
			return validate(raw)
		}
	}
	handlers := map[string]sdk.ToolHandler{}
	for _, definition := range catalog.definitions {
		handlers[definition.tool.Name] = func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return writeResult([]byte(`{"ok":true}`))
		}
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "validation-panic", Version: "1"}, &sdk.ServerOptions{SupportedProtocolVersions: []string{protocolRevision}})
	if err := catalog.register(server, handlers, nil); err != nil {
		t.Fatal(err)
	}
	probe := httptest.NewServer(newSDKTransport(server, sdk.DefaultMaxRequestBodyBytes))
	t.Cleanup(probe.Close)
	body := sdkProbeRequest("tools/call")
	params := body["params"].(map[string]any)
	params["name"], params["arguments"] = "graph_list", map[string]any{}
	for attempt := range 2 {
		response := exchangeSDKProbe(t, probe, http.MethodPost, body, map[string]string{"Mcp-Name": "graph_list"})
		result := decodeSDKProbe(t, response)["result"].(map[string]any)
		content := result["structuredContent"].(map[string]any)
		if (attempt == 0 && (result["isError"] != true || content["code"] != "internal")) || (attempt == 1 && content["ok"] != true) || strings.Contains(string(response.Body), "private-validator") {
			t.Fatalf("검증 panic 격리: %s", response.Body)
		}
	}
}
