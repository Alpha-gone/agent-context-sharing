package mcp

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPublicToolProjection(t *testing.T) {
	for _, original := range toolDefinitions() {
		before, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		published, err := publicTool(original)
		if err != nil {
			t.Fatal(err)
		}
		after, err := json.Marshal(original)
		if err != nil || string(before) != string(after) {
			t.Fatal("projection changed internal definition")
		}
		actual, err := json.Marshal(published)
		if err != nil {
			t.Fatal(err)
		}
		var want, got map[string]any
		if json.Unmarshal(before, &want) != nil || json.Unmarshal(actual, &got) != nil {
			t.Fatal("tool JSON")
		}
		var removePatterns func(any)
		removePatterns = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if _, keyword := x["pattern"].(string); keyword {
					delete(x, "pattern")
				}
				for key, child := range x {
					switch key {
					case "const", "enum", "examples", "default":
						continue
					}
					removePatterns(child)
				}
			case []any:
				for _, child := range x {
					removePatterns(child)
				}
			}
		}
		removePatterns(want["inputSchema"])
		delete(want["inputSchema"].(map[string]any), "allOf")
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: changed more than pattern and root allOf", original.Name)
		}
		schema := published.InputSchema.(map[string]any)
		if required, ok := schema["required"].([]any); ok && len(required) > 0 {
			required[0] = "corrupted"
		}
		published.Annotations.ReadOnlyHint = !published.Annotations.ReadOnlyHint
		schema["properties"].(map[string]any)["new-field"] = map[string]any{"type": "string"}
		final, _ := json.Marshal(original)
		if string(before) != string(final) {
			t.Fatal("public copy aliases internal definition")
		}
	}
	// Values held by schema annotations/constraints are data, not nested schemas.
	literal := map[string]any{"pattern": "keep-this-value"}
	original := &sdk.Tool{Name: "literal", InputSchema: &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"pattern": {Type: "string", Pattern: "secret-regex", Const: new(any(literal)), Enum: []any{literal}, Examples: []any{literal}}}}}
	published, err := publicTool(original)
	if err != nil {
		t.Fatal(err)
	}
	prop := published.InputSchema.(map[string]any)["properties"].(map[string]any)["pattern"].(map[string]any)
	if _, exists := prop["pattern"]; exists {
		t.Fatal("nested schema retained pattern")
	}
	if prop["const"].(map[string]any)["pattern"] != "keep-this-value" || prop["enum"].([]any)[0].(map[string]any)["pattern"] != "keep-this-value" || prop["examples"].([]any)[0].(map[string]any)["pattern"] != "keep-this-value" {
		t.Fatal("projection rewrote literal data")
	}
}

func TestPublicToolMetadataBudget(t *testing.T) {
	tools, err := mustCatalog(t).publicTools()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(tools, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) > 17500 {
		t.Fatalf("tools/list metadata bytes=%d, budget=17500", len(wire))
	}
	if len(serverInstructions) > 1500 {
		t.Fatalf("instructions bytes=%d, budget=1500", len(serverInstructions))
	}
	for _, tool := range tools {
		value, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if len(value) > 5000 {
			t.Fatalf("%s bytes=%d, budget=5000", tool.Name, len(value))
		}
	}
	t.Logf("tools=%d bytes=%d instructions=%d", len(tools), len(wire), len(serverInstructions))
}

func TestPublicRootSchemaCompatibility(t *testing.T) {
	for _, original := range toolDefinitions() {
		published, err := publicTool(original)
		if err != nil {
			t.Fatal(err)
		}
		schema := published.InputSchema.(map[string]any)
		for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
			if _, exists := schema[keyword]; exists {
				t.Errorf("%s: unsupported root keyword %s", original.Name, keyword)
			}
		}
		strict := original.InputSchema.(*jsonschema.Schema)
		if (original.Name == "node_create" || original.Name == "node_update") && len(strict.AllOf) == 0 {
			t.Errorf("%s lost strict conditional validation", original.Name)
		}
	}
}

func TestPublicSchemaKeepsStrictHTTPValidation(t *testing.T) {
	catalog := mustCatalog(t)
	server := sdk.NewServer(&sdk.Implementation{Name: "public-schema", Version: "1"}, &sdk.ServerOptions{SupportedProtocolVersions: []string{protocolRevision}})
	var calls atomic.Int64
	handlers := map[string]sdk.ToolHandler{}
	for _, definition := range catalog.definitions {
		handlers[definition.tool.Name] = func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
		}
	}
	if err := catalog.register(server, handlers, nil); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", newSDKTransport(server, sdk.DefaultMaxRequestBodyBytes))
	probe := httptest.NewServer(mux)
	t.Cleanup(probe.Close)
	for _, sample := range []struct {
		tool, field string
		value       any
		errorField  string
	}{
		{"graph_get", "graph_id", "not-a-uuid", ""},
		{"graph_create", "name", "\u2003\t", ""},
		{"node_create", "body", "private-input\u0000", ""},
		{"agent_register", "registration_key", "private-input invalid key", ""},
		{"node_create", "occurred_at", "2026-10-18T01:00:00+01:00", ""},
		{"node_create", "member_refs", []string{catalogID}, "layer"},
		{"node_update", "management_action", "keep", "management_action"},
	} {
		args := catalogSample(sample.tool)
		args[sample.field] = sample.value
		body := sdkProbeRequest("tools/call")
		params := body["params"].(map[string]any)
		params["name"], params["arguments"] = sample.tool, args
		before := calls.Load()
		response := exchangeSDKProbe(t, probe, http.MethodPost, body, map[string]string{"Mcp-Name": sample.tool})
		result := decodeSDKProbe(t, response)["result"].(map[string]any)
		failure := result["structuredContent"].(map[string]any)
		if result["isError"] != true || failure["code"] != "invalid_argument" || failure["field"] != cmp.Or(sample.errorField, sample.field) || calls.Load() != before || strings.Contains(string(response.Body), "private-input") {
			t.Fatalf("%s.%s validation changed: %s", sample.tool, sample.field, response.Body)
		}
	}
}
