package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/contract"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testAuthentication struct {
	ready      atomic.Bool
	authorizes atomic.Int32
	refreshes  atomic.Int32
	onAuth     func(context.Context) error
	onRefresh  func(context.Context) error
}

func (a *testAuthentication) Identity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !a.ready.Load() {
		return "", authorize.ErrAuthorization
	}
	return "account-a", nil
}

func (a *testAuthentication) Apply(ctx context.Context, r *http.Request) (authorize.Credential, error) {
	if _, err := a.Identity(ctx); err != nil {
		return authorize.Credential{}, err
	}
	r.Header.Set("Authorization", "DPoP test-token")
	r.Header.Set("DPoP", uuid.NewV7().String())
	return authorize.Credential{}, nil
}

func (a *testAuthentication) Reauthorize(ctx context.Context, _ authorize.Credential, _ authorize.Challenge) (authorize.Credential, error) {
	a.authorizes.Add(1)
	if a.onAuth != nil {
		if err := a.onAuth(ctx); err != nil {
			return authorize.Credential{}, err
		}
	}
	a.ready.Store(true)
	return authorize.Credential{}, nil
}

func (a *testAuthentication) Refresh(ctx context.Context, h http.Header) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(h.Values("Mcp-Access-Token")) > 0 {
		a.refreshes.Add(1)
	}
	if a.onRefresh != nil {
		return a.onRefresh(ctx)
	}
	return nil
}

func remoteConfig(t *testing.T, timeout string) config.Config {
	t.Helper()
	return remoteTimeoutConfig(t, timeout, "5s")
}

