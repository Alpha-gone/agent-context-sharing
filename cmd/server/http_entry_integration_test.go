package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
)

// 경로 목록은 구현에서 복사해 생성하지 않고 SDD의 외부 HTTP 계약을 대조한다.
func TestARCH07AssembledHTTPEntriesIntegration(t *testing.T) {
	_, _, address := newServerContractDatabase(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("HTTP 진입점 인수에서 임베딩 제공자를 호출했습니다")
	}))
	t.Cleanup(provider.Close)
	cfg := arch06Configuration(t, address, provider.URL)
	assembly, err := assembleApplication(t.Context(), cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := assembly.close(nil, true); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewTLSServer(assembly.app.handler())
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(client.CloseIdleConnections)
	graphID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	graph := graphID.String()
	for _, entry := range []struct {
		path, allow string
		get, post   int
	}{
		{"/healthz", "GET, HEAD", 200, 405},
		{"/readyz", "GET, HEAD", 200, 405},
		{"/login", "GET, POST", 200, 401},
		{"/register", "GET, POST", 200, 400},
		{"/logout", "GET, POST", 200, 303},
		{"/jwks.json", "GET, HEAD", 200, 405},
		{"/.well-known/oauth-protected-resource/mcp", "GET, HEAD", 200, 405},
		{"/.well-known/oauth-authorization-server", "GET, HEAD", 200, 405},
		{"/authorize", "GET, POST", 400, 400},
		{"/token", "POST", 405, 400},
		{"/mcp", "POST", 405, 401},
		{"/connections", "GET, POST", 303, 303},
		{"/graphs", "GET, POST", 303, 303},
		{"/access", "GET, POST", 303, 303},
		{"/graphs/" + graph, "GET", 303, 405},
		{"/graphs/" + graph + "/access", "GET, POST", 303, 303},
		{"/graphs/" + graph + "/deletion", "GET, POST", 303, 303},
		{"/graphs/" + graph + "/audit", "GET", 303, 405},
		{"/operator/restores", "GET, POST", 303, 303},
		{"/assets/app.css", "GET, HEAD", 200, 405},
		{"/assets/PretendardVariable.woff2", "GET, HEAD", 200, 405},
		{"/assets/cytoscape.min.js", "GET, HEAD", 200, 405},
		{"/assets/CYTOSCAPE-LICENSE.txt", "GET, HEAD", 200, 405},
		{"/assets/PRETENDARD-LICENSE.txt", "GET, HEAD", 200, 405},
	} {
		t.Run(entry.path, func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS"} {
				t.Run(method, func(t *testing.T) {
					request, err := http.NewRequestWithContext(t.Context(), method, server.URL+entry.path, strings.NewReader(""))
					if err != nil {
						t.Fatal(err)
					}
					request.Host = "untrusted-host.test"
					request.Header.Set("X-Forwarded-Host", "untrusted-host.test")
					request.Header.Set("X-Correlation-ID", "untrusted-correlation-marker")
					request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					if entry.path == "/mcp" {
						request.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
						request.ContentLength = -1
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Accept", "application/json, text/event-stream")
						request.Header.Set("MCP-Protocol-Version", "2026-07-28")
						request.Header.Set("Mcp-Method", "tools/list")
					}
					response, err := client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					data, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					want := 405
					if method == "GET" {
						want = entry.get
					} else if method == "POST" {
						want = entry.post
					} else if method == "HEAD" && strings.Contains(entry.allow, "HEAD") {
						want = entry.get
					}
					if response.StatusCode != want {
						t.Fatalf("상태 %d, 기대 %d", response.StatusCode, want)
					}
					if want == 405 && response.Header.Get("Allow") != entry.allow {
						t.Fatalf("메서드 허용값 %q, 기대 %q", response.Header.Get("Allow"), entry.allow)
					}
					id, err := model.ParseID(response.Header.Get("X-Correlation-ID"))
					if err != nil || !id.IsV7() {
						t.Fatal("외부 상관 헤더 대신 서버 UUIDv7을 발급하지 않았습니다")
					}
					if method == "HEAD" && len(data) != 0 {
						t.Fatal("HEAD 응답에 본문이 전송됐습니다")
					}
					if !strings.HasPrefix(entry.path, "/assets/") && entry.path != "/healthz" && entry.path != "/readyz" && response.Header.Get("Cache-Control") != "no-store" {
						t.Fatal("동적 응답의 no-store가 없습니다")
					}
					if strings.HasPrefix(entry.path, "/.well-known/") && method == "GET" && (strings.Contains(string(data), "untrusted-host.test") || !strings.Contains(string(data), cfg.AuthorizationServerURL.String())) {
						t.Fatal("발견 메타데이터가 고정 issuer를 유지하지 않았습니다")
					}
					plainBody := &arch07Body{Reader: strings.NewReader("x")}
					plain := httptest.NewRequest(method, entry.path, nil)
					plain.Body = plainBody
					plainResponse := httptest.NewRecorder()
					assembly.app.handler().ServeHTTP(plainResponse, plain)
					plainWant := 400
					if (entry.path == "/healthz" || entry.path == "/readyz") && (method == "GET" || method == "HEAD") {
						plainWant = 200
					}
					if plainResponse.Code != plainWant || plainBody.read != 0 || plainResponse.Header().Get("Allow") != "" || plainResponse.Header().Get("Location") != "" || len(plainResponse.Result().Cookies()) != 0 {
						t.Fatal("전송 판정 전에 메서드·본문·업무 경계를 통과했습니다")
					}
				})
			}
		})
	}

	// 실제 세션 검증과 본인 연결 관리가 같은 조립에 연결됐는지 대조한다.
	service, err := authz.New(assembly.database, authz.Config{Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	account, err := service.Register(t.Context(), "arch07", "synthetic password")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.WebSession(t.Context(), account, "web")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "https://service.test/connections", nil)
	request.AddCookie(&http.Cookie{Name: "agent_context_session", Value: session.Raw})
	response := httptest.NewRecorder()
	assembly.app.handler().ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "연결") {
		t.Fatal("실제 세션의 연결 관리 목록을 조회하지 못했습니다")
	}
	t.Run("body and rejection order", func(t *testing.T) { arch07RejectionOrder(t, assembly.app) })
	t.Run("management form bound", func(t *testing.T) {
		body := &arch07Body{Reader: strings.NewReader(strings.Repeat("x", 10<<20+2))}
		request := httptest.NewRequest("POST", "https://service.test/graphs", nil)
		request.Body, request.ContentLength = body, -1
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: "agent_context_session", Value: session.Raw})
		response := httptest.NewRecorder()
		assembly.app.handler().ServeHTTP(response, request)
		if response.Code != 400 || body.read != 10<<20+1 {
			t.Fatalf("관리 양식의 바이트 상한: 상태 %d, 읽기 %d", response.Code, body.read)
		}
	})
}

