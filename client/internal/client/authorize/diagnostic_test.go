package authorize

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

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
