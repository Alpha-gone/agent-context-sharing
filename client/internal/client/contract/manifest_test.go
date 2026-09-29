package contract

import (
	"bytes"
	"encoding/json/v2"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestManifestHasExpectedTools(t *testing.T) {
	var tools []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	if err := json.Unmarshal(Manifest(), &tools); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"graph_list", "graph_create", "graph_get", "graph_update",
		"node_create", "node_get", "node_update", "node_discard", "node_restore",
		"context_flow_get", "relation_list", "relation_confirm", "relation_discard",
	}
	if len(tools) != len(want) {
		t.Fatalf("도구 수 = %d, 기대값 %d", len(tools), len(want))
	}
	for i, tool := range tools {
		if tool.Name != want[i] || tool.Description == "" || tool.InputSchema["type"] != "object" {
			t.Fatalf("도구 %d의 계약이 유효하지 않습니다: %q", i, tool.Name)
		}
	}
	copyOfManifest := Manifest()
	copyOfManifest[0] = ' '
	if bytes.Equal(copyOfManifest, Manifest()) {
		t.Fatal("Manifest가 내부 스냅샷을 직접 노출합니다")
	}
}

func TestSDKSupportsHostProtocol(t *testing.T) {
	const revision = "2026-07-28"
	if !slices.Contains(mcp.SupportedProtocolVersions(), revision) {
		t.Fatalf("MCP Go SDK가 %s를 지원하지 않습니다", revision)
	}
	_ = mcp.ServerOptions{SupportedProtocolVersions: []string{revision}}
	_ = mcp.StdioTransport{MaxLineLength: 256 << 10}
	_ = mcp.DiscoverResult{}
	_ = mcp.ListToolsResult{}
	_ = mcp.Tool{}
}
