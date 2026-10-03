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
	"log/slog"
	"sync"
	"time"

	"agent_context_sharing/client/internal/client/contract"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxInputBytes  = 256 << 10
	maxOutputBytes = 32 << 20
)

type stdioTransport struct {
	in      io.ReadCloser
	out     io.Writer
	tools   *Tools
	logger  *slog.Logger
	version string
}

func (t *stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if t.logger == nil {
		t.logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	life, cancel := context.WithCancel(ctx)
	c := &stdioConn{
		in:      t.in,
		reader:  bufio.NewReader(t.in),
		out:     t.out,
		frames:  make(chan frameResult),
		writes:  make(chan writeRequest),
		closed:  make(chan struct{}),
		drained: make(chan struct{}),
		tools:   t.tools, life: life, cancel: cancel, calls: make(map[jsonrpc.ID]*activeCall),
		logger:  t.logger,
		version: t.version,
	}
	go c.readLoop()
	go c.writeLoop()
	return c, nil
}

type writeRequest struct {
	frame  []byte
	done   chan error
	before func() bool
}

type activeCall struct {
	cancel    context.CancelFunc
	cancelled bool // callsMu가 명시적 취소와 응답 기록 시작을 직렬화한다.
}

type frameResult struct {
	frame     []byte
	oversized bool
	err       error
}

type stdioConn struct {
	logger      *slog.Logger
	version     string
	tools       *Tools
	life        context.Context
	cancel      context.CancelFunc
	callsMu     sync.Mutex
	calls       map[jsonrpc.ID]*activeCall
	toolPending int
	stopped     bool
	workers     sync.WaitGroup
	in          io.ReadCloser
	reader      *bufio.Reader
	out         io.Writer
	frames      chan frameResult
	writes      chan writeRequest
	closed      chan struct{}
	closeOnce   sync.Once
	drained     chan struct{}
	pendingMu   sync.Mutex
	pending     int
	eofSeen     bool
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
		case <-c.life.Done():
			return nil, c.life.Err()
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
				c.cancelCalls()
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
		if request.IsCall() && request.Method == "server/discover" && !validRequestMeta(request.Params) {
			if err := c.writeError(ctx, request.ID, jsonrpc.CodeInvalidParams,
				"요청별 MCP 2026-07-28 메타데이터가 필요합니다.", "client_protocol"); err != nil {
				return nil, err
			}
			continue
		}
		switch request.Method {
		case "notifications/cancelled":
			if !request.IsCall() {
				c.cancelRequest(request.Params)
				return message, nil
			}
			if err := c.writeError(ctx, request.ID, jsonrpc.CodeInvalidRequest, "알림에는 요청 ID를 지정할 수 없습니다.", ""); err != nil {
				return nil, err
			}
		case "server/discover":
			if request.IsCall() {
				c.pendingMu.Lock()
				c.pending++
				c.pendingMu.Unlock()
			}
			return message, nil
		case "tools/list", "tools/call":
			if request.IsCall() {
				if err := c.dispatch(ctx, request); err != nil {
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
		c.cancel()
		c.callsMu.Lock()
		c.stopped = true
		for _, call := range c.calls {
			call.cancel()
		}
		c.callsMu.Unlock()
		close(c.closed)
		err = c.in.Close()
		c.workers.Wait()
	})
	return err
}

func (c *stdioConn) cancelCalls() {
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	for _, call := range c.calls {
		call.cancel()
	}
}

func (c *stdioConn) cancelRequest(params []byte) {
	var input struct {
		ID jsontext.Value `json:"requestId"`
	}
	if json.Unmarshal(params, &input) != nil {
		return
	}
	message, err := jsonrpc.DecodeMessage(append(append([]byte(`{"jsonrpc":"2.0","method":"tools/call","id":`), input.ID...), '}'))
	if err != nil {
		return
	}
	request, ok := message.(*jsonrpc.Request)
	if !ok {
		return
	}
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	if call := c.calls[request.ID]; call != nil {
		call.cancelled = true
		call.cancel()
	}
}

func (c *stdioConn) dispatch(ctx context.Context, request *jsonrpc.Request) error {
	c.callsMu.Lock()
	if c.stopped {
		c.callsMu.Unlock()
		return mcp.ErrConnectionClosed
	}
	if _, exists := c.calls[request.ID]; exists {
		c.callsMu.Unlock()
		return c.writeError(ctx, request.ID, jsonrpc.CodeInvalidRequest, "진행 중인 요청 ID를 재사용할 수 없습니다.", "")
	}
	if c.toolPending >= 8+128 {
		c.callsMu.Unlock()
		field, raw := clientErrorResponse(request.Method, contract.ErrBusy)
		return c.writeRaw(ctx, request.ID, request.Method, field, raw)
	}
	work, cancel := context.WithCancel(contract.WithCorrelation(c.life))
	call := &activeCall{cancel: cancel}
	c.calls[request.ID] = call
	c.toolPending++
	finish := func() {
		c.callsMu.Lock()
		if c.calls[request.ID] == call {
			delete(c.calls, request.ID)
		}
		c.callsMu.Unlock()
	}
	beginWrite := func() bool {
		c.callsMu.Lock()
		defer c.callsMu.Unlock()
		if c.calls[request.ID] == call {
			delete(c.calls, request.ID)
		}
		return !call.cancelled
	}
	c.pendingMu.Lock()
	c.pending++
	c.pendingMu.Unlock()
	c.workers.Go(func() {
		defer cancel()
		defer c.finishResponse()
		defer func() { finish(); c.callsMu.Lock(); c.toolPending--; c.callsMu.Unlock() }()
		process := func(work context.Context) error {
			started := time.Now()
			result, err := c.toolResult(work, request)
			if err == nil && len(result) > maxOutputBytes {
				result, err = nil, contract.ErrProtocol
			}
			outcome := "pass"
			if err != nil {
				outcome = contract.ClassifyError(err).Code
			} else {
				var flags struct {
					IsError bool `json:"isError"`
				}
				if json.Unmarshal(result, &flags) == nil && flags.IsError {
					outcome = "domain_error"
				}
			}
			field := "result"
			if err != nil {
				if protocol, ok := errors.AsType[interface {
					error
					Raw() jsontext.Value
				}](err); ok {
					result, field = protocol.Raw(), "error"
					outcome = "remote_protocol"
				} else {
					field, result = clientErrorResponse(request.Method, err)
				}
			}
			if writeErr := c.writeRaw(c.life, request.ID, request.Method, field, result, beginWrite); writeErr != nil {
				outcome = "output_failed"
				c.cancel()
				_ = c.in.Close()
			} else {
				c.callsMu.Lock()
				if call.cancelled {
					outcome, result = "cancelled", nil
				}
				c.callsMu.Unlock()
			}
			attrs := []any{"event", "host_call_complete", "correlation_id", contract.CorrelationID(work), "outcome", outcome, "duration_ms", time.Since(started).Milliseconds(), "request_bytes", len(request.Params), "response_bytes", len(result)}
			var input struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(request.Params, &input) == nil {
				if _, known := contract.Classify(input.Name); known {
					attrs = append(attrs, "tool", input.Name)
				}
			}
			c.logger.InfoContext(c.life, "", attrs...)
			return nil
		}
		if c.tools == nil {
			_ = process(work)
		} else {
			_ = c.tools.withinCall(work, process)
		}
	})
	c.callsMu.Unlock()
	return nil
}

func (c *stdioConn) toolResult(ctx context.Context, request *jsonrpc.Request) (jsontext.Value, error) {
	if c.tools == nil || !validRequestMeta(request.Params) {
		return nil, contract.ErrProtocol
	}
	if request.Method == "tools/list" {
		var input struct {
			Cursor jsontext.Value `json:"cursor"`
		}
		if json.Unmarshal(request.Params, &input) != nil || len(input.Cursor) != 0 && string(input.Cursor) != `""` {
			return nil, contract.ErrProtocol
		}
		tools, err := c.tools.ListTools(ctx)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(struct {
			Tools      []contract.Tool `json:"tools"`
			TTLMS      int64           `json:"ttlMs"`
			CacheScope string          `json:"cacheScope"`
		}{tools, 0, "private"})
		if err != nil {
			return nil, err
		}
		return annotateResult(raw, c.version)
	}
	var input struct {
		Name      string         `json:"name"`
		Arguments jsontext.Value `json:"arguments"`
	}
	if json.Unmarshal(request.Params, &input) != nil {
		return nil, contract.ErrProtocol
	}
	if len(input.Arguments) == 0 {
		input.Arguments = jsontext.Value(`{}`)
	}
	result, err := c.tools.CallTool(ctx, input.Name, input.Arguments)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, contract.ErrProtocol
	}
	return annotateResult(result.Raw(), c.version)
}

func clientErrorResult(err error) jsontext.Value {
	clientError := contract.ClassifyError(err)
	raw, _ := json.Marshal(struct {
		Content           []any `json:"content"`
		IsError           bool  `json:"isError"`
		StructuredContent struct {
			ClientError contract.ClientError `json:"client_error"`
		} `json:"structuredContent"`
	}{Content: []any{map[string]string{"type": "text", "text": clientError.Message}}, IsError: true, StructuredContent: struct {
		ClientError contract.ClientError `json:"client_error"`
	}{clientError}})
	return raw
}

func clientErrorResponse(method string, err error) (string, jsontext.Value) {
	if method != "tools/list" {
		return "result", clientErrorResult(err)
	}
	clientError := contract.ClassifyError(err)
	raw, _ := json.Marshal(struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
		Data    struct {
			ClientError contract.ClientError `json:"client_error"`
		} `json:"data"`
	}{Code: jsonrpc.CodeInternalError, Message: clientError.Message, Data: struct {
		ClientError contract.ClientError `json:"client_error"`
	}{clientError}})
	return "error", raw
}

