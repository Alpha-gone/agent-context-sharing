package mcp

import (
	"bytes"
	"encoding/json/v2"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateToolManifest = flag.Bool("update-tool-manifest", false, "서버 공개 도구 정의로 클라이언트 계약 스냅샷을 갱신")

func TestToolManifestMatchesClient(t *testing.T) {
	actual, err := json.Marshal(toolDefinitions(), json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	actual = append(actual, '\n')
	path := filepath.Join("..", "..", "client", "internal", "client", "contract", "tool_manifest.json")
	if *updateToolManifest {
		if err := os.WriteFile(path, actual, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, want) {
		t.Fatal("서버 공개 도구 정의와 클라이언트 계약 스냅샷이 다릅니다. 변경을 검토한 뒤 -update-tool-manifest로 갱신하세요")
	}
}