func remoteTimeoutConfig(t *testing.T, requestTimeout, authTimeout string) config.Config {
	t.Helper()
	env := map[string]string{"AGENT_CONTEXT_CLIENT_REMOTE_URL": "https://resource.test/mcp", "AGENT_CONTEXT_CLIENT_ID": "client", "AGENT_CONTEXT_CLIENT_AGENT_ID": uuid.NewV7().String(), "AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": authTimeout, "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": requestTimeout}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newHTTPTestClient(t *testing.T, auth Authentication, rt http.RoundTripper, options HTTPOptions) *Client {
	t.Helper()
	options.Transport = rt
	if options.Now == nil {
		options.Now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	}
	if options.Jitter == nil {
		options.Jitter = func(time.Duration) (time.Duration, error) { return 0, nil }
	}
	c, err := NewHTTP(remoteConfig(t, "5s"), auth, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func readyAuthentication() *testAuthentication {
	a := new(testAuthentication)
	a.ready.Store(true)
	return a
}

func wireID(t *testing.T, r *http.Request) string {
	t.Helper()
	var request struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name string                    `json:"name"`
			Meta map[string]jsontext.Value `json:"_meta"`
		} `json:"params"`
	}
	if err := json.UnmarshalRead(r.Body, &request); err != nil {
		t.Fatal(err)
	}
	if r.Method != http.MethodPost || r.URL.String() != "https://resource.test/mcp" || r.GetBody != nil || r.ContentLength <= 0 {
		t.Error("단일 POST·전송 대상·본문 replay 계약이 다릅니다")
	}
	for key, value := range map[string]string{"MCP-Protocol-Version": protocolVersion, "Mcp-Method": request.Method, "Content-Type": "application/json", "Accept": "application/json", "Accept-Encoding": "gzip"} {
		if len(r.Header.Values(key)) != 1 || r.Header.Get(key) != value {
			t.Errorf("헤더 %s 계약이 다릅니다", key)
		}
	}
	if request.Method == "tools/call" && r.Header.Get("Mcp-Name") != request.Params.Name || request.Method != "tools/call" && r.Header.Get("Mcp-Name") != "" {
		t.Error("도구 헤더 계약이 다릅니다")
	}
	var version string
	var capabilities struct {
		Extensions map[string]jsontext.Value `json:"extensions"`
	}
	if json.Unmarshal(request.Params.Meta["io.modelcontextprotocol/protocolVersion"], &version) != nil || version != protocolVersion || json.Unmarshal(request.Params.Meta["io.modelcontextprotocol/clientCapabilities"], &capabilities) != nil || capabilities.Extensions[idempotencyExtension] == nil {
		t.Error("요청별 revision·capability 선언이 다릅니다")
	}
	parsed, err := uuid.Parse(request.ID)
	if err != nil || parsed[6]>>4 != 7 {
		t.Error("원격 요청 식별자가 UUIDv7이 아닙니다")
	}
	return request.ID
}

func rpcResponse(id string, result jsontext.Value) *http.Response {
	body := []byte(`{"jsonrpc":"2.0","id":"` + id + `","result":` + string(result) + `}`)
	return rawResponse(http.StatusOK, body)
}

func rawResponse(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, ContentLength: int64(len(body)), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
}

func challengeResponse(kind string) *http.Response {
	r := rawResponse(http.StatusUnauthorized, nil)
	value := `DPoP resource_metadata="https://resource.test/.well-known/oauth-protected-resource", scope="agent-context"`
	if kind != "" {
		value += `, error="` + kind + `"`
	}
	r.Header.Set("WWW-Authenticate", value)
	return r
}

func executeTest(ctx context.Context, c *Client, name string) (jsontext.Value, error) {
	ctx, done := c.callContext(ctx)
	defer done()
	return c.source.CallTool(ctx, name, jsontext.Value(`{}`))
}

func TestHTTPRetryDeliveryMatrixAndFreshAttempts(t *testing.T) {
	for _, test := range []struct {
		name, tool, delivery string
		negotiated           bool
		want                 error
		attempts             int
	}{
		{"read-before", "graph_list", "before", false, contract.ErrTransport, 4},
		{"read-partial", "graph_list", "partial", false, contract.ErrTransport, 4},
		{"read-after", "graph_list", "after", false, contract.ErrTransport, 4},
		{"write-before", "graph_create", "before", false, contract.ErrTransport, 4},
		{"write-partial", "graph_create", "partial", false, contract.ErrIndeterminate, 1},
		{"write-after", "graph_create", "after", false, contract.ErrIndeterminate, 1},
		{"write-unknown", "graph_create", "unknown", false, contract.ErrIndeterminate, 1},
		{"negotiated-partial", "graph_create", "partial", true, contract.ErrIndeterminate, 4},
		{"negotiated-after", "graph_create", "after", true, contract.ErrIndeterminate, 4},
		{"negotiated-before", "graph_create", "before", true, contract.ErrTransport, 4},
		{"negotiated-unknown", "graph_create", "unknown", true, contract.ErrIndeterminate, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			ids, proofs, keys := map[string]bool{}, map[string]bool{}, map[string]bool{}
			var caps []time.Duration
			c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				proofs[r.Header.Get("DPoP")] = true
				if key := r.Header.Get("Idempotency-Key"); key != "" {
					keys[key] = true
				}
				trace := httptrace.ContextClientTrace(r.Context())
				if test.delivery == "before" {
					trace.GetConn("resource.test:443")
					return nil, io.EOF
				}
				if test.delivery == "after" {
					ids[wireID(t, r)] = true
				}
				if test.delivery == "partial" {
					if _, err := io.CopyN(io.Discard, r.Body, 1); err != nil {
						t.Fatal(err)
					}
				}
				return nil, io.EOF
			}), HTTPOptions{Jitter: func(cap time.Duration) (time.Duration, error) { caps = append(caps, cap); return 0, nil }})
			c.source.(*httpSource).negotiated.Store(test.negotiated)
			if _, err := executeTest(t.Context(), c, test.tool); !errors.Is(err, test.want) || attempts != test.attempts {
				t.Fatalf("전송 결과 %v, 시도 %d; 기대 %v, %d", err, attempts, test.want, test.attempts)
			}
			if len(proofs) != attempts || test.delivery == "after" && len(ids) != attempts {
				t.Fatal("시도마다 proof·원격 ID를 새로 만들지 않았습니다")
			}
			if test.negotiated && len(keys) != 1 || !test.negotiated && len(keys) != 0 {
				t.Fatal("협상된 쓰기 키의 생성·재사용 계약이 다릅니다")
			}
			if attempts == 4 && !slices.Equal(caps, []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}) {
				t.Fatal("재시도 지연 상한이 다릅니다")
			}
		})
	}
}

