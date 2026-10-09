package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"agent_context_sharing/internal/mcp"
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

// authorizationRoutes는 「HTTP 진입점」이 인가 서버에 배정한 세 경로의 처리기다.
// readiness와 같은 이유로 인터페이스를 두어 조립과 경로 등록을 서로 떼어 놓는다.
type authorizationRoutes interface {
	Authorize(http.ResponseWriter, *http.Request)
	Token(http.ResponseWriter, *http.Request)
	JWKS(http.ResponseWriter, *http.Request)
}

// application은 1단계 HTTP 상태 경로와 요청 로그를 관리한다.
type application struct {
	database  readiness
	logger    *slog.Logger
	transport transportSecurity
	mcp       *mcp.Server
	web       *web.Server
	authz     authorizationRoutes
	accepting atomic.Bool
}

// statusResponse는 상태 확인 경로가 반환하는 민감 정보 없는 본문이다.
type statusResponse struct {
	Status string `json:"status"`
}

// newApplication은 데이터베이스 준비 확인, 구조화 로그와 TLS 신뢰 경계를 주입해 상태 경로를 만든다.
func newApplication(database readiness, logger *slog.Logger, transport transportSecurity, resourceServer *mcp.Server) *application {
	if logger == nil {
		logger = slog.Default()
	}
	app := &application{database: database, logger: logger, transport: transport, mcp: resourceServer}
	app.accepting.Store(true)
	return app
}

// handler는 1단계에서 정한 상태 경로와 모든 요청의 안전한 구조화 로그를 등록한다.
//
// 경로 표면을 두 층으로 나눈다. 상태 확인 두 경로만 TLS 판정 앞에 두고 나머지는 전부
// 뒤에 둔다. `SRS.md`의 「transport와 프로토콜」이 `FR-AGENT_CONTEXT-140`에 이 예외를
// 두었으며, 프로브는 프록시를 거치지 않고 오므로 판정을 지나면 항상 실패한다.
//
// 등록되지 않은 경로도 판정 뒤에 둔다. 평문 요청에 어떤 경로가 있는지 알리지 않는다.
func (app *application) handler() http.Handler {
	// 업무 경로는 이후 단계에서 이 mux에 등록하며 모두 TLS 판정을 지난다.
	gated := http.NewServeMux()
	if app.mcp != nil {
		gated.Handle("POST /mcp", withRenewalHeader(app.mcp))
		gated.HandleFunc("GET /.well-known/oauth-protected-resource", app.mcp.ProtectedResourceMetadata)
		gated.HandleFunc("GET /.well-known/oauth-authorization-server", app.mcp.AuthorizationServerMetadata)
	}
	// 인가 서버의 세 경로를 등록한다. 등록하지 않으면 인가 서버 메타데이터가 알리는
	// 주소가 모두 404가 되어 클라이언트가 접근 토큰을 받을 수 없다.
	if app.authz != nil {
		gated.HandleFunc("GET /authorize", app.authz.Authorize)
		gated.HandleFunc("POST /token", app.authz.Token)
		gated.HandleFunc("GET /jwks.json", app.authz.JWKS)
	}
	if app.web != nil {
		gated.Handle("/", app.web)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /readyz", app.ready)
	mux.Handle("/", app.requireTLS(gated))
	return app.logRequests(app.recoverPanics(mux))
}

// recoverPanics는 처리기에서 빠져나온 panic을 500으로 바꾸고 원인을 로그에 남긴다.
//
// 차단막이 없으면 net/http이 연결을 끊어 클라이언트는 응답 대신 끊긴 연결을 받고, 구조화
// 로그에도 요청이 남지 않는다. MCP 처리기가 스키마 검증을 믿고 타입 단언을 쓰므로, 스키마와
// 처리기가 어긋나는 순간이 곧 이 자리다. 바깥의 logRequests가 상태를 기록할 수 있도록 이
// 미들웨어를 그 안쪽에 둔다.
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
				"method", request.Method, "path", request.URL.Path,
				"panic", fmt.Sprint(cause), "stack", string(debug.Stack()))
			writeStatus(writer, http.StatusInternalServerError, "internal_error")
		}()
		next.ServeHTTP(writer, request)
	})
}

// requireTLS는 배포가 정한 신뢰 경계에서 확인되지 않은 평문 요청을 처리하지 않는다.
func (app *application) requireTLS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !app.isTLSRequest(request) {
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
	return strings.EqualFold(strings.TrimSpace(forwarded[0]), "https")
}

// isTrustedProxy는 요청을 보낸 상대가 전달 헤더를 신뢰할 대역에 속하는지 확인한다.
func (security transportSecurity) isTrustedProxy(peer netip.Addr) bool {
	return slices.ContainsFunc(security.trustedProxies, func(prefix netip.Prefix) bool {
		return prefix.Contains(peer)
	})
}

// remoteAddr는 http.Request.RemoteAddr의 host:port 표기에서 비교 가능한 주소를 뽑는다.
// IPv4-mapped IPv6로 들어온 상대도 구성에 적은 IPv4 대역과 맞도록 편다.
func remoteAddr(value string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(value)
	if err != nil {
		host = value
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
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
type worker interface {
	Close(context.Context) error
}

// shutdownInOrder는 「기동과 종료」가 고정한 정상 종료 순서를 수행한다.
//
// 트래픽을 끊고 진행 중인 요청을 끝낸 뒤 작업자를 멈춘다. 연결 풀은 닫지 않는다. 작업자가
// 아직 연결을 빌려 갔을 수 있어 풀을 닫는 것은 조립 지점의 마지막 일이기 때문이다.
//
// 요청 대기가 실패해도 작업자 종료를 건너뛰지 않는다. 건너뛰면 풀 닫기가 작업자의 연결이
// 돌아오기를 기다리며 막힌다. 요청 대기에서 예산을 다 썼을 수 있으므로 작업자에는
// workerCtx에 적용할 작업자 기한은 요청 대기 뒤 closeWorkers가 만든다. 돌려주는 오류는
// 요청 대기의 것을 우선한다.
func shutdownInOrder(ctx, workerCtx context.Context, app *application, server *http.Server, logger *slog.Logger, workers ...worker) error {
	shutdownErr := app.shutdown(ctx, server)
	if shutdownErr != nil {
		logger.Error("진행 요청 종료 대기", "error", shutdownErr)
	}
	closeWorkers(workerCtx, logger, workers...)
	return shutdownErr
}

func closeWorkers(ctx context.Context, logger *slog.Logger, workers ...worker) {
	ctx, cancel := context.WithTimeout(ctx, workerShutdownTimeout)
	defer cancel()
	for _, closing := range workers {
		if err := closing.Close(ctx); err != nil {
			logger.Error("작업자 종료", "error", err)
		}
	}
}

// logRequests는 본문·자격 증명·쿼리 문자열을 기록하지 않고 요청 결과만 남긴다.
func (app *application) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		id, err := model.NewID()
		if err != nil {
			app.logger.Error("상관 식별자 생성 실패", "error", err)
			writeStatus(writer, http.StatusInternalServerError, "internal_error")
			return
		}
		request = request.WithContext(context.WithValue(request.Context(), correlationIDKey{}, id.String()))
		writer.Header().Set("X-Correlation-ID", id.String())
		startedAt := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(recorder, request)
		app.logger.Info("HTTP 요청 완료",
			"correlation_id", id.String(),
			"method", request.Method,
			"path", request.URL.Path,
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
}

// WriteHeader는 클라이언트로 보내는 상태와 로그에 남길 상태를 함께 기록한다.
func (recorder *statusRecorder) WriteHeader(status int) {
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
