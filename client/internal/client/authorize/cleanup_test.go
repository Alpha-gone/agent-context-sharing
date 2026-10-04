package authorize

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type observedListener struct {
	net.Listener
	accepted chan struct{}
}

func (l observedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		select {
		case l.accepted <- struct{}{}:
		default:
		}
	}
	return conn, err
}

func TestPreconnectedCallbackSocketDoesNotDelayCleanup(t *testing.T) {
	for _, action := range []string{"success", "denied", "cancel", "timeout", "opener", "close"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			opened := make(chan string, 1)
			failOpen := make(chan struct{})
			f.open = func(ctx context.Context, target string) error {
				opened <- target
				select {
				case <-failOpen:
					return errors.New("opener failure")
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			timeout := "5s"
			if action == "timeout" {
				timeout = "1s"
			}
			m := f.manager(timeout)
			listen := m.listen
			accepted := make(chan struct{}, 1)
			m.listen = func(ctx context.Context, network, address string) (net.Listener, error) {
				listener, err := listen(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return observedListener{listener, accepted}, nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := m.Credentials(ctx, Challenge{})
				result <- err
			}()
			var target string
			select {
			case target = <-opened:
			case <-time.After(2 * time.Second):
				t.Fatal("opener 미실행")
			}
			callback, err := url.Parse(callbackURL(target))
			if err != nil {
				t.Fatal(err)
			}
			conn, err := net.DialTimeout("tcp", callback.Host, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			select {
			case <-accepted:
			case <-time.After(time.Second):
				t.Fatal("사전 연결 미접수")
			}
			closed := make(chan struct{})
			switch action {
			case "success", "denied":
				if action == "denied" {
					query := callback.Query()
					query.Del("code")
					query.Set("error", "access_denied")
					callback.RawQuery = query.Encode()
				}
				if status, err := callbackRequest(callback.String(), ""); err != nil || status != http.StatusOK {
					t.Fatalf("callback 응답 실패: %d, %v", status, err)
				}
			case "cancel":
				cancel()
			case "close":
				go func() { m.Close(); close(closed) }()
			case "opener":
				close(failOpen)
			}
			select {
			case err := <-result:
				if action == "success" && err != nil || action != "success" && err == nil {
					t.Fatalf("인가 결과 불일치: %v", err)
				}
				if action == "cancel" && !errors.Is(err, context.Canceled) || action == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("취소·시간 초과 분류 불일치: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("사전 연결이 수신기 정리를 지연했다")
			}
			if action == "close" {
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("Manager.Close가 종료하지 않았다")
				}
			}
			if got := f.exchanges.Load(); action == "success" && got != 1 || action != "success" && got != 0 {
				t.Fatalf("코드 교환 횟수 불일치: %d", got)
			}
			f.assertClosed()
			if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("사전 연결이 닫히지 않았다: %v", err)
			}
		})
	}
}

func TestCallbackResponseCompletesBeforeListenerCloses(t *testing.T) {
	f := newFixture(t)
	opened := make(chan string, 1)
	f.open = func(ctx context.Context, target string) error { opened <- target; return nil }
	m := f.manager("5s")
	result := make(chan error, 1)
	go func() { _, err := m.Credentials(t.Context(), Challenge{}); result <- err }()
	var target string
	select {
	case target = <-opened:
	case <-time.After(time.Second):
		t.Fatal("opener 미실행")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Get(callbackURL(target))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if body, err := io.ReadAll(response.Body); err != nil || len(body) != 0 || response.StatusCode != http.StatusOK {
		t.Fatalf("완전한 HTTP 응답이 아니다: status=%d, bytes=%d, err=%v", response.StatusCode, len(body), err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback 뒤 인가 미완료")
	}
	f.assertClosed()
}
