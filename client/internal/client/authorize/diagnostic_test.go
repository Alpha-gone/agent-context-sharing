package authorize

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type diagnosticListener struct {
	net.Listener
	writing chan struct{}
	release chan struct{}
	fail    bool
}

func (l diagnosticListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &diagnosticConn{Conn: conn, writing: l.writing, release: l.release, fail: l.fail}, nil
}

type diagnosticConn struct {
	net.Conn
	writing chan struct{}
	release chan struct{}
	once    sync.Once
	fail    bool
}

func (c *diagnosticConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.writing) })
	<-c.release
	if c.fail {
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(p)
}

func TestDiagnosticResponseCompletesBeforeSuccess(t *testing.T) {
	writing, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	opened := make(chan string, 1)
	manager, err := New(testConfig(t, "5s"), Options{
		Listen: func(ctx context.Context, network, address string) (net.Listener, error) {
			listener, err := (&net.ListenConfig{}).Listen(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return diagnosticListener{listener, writing, release, false}, nil
		},
		OpenBrowser: func(_ context.Context, target string) error { opened <- target; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- manager.CheckBrowserLoopback(ctx) }()
	var target string
	select {
	case target = <-opened:
	case <-ctx.Done():
		t.Fatal("opener 미실행")
	}
	responseDone := make(chan error, 1)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	go func() {
		response, err := client.Get(target)
		if err == nil {
			defer response.Body.Close()
			var body []byte
			body, err = io.ReadAll(response.Body)
			if err == nil && (response.StatusCode != http.StatusOK || response.ContentLength != 0 || len(body) != 0) {
				err = errors.New("진단 응답의 상태·빈 본문 경계 오류")
			}
		}
		responseDone <- err
	}()
	select {
	case <-writing:
	case err := <-result:
		t.Fatalf("응답 전송 전에 결과 게시: %v", err)
	case <-ctx.Done():
		t.Fatal("응답 전송 미시작")
	}
	select {
	case err := <-result:
		t.Fatalf("응답 전송 완료 전에 결과 게시: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	unblock.Do(func() { close(release) })
	if err := <-responseDone; err != nil {
		t.Fatalf("진단 응답 미완결: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("전송 완료 뒤 결과 미게시")
	}
}

func TestDiagnosticWriteFailureIsNotSuccess(t *testing.T) {
	release := make(chan struct{})
	close(release)
	manager, err := New(testConfig(t, "1s"), Options{
		Listen: func(ctx context.Context, network, address string) (net.Listener, error) {
			listener, err := (&net.ListenConfig{}).Listen(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return diagnosticListener{listener, make(chan struct{}), release, true}, nil
		},
		OpenBrowser: func(ctx context.Context, target string) error {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return err
			}
			response, err := http.DefaultClient.Do(request)
			if response != nil {
				response.Body.Close()
			}
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := manager.CheckBrowserLoopback(ctx); !errors.Is(err, ErrAuthorization) {
		t.Fatalf("진단 전송 실패를 성공으로 보고했다: %v", err)
	}
}

func TestDiagnosticLoopbackRejectsOtherRequestsAndCleansUp(t *testing.T) {
	var callback string
	manager, err := New(testConfig(t, "1s"), Options{OpenBrowser: func(ctx context.Context, target string) error {
		callback = target
		for _, suffix := range []string{"/wrong", "?state=private"} {
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target+suffix, nil)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return err
			}
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Error("진단 경로 위반 승인")
			}
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Error("정상 진단 callback 거부")
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := manager.CheckBrowserLoopback(ctx); err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: time.Second}).Get(callback)
	if err == nil {
		response.Body.Close()
		t.Fatal("진단 종료 뒤 listener 잔존")
	}
}

func TestDiagnosticLoopbackFailuresAndTimeout(t *testing.T) {
	for _, mode := range []string{"listen", "open", "timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			options := Options{OpenBrowser: func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }}
			if mode == "listen" {
				options.Listen = func(context.Context, string, string) (net.Listener, error) { return nil, errors.New("private-listen") }
			}
			if mode == "open" {
				options.OpenBrowser = func(context.Context, string) error { return errors.New("private-url") }
			}
			manager, err := New(testConfig(t, "1s"), options)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			want := ErrAuthorization
			if mode == "timeout" {
				want = context.DeadlineExceeded
			}
			if mode == "cancel" {
				want = context.Canceled
			}
			if err := manager.CheckBrowserLoopback(ctx); !errors.Is(err, want) {
				t.Fatalf("진단 %s 오류=%v", mode, err)
			}
		})
	}
}
