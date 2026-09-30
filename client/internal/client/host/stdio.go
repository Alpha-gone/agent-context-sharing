package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxInputBytes  = 256 << 10
	maxOutputBytes = 32 << 20
)

type stdioTransport struct {
	in  io.ReadCloser
	out io.Writer
}

func (t *stdioTransport) Connect(context.Context) (mcp.Connection, error) {
	c := &stdioConn{
		in:      t.in,
		reader:  bufio.NewReader(t.in),
		out:     t.out,
		frames:  make(chan frameResult),
		writes:  make(chan writeRequest),
		closed:  make(chan struct{}),
		drained: make(chan struct{}),
	}
	go c.readLoop()
	go c.writeLoop()
	return c, nil
}

type writeRequest struct {
	frame []byte
	done  chan error
}

type frameResult struct {
	frame     []byte
	oversized bool
	err       error
}

type stdioConn struct {
	in        io.ReadCloser
	reader    *bufio.Reader
	out       io.Writer
	frames    chan frameResult
	writes    chan writeRequest
	closed    chan struct{}
	closeOnce sync.Once
	drained   chan struct{}
	pendingMu sync.Mutex
	pending   int
	eofSeen   bool
}

func (c *stdioConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var next frameResult
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, mcp.ErrConnectionClosed
		case received, ok := <-c.frames:
			if !ok {
				return nil, io.EOF
			}
			next = received
		}
		if next.err != nil {
			if errors.Is(next.err, io.EOF) {
				// SDK에 EOF를 알리기 전에 접수한 호출의 응답 쓰기를 끝낸다.
				c.pendingMu.Lock()
				c.eofSeen = true
				if c.pending == 0 {
					close(c.drained)
				}
				c.pendingMu.Unlock()
				select {
				case <-c.drained:
					return nil, io.EOF
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-c.closed:
					return nil, mcp.ErrConnectionClosed
				}
			}
			return nil, next.err
		}
		if next.oversized {
			if err := c.writeError(ctx, idFromPrefix(next.frame), jsonrpc.CodeInvalidRequest,
				"입력 크기 제한을 초과했습니다.", "client_busy"); err != nil {
				return nil, err
			}
			continue
		}
		if len(bytes.Trim(next.frame, " \t\r\n")) == 0 {
			continue
		}
		message, err := jsonrpc.DecodeMessage(next.frame)
		if err != nil {
			code := int64(jsonrpc.CodeParseError)
			if jsontext.Value(next.frame).IsValid() {
				code = jsonrpc.CodeInvalidRequest
			}
			if err := c.writeError(ctx, jsonrpc.ID{}, code,
				"잘못된 JSON-RPC 메시지입니다.", ""); err != nil {
				return nil, err
			}
			continue
		}
		request, ok := message.(*jsonrpc.Request)
		if !ok {
			// 이 경계는 서버에서 시작하는 요청을 만들지 않는다.
			continue
		}
		if request.IsCall() && (request.Method == "server/discover" || request.Method == "tools/list" || request.Method == "tools/call") && !validRequestMeta(request.Params) {
			if err := c.writeError(ctx, request.ID, jsonrpc.CodeInvalidParams,
				"요청별 MCP 2026-07-28 메타데이터가 필요합니다.", "client_protocol"); err != nil {
				return nil, err
			}
			continue
		}
		switch request.Method {
		case "server/discover", "notifications/cancelled":
			if request.IsCall() {
				c.pendingMu.Lock()
				c.pending++
				c.pendingMu.Unlock()
			}
			return message, nil
		case "tools/list", "tools/call":
			// 원격 계약 검증과 도구 중계는 다음 단계에서 연결한다.
			if request.IsCall() {
				if err := c.writeError(ctx, request.ID, jsonrpc.CodeInternalError,
					"원격 도구 계약이 준비되지 않았습니다.", "client_protocol"); err != nil {
					return nil, err
				}
			}
		default:
			if request.IsCall() {
				if err := c.writeError(ctx, request.ID, jsonrpc.CodeMethodNotFound,
					"지원하지 않는 MCP 메서드입니다.", ""); err != nil {
					return nil, err
				}
			}
		}
	}
}

func (c *stdioConn) readLoop() {
	defer close(c.frames)
	for {
		frame, oversized, err := readFrame(c.reader)
		select {
		case c.frames <- frameResult{frame: frame, oversized: oversized, err: err}:
		case <-c.closed:
			return
		}
		if err != nil {
			return
		}
	}
}