func (c *stdioConn) writeRaw(ctx context.Context, id jsonrpc.ID, method, field string, raw jsontext.Value, before ...func() bool) error {
	if len(raw) > maxOutputBytes {
		field, raw = clientErrorResponse(method, contract.ErrProtocol)
	}
	if !raw.IsValid() || raw.Kind() != '{' {
		field, raw = clientErrorResponse(method, contract.ErrProtocol)
	}
	// 줄 프레이밍만 정리하며 필드·숫자·문자열 표현을 다시 해석하지 않는다.
	if err := raw.Compact(); err != nil {
		return err
	}
	if field == "result" {
		var err error
		raw, err = annotateResult(raw, c.version)
		if err != nil {
			field, raw = clientErrorResponse(method, contract.ErrProtocol)
			return c.writeRaw(ctx, id, method, field, raw, before...)
		}
	}
	idRaw, err := json.Marshal(id.Raw())
	if err != nil {
		return err
	}
	frame := append([]byte(`{"jsonrpc":"2.0","id":`), idRaw...)
	frame = append(frame, []byte(`,"`+field+`":`)...)
	frame = append(frame, raw...)
	frame = append(frame, '}', '\n')
	if len(frame)-1 > maxOutputBytes {
		field, raw = clientErrorResponse(method, contract.ErrProtocol)
		return c.writeRaw(ctx, id, method, field, raw, before...)
	}
	return c.writeFrame(ctx, frame, before...)
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

func (c *stdioConn) writeFrame(ctx context.Context, frame []byte, before ...func() bool) error {
	request := writeRequest{frame: frame, done: make(chan error, 1)}
	if len(before) != 0 {
		request.before = before[0]
	}
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
			if request.before != nil && !request.before() {
				request.done <- nil
				continue
			}
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
