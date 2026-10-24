package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/web"
)

// readiness는 준비 확인과 종료에 필요한 데이터베이스 연결의 최소 계약이다.
// 풀을 닫는 것은 조립한 곳의 책임이므로 이 계약에는 준비 확인만 둔다.
type readiness interface {
	Ping(context.Context) error
}

// transportSecurity은 수신 연결의 TLS 여부를 판단하는 배포 경계다.
type transportSecurity struct {
	// directTLS 필드는 애플리케이션이 TLS 수신기를 직접 열었는지 나타낸다.
	directTLS bool
	// trustedProxies 필드에는 전달 헤더를 신뢰할 역방향 프록시의 주소 대역을 둔다.
	// 비어 있으면 어떤 상대의 전달 헤더도 TLS 판정에 쓰지 않는다.
	trustedProxies []netip.Prefix
}

// publicKeyRoutes는 웹 세션의 서명 공개 키 처리기다.
type publicKeyRoutes interface {
	JWKS(http.ResponseWriter, *http.Request)
}

type oauthMetadataRoutes interface {
	ProtectedResourceMetadata(http.ResponseWriter, *http.Request)
	AuthorizationServerMetadata(http.ResponseWriter, *http.Request)
}

type oauthTokenRoutes interface {
	Token(http.ResponseWriter, *http.Request)
}

// application은 HTTP 경로와 공통 요청 경계를 관리한다.
type application struct {
	diagnostics   diagnostic.Observer
	database      readiness
	logger        *slog.Logger
	transport     transportSecurity
	web           *web.Server
	publicKeys    publicKeyRoutes
	oauthMetadata oauthMetadataRoutes
	oauthTokens   oauthTokenRoutes
	mcp           http.Handler
	accepting     atomic.Bool
}

// statusResponse는 상태 확인 경로가 반환하는 민감 정보 없는 본문이다.
type statusResponse struct {
	Status string `json:"status"`
}

// newApplication은 데이터베이스 준비 확인, 구조화 로그와 TLS 신뢰 경계를 주입해 상태 경로를 만든다.
func newApplication(database readiness, logger *slog.Logger, transport transportSecurity) *application {
	if logger == nil {
		logger = slog.Default()
	}
	app := &application{database: database, logger: logger, transport: transport}
	app.accepting.Store(true)
	return app
}

// handler는 상태·업무 경로에 공통 전송 판정과 안전한 구조화 로그를 연결한다.
//
// 상태 확인 두 경로만 TLS 판정의 예외이며 나머지는 라우터보다 먼저 확인한다.
// FR-AGENT_CONTEXT-140의 프로브는 프록시를 거치지 않고 오기 때문이다.
//
// 등록되지 않은 경로도 판정 뒤에 둔다. 평문 요청에 어떤 경로가 있는지 알리지 않는다.
func (app *application) handler() http.Handler {
	mux := http.NewServeMux()
	handleHTTPMethods(mux, "/healthz", http.HandlerFunc(app.health), "GET", "HEAD")
	handleHTTPMethods(mux, "/readyz", http.HandlerFunc(app.ready), "GET", "HEAD")
	if app.publicKeys != nil {
		handleHTTPMethods(mux, "/jwks.json", http.HandlerFunc(app.publicKeys.JWKS), "GET", "HEAD")
	}
	if app.oauthMetadata != nil {
		for path, handler := range map[string]http.HandlerFunc{
			"/.well-known/oauth-protected-resource/mcp": app.oauthMetadata.ProtectedResourceMetadata,
			"/.well-known/oauth-authorization-server":   app.oauthMetadata.AuthorizationServerMetadata,
		} {
			handleHTTPMethods(mux, path, handler, "GET", "HEAD")
		}
	}
	if app.web != nil {
		for _, path := range []string{"/login", "/register", "/logout", "/authorize", "/connections", "/graphs", "/access", "/graphs/{graph_id}/access", "/graphs/{graph_id}/deletion", "/operator/restores"} {
			handleHTTPMethods(mux, path, app.web, "GET", "POST")
		}
		for _, path := range []string{"/graphs/{graph_id}", "/graphs/{graph_id}/audit"} {
			handleHTTPMethods(mux, path, app.web, "GET")
		}
		for _, name := range []string{"app.css", "PretendardVariable.woff2", "cytoscape.min.js", "CYTOSCAPE-LICENSE.txt", "PRETENDARD-LICENSE.txt"} {
			handleHTTPMethods(mux, "/assets/"+name, app.web, "GET", "HEAD")
		}
	}
	if app.oauthTokens != nil {
		// OAuth와 MCP는 메서드 거부의 채널별 외피·Origin 우선 순서를 소유한다.
		mux.HandleFunc("POST /token", app.oauthTokens.Token)
		mux.HandleFunc("/token", app.oauthTokens.Token)
	}
	if app.mcp != nil {
		mux.Handle("POST /mcp", app.mcp)
		mux.Handle("/mcp", app.mcp)
	}
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		http.NotFound(writer, request)
	})
	bounded := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// 채널이 더 좁은 제한·오류 순서를 적용하도록 여기서는 읽지 않는다.
		request.Body = http.MaxBytesReader(writer, request.Body, 10<<20)
		mux.ServeHTTP(writer, request)
	})
	return app.logRequests(app.recoverPanics(app.requireTLS(withRequestBudget(bounded, readTimeout, httpBudgetFailure))))
}

