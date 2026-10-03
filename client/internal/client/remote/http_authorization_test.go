package remote

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"
)

func TestHTTPInitialAuthorizationDoesNotResendProbe(t *testing.T) {
	a := new(testAuthentication)
	fixture := newCatalogSource(t)
	var sequence []string
	c := newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		method := r.Header.Get("Mcp-Method")
		if r.Header.Get("Authorization") == "" {
			sequence = append(sequence, method+"-anon")
			return challengeResponse(""), nil
		}
		sequence = append(sequence, method+"-auth")
		switch method {
		case "server/discover":
			return rpcResponse(id, fixture.discovery), nil
		case "tools/list":
			return rpcResponse(id, fixture.tools), nil
		default:
			return rpcResponse(id, fixture.result), nil
		}
	}), HTTPOptions{})
	if _, err := c.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); err != nil {
		t.Fatal(err)
	}
	want := []string{"tools/list-anon", "server/discover-auth", "tools/list-auth", "tools/call-auth"}
	if !slices.Equal(sequence, want) {
		t.Fatalf("초기 호출 순서 %v; 기대 %v", sequence, want)
	}
}

func TestHTTPAuthorizationWaitHasSeparateTimeout(t *testing.T) {
	for _, stage := range []string{"initial", "catalog", "tool"} {
		t.Run(stage, func(t *testing.T) {
			a := readyAuthentication()
			a.ready.Store(stage != "initial")
			a.onAuth = func(ctx context.Context) error { return waitDelay(ctx, 200*time.Millisecond) }
			fixture := newCatalogSource(t)
			challenged := false
			c, err := NewHTTP(remoteConfig(t, "100ms"), a, HTTPOptions{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				id := wireID(t, r)
				method := r.Header.Get("Mcp-Method")
				if !challenged && (stage == "initial" || stage == "catalog" && method == "tools/list" || stage == "tool" && method == "tools/call") {
					challenged = true
					kind := "invalid_token"
					if stage == "initial" {
						kind = ""
					}
					return challengeResponse(kind), nil
				}
				switch method {
				case "server/discover":
					return rpcResponse(id, fixture.discovery), nil
				case "tools/list":
					return rpcResponse(id, fixture.tools), nil
				default:
					return rpcResponse(id, fixture.result), nil
				}
			})})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Close)
			if _, err := c.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); err != nil || a.authorizes.Load() != 1 {
				t.Fatalf("원격 제한 시간보다 긴 인가를 완료하지 못했습니다: %v", err)
			}
		})
	}
}

func TestHTTPAuthorizationTimeoutAndHostDeadline(t *testing.T) {
	for _, scenario := range []string{"auth-timeout", "host-deadline"} {
		t.Run(scenario, func(t *testing.T) {
			a := new(testAuthentication)
			a.onAuth = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
			authTimeout := "1s"
			ctx := t.Context()
			if scenario == "auth-timeout" {
				authTimeout = "80ms"
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 80*time.Millisecond)
				defer cancel()
			}
			c, err := NewHTTP(remoteTimeoutConfig(t, "20ms", authTimeout), a, HTTPOptions{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				wireID(t, r)
				return challengeResponse(""), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Close)
			started := time.Now()
			if _, err := c.ListTools(ctx); !errors.Is(err, context.DeadlineExceeded) || a.ready.Load() || a.authorizes.Load() != 1 {
				t.Fatalf("인가/호스트 제한 시간의 실패·토큰 미저장 계약이 다릅니다: %v", err)
			}
			if time.Since(started) < 60*time.Millisecond {
				t.Fatal("원격 제한 시간으로 인가를 너무 일찍 중단했습니다")
			}
		})
	}
}

func TestHTTPReauthorizationPreservesRemainingTime(t *testing.T) {
	a := readyAuthentication()
	a.onAuth = func(ctx context.Context) error { return waitDelay(ctx, 500*time.Millisecond) }
	fixture := newCatalogSource(t)
	tools := 0
	c, err := NewHTTP(remoteConfig(t, "400ms"), a, HTTPOptions{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		switch r.Header.Get("Mcp-Method") {
		case "server/discover":
			return rpcResponse(id, fixture.discovery), nil
		case "tools/list":
			return rpcResponse(id, fixture.tools), nil
		default:
			tools++
			if tools == 1 {
				if err := waitDelay(r.Context(), 150*time.Millisecond); err != nil {
					return nil, err
				}
				return challengeResponse("invalid_token"), nil
			}
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > 275*time.Millisecond {
				t.Error("재인가 뒤 원격 처리 시간 예산이 초기화됐습니다")
			}
			<-r.Context().Done()
			return nil, io.EOF
		}
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := c.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); !errors.Is(err, context.DeadlineExceeded) || tools != 2 || a.authorizes.Load() != 1 {
		t.Fatalf("원격 처리 잔여 시간 종료 계약이 다릅니다: %v", err)
	}
}

func TestHTTPInitialAuthorizationKeepsRetryBudgetForCatalog(t *testing.T) {
	a := new(testAuthentication)
	fixture := newCatalogSource(t)
	lists, discovers := 0, 0
	c := newHTTPTestClient(t, a, transportFunc(func(r *http.Request) (*http.Response, error) {
		id := wireID(t, r)
		if r.Header.Get("Authorization") == "" {
			return challengeResponse(""), nil
		}
		switch r.Header.Get("Mcp-Method") {
		case "server/discover":
			discovers++
			return rpcResponse(id, fixture.discovery), nil
		case "tools/list":
			if discovers == 0 {
				t.Error("검증 없이 버리는 인증된 목록을 요청했습니다")
			}
			lists++
			if lists <= 3 {
				return nil, io.EOF
			}
			return rpcResponse(id, fixture.tools), nil
		default:
			return rpcResponse(id, fixture.result), nil
		}
	}), HTTPOptions{})
	if _, err := c.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); err != nil || lists != 4 || discovers != 1 || a.authorizes.Load() != 1 {
		t.Fatalf("실제 목록의 재시도 예산이 보존되지 않았습니다: %v", err)
	}
}
