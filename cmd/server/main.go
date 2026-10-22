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

	"agent_context_sharing/internal/config"
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
	return configureAndRun(os.Getenv, runConfigured)
}

// configureAndRun은 검증이 모두 끝난 구성만 자원을 여는 조립 함수에 넘긴다.
func configureAndRun(env config.Environment, start func(config.Config) error) error {
	cfg, err := config.Load(env)
	if err != nil {
		return fmt.Errorf("배포 구성 검증: %w", err)
	}
	return start(cfg)
}

func runConfigured(cfg config.Config) (resultErr error) {
	assembly, err := assembleApplication(context.Background(), cfg, nil, slog.Default())
	if err != nil {
		return err
	}
	poolCloseSafe := true
	defer func() { resultErr = assembly.close(resultErr, poolCloseSafe) }()
	assembly.startWorkers(context.Background())
	app := assembly.app
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

	// 순서는 shutdownInOrder가 고정한다. 트래픽을 끊고 진행 요청을 마친 뒤 작업자를
	// 멈추고, 둘 다 풀을 쓰지 않게 된 다음에 defer가 풀을 닫는다.
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := shutdownInOrder(shutdownContext, context.Background(), app, server, slog.Default(), assembly.workers...)
	assembly.workers = nil
	if shutdownErr != nil {
		poolCloseSafe = false
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
