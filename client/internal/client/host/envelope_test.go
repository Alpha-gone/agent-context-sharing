package host

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"testing"

	"agent_context_sharing/client/internal/client/contract"
)

func TestHostEnvelopeOnlyReplacesServerInfo(t *testing.T) {
	raw := jsontext.Value(`{"resultType":"com\u0070lete","_meta":{"extension":{"z":9007199254740993,"a":"\u003c"},"io.modelcontextprotocol/serverInfo":{"name":"remote","version":"old"},"after":null},"structuredContent":{"contexts":["b","a"],"channels":["z","a"]},"content":[],"isError":false}`)
	got, err := annotateResult(raw, "local-build")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(raw, []byte(`{"name":"remote","version":"old"}`), []byte(`{"name":"agent-context-client","version":"local-build"}`), 1)
	if !bytes.Equal(got, want) {
		t.Fatalf("외피 외 필드 손상: %s", got)
	}
	if bytes.Contains(raw, []byte("local-build")) {
		t.Fatal("원격 결과 원문 수정")
	}
	for _, invalid := range []string{`{"_meta":null}`, `{"_meta":[]}`, `{"resultType":"input_required"}`, `{"resultType":null}`, `{"_meta":{"duplicate":1,"duplicate":2}}`} {
		if _, err := annotateResult(jsontext.Value(invalid), "local-build"); !errors.Is(err, contract.ErrProtocol) {
			t.Fatal("잘못된 MCP 결과 외피 수락")
		}
	}
}
