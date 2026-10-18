package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"agent_context_sharing/internal/model"
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const catalogID = "0199aabb-ccdd-7000-8000-000000000001"

func catalogSample(name string) map[string]any {
	graph := map[string]any{"graph_id": catalogID}
	switch name {
	case "agent_register":
		return map[string]any{"registration_key": "registration_key_1"}
	case "graph_list":
		return map[string]any{}
	case "graph_create":
		return map[string]any{"name": "공유 그래프"}
	case "graph_get":
		return graph
	case "graph_update":
		graph["expected_version"], graph["name"] = 1, "새 이름"
	case "node_create":
		graph["layer"], graph["body"], graph["agent_id"] = "source", "원천 본문", catalogID
		graph["source_ref"] = map[string]any{"channel": "conversation", "locator": "chat:turn-1"}
		graph["origin_kind"], graph["occurred_at"] = "user_utterance", "2026-10-18T00:00:00Z"
	case "node_get", "relation_list":
		graph["context_id"] = catalogID
	case "node_update":
		graph["context_id"], graph["agent_id"], graph["expected_version"], graph["body"] = catalogID, catalogID, 1, "새 본문"
	case "node_discard", "node_restore":
		graph["context_id"], graph["agent_id"] = catalogID, catalogID
	case "context_flow_get":
		graph["work_context"] = "현재 작업"
	case "relation_confirm":
		graph["relation_type"], graph["from_context_id"], graph["to_context_id"], graph["agent_id"] = "precedes", catalogID, catalogID, catalogID
	case "relation_discard":
		graph["relation_id"], graph["agent_id"] = catalogID, catalogID
	default:
		panic(name)
	}
	return graph
}

func derivedCatalogSample() map[string]any {
	return map[string]any{"graph_id": catalogID, "agent_id": catalogID, "layer": "derived", "body": "파생 본문", "derived_from": []string{catalogID}, "derivation_kind": "proposition", "evidence_state": "observation"}
}

func eventCatalogSample() map[string]any {
	return map[string]any{"graph_id": catalogID, "agent_id": catalogID, "layer": "event", "body": "사건 본문", "member_refs": []string{catalogID}, "event_start": "2026-10-18T00:00:00Z"}
}

func mustCatalog(t *testing.T) *toolCatalog {
	t.Helper()
	catalog, err := newToolCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func validateCatalogSample(t *testing.T, catalog *toolCatalog, tool string, sample map[string]any, wantField string) map[string]any {
	t.Helper()
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	value, err := catalog.validate(tool, data)
	if wantField == "" {
		if err != nil {
			t.Fatalf("%s 유효 입력 거부: %v", tool, err)
		}
		return value
	}
	failure, ok := errors.AsType[model.FieldError](err)
	if !ok || failure.Field != wantField {
		t.Fatalf("%s 기대 필드 %q, 실제 %v", tool, wantField, err)
	}
	return nil
}

func TestCatalogToolSetAndAnnotations(t *testing.T) {
	catalog := mustCatalog(t)
	names := []string{"agent_register", "graph_list", "graph_create", "graph_get", "graph_update", "node_create", "node_get", "node_update", "node_discard", "node_restore", "context_flow_get", "relation_list", "relation_confirm", "relation_discard"}
	reads := []string{"graph_list", "graph_get", "node_get", "context_flow_get", "relation_list"}
	idempotent := append(slices.Clone(reads), "agent_register", "relation_confirm")
	if len(catalog.definitions) != len(names) {
		t.Fatal("14종 도구가 아니다")
	}
	for index, definition := range catalog.definitions {
		t.Run(names[index], func(t *testing.T) {
			tool := definition.tool
			if tool.Name != names[index] || tool.Description == "" || tool.Annotations == nil {
				t.Fatalf("도구 계약: %+v", tool)
			}
			a := tool.Annotations
			if a.ReadOnlyHint != slices.Contains(reads, tool.Name) || a.IdempotentHint != slices.Contains(idempotent, tool.Name) || a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Fatalf("annotations: %+v", a)
			}
			if a.DestructiveHint == nil || *a.DestructiveHint != (!a.ReadOnlyHint && tool.Name != "agent_register" && tool.Name != "graph_create") {
				t.Fatalf("변경 특성: %+v", a)
			}
			encoded, err := json.Marshal(tool)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if json.Unmarshal(encoded, &wire) != nil || wire["inputSchema"].(map[string]any)["additionalProperties"] != false {
				t.Fatalf("닫힌 스키마: %s", encoded)
			}
			if strings.Contains(string(encoded), "x-mcp-header") {
				t.Fatal("서비스 전용 필수 헤더가 있다")
			}
			for field, property := range tool.InputSchema.(*jsonschema.Schema).Properties {
				if property.Description == "" {
					t.Fatalf("%s 설명 누락", field)
				}
			}
			validateCatalogSample(t, catalog, tool.Name, catalogSample(tool.Name), "")
			sample := catalogSample(tool.Name)
			sample["unexpected"] = true
			validateCatalogSample(t, catalog, tool.Name, sample, "unexpected")
		})
	}
	for _, required := range []string{"agent_register", "graph_list", "expected_version", "request_id", "24시간", "웹", "annotations"} {
		if !strings.Contains(serverInstructions, required) {
			t.Fatalf("instructions 안내 누락: %s", required)
		}
	}
}

