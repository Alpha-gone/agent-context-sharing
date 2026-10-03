package host

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"agent_context_sharing/client/internal/client/contract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// annotateResult는 MCP 외피만 수정하고 도구 본문을 재직렬화하지 않는다.
func annotateResult(raw jsontext.Value, version string) (jsontext.Value, error) {
	if len(raw) > maxOutputBytes {
		return nil, contract.ErrProtocol
	}
	info, err := json.Marshal(mcp.Implementation{Name: "agent-context-client", Version: version})
	if err != nil {
		return nil, contract.ErrProtocol
	}
	meta, _, _, found, err := member(raw, "_meta")
	if err != nil {
		return nil, err
	}
	if !found {
		meta = jsontext.Value(`{}`)
	}
	meta, err = setMember(meta, mcp.MetaKeyServerInfo, info)
	if err != nil {
		return nil, err
	}
	raw, err = setMember(raw, "_meta", meta)
	if err != nil {
		return nil, err
	}
	resultType, _, _, found, err := member(raw, "resultType")
	var kind string
	if err != nil || found && (json.Unmarshal(resultType, &kind) != nil || kind != "complete") {
		return nil, contract.ErrProtocol
	}
	if !found {
		return setMember(raw, "resultType", jsontext.Value(`"complete"`))
	}
	return raw, nil
}

// member는 최상위 값의 원문과 byte 범위를 찾아 다른 필드의 표현을 보존한다.
func member(raw jsontext.Value, name string) (value jsontext.Value, start, end int, found bool, err error) {
	if raw.Kind() != '{' {
		return nil, 0, 0, false, contract.ErrProtocol
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.ReadToken(); err != nil {
		return nil, 0, 0, false, contract.ErrProtocol
	}
	for decoder.PeekKind() != '}' {
		key, err := decoder.ReadToken()
		if err != nil {
			return nil, 0, 0, false, contract.ErrProtocol
		}
		keyName := key.String()
		value, err := decoder.ReadValue()
		if err != nil {
			return nil, 0, 0, false, contract.ErrProtocol
		}
		if keyName == name {
			end := int(decoder.InputOffset())
			return bytes.Clone(value), end - len(value), end, true, nil
		}
	}
	return nil, 0, 0, false, nil
}

func setMember(raw jsontext.Value, name string, value jsontext.Value) (jsontext.Value, error) {
	_, start, end, found, err := member(raw, name)
	if err != nil {
		return nil, err
	}
	if found {
		if bytes.Equal(raw[start:end], value) {
			return raw, nil
		}
		result := append(bytes.Clone(raw[:start]), value...)
		return append(result, raw[end:]...), nil
	}
	// 호출자는 공백을 제거한 객체를 제공한다.
	end = len(raw) - 1
	result := bytes.Clone(raw[:end])
	if len(result) > 1 {
		result = append(result, ',')
	}
	key, err := json.Marshal(name)
	if err != nil {
		return nil, contract.ErrProtocol
	}
	result = append(result, key...)
	result = append(result, ':')
	result = append(result, value...)
	return append(result, '}'), nil
}
