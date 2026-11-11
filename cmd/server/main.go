// Command server는 에이전트 컨텍스트 관리 HTTP 애플리케이션을 실행한다.
package main

import (
	"context"
	"crypto/tls"
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

// poolCloseTimeout은 요청·작업자 종료가 끝난 뒤 연결 풀의 미반환 연결을 기다리는 최대 시간이다.
const poolCloseTimeout = 5 * time.Second

const (
	// readHeaderTimeout은 요청 헤더를 모두 받기까지 기다리는 최대 시간이다.
	readHeaderTimeout = 10 * time.Second
	// readTimeout은 헤더와 본문을 모두 받기까지 기다리는 최대 시간이다.
	readTimeout = 30 * time.Second
	// idleTimeout은 keep-alive 연결에서 다음 요청을 기다리는 최대 시간이다.
	idleTimeout = 120 * time.Second
	// writeTimeout은 요청 헤더를 읽은 뒤 응답 전송을 마치기까지 기다리는 최대 시간이다.
	writeTimeout = readTimeout + 30*time.Second
)

// newHTTPServer는 수신 서버에 읽기·쓰기 제한을 걸어 만든다.
//
// 헤더를 느리게 보내는 연결이 고루틴과 파일 디스크립터를 붙잡지 못하게 한다. TLS 판정보다
// 앞이라 인증 없이도 열 수 있는 자리이며, 명세가 값을 정하지 않았으므로 여기에서 고른다.
// 쓰기 제한은 응답을 느리게 읽는 연결도 같은 자원을 붙잡지 못하게 한다. 처리 자체는
// 요청 공통 30초 예산이 끊으므로 흐름 검색이 제공자를 기다려도 이 제한이 먼저 닿지 않으며,
// 남은 30초는 응답 전송에만 쓴다.
func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
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

func runConfigured(cfg config.Config) error {
	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopSignals)

	// 직접 TLS 인증서는 자원을 열기 전에 읽어, 작업자를 시작한 뒤에야 실패하지 않게 한다.
	tlsConfig, err := serverTLSConfig(cfg)
	if err != nil {
		return err
	}
	// 조립 중 종료 신호는 조립 context를 취소해 DB 무응답에도 기동을 멈춘다. 조립 뒤의
	// 신호는 stopSignals가 받아 정상 종료 순서를 따른다.
	assemblyContext, stopAssembly := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	assembly, err := assembleApplication(assemblyContext, cfg, nil, slog.Default())
	stopAssembly()
	if err != nil {
		return err
	}
	server := newHTTPServer(cfg.HTTPAddr, assembly.app.handler())
	server.TLSConfig = tlsConfig
	return assembly.runHTTP(server, func() error { return serve(server) }, stopSignals)
}

// serverTLSConfig는 직접 TLS 종단의 인증서와 키를 읽어 수신기 구성을 만든다.
func serverTLSConfig(cfg config.Config) (*tls.Config, error) {
	if cfg.TLSMode != config.TLSModeDirect {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("TLS 인증서 준비: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}}, nil
}

// runHTTP는 수신 실패와 종료 신호에 같은 요청·작업자·풀·진단 정리를 적용한다.
// 수신기의 반환만으로 진행 요청이 끝났다고 판단하지 않는다.
func (assembly *serverAssembly) runHTTP(server *http.Server, listen func() error, stopSignals <-chan os.Signal) (resultErr error) {
	poolCloseSafe := true
	defer func() { resultErr = assembly.close(resultErr, poolCloseSafe) }()
	assembly.startWorkers(context.Background())
	serveErrors := make(chan error, 1)
	// 수신 루프는 종료 신호를 기다리는 주 흐름과 독립적으로 실행하고 종료 결과를 한 번 전달한다.
	go func() {
		serveErrors <- listen()
	}()

	var serveErr error
	serveReturned := false
	select {
	case err := <-serveErrors:
		serveErr, serveReturned = err, true
	case signal := <-stopSignals:
		assembly.logger.Info("종료 신호 수신", "signal", signal.String())
	}

	// 순서는 shutdownInOrder가 고정한다. 트래픽을 끊고 진행 요청을 마친 뒤 작업자를
	// 멈추고, 둘 다 풀을 쓰지 않게 된 다음에 defer가 풀을 닫는다.
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := shutdownInOrder(shutdownContext, context.Background(), assembly.app, server, assembly.logger, assembly.workers...)
	assembly.workers = nil
	poolCloseSafe = shutdownErr == nil
	if !serveReturned {
		serveErr = <-serveErrors
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = fmt.Errorf("HTTP 서버 수신: %w", serveErr)
	} else {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}

// serve는 TLS 종단 배치에 맞는 HTTP 수신기를 시작한다. 직접 종단의 인증서는 기동 전에
// TLSConfig에 읽어 두었으므로 파일을 다시 읽지 않는다.
func serve(server *http.Server) error {
	if server.TLSConfig != nil {
		return server.ListenAndServeTLS("", "")
	}
	return server.ListenAndServe()
}