func TestCatalogRequiredFieldsAndTypes(t *testing.T) {
	catalog := mustCatalog(t)
	for _, definition := range catalog.definitions {
		tool := definition.tool.Name
		schema := definition.tool.InputSchema.(*jsonschema.Schema)
		for _, field := range schema.Required {
			t.Run(tool+"/"+field, func(t *testing.T) {
				sample := catalogSample(tool)
				delete(sample, field)
				validateCatalogSample(t, catalog, tool, sample, field)
				sample = catalogSample(tool)
				sample[field] = nil
				validateCatalogSample(t, catalog, tool, sample, field)
			})
		}
	}
	for _, raw := range []string{`null`, `[]`, `"text"`, `{"name":"a","name":"b"}`, `{}`} {
		if _, err := catalog.validate("graph_create", []byte(raw)); err == nil {
			t.Fatalf("유효하지 않은 입력 수락: %s", raw)
		}
	}
	validateCatalogSample(t, catalog, "graph_delete", map[string]any{}, "name")
}

func TestCatalogTextAndKeyBoundaries(t *testing.T) {
	catalog := mustCatalog(t)
	for _, sample := range []struct {
		tool, field string
		low, high   int
	}{
		{"graph_create", "name", 1, 200}, {"graph_create", "description", 0, 2000},
		{"node_create", "body", 1, 8000}, {"context_flow_get", "work_context", 1, 8000},
		{"node_create", "judgment_input", 1, 8000},
	} {
		t.Run(sample.tool+"/"+sample.field, func(t *testing.T) {
			for _, length := range []int{0, sample.low, sample.high, sample.high + 1} {
				input := catalogSample(sample.tool)
				input[sample.field] = strings.Repeat("가", length)
				field := ""
				if length < sample.low || length > sample.high {
					field = sample.field
				}
				validateCatalogSample(t, catalog, sample.tool, input, field)
			}
			input := catalogSample(sample.tool)
			input[sample.field] = "a\x00b"
			validateCatalogSample(t, catalog, sample.tool, input, sample.field)
		})
	}
	for _, tool := range []string{"graph_create", "context_flow_get"} {
		field := "name"
		if tool == "context_flow_get" {
			field = "work_context"
		}
		input := catalogSample(tool)
		input[field] = " \t\n\u3000\u00a0"
		validateCatalogSample(t, catalog, tool, input, field)
	}
	for _, sample := range []struct{ tool, field string }{{"agent_register", "registration_key"}, {"graph_create", "request_id"}} {
		t.Run(sample.field, func(t *testing.T) {
			for _, key := range []string{strings.Repeat("x", 15), strings.Repeat("a", 16), strings.Repeat("Z_9-", 32), strings.Repeat("a", 129), "registration.key_1", "키를포함한_등록키_123456"} {
				input := catalogSample(sample.tool)
				input[sample.field] = key
				want := sample.field
				if len(key) == 16 || len(key) == 128 {
					want = ""
				}
				validateCatalogSample(t, catalog, sample.tool, input, want)
			}
		})
	}
}