func handleHTTPMethods(mux *http.ServeMux, path string, handler http.Handler, methods ...string) {
	reject := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Allow", strings.Join(methods, ", "))
		writer.WriteHeader(http.StatusMethodNotAllowed)
	})
	mux.Handle(path, reject)
	for _, method := range methods {
		mux.Handle(method+" "+path, handler)
	}
	// ServeMux의 GET은 HEAD도 일치시키지만 기존 웹 계약은 GET만 받는다.
	if slices.Contains(methods, "GET") && !slices.Contains(methods, "HEAD") {
		mux.Handle("HEAD "+path, reject)
	}
}

// recoverPanics는 처리기에서 빠져나온 panic을 500으로 바꾸고 원인을 로그에 남긴다.
//
// 차단막이 없으면 net/http이 연결을 끊어 클라이언트는 응답 대신 끊긴 연결을 받고, 구조화
// 로그에도 요청이 남지 않는다. 처리기 오류도 요청 상태와 함께 기록하도록
// 이 미들웨어를 logRequests 안쪽에 둔다.
func (app *application) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			cause := recover()
			if cause == nil {
				return
			}
			// http.ErrAbortHandler은 처리기가 의도적으로 연결을 끊는 신호이므로 그대로 둔다.
			if cause == http.ErrAbortHandler {
				panic(cause)
			}
			app.logger.Error("요청 처리 중 panic",
				"correlation_id", requestID(request.Context()),
				"method", diagnosticMethod(request.Method), "route", diagnosticRoute(request.URL.Path), "reason_code", "handler_panic")
			writeStatus(writer, http.StatusInternalServerError, "internal_error")
		}()
		next.ServeHTTP(writer, request)
	})
}

