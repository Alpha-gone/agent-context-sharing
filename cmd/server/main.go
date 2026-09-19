// Command server는 에이전트 컨텍스트 관리 HTTP 애플리케이션을 실행한다.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

// shutdownTimeout은 종료 신호 뒤 진행 중인 요청을 기다리는 최대 시간이다.
const shutdownTimeout = 30 * time.Second

// workerShutdownTimeout은 요청 대기가 끝난 뒤 작업자 종료를 기다리는 최대 시간이다.
// 요청 대기와 예산을 나눠 두어야 그쪽이 시간을 다 써도 작업자를 닫을 수 있다.
const workerShutdownTimeout = 10 * time.Second

const (
	// readHeaderTimeout은 요청 헤더를 모두 받기까지 기다리는 최대 시간이다.
	readHeaderTimeout = 10 * time.Second
	// readTimeout은 헤더와 본문을 모두 받기까지 기다리는 최대 시간이다.
	readTimeout = 30 * time.Second
	// idleTimeout은 keep-alive 연결에서 다음 요청을 기다리는 최대 시간이다.
	idleTimeout = 120 * time.Second
)

// newHTTPServer는 수신 서버에 읽기 쪽 제한을 걸어 만든다.
//
// 헤더를 느리게 보내는 연결이 고루틴과 파일 디스크립터를 붙잡지 못하게 한다. TLS 판정보다
// 앞이라 인증 없이도 열 수 있는 자리이며, 명세가 값을 정하지 않았으므로 여기에서 고른다.
// 쓰기 제한은 두지 않는다. 흐름 검색이 외부 임베딩 제공자를 기다리는 동안 응답이 끊기면
// 안 되기 때문이다.
func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// main 함수는 구성 검증, 데이터베이스 풀 준비와 HTTP 서버의 정상 종료를 조립한다.
func main() {
	if err := run(); err != nil {
		slog.Error("서버 종료", "error", err)
		os.Exit(1)
	}
}