func TestCatalogLayerConditionsAndMutableFields(t *testing.T) {
	catalog := mustCatalog(t)
	validateCatalogSample(t, catalog, "node_create", derivedCatalogSample(), "")
	validateCatalogSample(t, catalog, "node_create", eventCatalogSample(), "")
	for _, sample := range []struct {
		name  string
		input map[string]any
		field string
	}{
		{"source-missing-reference", catalogSample("node_create"), "source_ref"},
		{"derived-missing-reference", derivedCatalogSample(), "derived_from"},
		{"event-missing-start", eventCatalogSample(), "event_start"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			delete(sample.input, sample.field)
			validateCatalogSample(t, catalog, "node_create", sample.input, sample.field)
		})
	}
	summary := derivedCatalogSample()
	summary["derivation_kind"] = "summary"
	validateCatalogSample(t, catalog, "node_create", summary, "summary_scope")
	summary["summary_scope"] = "global"
	validateCatalogSample(t, catalog, "node_create", summary, "")
	opinion := derivedCatalogSample()
	opinion["evidence_state"] = "opinion"
	validateCatalogSample(t, catalog, "node_create", opinion, "confidence_state")
	opinion["confidence_state"] = "disputed"
	validateCatalogSample(t, catalog, "node_create", opinion, "")
	for _, field := range []string{"source_ref", "member_refs", "supersedes_context_id", "summary_scope", "confidence_state"} {
		t.Run("wrong-layer/"+field, func(t *testing.T) {
			input := derivedCatalogSample()
			switch field {
			case "source_ref":
				input[field] = catalogSample("node_create")[field]
			case "member_refs":
				input[field] = []string{catalogID}
			case "summary_scope":
				input[field] = "global"
			case "confidence_state":
				input[field] = "supported"
			case "supersedes_context_id":
				input = eventCatalogSample()
				input[field] = catalogID
			}
			validateCatalogSample(t, catalog, "node_create", input, "layer")
		})
	}
	for _, field := range []string{"layer", "evidence_state", "derivation_kind", "derived_from", "summary_scope", "source_ref", "supersedes_context_id", "evidence_invalidated"} {
		input := catalogSample("node_update")
		input[field] = "immutable"
		validateCatalogSample(t, catalog, "node_update", input, field)
	}
	keep := catalogSample("node_update")
	keep["management_action"] = "keep"
	validateCatalogSample(t, catalog, "node_update", keep, "management_action")
	delete(keep, "body")
	validateCatalogSample(t, catalog, "node_update", keep, "")
	keep["valid_to"] = nil
	validateCatalogSample(t, catalog, "node_update", keep, "management_action")
	delete(keep, "management_action")
	validateCatalogSample(t, catalog, "node_update", keep, "")
}

func TestCatalogArrayBounds(t *testing.T) {
	catalog := mustCatalog(t)
	for _, sample := range []struct {
		field  string
		max    int
		sample func() map[string]any
	}{
		{"derived_from", 100, derivedCatalogSample}, {"member_refs", 1000, eventCatalogSample},
	} {
		t.Run(sample.field, func(t *testing.T) {
			for _, size := range []int{0, 1, sample.max, sample.max + 1} {
				input := sample.sample()
				refs := make([]string, size)
				for index := range refs {
					refs[index] = catalogID
				}
				input[sample.field] = refs
				field := ""
				if size == 0 || size > sample.max {
					field = sample.field
				}
				validateCatalogSample(t, catalog, "node_create", input, field)
			}
		})
	}
	for _, sample := range []struct {
		tool, field, value string
		max                int
	}{
		{"graph_list", "grade_filter", "owner", 3}, {"node_get", "traversal_filter", "member_refs", 7},
		{"relation_list", "state_filter", "confirmed", 3}, {"relation_list", "type_filter", "causes", 4},
	} {
		t.Run(sample.field, func(t *testing.T) {
			for _, size := range []int{0, sample.max, sample.max + 1} {
				input := catalogSample(sample.tool)
				values := make([]string, size)
				for i := range values {
					values[i] = sample.value
				}
				input[sample.field] = values
				field := ""
				if size > sample.max {
					field = sample.field
				}
				validateCatalogSample(t, catalog, sample.tool, input, field)
			}
			input := catalogSample(sample.tool)
			input[sample.field] = []string{"invalid"}
			validateCatalogSample(t, catalog, sample.tool, input, sample.field)
		})
	}
}