func TestHTTPAuthorizationAndRetryShareBudgets(t *testing.T) {
	a := readyAuthentication()
	attempts := 0
	keys, ids, proofs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	c := newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		id := wireID(t, r)
		ids[id], keys[r.Header.Get("Idempotency-Key")], proofs[r.Header.Get("DPoP")] = true, true, true
		if attempts == 2 {
			return challengeResponse("invalid_token"), nil
		}
		if attempts < 5 {
			return nil, io.EOF
		}
		return rpcResponse(id, jsontext.Value(`{"content":[],"isError":false}`)), nil
	}), HTTPOptions{})
	c.source.(*httpSource).negotiated.Store(true)
	if _, err := executeTest(t.Context(), c, "graph_create"); err != nil || attempts != 5 || a.authorizes.Load() != 1 || len(keys) != 1 || len(ids) != 5 || len(proofs) != 5 {
		t.Fatalf("재인가와 전송 재시도의 공통 상태: %v, %d회", err, attempts)
	}
	// 재인가 뒤에도 재시도 예산을 초기화하지 않는다.
	attempts = 0
	c.source.(*httpSource).http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		wireID(t, r)
		if attempts == 2 {
			return challengeResponse("invalid_token"), nil
		}
		return nil, io.EOF
	})
	if _, err := executeTest(t.Context(), c, "graph_create"); !errors.Is(err, contract.ErrIndeterminate) || attempts != 5 {
		t.Fatal("재인가가 전송 재시도 횟수를 초기화했습니다")
	}
}

func TestHTTPDoesNotRetryCompleteResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		response func(string) *http.Response
		want     error
		auths    int
	}{
		{"second-401", func(string) *http.Response { return challengeResponse("invalid_token") }, authorize.ErrAuthorization, 1},
		{"proof", func(string) *http.Response { return challengeResponse("invalid_dpop_proof") }, contract.ErrProtocol, 0},
		{"nonce", func(string) *http.Response { return challengeResponse("use_dpop_nonce") }, contract.ErrProtocol, 0},
		{"redirect", func(string) *http.Response {
			r := rawResponse(307, nil)
			r.Header.Set("Location", "https://other.test/mcp")
			return r
		}, contract.ErrProtocol, 0},
		{"internal", func(id string) *http.Response {
			return rawResponse(500, []byte(`{"jsonrpc":"2.0","id":"`+id+`","error":{"code":-32603,"message":"secret-server-error","data":{"version":7}}}`))
		}, nil, 0},
		{"bad-500", func(string) *http.Response { return rawResponse(500, []byte(`{}`)) }, contract.ErrProtocol, 0},
		{"domain", func(id string) *http.Response {
			return rpcResponse(id, jsontext.Value(`{"content":[],"isError":true,"structuredContent":{"error":{"code":"version_conflict","version":7}}}`))
		}, nil, 0},
		{"mismatched-id", func(string) *http.Response { return rpcResponse("other-id", jsontext.Value(`{}`)) }, contract.ErrProtocol, 0},
		{"duplicate", func(id string) *http.Response {
			return rawResponse(200, []byte(`{"jsonrpc":"2.0","id":"`+id+`","result":{},"result":{}}`))
		}, contract.ErrProtocol, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := readyAuthentication()
			attempts := 0
			c := newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) { attempts++; return test.response(wireID(t, r)), nil }), HTTPOptions{})
			_, err := executeTest(t.Context(), c, "graph_list")
			if test.name == "internal" {
				rpc, ok := errors.AsType[*ProtocolError](err)
				if !ok || !strings.Contains(string(rpc.Raw()), "secret-server-error") || strings.Contains(fmt.Sprintf("%v %#v", rpc, rpc), "secret-server-error") {
					t.Fatal("원격 RPC 오류의 보존·비노출 계약이 다릅니다")
				}
			} else if test.want != nil && !errors.Is(err, test.want) || test.want == nil && err != nil {
				t.Fatalf("오류 분류: %v", err)
			}
			if attempts != 1+test.auths || int(a.authorizes.Load()) != test.auths {
				t.Fatal("완전한 응답을 재시도했거나 재인가 판정이 다릅니다")
			}
		})
	}
	permanentCalls := 0
	c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(*http.Request) (*http.Response, error) {
		permanentCalls++
		return nil, x509.UnknownAuthorityError{}
	}), HTTPOptions{})
	if _, err := executeTest(t.Context(), c, "graph_list"); !errors.Is(err, contract.ErrTransport) || permanentCalls != 1 {
		t.Fatal("인증서 검증 오류를 반복했습니다")
	}
}

