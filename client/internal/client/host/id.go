package host

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"strings"

	"agent_context_sharing/client/internal/client/contract"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

const (
	numberIDPrefix = "\x00number:"
	stringIDPrefix = "\x00string:"
)

// decodeHostMessage는 SDK의 float64 ID 해석을 거치지 않고 호스트 요청을 구성한다.
func decodeHostMessage(frame []byte) (jsonrpc.Message, error) {
	var input struct {
		Version string         `json:"jsonrpc"`
		ID      jsontext.Value `json:"id"`
		Method  jsontext.Value `json:"method"`
		Params  jsontext.Value `json:"params"`
	}
	if json.Unmarshal(frame, &input) != nil || input.Version != "2.0" {
		return nil, contract.ErrProtocol
	}
	if len(input.Method) == 0 {
		// 이 서버는 응답을 기다리는 요청을 시작하지 않는다. 기존 응답 해석만 SDK에 맡긴다.
		return jsonrpc.DecodeMessage(frame)
	}
	var method string
	if input.Method.Kind() != '"' || json.Unmarshal(input.Method, &method) != nil || method == "" {
		return nil, contract.ErrProtocol
	}
	var id jsonrpc.ID
	if len(input.ID) != 0 {
		var ok bool
		id, ok = parseHostID(input.ID)
		if !ok {
			return nil, contract.ErrProtocol
		}
	}
	return &jsonrpc.Request{ID: id, Method: method, Params: input.Params}, nil
}

// parseHostID는 입력 상한 안의 정수를 자르지 않고 문자열과 별도의 상관 키로 만든다.
func parseHostID(raw jsontext.Value) (jsonrpc.ID, bool) {
	if !raw.IsValid() {
		return jsonrpc.ID{}, false
	}
	var value string
	switch raw.Kind() {
	case '"':
		if json.Unmarshal(raw, &value) != nil {
			return jsonrpc.ID{}, false
		}
		// 내부 숫자 키와 같은 접두사를 가진 호스트 문자열도 충돌하지 않게 한다.
		if strings.HasPrefix(value, "\x00") {
			value = stringIDPrefix + value
		}
	case '0':
		if strings.ContainsAny(string(raw), ".eE") {
			return jsonrpc.ID{}, false
		}
		// 정확히 표현 가능한 작은 정수만 SDK의 기존 정수 ID와 호환시킨다.
		if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil && n >= -(1<<53) && n <= 1<<53 && string(raw) != "-0" {
			id, err := jsonrpc.MakeID(float64(n))
			return id, err == nil
		}
		value = numberIDPrefix + string(raw)
	default:
		return jsonrpc.ID{}, false
	}
	id, err := jsonrpc.MakeID(value)
	return id, err == nil
}

func marshalHostID(id jsonrpc.ID) (jsontext.Value, error) {
	if value, ok := id.Raw().(string); ok {
		if raw, ok := strings.CutPrefix(value, numberIDPrefix); ok {
			return jsontext.Value(raw), nil
		}
		if value, ok := strings.CutPrefix(value, stringIDPrefix); ok {
			return json.Marshal(value)
		}
	}
	return json.Marshal(id.Raw())
}