func TestCatalogFormatsAndIntegers(t *testing.T) {
	catalog := mustCatalog(t)
	for _, id := range []string{catalogID, strings.ToUpper(strings.ReplaceAll(catalogID, "-", "")), "0199aabb-ccdd-4000-8000-000000000001", "not-an-id"} {
		input := catalogSample("graph_get")
		input["graph_id"] = id
		field := "graph_id"
		if id == catalogID || len(id) == 32 {
			field = ""
		}
		validateCatalogSample(t, catalog, "graph_get", input, field)
	}
	for _, timestamp := range []string{"2026-10-18T00:00:00Z", "2026-10-18T00:00:00.123+00:00", "2026-10-18T09:00:00+09:00", "2026-02-30T00:00:00Z", "0001-01-01T00:00:00Z", "2026-10-18T00:00:00-00:00"} {
		input := catalogSample("context_flow_get")
		input["as_of"] = timestamp
		want := "as_of"
		if timestamp == "2026-10-18T00:00:00Z" || timestamp == "2026-10-18T00:00:00.123+00:00" {
			want = ""
		}
		validateCatalogSample(t, catalog, "context_flow_get", input, want)
	}
	for _, locator := range []string{"chat:turn-1", "relative/path", "https://example.test/%zz", "other:" + strings.Repeat("가", 1018), "other:" + strings.Repeat("가", 1019)} {
		input := catalogSample("node_create")
		input["source_ref"].(map[string]any)["locator"] = locator
		want := "source_ref"
		if locator == "chat:turn-1" || strings.HasPrefix(locator, "other:") && len([]rune(locator)) == 1024 {
			want = ""
		}
		if strings.Contains(locator, "%zz") {
			want = "source_ref.locator"
		}
		validateCatalogSample(t, catalog, "node_create", input, want)
	}
	for _, cursor := range []string{"", base64.RawURLEncoding.EncodeToString([]byte("opaque")), "bad=", "a"} {
		input := catalogSample("graph_list")
		input["cursor"] = cursor
		want := ""
		if cursor == "bad=" || cursor == "a" {
			want = "cursor"
		}
		validateCatalogSample(t, catalog, "graph_list", input, want)
	}
	for _, sample := range []struct {
		tool, field string
		values      []any
	}{
		{"graph_update", "expected_version", []any{0, -1, 1.5, "1"}}, {"graph_list", "page_size", []any{0, -1, 1.5}},
		{"node_get", "hops", []any{-1, 1.5}}, {"context_flow_get", "budget", []any{0, -1, 1.5}},
	} {
		for _, value := range sample.values {
			input := catalogSample(sample.tool)
			input[sample.field] = value
			validateCatalogSample(t, catalog, sample.tool, input, sample.field)
		}
	}
	value, err := catalog.validate("graph_update", []byte(`{"graph_id":"`+catalogID+`","expected_version":9007199254740993}`))
	if err != nil || value["expected_version"] != int64(9007199254740993) {
		t.Fatalf("기대 판 정밀도 손실: %+v %v", value, err)
	}
}

