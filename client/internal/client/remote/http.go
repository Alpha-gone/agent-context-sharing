package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"syscall"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/contract"
)

// Authentication은 비대화형 요청 적용과 공유 인가를 분리한 인증 경계다.
type Authentication interface {
	Identity(context.Context) (string, error)
	Apply(context.Context, *http.Request) (authorize.Credential, error)
	Reauthorize(context.Context, authorize.Credential, authorize.Challenge) (authorize.Credential, error)
	Refresh(context.Context, http.Header) error
}

// HTTPOptions는 빌드 판·안전한 로그와 전송·시계·재시도 지연 경계를 지정한다.
// nil인 경계는 운영 기본값을 사용한다.
type HTTPOptions struct {
	Version   string
	Logger    *slog.Logger
	Transport http.RoundTripper
	Now       func() time.Time
	Jitter    func(time.Duration) (time.Duration, error)
	Wait      func(context.Context, time.Duration) error
}

type callState struct {
	correlation string
	owner       *Client
	base        context.Context
	work        context.Context
	cancel      context.CancelFunc
	remaining   time.Duration
	started     time.Time
	retries     int
	authUsed    bool
	entered     bool
	lease       *callQueue
}

func (s *callState) resume() {
	s.work = s.base
	if s.owner != nil && s.owner.timeout > 0 {
		s.started = time.Now()
		s.work, s.cancel = context.WithTimeout(s.base, s.remaining)
	}
}

func (s *callState) pause() error {
	if err := s.work.Err(); err != nil {
		return err
	}
	if s.cancel != nil {
		s.remaining -= time.Since(s.started)
		s.cancel()
		s.cancel = nil
		s.work = s.base
		if s.remaining <= 0 {
			return context.DeadlineExceeded
		}
	}
	return nil
}

func requestContext(ctx context.Context) context.Context {
	if state, ok := ctx.Value(stateKey{}).(*callState); ok {
		return state.work
	}
	return ctx
}

type stateKey struct{}
type cacheKey struct{}

type authorizationRequired struct {
	credential authorize.Credential
	challenge  authorize.Challenge
}

func (*authorizationRequired) Error() string { return authorize.ErrAuthorization.Error() }

type httpSource struct {
	logger     *slog.Logger
	version    string
	cfg        config.Config
	auth       Authentication
	http       *http.Client
	owned      *http.Transport
	now        func() time.Time
	jitter     func(time.Duration) (time.Duration, error)
	wait       func(context.Context, time.Duration) error
	queue      callQueue
	negotiated atomic.Bool
}