// requireTLS는 배포가 정한 신뢰 경계에서 확인되지 않은 평문 요청을 처리하지 않는다.
func (app *application) requireTLS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		probe := (request.Method == http.MethodGet || request.Method == http.MethodHead) &&
			(request.URL.Path == "/healthz" || request.URL.Path == "/readyz")
		// ServeMux의 경로 정규화 리디렉션도 전송 판정 뒤에서만 실행한다.
		if !probe && !app.isTLSRequest(request) {
			writeStatus(writer, http.StatusBadRequest, "https_required")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// isTLSRequest는 직접 TLS 연결 또는 신뢰된 프록시가 전달한 HTTPS 표시만 수락한다.
//
// 프록시 배치에서 전달 헤더는 누구나 붙일 수 있으므로 헤더만으로 판정하지 않는다.
// 요청을 보낸 상대가 신뢰 대역 안에 있을 때에만 그 헤더를 읽으며, 대역 밖에서 온
// 요청은 헤더가 있어도 평문으로 다뤄 FR-AGENT_CONTEXT-140의 거부 대상이 된다.
func (app *application) isTLSRequest(request *http.Request) bool {
	return app.transport.isTLSRequest(request)
}

// isTLSRequest는 HTTP 요청이 현재 TLS 종단 계약을 충족하는지 판정한다.
func (security transportSecurity) isTLSRequest(request *http.Request) bool {
	if security.directTLS {
		return request.TLS != nil
	}
	peer, ok := remoteAddr(request.RemoteAddr)
	if !ok || !security.isTrustedProxy(peer) {
		return false
	}
	// 값이 하나일 때만 읽는다. 프록시가 헤더를 덮어쓰지 않고 덧붙이도록 설정되면 원
	// 요청자가 넣은 값이 앞에 남아, 신뢰된 프록시를 거친 요청에서도 위조가 통과한다.
	forwarded := request.Header.Values("X-Forwarded-Proto")
	if len(forwarded) != 1 {
		return false
	}
	return strings.ToLower(strings.Trim(forwarded[0], " \t")) == "https"
}

// isTrustedProxy는 요청을 보낸 상대가 전달 헤더를 신뢰할 대역에 속하는지 확인한다.
func (security transportSecurity) isTrustedProxy(peer netip.Addr) bool {
	return slices.ContainsFunc(security.trustedProxies, func(prefix netip.Prefix) bool {
		return prefix.Contains(peer)
	})
}

// clientAddress는 기존 HTTPS·프록시 경계 안에서만 Cloudflare의 단일 주소를 읽는다.
// 범용 전달 주소 목록은 사용자 입력을 포함하므로 출처별 예산의 키로 사용하지 않는다.
func (security transportSecurity) clientAddress(request *http.Request) netip.Addr {
	peer, _ := remoteAddr(request.RemoteAddr)
	if security.directTLS || !security.isTrustedProxy(peer) || !security.isTLSRequest(request) {
		return peer
	}
	values := request.Header.Values("CF-Connecting-IP")
	if len(values) == 1 {
		address, err := netip.ParseAddr(strings.Trim(values[0], " \t"))
		if err == nil && address.Zone() == "" {
			return address.Unmap()
		}
	}
	return peer
}

// remoteAddr는 http.Request.RemoteAddr의 host:port 표기에서 비교 가능한 주소를 뽑는다.
// IPv4-mapped IPv6로 들어온 상대도 구성에 적은 IPv4 대역과 맞도록 편다.
func remoteAddr(value string) (netip.Addr, bool) {
	peer, err := netip.ParseAddrPort(value)
	if err != nil || peer.Addr().Zone() != "" {
		return netip.Addr{}, false
	}
	return peer.Addr().Unmap(), true
}

// health는 프로세스가 HTTP 요청에 응답하는지만 확인한다.
func (app *application) health(writer http.ResponseWriter, _ *http.Request) {
	writeStatus(writer, http.StatusOK, "ok")
}

// ready는 새 트래픽을 받을 수 있고 데이터베이스 연결이 가능한지 확인한다.
func (app *application) ready(writer http.ResponseWriter, request *http.Request) {
	if !app.accepting.Load() {
		writeStatus(writer, http.StatusServiceUnavailable, "not_ready")
		return
	}
	if app.database == nil {
		writeStatus(writer, http.StatusServiceUnavailable, "not_ready")
		return
	}
	if err := app.database.Ping(request.Context()); err != nil {
		app.logger.Warn("준비 확인 실패", "correlation_id", requestID(request.Context()), "error", err)
		writeStatus(writer, http.StatusServiceUnavailable, "not_ready")
		return
	}
	writeStatus(writer, http.StatusOK, "ready")
}

// shutdown은 트래픽을 끊고 진행 중인 요청이 끝날 때까지 기다린다.
//
// 연결 풀은 여기에서 닫지 않는다. 풀을 쓰는 것이 요청 경로만이 아니라 색인 작업자도
// 있으므로, 닫는 순서를 조립한 곳이 정해야 작업자가 도는 중에 풀이 사라지지 않는다.
// 주기 작업의 자문 잠금도 조립한 곳이 이 메서드 뒤에 해제한다.
func (app *application) shutdown(ctx context.Context, server *http.Server) error {
	app.accepting.Store(false)
	if err := server.Shutdown(ctx); err != nil {
		// Shutdown은 기한 초과 시 진행 연결을 닫지 않는다. 취소를 전파하되
		// 취소를 따르지 않는 처리기의 반환까지 기다리지는 않는다.
		if closeErr := server.Close(); closeErr != nil {
			return fmt.Errorf("진행 요청 종료 대기: %w; HTTP 연결 종료: %w", err, closeErr)
		}
		return fmt.Errorf("진행 요청 종료 대기: %w", err)
	}
	return nil
}

// worker는 종료 순서가 기다리는 상시 작업자다. 색인 작업자와 주기 작업자가 이 모양이다.
// Close는 실행을 취소하고 ctx의 기한 안에 완료 여부를 반환해야 한다.
type worker interface {
	Close(context.Context) error
}

// shutdownInOrder는 「기동과 종료」가 고정한 정상 종료 순서를 수행한다.
//
// 트래픽을 끊고 진행 중인 요청을 끝낸 뒤 작업자를 멈춘다. 연결 풀은 닫지 않는다. 작업자가
// 아직 연결을 빌려 갔을 수 있어 풀을 닫는 것은 조립 지점의 마지막 일이기 때문이다.
//
// 요청 대기가 실패해도 작업자 종료를 건너뛰지 않는다. 건너뛰면 풀 닫기가 작업자의 연결이
// 돌아오기를 기다리며 막힌다. 요청 대기에서 예산을 다 썼을 수 있으므로 workerCtx에
// 적용할 작업자 기한은 요청 대기 뒤 closeWorkers가 만든다. 요청과 작업자의
// 종료 오류를 함께 보존한다.
func shutdownInOrder(ctx, workerCtx context.Context, app *application, server *http.Server, logger *slog.Logger, workers ...worker) error {
	shutdownErr := app.shutdown(ctx, server)
	if shutdownErr != nil {
		logger.Error("진행 요청 종료 대기", "error", shutdownErr)
	}
	return errors.Join(shutdownErr, closeWorkers(workerCtx, logger, workers...))
}

func closeWorkers(ctx context.Context, logger *slog.Logger, workers ...worker) error {
	ctx, cancel := context.WithTimeout(ctx, workerShutdownTimeout)
	defer cancel()
	var failures []error
	for position, closing := range workers {
		if err := closing.Close(ctx); err != nil {
			failure := fmt.Errorf("작업자 %d 종료: %w", position+1, err)
			logger.Error("작업자 종료", "error", failure)
			failures = append(failures, failure)
		}
	}
	return errors.Join(failures...)
}

// closeServerResources는 정상 종료와 기동 실패에 같은 풀 종료 조건을 적용한다.
// 종료 실패 뒤 풀을 닫으면 아직 반환되지 않은 연결을 기다려 오류를 보고하지 못한다.
func closeServerResources(ctx context.Context, logger *slog.Logger, closePool func(), poolCloseSafe bool, workers ...worker) error {
	workerErr := closeWorkers(ctx, logger, workers...)
	if poolCloseSafe && workerErr == nil {
		closePool()
	}
	return workerErr
}

// logRequests는 본문·자격 증명·쿼리 문자열을 기록하지 않고 요청 결과만 남긴다.
func (app *application) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		trace := diagnostic.NewRequestTrace()
		id, err := model.ParseID(trace.CorrelationID())
		if err != nil {
			app.logger.Error("상관 식별자 생성 실패", "error", err)
			writeStatus(writer, http.StatusInternalServerError, "internal_error")
			return
		}
		ctx, _ := model.WithCorrelationID(diagnostic.WithTrace(request.Context(), trace), id)
		request = request.WithContext(context.WithValue(ctx, correlationIDKey{}, id.String()))
		writer.Header().Set("X-Correlation-ID", id.String())
		startedAt := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		fields := diagnostic.Fields{Request: diagnostic.RequestFields{Route: diagnosticRoute(request.URL.Path), Method: diagnosticMethod(request.Method)}}
		diagnostic.Observe(app.diagnostics, request.Context(), "INFO", "receive", "pending", "completed", false, fields)
		next.ServeHTTP(recorder, request)
		fields.Request.Status = uint16(recorder.status)
		fields.Request.LatencyMS = uint64(time.Since(startedAt).Milliseconds())
		result, reason, level := "completed", "completed", "INFO"
		if recorder.status >= 400 {
			result, reason, level = "rejected", "invalid_argument", "WARN"
		}
		if recorder.status >= 500 {
			result, reason, level = "failed", "store_unavailable", "ERROR"
		}
		diagnostic.Observe(app.diagnostics, request.Context(), level, "response", result, reason, false, fields)
		app.logger.Info("HTTP 요청 완료",
			"correlation_id", id.String(),
			"method", diagnosticMethod(request.Method),
			"route", diagnosticRoute(request.URL.Path),
			"status", recorder.status,
			"duration", time.Since(startedAt),
		)
	})
}