func TestHTTPClientCacheAuthorizationOutsideGate(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			a := readyAuthentication()
			a.ready.Store(!initial)
			fixture := newCatalogSource(t)
			var c *Client
			lists := 0
			c = newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
				id := wireID(t, r)
				switch r.Header.Get("Mcp-Method") {
				case "server/discover":
					return rpcResponse(id, fixture.discovery), nil
				case "tools/list":
					lists++
					if lists == 1 {
						return challengeResponse(map[bool]string{true: "", false: "invalid_token"}[initial]), nil
					}
					return rpcResponse(id, fixture.tools), nil
				default:
					return rpcResponse(id, fixture.result), nil
				}
			}), HTTPOptions{})
			a.onAuth = func(context.Context) error {
				select {
				case c.gate <- struct{}{}:
					<-c.gate
				default:
					t.Error("캐시 gate를 쥔 채 인가를 시작했습니다")
				}
				q := &c.source.(*httpSource).queue
				q.mu.Lock()
				defer q.mu.Unlock()
				if q.active != 0 || len(q.waiters) != 0 {
					t.Error("인가 대기와 원격 전송 slot을 동시에 점유했습니다")
				}
				return nil
			}
			result, err := c.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`))
			if err != nil || a.authorizes.Load() != 1 || !bytes.Equal(result.Raw(), fixture.result) {
				t.Fatalf("인가·발견·목록·호출: %v", err)
			}
			if !c.source.(*httpSource).negotiated.Load() {
				t.Fatal("검증된 발견의 멱등성 협상을 반영하지 않았습니다")
			}
		})
	}
}

func TestHTTPDeadlineCancellationAndClose(t *testing.T) {
	for _, action := range []string{"deadline", "cancel", "close", "retry-wait", "authorization"} {
		t.Run(action, func(t *testing.T) {
			a := readyAuthentication()
			started := make(chan struct{})
			var once sync.Once
			options := HTTPOptions{}
			if action == "retry-wait" {
				options.Wait = func(ctx context.Context, _ time.Duration) error {
					once.Do(func() { close(started) })
					<-ctx.Done()
					return ctx.Err()
				}
			}
			if action == "authorization" {
				a.ready.Store(false)
				a.onAuth = func(ctx context.Context) error { once.Do(func() { close(started) }); <-ctx.Done(); return ctx.Err() }
			}
			c := newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
				if action == "retry-wait" {
					httptrace.ContextClientTrace(r.Context()).GetConn("resource.test:443")
					return nil, io.EOF
				}
				if action == "authorization" {
					return challengeResponse(""), nil
				}
				once.Do(func() { close(started) })
				<-r.Context().Done()
				response := rawResponse(200, []byte(`{}`))
				response.Header.Set("Mcp-Access-Token", "late-token")
				return response, nil
			}), options)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if action == "deadline" {
				c.timeout = 50 * time.Millisecond
			}
			result := make(chan error, 1)
			go func() { _, err := c.ListTools(ctx); result <- err }()
			<-started
			if action == "close" {
				c.Close()
			} else if action != "deadline" {
				cancel()
			}
			err := <-result
			want := error(context.Canceled)
			if action == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || a.refreshes.Load() != 0 {
				t.Fatalf("취소·제한 시간·늦은 갱신: %v", err)
			}
		})
	}
}

func TestHTTPResponseLimitsAndGzip(t *testing.T) {
	for _, size := range []int{maxResponseBytes, maxResponseBytes + 1} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-%v", size, compressed), func(t *testing.T) {
				body := []byte(`{"padding":"` + strings.Repeat("x", size-len(`{"padding":""}`)) + `"}`)
				encoded := body
				if compressed {
					var buffer bytes.Buffer
					writer := gzip.NewWriter(&buffer)
					writer.Write(body)
					writer.Close()
					encoded = buffer.Bytes()
				}
				response := rawResponse(200, encoded)
				if compressed {
					response.Header.Set("Content-Encoding", "gzip")
				}
				got, err := responseBody(response)
				if size == maxResponseBytes && (err != nil || !bytes.Equal(got, body)) || size > maxResponseBytes && !errors.Is(err, errResponseLimit) {
					t.Fatalf("전송·해제 크기 경계: %v", err)
				}
			})
		}
	}
	response := rawResponse(200, nil)
	response.ContentLength = maxResponseBytes + 1
	if _, err := responseBody(response); !errors.Is(err, errResponseLimit) {
		t.Fatal("Content-Length 사전 검사를 생략했습니다")
	}
	response = rawResponse(200, []byte("not-gzip"))
	response.Header.Set("Content-Encoding", "gzip")
	if _, err := responseBody(response); err == nil {
		t.Fatal("손상된 압축 응답을 허용했습니다")
	}
}

func TestHTTPRequestLimitBeforeSending(t *testing.T) {
	requests := 0
	c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return rpcResponse(wireID(t, r), jsontext.Value(`{}`)), nil
	}), HTTPOptions{})
	source := c.source.(*httpSource)
	ctx, done := c.callContext(t.Context())
	defer done()
	base := jsontext.Value(`{"padding":""}`)
	// 같은 메타데이터와 고정 길이 UUID로 실제 전송 본문의 경계를 계산한다.
	var frameSize int64
	source.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		frameSize = r.ContentLength
		return rpcResponse(wireID(t, r), jsontext.Value(`{}`)), nil
	})
	if _, err := source.CallTool(ctx, "graph_list", base); err != nil {
		t.Fatal(err)
	}
	overhead := int(frameSize)
	for _, extra := range []int{0, 1} {
		args := jsontext.Value(`{"padding":"` + strings.Repeat("x", maxRequestBytes-overhead+extra) + `"}`)
		_, err := source.CallTool(ctx, "graph_list", args)
		if extra == 0 && err != nil || extra == 1 && !errors.Is(err, contract.ErrBusy) {
			t.Fatalf("요청 크기 경계: %v", err)
		}
	}
	if requests != 2 {
		t.Fatal("상한 초과 요청을 전송했습니다")
	}
}

func TestHTTPAdmissionBoundsCacheWaiters(t *testing.T) {
	a := readyAuthentication()
	fixture := newCatalogSource(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return rpcResponse(wireID(t, r), fixture.discovery), nil
	}), HTTPOptions{})
	q := &c.source.(*httpSource).queue
	results := make(chan error, 136)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, err := c.Discover(t.Context()); results <- err })
	}
	<-started
	await(t, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.active == 8 })
	var cancelQueued context.CancelFunc
	for index := range 128 {
		ctx := t.Context()
		if index == 40 {
			ctx, cancelQueued = context.WithCancel(ctx)
		}
		wg.Go(func() { _, err := c.Discover(ctx); results <- err })
		await(t, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.waiters) == index+1 })
	}
	if _, err := c.Discover(t.Context()); !errors.Is(err, contract.ErrBusy) {
		t.Fatal("캐시 gate 대기를 호출 대기 상한에서 제외했습니다")
	}
	cancelQueued()
	await(t, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.waiters) == 127 })
	close(release)
	wg.Wait()
	close(results)
	canceled := 0
	for err := range results {
		if errors.Is(err, context.Canceled) {
			canceled++
		} else if err != nil {
			t.Error(err)
		}
	}
	if canceled != 1 || q.active != 0 || len(q.waiters) != 0 {
		t.Fatal("대기 취소가 다른 호출을 오염시켰거나 slot이 남았습니다")
	}
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }

func TestHTTPPartialBodiesTimeoutAndRetention(t *testing.T) {
	for _, test := range []struct {
		tool       string
		negotiated bool
		want       error
		attempts   int
	}{{"graph_list", false, contract.ErrTransport, 4}, {"graph_create", false, contract.ErrIndeterminate, 1}, {"graph_create", true, contract.ErrIndeterminate, 4}} {
		t.Run(fmt.Sprintf("partial-%s-%v", test.tool, test.negotiated), func(t *testing.T) {
			attempts := 0
			c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				wireID(t, r)
				response := rawResponse(200, nil)
				response.ContentLength = -1
				response.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0",`), failedReader{io.ErrUnexpectedEOF}))
				return response, nil
			}), HTTPOptions{})
			c.source.(*httpSource).negotiated.Store(test.negotiated)
			if _, err := executeTest(t.Context(), c, test.tool); !errors.Is(err, test.want) || attempts != test.attempts {
				t.Fatalf("본문 수신 실패: %v, %d회", err, attempts)
			}
		})
	}
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout-%v", delivered), func(t *testing.T) {
			c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
				if delivered {
					wireID(t, r)
				} else {
					httptrace.ContextClientTrace(r.Context()).GetConn("resource.test:443")
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			}), HTTPOptions{})
			c.timeout = 20 * time.Millisecond
			_, err := executeTest(t.Context(), c, "graph_create")
			want := error(context.DeadlineExceeded)
			if delivered {
				want = contract.ErrIndeterminate
			}
			if !errors.Is(err, want) {
				t.Fatalf("쓰기 제한 시간: %v", err)
			}
		})
	}
	now := time.Unix(1_800_000_000, 0)
	attempts := 0
	c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) { attempts++; wireID(t, r); return nil, io.EOF }), HTTPOptions{Now: func() time.Time { return now }, Wait: func(context.Context, time.Duration) error { now = now.Add(24 * time.Hour); return nil }})
	c.source.(*httpSource).negotiated.Store(true)
	if _, err := executeTest(t.Context(), c, "graph_create"); !errors.Is(err, contract.ErrIndeterminate) || attempts != 1 {
		t.Fatal("멱등성 보관 시간 이후 불확실 쓰기를 재전송했습니다")
	}
}

