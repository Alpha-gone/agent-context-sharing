package host

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"agent_context_sharing/client/internal/client/contract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func validInitialize(params []byte) bool {
	var input struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    jsontext.Value `json:"capabilities"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	return json.Unmarshal(params, &input) == nil && input.ProtocolVersion != "" &&
		input.Capabilities.Kind() == '{' && input.ClientInfo.Name != "" && input.ClientInfo.Version != ""
}

func (c *stdioConn) legacyRequest(params []byte) bool {
	if !c.legacyReady.Load() {
		return false
	}
	if len(params) == 0 {
		return true
	}
	var input struct {
		Meta map[string]jsontext.Value `json:"_meta"`
	}
	if json.Unmarshal(params, &input) != nil {
		return false
	}
	_, explicit := input.Meta[mcp.MetaKeyProtocolVersion]
	return !explicit
}

// legacyResult는 무상태 프로토콜의 외피만 제거하여 도구 본문의 표현을 보존한다.
func legacyResult(raw jsontext.Value) (jsontext.Value, error) {
	var err error
	for _, name := range []string{"resultType", "ttlMs", "cacheScope"} {
		raw, err = removeMember(raw, name)
		if err != nil {
			return nil, err
		}
	}
	meta, _, _, found, err := member(raw, "_meta")
	if err != nil || !found {
		return raw, err
	}
	meta, err = removeMember(meta, mcp.MetaKeyServerInfo)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(meta, []byte(`{}`)) {
		return removeMember(raw, "_meta")
	}
	return setMember(raw, "_meta", meta)
}

// removeMember는 공백을 정리한 객체의 지정 필드만 잘라 나머지 원문을 유지한다.
func removeMember(raw jsontext.Value, name string) (jsontext.Value, error) {
	if raw.Kind() != '{' {
		return nil, contract.ErrProtocol
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.ReadToken(); err != nil {
		return nil, contract.ErrProtocol
	}
	for decoder.PeekKind() != '}' {
		start := int(decoder.InputOffset())
		key, err := decoder.ReadToken()
		if err != nil {
			return nil, contract.ErrProtocol
		}
		matches := key.String() == name
		if _, err := decoder.ReadValue(); err != nil {
			return nil, contract.ErrProtocol
		}
		if !matches {
			continue
		}
		end := int(decoder.InputOffset())
		if start == 1 && end < len(raw) && raw[end] == ',' {
			end++
		}
		result := append(bytes.Clone(raw[:start]), raw[end:]...)
		return result, nil
	}
	return raw, nil
}
