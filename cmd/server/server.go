package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"agent_context_sharing/internal/model"
)

// readiness는 준비 확인과 종료에 필요한 데이터베이스 연결의 최소 계약이다.
type readiness interface {
	Ping(context.Context) error
	Close()
}

// transportSecurity은 수신 연결의 TLS 여부를 판단하는 배포 경계다.
type transportSecurity struct {
	// directTLS 필드는 애플리케이션이 TLS 수신기를 직접 열었는지 나타낸다.
	directTLS bool
	// trustedProxies 필드에는 전달 헤더를 신뢰할 역방향 프록시의 주소 대역을 둔다.
	// 비어 있으면 어떤 상대의 전달 헤더도 TLS 판정에 쓰지 않는다.
	trustedProxies []netip.Prefix
}

// application은 1단계 HTTP 상태 경로와 요청 로그를 관리한다.
type application struct {
	database  readiness
	logger    *slog.Logger
	transport transportSecurity
	accepting atomic.Bool
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

// handler는 1단계에서 정한 상태 경로와 모든 요청의 안전한 구조화 로그를 등록한다.
func (app *application) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /readyz", app.ready)
	return app.logRequests(app.requireTLS(mux))
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
	if app.transport.directTLS {
		return request.TLS != nil
	}
	peer, ok := remoteAddr(request.RemoteAddr)
	if !ok || !app.isTrustedProxy(peer) {
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
func (app *application) isTrustedProxy(peer netip.Addr) bool {
	return slices.ContainsFunc(app.transport.trustedProxies, func(prefix netip.Prefix) bool {
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

// shutdown은 준비 상태를 먼저 내려 새 트래픽을 막고 진행 요청을 끝낸 뒤 풀을 닫는다.
func (app *application) shutdown(ctx context.Context, server *http.Server) error {
	app.accepting.Store(false)
	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("진행 요청 종료 대기: %w", err)
	}
	// 1단계에서는 주기 작업 잠금을 아직 획득하지 않으므로, 이 시점에 해제할 잠금은 없다.
	// 이후 단계에서 잠금이 추가돼도 연결 풀을 닫기 전 이 순서에 해제한다.
	if app.database != nil {
		app.database.Close()
	}
	return nil
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