type arch07Body struct {
	io.Reader
	read int
}

func (body *arch07Body) Read(data []byte) (int, error) {
	n, err := body.Reader.Read(data)
	body.read += n
	return n, err
}
func (*arch07Body) Close() error { return nil }

func arch07RejectionOrder(t *testing.T, app *application) {
	t.Helper()
	for _, test := range []struct {
		name, method, path, origin string
		size                       int
		unknown, plain, read       bool
		status                     int
	}{
		{"plain", "POST", "/mcp", "", 4<<20 + 1, false, true, false, 400},
		{"unknown", "POST", "/missing", "https://cross.test", 64<<10 + 1, true, false, false, 404},
		{"unknown asset", "POST", "/assets/private.txt", "https://cross.test", 1, true, false, false, 404},
		{"unknown graph child", "POST", "/graphs/invalid/extra/path", "https://cross.test", 1, true, false, false, 404},
		{"method", "PUT", "/connections", "", 8<<10 + 1, true, false, false, 405},
		{"mcp origin", "GET", "/mcp", "https://cross.test", 4<<20 + 1, true, false, false, 403},
		{"mcp method", "GET", "/mcp", "", 4<<20 + 1, true, false, false, 405},
		{"mcp declared", "POST", "/mcp", "", 4<<20 + 1, false, false, false, 413},
		{"mcp streamed", "POST", "/mcp", "", 4<<20 + 1, true, false, true, 413},
		{"mcp auth before JSON", "POST", "/mcp", "", 1, true, false, true, 401},
		{"web origin", "POST", "/login", "https://cross.test", 8<<10 + 1, true, false, false, 403},
		{"login declared", "POST", "/login", "", 8<<10 + 1, false, false, false, 413},
		{"login streamed", "POST", "/login", "", 8<<10 + 1, true, false, true, 413},
		{"register streamed", "POST", "/register", "", 8<<10 + 1, true, false, true, 413},
		{"token declared", "POST", "/token", "", 64<<10 + 1, false, false, false, 413},
		{"token streamed", "POST", "/token", "", 64<<10 + 1, true, false, true, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &arch07Body{Reader: strings.NewReader(strings.Repeat("x", test.size))}
			request := httptest.NewRequest(test.method, "https://service.test"+test.path, nil)
			request.Body = body
			request.ContentLength = int64(test.size)
			if test.unknown {
				request.ContentLength = -1
			}
			if test.plain {
				request.TLS = nil
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if test.path == "/mcp" {
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Accept", "application/json, text/event-stream")
				request.Header.Set("MCP-Protocol-Version", "2026-07-28")
				request.Header.Set("Mcp-Method", "tools/list")
			}
			response := httptest.NewRecorder()
			app.handler().ServeHTTP(response, request)
			if response.Code != test.status || (body.read > 0) != test.read || body.read > test.size {
				t.Fatalf("거부 상태/읽기: %d/%d, 기대 %d/%t", response.Code, body.read, test.status, test.read)
			}
			if response.Header().Get("Location") != "" || len(response.Result().Cookies()) != 0 {
				t.Fatal("거부에 리디렉션 또는 세션 쿠키가 있습니다")
			}
			if test.path == "/missing" && response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("미등록 경로의 캐시 방지가 없습니다")
			}
		})
	}
}