func TestHTTPLateRefreshAndIdentityFailures(t *testing.T) {
	for _, failure := range []error{contract.ErrProtocol, contract.ErrIdentityChanged} {
		t.Run(failure.Error(), func(t *testing.T) {
			a := readyAuthentication()
			a.onRefresh = func(context.Context) error { return failure }
			attempts := 0
			c := newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				return rpcResponse(wireID(t, r), jsontext.Value(`{}`)), nil
			}), HTTPOptions{})
			if _, err := executeTest(t.Context(), c, "graph_list"); !errors.Is(err, failure) || attempts != 1 || a.authorizes.Load() != 0 {
				t.Fatal("계정·갱신 검증 실패가 재인가나 재전송을 시작했습니다")
			}
		})
	}
	a := readyAuthentication()
	a.onAuth = func(context.Context) error { return contract.ErrIdentityChanged }
	c := newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
		wireID(t, r)
		return challengeResponse("invalid_token"), nil
	}), HTTPOptions{})
	if _, err := executeTest(t.Context(), c, "graph_list"); !errors.Is(err, contract.ErrIdentityChanged) || a.authorizes.Load() != 1 {
		t.Fatal("재인가 계정 전환을 반복했거나 오류를 바꿨습니다")
	}
}

func TestHTTPWriteKeysAllToolsAndWithoutNegotiation(t *testing.T) {
	var tools []contract.Tool
	if err := json.Unmarshal(contract.Manifest(), &tools); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		kind, _ := contract.Classify(r.Header.Get("Mcp-Name"))
		key := r.Header.Get("Idempotency-Key")
		if kind == contract.Write {
			value, err := strconv.Unquote(key)
			parsed, e := uuid.Parse(value)
			if err != nil || e != nil || parsed[6]>>4 != 7 || keys[key] {
				t.Error("쓰기 키가 Structured Fields UUIDv7이 아니거나 다른 호출과 중복됩니다")
			}
			keys[key] = true
		} else if key != "" {
			t.Error("읽기에 멱등성 키를 보냈습니다")
		}
		return rpcResponse(id, jsontext.Value(`{}`)), nil
	}), HTTPOptions{})
	c.source.(*httpSource).negotiated.Store(true)
	for _, tool := range tools {
		if _, err := executeTest(t.Context(), c, tool.Name); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 8 {
		t.Fatal("협상된 쓰기 8종에 키를 생성하지 않았습니다")
	}
	fixture := newCatalogSource(t)
	fixture.discovery = changeField(t, fixture.discovery, "capabilities", map[string]any{"tools": map[string]any{}})
	writeRequests := 0
	unnegotiated := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		switch r.Header.Get("Mcp-Method") {
		case "server/discover":
			return rpcResponse(id, fixture.discovery), nil
		case "tools/list":
			return rpcResponse(id, fixture.tools), nil
		default:
			writeRequests++
			if r.Header.Get("Idempotency-Key") != "" {
				t.Error("비협상 쓰기에 키를 보냈습니다")
			}
			return nil, io.EOF
		}
	}), HTTPOptions{})
	if _, err := unnegotiated.CallTool(t.Context(), "graph_create", jsontext.Value(`{"name":"write-test"}`)); !errors.Is(err, contract.ErrIndeterminate) || writeRequests != 1 {
		t.Fatal("비협상 쓰기의 응답 유실을 자동 재시도했습니다")
	}
}