// run 함수는 종료 신호를 기다렸다가 준비 상태를 내리고 받은 요청을 마친 뒤 풀을 닫는다.
func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("배포 구성 검증: %w", err)
	}

	database, err := store.New(context.Background(), cfg.DatabaseURL, cfg.GraphName, &store.RelationProposalConfig{
		AdjacencyWindow:     cfg.RelationAdjacencyWindow,
		SimilarityThreshold: cfg.RelationSimilarityThreshold,
		Limit:               cfg.RelationProposalLimit,
	}, func(accountID model.ID) int { return cfg.AccountPlans.For(accountID).GraceDays })
	if err != nil {
		return fmt.Errorf("데이터베이스 풀 준비: %w", err)
	}
	defer database.Close()
	periodic, err := newPeriodicWorker(database, cfg.AccountPlans, slog.Default())
	if err != nil {
		return fmt.Errorf("주기 작업 준비: %w", err)
	}
	indexer, err := index.New(database, index.Config{
		BaseURL:    cfg.EmbeddingBaseURL,
		Model:      cfg.EmbeddingModel,
		VectorType: cfg.EmbeddingVectorType,
		Dimension:  cfg.EmbeddingDimension,
	}, nil, slog.Default())
	if err != nil {
		return fmt.Errorf("색인 작업자 준비: %w", err)
	}
	// 작업자 종료는 defer가 아니라 아래 종료 순서에서 처리한다. defer로 두면 요청 종료와
	// 풀 종료 사이가 아니라 그 뒤에 실행되어, 작업자가 도는 중에 풀이 닫힌다.
	// 오래된 모델의 재색인 등록은 작업자가 시작하면서 스스로 한다.
	indexer.Start(context.Background())
	periodic.Start(context.Background())
	searcher, err := search.New(database, indexer, search.Config{Execution: search.Execution(cfg.SearchExecution), CandidateLimit: cfg.SearchCandidateLimit, FoldThreshold: cfg.SearchFoldThreshold, GraphStage: search.GraphStage(cfg.SearchGraphStage)}, slog.Default())
	if err != nil {
		return fmt.Errorf("검색 실행기 준비: %w", err)
	}
	authorization, err := authz.New(database, authz.Config{
		Issuer:       cfg.AuthorizationServerURL.String(),
		Resource:     cfg.ResourceServerURL.String(),
		Clients:      cfg.OAuthClientIDs,
		RedirectURIs: cfg.OAuthRedirectURIs,
		BcryptCost:   cfg.BcryptCost,
	})
	if err != nil {
		return fmt.Errorf("인가 서버 준비: %w", err)
	}
	resourceServer, err := mcp.New(mcp.Config{
		ResourceURL:            cfg.ResourceServerURL,
		AuthorizationServerURL: cfg.AuthorizationServerURL,
		AllowedOrigins:         cfg.MCPAllowedOrigins,
	}, verifyWithRenewal(authorization), mcp.NewHandlerWithSearch(database, cfg.AccountPlans, searcher, slog.Default()))
	if err != nil {
		return fmt.Errorf("MCP 리소스 서버 준비: %w", err)
	}
	transport := transportSecurity{
		directTLS:      cfg.TLSMode == config.TLSModeDirect,
		trustedProxies: cfg.TrustedProxies,
	}
	webServer, err := web.New(webAuthentication{service: authorization}, database, web.Config{
		SecureCookie: transport.isTLSRequest,
		Plans:        cfg.AccountPlans,
	})
	if err != nil {
		return fmt.Errorf("웹 서버 준비: %w", err)
	}
	// 인가 서버 경로는 로그인 화면과 세션 쿠키 판정을 웹에서 받아 쓴다. 쿠키의 이름과
	// 수명은 web이 소유하므로 authz는 판정 결과만 주입받는다.
	authorizationHandler, err := authz.NewHandler(authorization, webServer.SessionAccount, "/login")
	if err != nil {
		return fmt.Errorf("인가 서버 경로 준비: %w", err)
	}
	app := newApplication(database, slog.Default(), transport, resourceServer)
	app.web = webServer
	app.authz = authorizationHandler
	server := newHTTPServer(cfg.HTTPAddr, app.handler())

	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopSignals)

	serveErrors := make(chan error, 1)
	// 수신 루프는 종료 신호를 기다리는 주 흐름과 독립적으로 실행하고 종료 결과를 한 번 전달한다.
	go func() {
		serveErrors <- serve(server, cfg)
	}()

	select {
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP 서버 수신: %w", err)
		}
		return nil
	case signal := <-stopSignals:
		slog.Info("종료 신호 수신", "signal", signal.String())
	}

	// 순서를 고정한다. 트래픽을 끊고 진행 요청을 마친 뒤 색인 작업자를 멈추고, 둘 다 풀을
	// 쓰지 않게 된 다음에 defer가 풀을 닫는다.
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := app.shutdown(shutdownContext, server)
	if shutdownErr != nil {
		slog.Error("진행 요청 종료 대기", "error", shutdownErr)
	}
	// 요청 대기가 실패해도 작업자 종료를 건너뛰지 않는다. 건너뛰면 defer된 풀 닫기가
	// 작업자가 빌려 간 연결이 돌아오기를 기다리며 막힌다. 요청 대기에서 이미 예산을 다
	// 썼을 수 있으므로 작업자에는 새 기한을 준다.
	workerContext, cancelWorkers := context.WithTimeout(context.Background(), workerShutdownTimeout)
	defer cancelWorkers()
	if err := periodic.Close(workerContext); err != nil {
		slog.Error("주기 작업 종료", "error", err)
	}
	if err := indexer.Close(workerContext); err != nil {
		slog.Error("색인 작업자 종료", "error", err)
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 서버 종료: %w", err)
	}
	return nil
}

// serve는 TLS 종단 배치에 맞는 HTTP 수신기를 시작한다.
func serve(server *http.Server, cfg config.Config) error {
	if cfg.TLSMode == config.TLSModeDirect {
		return server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	}
	return server.ListenAndServe()
}
