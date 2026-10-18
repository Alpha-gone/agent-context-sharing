package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const protocolRevision = "2026-07-28"

// newSDKTransport는 SDK의 이전 수명주기 경로를 서비스 외피로 차단한다.
// 인증·Origin 정책·업무 도구 조립은 MCP-02 이후의 진입점 책임이다.
// 이 처리기만으로 보호된 /mcp 경로를 공개하지 않는다.
func newSDKTransport(server *sdk.Server, maxBodyBytes int64) http.Handler {
	if server == nil || maxBodyBytes <= 0 {
		panic("MCP SDK 서버와 양수 본문 상한이 필요하다")
	}
	next := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxBodyBytes,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body := http.MaxBytesReader(w, request.Body, maxBodyBytes)
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				writeTransportError(w, http.StatusRequestEntityTooLarge, nil, -32600, "request body too large", nil)
			} else {
				writeTransportError(w, http.StatusBadRequest, nil, -32600, "cannot read request body", nil)
			}
			return
		}
		var wire map[string]jsontext.Value
		if err := json.Unmarshal(data, &wire); err != nil {
			code := -32600
			if _, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
				code = -32700
			}
			writeTransportError(w, http.StatusBadRequest, nil, code, "expected a single JSON-RPC request", nil)
			return
		}
		if wire == nil {
			writeTransportError(w, http.StatusBadRequest, nil, -32600, "expected a single JSON-RPC request", nil)
			return
		}
		id := wire["id"]
		if !validTransportID(id) {
			writeTransportError(w, http.StatusBadRequest, nil, -32600, "a string or integer request id is required", nil)
			return
		}
		var version, method string
		if json.Unmarshal(wire["jsonrpc"], &version) != nil || version != "2.0" ||
			json.Unmarshal(wire["method"], &method) != nil || method == "" || wire["result"] != nil || wire["error"] != nil {
			writeTransportError(w, http.StatusBadRequest, id, -32600, "invalid JSON-RPC request", nil)
			return
		}
		var params map[string]jsontext.Value
		var meta map[string]jsontext.Value
		var revision string
		var capabilities map[string]jsontext.Value
		if json.Unmarshal(wire["params"], &params) != nil ||
			json.Unmarshal(params["_meta"], &meta) != nil ||
			json.Unmarshal(meta[sdk.MetaKeyProtocolVersion], &revision) != nil || revision == "" ||
			json.Unmarshal(meta[sdk.MetaKeyClientCapabilities], &capabilities) != nil || capabilities == nil {
			writeTransportError(w, http.StatusBadRequest, id, -32602, "protocolVersion and clientCapabilities metadata are required", nil)
			return
		}
		headerRevision, revisionOK := transportHeader(request.Header, "MCP-Protocol-Version")
		headerMethod, methodOK := transportHeader(request.Header, "Mcp-Method")
		if !revisionOK || !methodOK || headerRevision != revision || headerMethod != method {
			writeTransportError(w, http.StatusBadRequest, id, -32020, "missing or mismatched MCP headers", nil)
			return
		}
		if revision != protocolRevision {
			writeTransportError(w, http.StatusBadRequest, id, -32022, "unsupported protocol version", map[string]any{
				"supported": []string{protocolRevision}, "requested": revision,
			})
			return
		}
		switch method {
		case "server/discover", "tools/list":
		case "tools/call":
			var name string
			if json.Unmarshal(params["name"], &name) != nil || name == "" {
				writeTransportError(w, http.StatusBadRequest, id, -32602, "tool name is required", nil)
				return
			}
			headerName, ok := transportHeader(request.Header, "Mcp-Name")
			decoded, decodeOK := decodeTransportName(headerName)
			if !ok || !decodeOK || decoded != name {
				writeTransportError(w, http.StatusBadRequest, id, -32020, "missing or mismatched Mcp-Name", nil)
				return
			}
			// SDK v1.8.0은 Mcp-Name을 해독하지 않고 비교한다. 본문과 대조한
			// 값만 요청 사본에 정규화하며 원래 헤더와 본문은 변경하지 않는다.
			request = request.Clone(request.Context())
			request.Header.Set("Mcp-Name", decoded)
		default:
			writeTransportError(w, http.StatusNotFound, id, -32601, "method not found", nil)
			return
		}
		// 이미 제한한 원문 바이트를 SDK에도 그대로 전달한다. 인자를 재직렬화하지 않는다.
		forward := request.WithContext(request.Context())
		forward.Body = io.NopCloser(bytes.NewReader(data))
		next.ServeHTTP(w, forward)
	})
}

func validTransportID(id jsontext.Value) bool {
	switch id.Kind() {
	case '"':
		return true
	case '0':
		var integer int64
		return json.Unmarshal(id, &integer) == nil
	default:
		return false
	}
}

// 중복 헤더를 첫 값으로 축약하면 프록시와 업무 처리의 판정이 달라질 수 있다.
func transportHeader(headers http.Header, name string) (string, bool) {
	values := headers.Values(name)
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] {
		return "", false
	}
	for _, value := range []byte(values[0]) {
		if value < 0x20 || value > 0x7e {
			return "", false
		}
	}
	return values[0], true
}

func decodeTransportName(value string) (string, bool) {
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		encoded := value[len("=?base64?") : len(value)-len("?=")]
		decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || !utf8.Valid(decoded) || base64.StdEncoding.EncodeToString(decoded) != encoded {
			return "", false
		}
		return string(decoded), true
	}
	return value, true
}

func writeTransportError(w http.ResponseWriter, status int, id jsontext.Value, code int, message string, data any) {
	// 입력·SDK 오류 원문은 넣지 않으며 id는 사전 검증한 JSON 값만 사용한다.
	response := struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      jsontext.Value `json:"id,omitempty"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data,omitempty"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	response.Error.Code, response.Error.Message, response.Error.Data = code, message, data
	encoded, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "cannot encode MCP error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(encoded)
}
