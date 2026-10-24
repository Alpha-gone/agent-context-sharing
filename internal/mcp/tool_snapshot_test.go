package mcp

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"testing"
)

// OPS-08의 배포 목록이다. 실제 호스트·도구 결과 인수는 MCP-09에서 별도로 수행한다.
func TestOPS08ToolSnapshot(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"protocolVersion": protocolRevision,
		"instructions":    serverInstructions,
		"tools":           toolDefinitions(),
	}, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	const path = "testdata/tools.json"
	if os.Getenv("UPDATE_TOOL_SNAPSHOT") == "1" {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatal("배포 도구 목록이 스냅샷과 다르다. 변경 계약을 검토한 뒤 UPDATE_TOOL_SNAPSHOT=1로 갱신한다")
	}
}