func validRequestMeta(params []byte) bool {
	var envelope struct {
		Meta map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil {
		return false
	}
	version, ok := envelope.Meta[mcp.MetaKeyProtocolVersion].(string)
	if !ok || version != protocolVersion {
		return false
	}
	_, ok = envelope.Meta[mcp.MetaKeyClientCapabilities].(map[string]any)
	return ok
}

func (c *stdioConn) Write(ctx context.Context, message jsonrpc.Message) error {
	if _, ok := message.(*jsonrpc.Response); ok {
		defer c.finishResponse()
	}
	frame, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return err
	}
	if len(frame) > maxOutputBytes {
		response, ok := message.(*jsonrpc.Response)
		if !ok {
			return fmt.Errorf("호스트 출력 크기 제한을 초과했습니다")
		}
		return c.writeError(ctx, response.ID, jsonrpc.CodeInternalError,
			"호스트 출력 크기 제한을 초과했습니다.", "client_protocol")
	}
	return c.writeFrame(ctx, append(frame, '\n'))
}

func (c *stdioConn) finishResponse() {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.pending == 0 {
		return
	}
	c.pending--
	if c.eofSeen && c.pending == 0 {
		close(c.drained)
	}
}

func (c *stdioConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.in.Close()
	})
	return err
}

func (*stdioConn) SessionID() string { return "" }

func (c *stdioConn) writeError(ctx context.Context, id jsonrpc.ID, code int64, message, clientCode string) error {
	response := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   struct {
			Code    int64  `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data,omitempty"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id.Raw()}
	response.Error.Code = code
	response.Error.Message = message
	if clientCode != "" {
		response.Error.Data = map[string]any{
			"client_error": map[string]string{"code": clientCode},
		}
	}
	frame, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return c.writeFrame(ctx, append(frame, '\n'))
}

func (c *stdioConn) writeFrame(ctx context.Context, frame []byte) error {
	request := writeRequest{frame: frame, done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return mcp.ErrConnectionClosed
	case c.writes <- request:
	}
	select {
	case err := <-request.done:
		return err
	case <-c.closed:
		return mcp.ErrConnectionClosed
	}
}

func (c *stdioConn) writeLoop() {
	for {
		select {
		case request := <-c.writes:
			n, err := c.out.Write(request.frame)
			if err == nil && n != len(request.frame) {
				err = io.ErrShortWrite
			}
			request.done <- err
		case <-c.closed:
			return
		}
	}
}

func readFrame(reader *bufio.Reader) ([]byte, bool, error) {
	var frame []byte
	oversized := false
	for {
		piece, err := reader.ReadSlice('\n')
		if !oversized {
			remaining := maxInputBytes + 2 - len(frame)
			if len(piece) > remaining {
				frame = append(frame, piece[:remaining]...)
				oversized = true
			} else {
				frame = append(frame, piece...)
			}
		}
		switch {
		case err == nil:
			frame = bytes.TrimSuffix(frame, []byte{'\n'})
			frame = bytes.TrimSuffix(frame, []byte{'\r'})
			return frame, oversized || len(frame) > maxInputBytes, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(frame) > 0:
			return frame, oversized || len(frame) > maxInputBytes, nil
		default:
			return nil, false, err
		}
	}
}

// idFromPrefix는 초과 프레임의 완전히 읽힌 최상위 id만 회수한다.
func idFromPrefix(frame []byte) jsonrpc.ID {
	decoder := jsontext.NewDecoder(bytes.NewReader(frame))
	start, err := decoder.ReadToken()
	if err != nil || start.Kind() != '{' {
		return jsonrpc.ID{}
	}
	for {
		key, err := decoder.ReadToken()
		if err != nil || key.Kind() == '}' {
			return jsonrpc.ID{}
		}
		if key.Kind() != '"' {
			return jsonrpc.ID{}
		}
		if key.String() != "id" {
			if err := decoder.SkipValue(); err != nil {
				return jsonrpc.ID{}
			}
			continue
		}
		value, err := decoder.ReadValue()
		if err != nil {
			return jsonrpc.ID{}
		}
		candidate := append([]byte(`{"jsonrpc":"2.0","id":`), value...)
		candidate = append(candidate, []byte(`,"method":"ping"}`)...)
		message, err := jsonrpc.DecodeMessage(candidate)
		if err != nil {
			return jsonrpc.ID{}
		}
		request, ok := message.(*jsonrpc.Request)
		if !ok {
			return jsonrpc.ID{}
		}
		return request.ID
	}
}