// NewHTTP는 인증·전송·발견 캐시를 조립한다. 인가나 외부 요청은 생성 시 시작하지 않는다.
// 주입 인증 조정자의 종료는 호출자가, 복제한 HTTP transport의 종료는 Close가 소유한다.
func NewHTTP(cfg config.Config, authentication Authentication, options HTTPOptions) (*Client, error) {
	if cfg.RemoteURL() == "" || cfg.RequestTimeout() <= 0 || authentication == nil {
		return nil, contract.ErrConfiguration
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Jitter == nil {
		options.Jitter = randomDelay
	}
	if options.Wait == nil {
		options.Wait = waitDelay
	}
	if options.Version == "" {
		options.Version = "dev"
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	s := &httpSource{cfg: cfg, auth: authentication, now: options.Now, jitter: options.Jitter, wait: options.Wait, version: options.Version, logger: options.Logger}
	transport := options.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if standard, ok := transport.(*http.Transport); ok {
		s.owned = standard.Clone()
		s.owned.DisableCompression = true
		transport = s.owned
	}
	s.http = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c := New(s, options.Now)
	c.timeout = cfg.RequestTimeout()
	c.prepare = s.prepare
	c.reauthorize = s.reauthorize
	c.negotiation = s.negotiated.Store
	c.admission = &s.queue
	closed, cancel := context.WithCancel(context.Background())
	c.closed = closed
	c.close = func() {
		cancel()
		if s.owned != nil {
			s.owned.CloseIdleConnections()
		}
	}
	return c, nil
}

func (s *httpSource) Identity(ctx context.Context) (string, error) { return s.auth.Identity(ctx) }

func (s *httpSource) prepare(ctx context.Context) error {
	if _, err := s.auth.Identity(requestContext(ctx)); err == nil {
		return nil
	} else if !errors.Is(err, authorize.ErrAuthorization) {
		return err
	}
	// 토큰 없는 보호 요청의 도전을 캐시 잠금·호출 slot 밖의 인가로 연결한다.
	_, err := s.execute(ctx, "tools/list", "", nil, false)
	return err
}

func (s *httpSource) Discover(ctx context.Context) (jsontext.Value, error) {
	return s.execute(ctx, "server/discover", "", nil, true)
}

func (s *httpSource) ListTools(ctx context.Context) (jsontext.Value, error) {
	return s.execute(ctx, "tools/list", "", nil, true)
}

func (s *httpSource) CallTool(ctx context.Context, name string, arguments jsontext.Value) (jsontext.Value, error) {
	if _, ok := contract.Classify(name); !ok || !arguments.IsValid() || arguments.Kind() != '{' {
		return nil, contract.ErrProtocol
	}
	return s.execute(ctx, "tools/call", name, arguments, true)
}

func (s *httpSource) reauthorize(ctx context.Context, required *authorizationRequired) error {
	if err := requestContext(ctx).Err(); err != nil {
		return err
	}
	state := ctx.Value(stateKey{}).(*callState)
	if state.authUsed {
		return authorize.ErrAuthorization
	}
	if err := state.pause(); err != nil {
		return err
	}
	state.authUsed = true
	lease := state.lease
	if lease != nil {
		state.lease = nil
		lease.release()
	}
	// 호스트 취소는 유지하고 구성의 원격 시간 예산에서 인가 대기만 제외한다.
	authCtx, cancel := context.WithTimeout(state.base, s.cfg.AuthTimeout())
	defer cancel()
	started := time.Now()
	s.log(ctx, "authorization_start", "", "started", 0)
	_, err := s.auth.Reauthorize(authCtx, required.credential, required.challenge)
	outcome := "pass"
	if err != nil {
		outcome = contract.ClassifyError(err).Code
	}
	s.log(ctx, "authorization_complete", "", outcome, time.Since(started))
	cancel()
	state.resume()
	if err == nil {
		err = state.work.Err()
	}
	if err != nil || lease == nil {
		return err
	}
	if err := lease.acquire(state.work); err != nil {
		return err
	}
	state.lease = lease
	return nil
}

func (s *httpSource) execute(ctx context.Context, method, name string, arguments jsontext.Value, authenticated bool) (jsontext.Value, error) {
	state, ok := ctx.Value(stateKey{}).(*callState)
	if !ok {
		state = &callState{}
		ctx = context.WithValue(ctx, stateKey{}, state)
		state.base, state.work = ctx, ctx
	}
	gated, _ := ctx.Value(cacheKey{}).(bool)
	kind, _ := contract.Classify(name)
	write := method == "tools/call" && kind == contract.Write
	key := ""
	keyDeadline := s.now().Add(24 * time.Hour)
	if write && s.negotiated.Load() {
		key = `"` + uuid.NewV7().String() + `"`
	}
	defer func() { key = "" }()
	possiblyDelivered := false
	for {
		ctx = requestContext(ctx)
		if err := ctx.Err(); err != nil {
			return nil, completionError(err, write && possiblyDelivered)
		}
		if possiblyDelivered && key != "" && !s.now().Before(keyDeadline) {
			return nil, contract.ErrIndeterminate
		}
		// 이전 쓰기의 적용 여부는 이번 인증·프로토콜 오류로 확정되지 않는다.
		unresolvedWrite := write && possiblyDelivered
		result, required, sent, retry, err := s.attempt(ctx, method, name, arguments, key, authenticated)
		possiblyDelivered = possiblyDelivered || sent
		if required != nil {
			if gated {
				return nil, required
			}
			if err := s.reauthorize(ctx, required); err != nil {
				return nil, completionError(err, write && possiblyDelivered)
			}
			if !authenticated {
				// 초기 도전은 여기서 끝내고 인증된 목록은 정상 캐시 경로에서 조회한다.
				return nil, nil
			}
			authenticated = true
			continue
		}
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return nil, completionError(ctx.Err(), write && possiblyDelivered)
		}
		if errors.Is(err, contract.ErrTransport) {
			if write && possiblyDelivered && key == "" {
				return nil, contract.ErrIndeterminate
			}
			if retry && state.retries < 3 {
				cap := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}[state.retries]
				state.retries++
				s.log(ctx, "remote_retry", name, "retry", 0)
				delay, delayErr := s.jitter(cap)
				if delayErr != nil || delay < 0 || delay > cap {
					return nil, completionError(contract.ErrTransport, write && possiblyDelivered)
				}
				if err := s.wait(ctx, delay); err != nil {
					return nil, completionError(err, write && possiblyDelivered)
				}
				continue
			}
		}
		return nil, completionError(err, unresolvedWrite || write && sent && errors.Is(err, contract.ErrTransport))
	}
}

func completionError(err error, writeUncertain bool) error {
	if writeUncertain {
		return contract.ErrIndeterminate
	}
	return err
}

type requestBody struct {
	reader *bytes.Reader
	read   atomic.Int64
}