type arch07Readiness func(context.Context) error

func (ping arch07Readiness) Ping(ctx context.Context) error { return ping(ctx) }

func TestARCH07RequestContextAndCancellation(t *testing.T) {
	var observed context.Context
	var correlation string
	app := newApplication(arch07Readiness(func(ctx context.Context) error {
		observed = ctx
		correlation = requestID(ctx)
		if correlation == "" || diagnostic.TraceFromContext(ctx).CorrelationID() != correlation || model.CorrelationID(ctx).String() != correlation {
			t.Error("하위 호출의 모델·진단 상관 ID가 다릅니다")
		}
		return nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true})
	parent, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil).WithContext(parent))
	wantDeadline, _ := parent.Deadline()
	if observed == nil {
		t.Fatal("준비 확인에 context가 전달되지 않았습니다")
	}
	deadline, ok := observed.Deadline()
	if !ok || !deadline.Equal(wantDeadline) || response.Header().Get("X-Correlation-ID") != correlation || !errors.Is(observed.Err(), context.Canceled) {
		t.Fatal("상위 마감·요청 상관 ID·반환 후 context 정리가 다릅니다")
	}
	observed = nil
	cancel()
	app.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/readyz", nil).WithContext(parent))
	if observed != nil {
		t.Fatal("사전 취소 뒤 업무를 시작했습니다")
	}
}

