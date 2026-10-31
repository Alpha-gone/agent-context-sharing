package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/diagnostic"
	"golang.org/x/sys/unix"
)

// 첫 연결 뒤의 Accept를 제어해 진행 요청이 있는 실제 수신 실패를 만든다.
type ops05Listener struct {
	net.Listener
	accepted bool
	fail     <-chan struct{}
	cause    error
}

func TestOPS05ProcessFailureExit(t *testing.T) {
	if os.Getenv("ACS_OPS05_EXIT_HELPER") == "1" {
		main()
		return
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOPS05ProcessFailureExit$")
	command.Env = append(os.Environ(), "ACS_OPS05_EXIT_HELPER=1", "EMBEDDING_PROVIDER=unsupported")
	err := command.Run()
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 1 {
		t.Fatalf("구성 실패의 실제 프로세스 종료 코드: %v", err)
	}
}

func TestOPS05DiagnosticFlushBudgetIntegration(t *testing.T) {
	_, _, address := newServerContractDatabase(t)
	cfg := arch06Configuration(t, address, "https://provider.invalid")
	a, err := assembleApplication(t.Context(), cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.database.Close)
	t.Cleanup(func() { _ = a.close(nil, true) })
	lock, err := os.OpenFile(filepath.Join(cfg.Diagnostic.Directory, ".storage.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	unlock := sync.OnceFunc(func() {
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
			t.Error(err)
		}
	})
	defer unlock()
	// 기록 루프는 회차마다 최대 128건을 묶고 공용 잠금을 최대 1초 기다린다.
	// 5초 안에 끝낼 수 없는 7회차 이상을 대기열 상한 1024건 안에서 대기시켜
	// 개별 묶음 실패보다 공통 5초 flush 마감이 먼저 남은 대기열을 끝내게 한다.
	for range 900 {
		a.system.RecordFields(a.jobContext, "INFO", "startup", "pending", "completed", false, diagnostic.Fields{System: diagnostic.SystemFields{Job: "startup"}})
	}
	cause := errors.New("original startup failure")
	started := time.Now()
	err = a.close(cause, true)
	elapsed := time.Since(started)
	if !errors.Is(err, cause) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("기동 원인 또는 flush 마감 유실", err)
	}
	if elapsed < 5*time.Second || elapsed > 7*time.Second {
		t.Fatalf("진단 정리의 독립 5초 예산: %s", elapsed)
	}
	if a.recorder.Stats().Dropped == 0 || a.database.Ping(t.Context()) == nil {
		t.Fatal("진단 누락 또는 성공한 업무 정리의 풀 종료가 반영되지 않았다")
	}
	unlock()
	source, _ := diagnostic.NewSource(cfg.Diagnostic.InstanceID)
	limit := time.Now().Add(2 * time.Second)
	for {
		reopened, openErr := diagnostic.NewRecorder(t.Context(), cfg.Diagnostic.Directory, source)
		if openErr == nil {
			if err := reopened.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(limit) {
			t.Fatal("flush 실패 뒤 시험 자원 미반환", openErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("진단 flush 마감 %s", elapsed)
}

func TestOPS05RequestAndWorkerBudgetsIntegration(t *testing.T) {
	_, _, address := newServerContractDatabase(t)
	cfg := arch06Configuration(t, address, "https://provider.invalid")
	cfg.IndexWorkerPlacement = config.PlacementExternal
	a, err := assembleApplication(t.Context(), cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.database.Close)
	t.Cleanup(func() { _ = a.close(nil, true) })
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	requestDone := make(chan error, 1)
	server := newHTTPServer("127.0.0.1:0", http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, _, err := a.database.WithTryAdvisoryLock(request.Context(), 4182026105105, func(ctx context.Context) (int, error) {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return 0, ctx.Err()
		})
		requestDone <- err
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	workerCause := errors.New("budget worker failure")
	var workerStarted time.Time
	lastCalled := false
	a.workers = append(a.workers, workerFunc(func(ctx context.Context) error {
		workerStarted = time.Now()
		<-ctx.Done()
		return errors.Join(ctx.Err(), workerCause)
	}), workerFunc(func(ctx context.Context) error {
		lastCalled = true
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Error("후행 작업자가 공용 기한을 받지 않았다")
		}
		return nil
	}))
	signals := make(chan os.Signal, 1)
	results, responses := make(chan error, 1), make(chan error, 1)
	go func() { results <- a.runHTTP(server, func() error { return server.Serve(listener) }, signals) }()
	go func() {
		response, err := (&http.Client{Timeout: 45 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
		responses <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("DB 연결을 가진 요청이 시작되지 않았다")
	}
	started := time.Now()
	signals <- syscall.SIGTERM
	select {
	case err = <-results:
	case <-time.After(48 * time.Second):
		t.Fatal("미반환 연결 때문에 종료가 막혔다")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, workerCause) || !lastCalled {
		t.Fatalf("요청·작업자 마감 또는 후행 정리 유실: %v", err)
	}
	requestTime, workerTime := workerStarted.Sub(started), time.Since(workerStarted)
	if requestTime < shutdownTimeout || requestTime > shutdownTimeout+2*time.Second || workerTime < workerShutdownTimeout || workerTime > workerShutdownTimeout+5*time.Second {
		t.Fatalf("분리된 요청/작업자 예산: %s / %s", requestTime, workerTime)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("강제 연결 종료가 요청에 전파되지 않았다")
	}
	if pingErr := a.database.Ping(t.Context()); pingErr != nil {
		t.Fatal("미반환 요청이 있는데 풀을 닫았다", pingErr)
	}
	if responseErr := <-responses; responseErr == nil {
		t.Fatal("요청 마감 뒤 연결이 남았다")
	}
	unblock()
	select {
	case requestErr := <-requestDone:
		if !errors.Is(requestErr, context.Canceled) {
			t.Fatal("요청 취소 원인 유실", requestErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("시험 해제 뒤 DB 실행권을 반환하지 않았다")
	}
	t.Logf("요청 대기 %s, 작업자 정리와 진단 flush %s, 전체 %s", requestTime, workerTime, time.Since(started))
}

func (listener *ops05Listener) Accept() (net.Conn, error) {
	if listener.accepted {
		<-listener.fail
		return nil, listener.cause
	}
	connection, err := listener.Listener.Accept()
	listener.accepted = err == nil
	return connection, err
}

func TestOPS05HTTPExitCleanupIntegration(t *testing.T) {
	_, _, address := newServerContractDatabase(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, placement := range []config.ComponentPlacement{config.PlacementEmbedded, config.PlacementExternal} {
		for _, mode := range []string{"signal", "listen_failure_clean", "listen_failure", "signal_and_listen_failure"} {
			t.Run(string(placement)+"/"+mode, func(t *testing.T) {
				workerFailure := mode == "listen_failure" || mode == "signal_and_listen_failure"
				cfg := arch06Configuration(t, address, "https://provider.invalid")
				cfg.IndexWorkerPlacement = placement
				a, err := assembleApplication(t.Context(), cfg, nil, logger)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(a.database.Close)
				t.Cleanup(func() { _ = a.close(nil, true) })
				listenCause := errors.New("synthetic listener failure")
				workerCauses := []error{errors.New("first worker failure"), errors.New("last worker failure")}
				calls := make([]int, len(workerCauses))
				var deadlines []time.Time
				for i, cause := range workerCauses {
					a.workers = append(a.workers, workerFunc(func(ctx context.Context) error {
						calls[i]++
						if a.app.accepting.Load() {
							t.Error("준비 상태 이탈 전에 작업자를 닫았다")
						}
						deadline, _ := ctx.Deadline()
						deadlines = append(deadlines, deadline)
						if workerFailure {
							return cause
						}
						return nil
					}))
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				entered, release, fail := make(chan struct{}), make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				failAccept := sync.OnceFunc(func() { close(fail) })
				defer unblock()
				defer failAccept()
				server := newHTTPServer(listener.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					close(entered)
					<-release
					w.WriteHeader(http.StatusNoContent)
				}))
				defer server.Close()
				var receiving net.Listener = listener
				if mode == "listen_failure" || mode == "listen_failure_clean" {
					receiving = &ops05Listener{Listener: listener, fail: fail, cause: listenCause}
				}
				signals := make(chan os.Signal, 1)
				results, responses := make(chan error, 1), make(chan error, 1)
				go func() {
					results <- a.runHTTP(server, func() error {
						err := server.Serve(receiving)
						if mode == "signal_and_listen_failure" {
							return listenCause
						}
						return err
					}, signals)
				}()
				go func() {
					client := &http.Client{Timeout: 5 * time.Second}
					response, err := client.Get("http://" + listener.Addr().String())
					if err == nil {
						response.Body.Close()
						if response.StatusCode != http.StatusNoContent {
							err = errors.New("진행 요청의 응답 유실")
						}
					}
					responses <- err
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("진행 요청이 시작되지 않았다")
				}
				if mode == "listen_failure" || mode == "listen_failure_clean" {
					failAccept()
				} else {
					signals <- syscall.SIGTERM
				}
				limit := time.Now().Add(2 * time.Second)
				for a.app.accepting.Load() && time.Now().Before(limit) {
					time.Sleep(time.Millisecond)
				}
				if a.app.accepting.Load() {
					t.Fatal("수신 종료 뒤 준비 상태가 유지됐다")
				}
				select {
				case err := <-results:
					t.Fatalf("진행 요청을 기다리지 않고 종료했다: %v", err)
				default:
				}
				unblock()
				select {
				case err = <-results:
				case <-time.After(5 * time.Second):
					t.Fatal("공통 정리가 반환하지 않았다")
				}
				if responseErr := <-responses; responseErr != nil {
					t.Fatal(responseErr)
				}
				if mode == "signal" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, listenCause) {
					t.Fatalf("수신 오류 유실: %v", err)
				}
				if !workerFailure {
					if a.database.Ping(t.Context()) == nil {
						t.Fatal("정리 성공 뒤 풀이 열려 있다")
					}
				} else {
					if !errors.Is(err, workerCauses[0]) || !errors.Is(err, workerCauses[1]) {
						t.Fatalf("수신 또는 작업자 원인 유실: %v", err)
					}
					if pingErr := a.database.Ping(t.Context()); pingErr != nil {
						t.Fatalf("작업자 실패에도 풀을 닫았다: %v", pingErr)
					}
				}
				_ = a.close(nil, true)
				if calls[0] != 1 || calls[1] != 1 || len(deadlines) != 2 || deadlines[0].IsZero() || !deadlines[0].Equal(deadlines[1]) {
					t.Fatalf("작업자 호출·공유 기한: %v %v", calls, deadlines)
				}
				data, readErr := os.ReadFile(filepath.Join(cfg.Diagnostic.Directory, cfg.Diagnostic.InstanceID, "system.jsonl"))
				if readErr != nil || !bytes.Contains(data, []byte(`"stage":"shutdown"`)) {
					t.Fatalf("종료 기록 flush 실패: %v", readErr)
				}
				source, _ := diagnostic.NewSource(cfg.Diagnostic.InstanceID)
				reopened, openErr := diagnostic.NewRecorder(t.Context(), cfg.Diagnostic.Directory, source)
				if openErr != nil {
					t.Fatal("종료 뒤 진단 잠금 누수", openErr)
				}
				if closeErr := reopened.Close(context.Background()); closeErr != nil {
					t.Fatal(closeErr)
				}
			})
		}
	}
}