func TestCatalogSDKRegistrationAndValidationBeforeHandler(t *testing.T) {
	catalog := mustCatalog(t)
	server := sdk.NewServer(&sdk.Implementation{Name: "catalog-probe", Version: "1"}, &sdk.ServerOptions{
		SupportedProtocolVersions: []string{protocolRevision}, Instructions: serverInstructions,
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
		SetCacheable: func(_ context.Context, _ sdk.Request, cache *sdk.Cacheable) {
			cache.TTLMs, cache.CacheScope = 0, "private"
		},
	})
	var calls atomic.Int64
	handlers := map[string]sdk.ToolHandler{}
	for _, definition := range catalog.definitions {
		name := definition.tool.Name
		handlers[name] = func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name}}, StructuredContent: map[string]any{"tool": name}}, nil
		}
	}
	if err := catalog.register(server, handlers); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", newSDKTransport(server, sdk.DefaultMaxRequestBodyBytes))
	probe := httptest.NewServer(mux)
	t.Cleanup(probe.Close)
	response := exchangeSDKProbe(t, probe, http.MethodPost, sdkProbeRequest("tools/list"), nil)
	list := decodeSDKProbe(t, response)["result"].(map[string]any)
	tools := list["tools"].([]any)
	if len(tools) != 14 || list["ttlMs"] != float64(0) || list["cacheScope"] != "private" {
		t.Fatalf("실제 SDK 목록: %s", response.Body)
	}
	seen := map[string]bool{}
	listed := map[string]any{}
	for _, raw := range tools {
		name := raw.(map[string]any)["name"].(string)
		seen[name], listed[name] = true, raw
	}
	for _, definition := range catalog.definitions {
		name := definition.tool.Name
		if !seen[name] {
			t.Fatalf("도구 누락: %s", name)
		}
		encoded, err := json.Marshal(definition.tool)
		if err != nil {
			t.Fatal(err)
		}
		var expected map[string]any
		if json.Unmarshal(encoded, &expected) != nil || !reflect.DeepEqual(listed[name], expected) {
			t.Fatalf("실제 SDK 도구 정의 불일치: %s", name)
		}
		body := sdkProbeRequest("tools/call")
		params := body["params"].(map[string]any)
		params["name"], params["arguments"] = name, catalogSample(name)
		response := exchangeSDKProbe(t, probe, http.MethodPost, body, map[string]string{"Mcp-Name": name})
		result := decodeSDKProbe(t, response)["result"].(map[string]any)
		if result["isError"] == true || !reflect.DeepEqual(result["structuredContent"], map[string]any{"tool": name}) {
			t.Fatalf("실제 호출 %s: %s", name, response.Body)
		}
		params["arguments"].(map[string]any)["unexpected"] = "sensitive-input-must-not-leak"
		before := calls.Load()
		response = exchangeSDKProbe(t, probe, http.MethodPost, body, map[string]string{"Mcp-Name": name})
		result = decodeSDKProbe(t, response)["result"].(map[string]any)
		failure := result["structuredContent"].(map[string]any)
		if result["isError"] != true || result["resultType"] != "complete" || failure["code"] != "invalid_argument" || failure["field"] != "unexpected" || calls.Load() != before || strings.Contains(string(response.Body), "sensitive-input") {
			t.Fatalf("입력 거부 %s: %s", name, response.Body)
		}
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		var textFailure map[string]any
		if json.Unmarshal([]byte(text), &textFailure) != nil || !reflect.DeepEqual(textFailure, failure) {
			t.Fatalf("실패의 text·구조화 결과 불일치: %s", response.Body)
		}
	}
	if calls.Load() != 14 {
		t.Fatalf("업무 처리기 호출 수: %d", calls.Load())
	}
	discovery := exchangeSDKProbe(t, probe, http.MethodPost, sdkProbeRequest("server/discover"), nil)
	if decodeSDKProbe(t, discovery)["result"].(map[string]any)["instructions"] != serverInstructions {
		t.Fatal("공통 instructions 누락")
	}
}

func TestCatalogRegistrationRequiresEveryHandler(t *testing.T) {
	catalog := mustCatalog(t)
	server := sdk.NewServer(&sdk.Implementation{Name: "missing-handlers", Version: "1"}, nil)
	if err := catalog.register(server, map[string]sdk.ToolHandler{}); err == nil {
		t.Fatal("미구현 도구를 등록했다")
	}
	handlers := map[string]sdk.ToolHandler{}
	for _, definition := range catalog.definitions {
		handlers[definition.tool.Name] = nil
	}
	if err := catalog.register(server, handlers); err == nil {
		t.Fatal("nil 처리기를 등록했다")
	}
}

func TestCatalogExactJSONIntegers(t *testing.T) {
	catalog := mustCatalog(t)
	for _, sample := range []struct {
		text  string
		want  int64
		valid bool
	}{
		{"1", 1, true}, {"1.0", 1, true}, {"1e3", 1000, true}, {"1000E-3", 1, true},
		{"9007199254740993", 9007199254740993, true}, {"9223372036854775807", 9223372036854775807, true},
		{"9223372036854775808", 0, false}, {"1.00000000000000001", 0, false},
		{"1e999999999999999999999999", 0, false}, {"1e-99999999999999999999999", 0, false},
		{"1000000000000000000000000000000e-30", 1, true},
	} {
		t.Run(sample.text, func(t *testing.T) {
			raw := []byte(`{"graph_id":"` + catalogID + `","expected_version":` + sample.text + `}`)
			value, err := catalog.validate("graph_update", raw)
			if sample.valid && (err != nil || value["expected_version"] != sample.want) || !sample.valid && err == nil {
				t.Fatalf("정수 입력 %s: %+v %v", sample.text, value, err)
			}
		})
	}
}
