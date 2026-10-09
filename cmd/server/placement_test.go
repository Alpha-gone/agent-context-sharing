package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// recordingWorker는 종료 순서를 관찰하려고 닫힌 시점의 준비 상태를 함께 기록한다.
type recordingWorker struct {
	name    string
	app     *application
	events  *[]string
	failure error
}

type deadlineWorker struct{ remaining time.Duration }

func (closing *deadlineWorker) Close(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if ok {
		closing.remaining = time.Until(deadline)
	}
	return nil
}

func TestShutdownGivesWorkersFreshBudgetWhenRequestDrainFails(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	defer server.Close()
	finished := make(chan error, 1)
	go func() {
		response, err := http.Get(server.URL)
		if err == nil {
			response.Body.Close()
		}
		finished <- err
	}()
	<-entered
	// 요청 대기의 취소를 작업자 종료에 전파하거나 시간 예산을 소진한 채 넘기지 않는다.
	requestCtx, cancel := context.WithCancel(t.Context())
	cancel()
	worker := &deadlineWorker{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := newApplication(&fakeReadiness{}, logger, proxyTransport(), nil)
	if err := shutdownInOrder(requestCtx, t.Context(), app, server.Config, logger, worker); err == nil {
		t.Error("요청 종료 대기 실패가 보고되지 않았다")
	}
	close(release)
	if err := <-finished; err == nil {
		t.Fatal("요청 대기 실패 뒤 진행 연결이 남았다")
	}
	if worker.remaining <= 0 || worker.remaining > workerShutdownTimeout {
		t.Fatalf("작업자에게 새 종료 예산이 없다: %s", worker.remaining)
	}
}

func TestShutdownFailureClosesActiveConnectionsWithoutWaitingForHandler(t *testing.T) {
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(entered)
		select {
		case <-request.Context().Done():
			close(canceled)
		case <-release:
			return
		}
		// 요청 취소에도 연결을 쥔 처리기를 기다리지 않고 종료 실패를 반환해야 한다.
		<-release
	}))
	defer server.Close()
	defer close(release)
	finished := make(chan error, 1)
	go func() {
		response, err := http.Get(server.URL)
		if err == nil {
			response.Body.Close()
		}
		finished <- err
	}()
	<-entered
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := newApplication(&fakeReadiness{}, logger, proxyTransport(), nil)
	if err := app.shutdown(ctx, server.Config); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("종료 실패 = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("진행 요청의 연결과 컨텍스트가 종료되지 않았다")
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("진행 요청의 연결이 정상 응답으로 남았다")
		}
	case <-time.After(time.Second):
		t.Fatal("진행 요청의 클라이언트 연결이 남았다")
	}
}

// Close는 작업자를 닫으면서 그 시점에 트래픽이 이미 끊겼는지 함께 남긴다.
func (closing recordingWorker) Close(context.Context) error {
	if closing.app.accepting.Load() {
		*closing.events = append(*closing.events, "준비 상태가 남은 채 "+closing.name+" 종료")
	}
	*closing.events = append(*closing.events, closing.name)
	return closing.failure
}

// TestShutdownFollowsDesignOrder는 「기동과 종료」가 고정한 정상 종료 순서를 확인한다.
//
// 트래픽 이탈, 진행 요청 완료, 작업자 종료가 이 차례여야 하고 연결 풀은 이 단계에서
// 닫히지 않아야 한다. 작업자가 아직 연결을 빌려 갔을 수 있어 풀 닫기는 조립 지점의
// 마지막 일이기 때문이다.
func TestShutdownFollowsDesignOrder(t *testing.T) {
	database := &fakeReadiness{}
	app := newApplication(database, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	server := &http.Server{Handler: app.handler()}
	events := make([]string, 0, 2)

	err := shutdownInOrder(t.Context(), t.Context(), app, server, slog.New(slog.NewTextHandler(io.Discard, nil)),
		recordingWorker{name: "주기 작업자", app: app, events: &events},
		recordingWorker{name: "색인 작업자", app: app, events: &events})
	if err != nil {
		t.Fatalf("정상 종료: %v", err)
	}

	if want := []string{"주기 작업자", "색인 작업자"}; !slices.Equal(events, want) {
		t.Fatalf("종료 순서 = %v, want %v", events, want)
	}
	if database.closed {
		t.Fatal("종료 순서가 연결 풀까지 닫았다")
	}
	ready := httptest.NewRecorder()
	app.handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("종료 뒤 readyz 상태 = %d, want 503", ready.Code)
	}
}

