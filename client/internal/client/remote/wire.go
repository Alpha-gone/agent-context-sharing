package remote

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
)

const (
	maxRequestBytes  = 256 << 10
	maxResponseBytes = 32 << 20
)

// ProtocolError는 검증된 원격 JSON-RPC 오류다. 로컬 오류와 구분해 호스트에 전달한다.
type ProtocolError struct{ raw jsontext.Value }

// Error는 원격 오류 본문을 로그나 로컬 진단에 노출하지 않는다.
func (ProtocolError) Error() string { return "원격 MCP가 JSON-RPC 오류를 반환했습니다." }

// GoString은 상세 디버그 표현에서도 원격 본문을 노출하지 않는다.
func (e ProtocolError) GoString() string { return e.Error() }

// Raw는 code·message·data를 보존한 JSON-RPC error 객체의 복사본이다.
func (e ProtocolError) Raw() jsontext.Value { return bytes.Clone(e.raw) }

var errResponseLimit = errors.New("응답 크기 상한을 넘었습니다")

type boundedBody struct {
	reader io.Reader
	read   int64
	err    error
}

func (r *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	p = p[:min(len(p), maxResponseBytes+1-int(r.read))]
	if len(p) == 0 {
		return 0, errResponseLimit
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if err != nil {
		r.err = err
	}
	if r.read > maxResponseBytes {
		return n, errResponseLimit
	}
	return n, err
}

func responseBody(response *http.Response) ([]byte, error) {
	if response.ContentLength > maxResponseBytes || response.Uncompressed {
		return nil, errResponseLimit
	}
	encoded := &boundedBody{reader: response.Body}
	var decoded io.Reader = encoded
	encoding := response.Header.Values("Content-Encoding")
	if len(encoding) > 1 {
		return nil, gzip.ErrHeader
	}
	if len(encoding) == 1 && encoding[0] != "" && encoding[0] != "identity" {
		if encoding[0] != "gzip" {
			return nil, gzip.ErrHeader
		}
		reader, err := gzip.NewReader(encoded)
		if err != nil {
			return nil, bodyError(err, encoded, response.ContentLength, true)
		}
		defer reader.Close()
		decoded = reader
	}
	body, err := io.ReadAll(&boundedBody{reader: decoded})
	if err != nil {
		return nil, bodyError(err, encoded, response.ContentLength, len(encoding) == 1 && encoding[0] == "gzip")
	}
	if response.ContentLength > 0 && encoded.read != response.ContentLength {
		return nil, io.ErrUnexpectedEOF
	}
	return body, nil
}

func bodyError(err error, encoded *boundedBody, length int64, compressed bool) error {
	if _, corrupt := errors.AsType[flate.CorruptInputError](err); corrupt {
		return errWireProtocol
	}
	if compressed && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && (errors.Is(encoded.err, io.EOF) || length >= 0 && encoded.read == length) {
		// 전송 자체가 완전한데 gzip framing이 끝나지 않은 경우는 통신 실패가 아니다.
		return errWireProtocol
	}
	return err
}

func parseEnvelope(response *http.Response, body []byte, id string) (jsontext.Value, error) {
	types := response.Header.Values("Content-Type")
	if len(types) != 1 {
		return nil, errWireProtocol
	}
	mediaType, _, err := mime.ParseMediaType(types[0])
	if err != nil || mediaType != "application/json" {
		return nil, errWireProtocol
	}
	var envelope map[string]jsontext.Value
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		return nil, errWireProtocol
	}
	var version, actualID string
	if json.Unmarshal(envelope["jsonrpc"], &version) != nil || version != "2.0" || json.Unmarshal(envelope["id"], &actualID) != nil || actualID != id {
		return nil, errWireProtocol
	}
	result, hasResult := envelope["result"]
	rpcError, hasError := envelope["error"]
	if hasResult == hasError {
		return nil, errWireProtocol
	}
	if hasResult {
		if response.StatusCode != http.StatusOK || result.Kind() != '{' {
			return nil, errWireProtocol
		}
		return bytes.Clone(result), nil
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(rpcError, &fields) != nil || fields == nil {
		return nil, errWireProtocol
	}
	var code int64
	var message string
	if json.Unmarshal(fields["code"], &code) != nil || json.Unmarshal(fields["message"], &message) != nil {
		return nil, errWireProtocol
	}
	status := http.StatusBadRequest
	switch code {
	case -32700, -32600, -32602, -32020, -32022:
	case -32601:
		status = http.StatusNotFound
	case -32603:
		status = http.StatusInternalServerError
	default:
		return nil, errWireProtocol
	}
	if response.StatusCode != status {
		return nil, errWireProtocol
	}
	return nil, &ProtocolError{raw: bytes.Clone(rpcError)}
}

var errWireProtocol = errors.New("HTTP·JSON-RPC 외피 계약이 일치하지 않습니다")