// correlationIDKey는 요청 컨텍스트 안에서만 상관 식별자를 구분하는 비공개 키다.
type correlationIDKey struct{}

// requestID는 로그가 요청 흐름을 잇는 데 쓸 상관 식별자를 돌려준다.
func requestID(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey{}).(string)
	return id
}

// statusRecorder는 응답 상태를 로그에 남기기 위해 한 번만 기록한다.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// WriteHeader는 클라이언트로 보내는 상태와 로그에 남길 상태를 함께 기록한다.
func (recorder *statusRecorder) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		recorder.ResponseWriter.WriteHeader(status)
		return
	}
	if recorder.wrote {
		return
	}
	recorder.wrote = true
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

// writeStatus는 상태 확인 응답을 JSON으로 쓰고 본문에 내부 오류를 담지 않는다.
func writeStatus(writer http.ResponseWriter, status int, value string) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(statusResponse{Status: value}); err != nil {
		return
	}
}

func diagnosticMethod(method string) string {
	if slices.Contains([]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE"}, method) {
		return method
	}
	return "unknown"
}
func diagnosticRoute(path string) string {
	switch path {
	case "/login":
		return "login"
	case "/register":
		return "register"
	case "/logout":
		return "logout"
	case "/authorize":
		return "authorize"
	case "/token":
		return "token"
	case "/operator/restores":
		return "operator_restores"
	case "/access":
		return "graph_access"
	case "/connections":
		return "connections"
	case "/graphs":
		return "graphs"
	case "/teams":
		return "teams"
	case "/mcp":
		return "mcp"
	case "/healthz", "/readyz":
		return "health"
	}
	if strings.HasPrefix(path, "/.well-known/") || path == "/jwks.json" {
		return "discovery"
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 3 && parts[1] == "graphs" {
		switch {
		case len(parts) == 3:
			return "graph_detail"
		case len(parts) == 4 && parts[3] == "access":
			return "graph_access"
		case len(parts) == 4 && parts[3] == "deletion":
			return "graph_deletion"
		case len(parts) == 4 && parts[3] == "audit":
			return "audit"
		}
	}
	return "unknown"
}

func (recorder *statusRecorder) Write(data []byte) (int, error) {
	if !recorder.wrote {
		recorder.WriteHeader(http.StatusOK)
	}
	return recorder.ResponseWriter.Write(data)
}
func (recorder *statusRecorder) Unwrap() http.ResponseWriter { return recorder.ResponseWriter }