// TestShutdownClosesWorkersEvenWhenRequestsDoNotDrain은 요청 대기가 실패해도 작업자를
// 닫는지 확인한다. 건너뛰면 조립 지점의 풀 닫기가 작업자의 연결을 기다리며 막힌다.
func TestShutdownClosesWorkersEvenWhenRequestsDoNotDrain(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	// 끝나지 않는 요청을 하나 띄워 두어야 진행 요청 대기가 실제로 실패한다. 받은 요청이
	// 없으면 Shutdown이 기다릴 것이 없어 만료된 컨텍스트로도 성공한다.
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("수신기 준비: %v", err)
	}
	go server.Serve(listener)
	go http.Get("http://" + listener.Addr().String())
	<-entered

	expired, cancel := context.WithCancel(t.Context())
	cancel()
	events := make([]string, 0, 1)

	if err := shutdownInOrder(expired, t.Context(), app, server, slog.New(slog.NewTextHandler(io.Discard, nil)),
		recordingWorker{name: "색인 작업자", app: app, events: &events}); err == nil {
		t.Fatal("요청 대기 실패가 종료 결과에 드러나지 않았다")
	}
	if want := []string{"색인 작업자"}; !slices.Equal(events, want) {
		t.Fatalf("종료한 작업자 = %v, want %v", events, want)
	}
}

// TestShutdownReportsRequestDrainErrorOverWorkerError는 작업자 종료가 실패해도 요청 대기
// 결과를 우선 돌려주는지 확인한다. 호출자가 보는 값은 트래픽을 안전하게 뺐는지다.
func TestShutdownReportsRequestDrainErrorOverWorkerError(t *testing.T) {
	app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
	server := &http.Server{Handler: app.handler()}
	events := make([]string, 0, 1)

	err := shutdownInOrder(t.Context(), t.Context(), app, server, slog.New(slog.NewTextHandler(io.Discard, nil)),
		recordingWorker{name: "색인 작업자", app: app, events: &events, failure: fmt.Errorf("작업자 종료 실패")})
	if err != nil {
		t.Fatalf("작업자 종료 실패가 정상 종료를 실패로 만들었다: %v", err)
	}
}

// TestAuthorizationRoutesFollowPlacement는 인가 서버 배치가 경로 등록을 가르는지 확인한다.
//
// 「배치 조합」이 인가 서버를 떼어낼 수 있게 두었고, 분리 배치에서 이 인스턴스는 리소스
// 서버로만 동작한다. 발급 경로가 남아 있으면 두 배포 단위가 같은 경로를 제공하게 된다.
func TestAuthorizationRoutesFollowPlacement(t *testing.T) {
	paths := []struct{ method, path string }{
		{http.MethodGet, "/authorize"},
		{http.MethodPost, "/token"},
		{http.MethodGet, "/jwks.json"},
	}
	for _, testCase := range []struct {
		name     string
		embedded bool
	}{
		{name: "내장", embedded: true},
		{name: "분리", embedded: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newApplication(&fakeReadiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport(), nil)
			if testCase.embedded {
				app.authz = stubAuthorizationRoutes{}
			}
			handler := app.handler()
			for _, route := range paths {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(route.method, route.path, nil)
				request.Header.Set("X-Forwarded-Proto", "https")
				handler.ServeHTTP(recorder, request)
				registered := recorder.Code != http.StatusNotFound
				if registered != testCase.embedded {
					t.Fatalf("%s %s 등록 = %v, want %v", route.method, route.path, registered, testCase.embedded)
				}
			}
		})
	}
}