func TestHTTPCompleteMalformedCompressionIsProtocolFailure(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("bad-gzip"), {0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff}} {
		calls := 0
		c := newHTTPTestClient(t, readyAuthentication(), transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			wireID(t, r)
			response := rawResponse(200, data)
			response.Header.Set("Content-Encoding", "gzip")
			return response, nil
		}), HTTPOptions{})
		if _, err := executeTest(t.Context(), c, "graph_list"); !errors.Is(err, contract.ErrProtocol) || calls != 1 {
			t.Fatalf("완전한 잘못된 압축 응답을 통신 실패로 재시도했습니다: %v", err)
		}
	}
	response := rawResponse(200, []byte(strings.Repeat("x", maxResponseBytes+1)))
	response.ContentLength = -1
	if _, err := responseBody(response); !errors.Is(err, errResponseLimit) {
		t.Fatal("전송 길이 미선언의 초과 본문을 허용했습니다")
	}
}

func TestHTTPCacheEntryExpiryStartsAuthorizationOutsideGate(t *testing.T) {
	a := readyAuthentication()
	fixture := newCatalogSource(t)
	var c *Client
	c = newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		if r.Header.Get("Mcp-Method") == "server/discover" {
			return rpcResponse(id, fixture.discovery), nil
		}
		return rpcResponse(id, fixture.tools), nil
	}), HTTPOptions{})
	// 인가 준비는 끝났지만 gate를 얻기 전에 토큰이 사라진 경우를 주입한다.
	prepare := c.prepare
	c.prepare = func(ctx context.Context) error {
		if err := prepare(ctx); err != nil {
			return err
		}
		a.ready.Store(false)
		return nil
	}
	a.onAuth = func(context.Context) error {
		select {
		case c.gate <- struct{}{}:
			<-c.gate
		default:
			t.Error("gate 안에서 만료 인가를 시작했습니다")
		}
		return nil
	}
	if _, err := c.ListTools(t.Context()); err != nil || a.authorizes.Load() != 1 {
		t.Fatalf("캐시 조회 전 만료 인가: %v", err)
	}
}