func (b *requestBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (*requestBody) Close() error { return nil }

func (s *httpSource) attempt(ctx context.Context, method, name string, arguments jsontext.Value, key string, authenticated bool) (result jsontext.Value, required *authorizationRequired, sent, retry bool, finalErr error) {
	attemptStarted := time.Now()
	defer func() {
		outcome := "pass"
		if required != nil {
			outcome = "authorization_required"
		} else if finalErr != nil {
			outcome = contract.ClassifyError(finalErr).Code
			if _, ok := errors.AsType[*ProtocolError](finalErr); ok {
				outcome = "remote_protocol"
			}
		}
		s.log(ctx, "remote_attempt", name, outcome, time.Since(attemptStarted))
	}()
	id := uuid.NewV7().String()
	params := map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": protocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{"extensions": map[string]any{idempotencyExtension: map[string]any{}}}}}
	params["_meta"].(map[string]any)["io.modelcontextprotocol/clientInfo"] = map[string]string{"name": "agent-context-client", "version": s.version}
	if method == "tools/call" {
		params["name"], params["arguments"] = name, arguments
	}
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, nil, false, false, contract.ErrProtocol
	}
	if len(frame) > maxRequestBytes {
		return nil, nil, false, false, contract.ErrBusy
	}
	if state := ctx.Value(stateKey{}).(*callState); state.lease == nil {
		if err := s.queue.acquire(ctx); err != nil {
			return nil, nil, false, false, err
		}
		defer s.queue.release()
	}
	body := &requestBody{reader: bytes.NewReader(frame)}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.RemoteURL(), body)
	if err != nil {
		return nil, nil, false, false, contract.ErrProtocol
	}
	request.ContentLength = int64(len(frame))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	request.Header.Set("Mcp-Method", method)
	if method == "tools/call" {
		request.Header.Set("Mcp-Name", name)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	var credential authorize.Credential
	if authenticated {
		credential, err = s.auth.Apply(ctx, request)
		if err != nil {
			if errors.Is(err, authorize.ErrAuthorization) {
				return nil, &authorizationRequired{}, false, false, nil
			}
			return nil, nil, false, false, err
		}
	}
	var started, connected atomic.Bool
	trace := &httptrace.ClientTrace{GetConn: func(string) { started.Store(true) }, GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	request = request.WithContext(httptrace.WithClientTrace(ctx, trace))
	response, err := s.http.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		sent := body.read.Load() > 0 || !started.Load() || connected.Load()
		return nil, nil, sent, transient(err), contract.ErrTransport
	}
	defer response.Body.Close()
	raw, err := responseBody(response)
	if err != nil {
		if errors.Is(err, errResponseLimit) || errors.Is(err, errWireProtocol) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) {
			return nil, nil, true, false, contract.ErrProtocol
		}
		return nil, nil, true, transient(err), contract.ErrTransport
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, true, false, err
	}
	challenge, shouldAuthorize, err := authorize.ParseChallenge(response.StatusCode, response.Header, authenticated, ctx.Value(stateKey{}).(*callState).authUsed)
	if err != nil {
		return nil, nil, true, false, err
	}
	if shouldAuthorize {
		return nil, &authorizationRequired{credential: credential, challenge: challenge}, false, false, nil
	}
	if !authenticated {
		return nil, nil, false, false, contract.ErrProtocol
	}
	result, err = parseEnvelope(response, raw, id)
	if errors.Is(err, errWireProtocol) {
		return nil, nil, true, false, contract.ErrProtocol
	}
	if err != nil {
		if _, ok := errors.AsType[*ProtocolError](err); !ok {
			return nil, nil, true, false, contract.ErrProtocol
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, true, false, ctxErr
	}
	if refreshErr := s.auth.Refresh(ctx, response.Header); refreshErr != nil {
		return nil, nil, true, false, refreshErr
	}
	return result, nil, true, false, err
}

func (s *httpSource) log(ctx context.Context, event, tool, outcome string, duration time.Duration) {
	state, _ := ctx.Value(stateKey{}).(*callState)
	attrs := []any{"event", event, "outcome", outcome, "duration_ms", duration.Milliseconds()}
	if state != nil {
		attrs = append(attrs, "correlation_id", state.correlation, "retry_index", state.retries)
	}
	if _, known := contract.Classify(tool); known {
		attrs = append(attrs, "tool", tool)
	}
	s.logger.InfoContext(ctx, "", attrs...)
}

func transient(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	if e, ok := errors.AsType[net.Error](err); ok {
		return e.Timeout() || e.Temporary()
	}
	return false
}

func randomDelay(cap time.Duration) (time.Duration, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(cap)+1))
	if err != nil {
		return 0, err
	}
	return time.Duration(n.Int64()), nil
}

func waitDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