func TestARCH07CommonRequestBudget(t *testing.T) {
	for _, path := range []string{"/login", "/token", "/mcp", "/readyz"} {
		for _, mode := range []string{"timeout", "started", "read deadline failure"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				response := httptest.NewRecorder()
				var writer http.ResponseWriter = response
				if mode == "read deadline failure" {
					writer = mcp08ReadDeadlineFailure{response}
				}
				called := false
				next := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					called = true
					if mode == "started" {
						w.WriteHeader(202)
						w.Write([]byte("preserved"))
					}
					<-request.Context().Done()
				})
				withRequestBudget(next, 10*time.Millisecond, httpBudgetFailure).ServeHTTP(writer, httptest.NewRequest("GET", path, nil))
				if called != (mode != "read deadline failure") {
					t.Fatal("읽기 마감 설정 실패 뒤 업무가 시작됐습니다")
				}
				if mode == "started" {
					if response.Code != 202 || response.Body.String() != "preserved" {
						t.Fatal("시작한 응답을 덧썼습니다")
					}
					return
				}
				var result map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if response.Code != 500 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("WWW-Authenticate") != "" {
					t.Fatal("공통 시간 예산 오류의 외피가 다릅니다")
				}
				if slices.Contains([]string{"/mcp", "/token"}, path) {
					if result["error"] != "server_error" {
						t.Fatal("OAuth/MCP 오류 외피가 다릅니다")
					}
				} else if result["status"] != "internal_error" {
					t.Fatal("공통 상태 오류 외피가 다릅니다")
				}
			})
		}
	}
}

func TestARCH07WireCancellation(t *testing.T) {
	started := make(chan context.Context, 1)
	stopped := make(chan struct{})
	app := newApplication(arch07Readiness(func(ctx context.Context) error {
		started <- ctx
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true})
	server := httptest.NewTLSServer(app.handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(request)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	var observed context.Context
	select {
	case observed = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("실제 HTTP 요청이 시작되지 않았습니다")
	}
	deadline, ok := observed.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > readTimeout {
		t.Fatal("실제 수신 context의 공통 마감이 없습니다")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP 단절이 하위 호출에 전파되지 않았습니다")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("클라이언트 취소 오류: %v", err)
	}
	response, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("취소 뒤 상태 확인 요청에 응답하지 못했습니다")
	}
}

type arch07TokenRoute func(http.ResponseWriter, *http.Request)

func (handler arch07TokenRoute) Token(w http.ResponseWriter, r *http.Request) { handler(w, r) }

func TestARCH07WireBodyReadDeadline(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{
		trustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")},
	})
	readResult := make(chan error, 1)
	app.oauthTokens = arch07TokenRoute(func(_ http.ResponseWriter, request *http.Request) {
		_, err := io.ReadAll(request.Body)
		readResult <- err
	})
	server := httptest.NewServer(withRequestBudget(app.handler(), 40*time.Millisecond, httpBudgetFailure))
	t.Cleanup(server.Close)
	connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(connection, "POST /token HTTP/1.1\r\nHost: service.test\r\nX-Forwarded-Proto: https\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nx")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 500 || string(data) != `{"error":"server_error"}` || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("WWW-Authenticate") != "" {
		t.Fatal("공통 본문 읽기 마감의 응답 외피가 다릅니다", response.StatusCode, string(data), err)
	}
	if err := <-readResult; err == nil {
		t.Fatal("미완성 본문 읽기가 소켓 마감으로 중단되지 않았습니다")
	}
}

func TestARCH07KeepAliveAfterRequestDeadline(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true})
	server := httptest.NewTLSServer(withRequestBudget(app.handler(), 40*time.Millisecond, httpBudgetFailure))
	t.Cleanup(server.Close)
	t.Cleanup(server.Client().CloseIdleConnections)
	for i := range 2 {
		reused := false
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})
		request, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode != 200 || i == 1 && !reused {
			t.Fatal("완료한 요청의 읽기 마감이 keep-alive의 후속 요청을 방해했습니다")
		}
		if i == 0 {
			<-time.After(80 * time.Millisecond)
		}
	}
}
