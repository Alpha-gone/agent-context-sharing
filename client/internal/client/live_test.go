//go:build client_live

package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/contract"
	"agent_context_sharing/client/internal/client/doctor"
	"agent_context_sharing/client/internal/client/host"
	"agent_context_sharing/client/internal/client/remote"
)

// TestClientLiveService는 서버 모듈이 제공한 실제 서비스 fixture만 사용한다.
func TestClientLiveService(t *testing.T) {
	if os.Getenv("CLIENT_LIVE_URL") == "" {
		t.Fatal("test-live.sh의 실제 서버 fixture가 필요하다")
	}
	certDER, err := base64.StdEncoding.DecodeString(os.Getenv("CLIENT_LIVE_CERT"))
	if err != nil {
		t.Fatal("시험 TLS 인증서 인코딩 오류")
	}
	certificate, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal("시험 TLS 인증서 오류")
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	defer transport.CloseIdleConnections()
	values := map[string]string{
		"AGENT_CONTEXT_CLIENT_REMOTE_URL": os.Getenv("CLIENT_LIVE_URL"),
		"AGENT_CONTEXT_CLIENT_ID":         "client-live", "AGENT_CONTEXT_CLIENT_AGENT_ID": os.Getenv("CLIENT_LIVE_AGENT"),
		"AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT": "30s", "AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "30s",
	}
	cfg, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	authorizationOptions := authorize.Options{Transport: transport, OpenBrowser: func(ctx context.Context, target string) error {
		opens.Add(1)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		response, err := (&http.Client{Transport: transport}).Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("시험 callback HTTP 상태=%d", response.StatusCode)
		}
		return err
	}}
	manager, err := authorize.New(cfg, authorizationOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	// 원격 도구 본문과 호스트 결과를 직접 비교하되 토큰·본문을 로그에 남기지 않는다.
	capture := &liveCapture{next: transport}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	upstream, err := remote.NewHTTP(cfg, manager, remote.HTTPOptions{Transport: capture, Logger: logger, Version: "live-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	relay := host.NewTools(upstream, cfg.PublicationPolicy(), cfg.AgentID())
	input, send := io.Pipe()
	receive, output := io.Pipe()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- host.Run(ctx, input, output, logger, "live-test", relay) }()
	t.Cleanup(func() {
		cancel()
		send.Close()
		receive.Close()
		output.Close()
		if err := <-done; err != nil {
			t.Errorf("호스트 종료: %v", err)
		}
	})
	reader := bufio.NewReader(receive)
	exchange := func(t *testing.T, method string, params map[string]any) map[string]jsontext.Value {
		t.Helper()
		params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
		id := uuid.NewV7().String()
		message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := send.Write(append(message, '\n')); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			ID     string                    `json:"id"`
			Result map[string]jsontext.Value `json:"result"`
			Error  jsontext.Value            `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatal("호스트 JSON 오류")
		}
		if response.ID != id || len(response.Error) != 0 {
			var detail struct {
				Code int `json:"code"`
				Data struct {
					ClientError struct {
						Code string `json:"code"`
					} `json:"client_error"`
				} `json:"data"`
			}
			json.Unmarshal(response.Error, &detail)
			t.Fatalf("%s 호스트 응답 오류: id_match=%t rpc_code=%d client_code=%s", method, response.ID == id, detail.Code, detail.Data.ClientError.Code)
		}
		if bytes.Equal(response.Result["isError"], []byte("true")) {
			t.Fatal("실제 서비스 도구 오류")
		}
		return response.Result
	}
	exchange(t, "server/discover", map[string]any{})
	listed := exchange(t, "tools/list", map[string]any{})
	var tools []contract.Tool
	if err := json.Unmarshal(listed["tools"], &tools); err != nil || len(tools) != 13 {
		t.Fatal("실제 서비스 도구 13종 목록 불일치")
	}
	fixtures, err := base64.StdEncoding.DecodeString(os.Getenv("CLIENT_LIVE_CASES"))
	if err != nil {
		t.Fatal("시험 인자 인코딩 오류")
	}
	var cases map[string]map[string]any
	if err := json.Unmarshal(fixtures, &cases); err != nil || len(cases) != 13 {
		t.Fatal("도구 13종 시험 인자 누락")
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			arguments := cases[name]
			if _, bypass := arguments["created_by_agent"]; bypass {
				t.Fatal("호스트 시험 입력에 행위 에이전트 포함")
			}
			result := exchange(t, "tools/call", map[string]any{"name": name, "arguments": arguments})
			capture.mu.Lock()
			defer capture.mu.Unlock()
			for _, field := range []string{"content", "structuredContent", "isError"} {
				if !bytes.Equal(result[field], capture.result[field]) {
					t.Fatalf("%s의 %s 원문 불일치", name, field)
				}
			}
		})
	}
	if opens.Load() != 1 {
		t.Fatalf("브라우저 대체 실행 수=%d", opens.Load())
	}
	// 유효 토큰을 Bearer로 내리거나 같은 proof를 다시 보내도 실제 수신 측이 거부한다.
	requestBody := `{"jsonrpc":"2.0","id":"negative-proof","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, cfg.RemoteURL(), strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/list")
	if _, err := manager.Apply(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	check := func(r *http.Request, want int) {
		t.Helper()
		response, err := (&http.Client{Transport: transport}).Do(r)
		if err != nil {
			t.Fatal("proof 시험 전송 실패")
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		if response.StatusCode != want {
			t.Fatalf("proof 시험 HTTP 상태=%d, 기대=%d", response.StatusCode, want)
		}
	}
	check(request, http.StatusOK)
	replay := request.Clone(t.Context())
	replay.Body = io.NopCloser(strings.NewReader(requestBody))
	check(replay, http.StatusUnauthorized)
	bearer := request.Clone(t.Context())
	bearer.Body = io.NopCloser(strings.NewReader(requestBody))
	bearer.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(request.Header.Get("Authorization"), "DPoP "))
	check(bearer, http.StatusUnauthorized)
	wrongKey := request.Clone(t.Context())
	wrongKey.Body = io.NopCloser(strings.NewReader(requestBody))
	wrongKey.Header.Set("DPoP", liveWrongKeyProof(t, cfg.RemoteURL(), strings.TrimPrefix(request.Header.Get("Authorization"), "DPoP ")))
	check(wrongKey, http.StatusUnauthorized)
	parallel := func(requests []*http.Request, refresh *authorize.Manager) (int, int) {
		t.Helper()
		var success, rejected atomic.Int32
		var workers sync.WaitGroup
		start := make(chan struct{})
		for _, prepared := range requests {
			workers.Go(func() {
				<-start
				response, err := (&http.Client{Transport: transport}).Do(prepared)
				if err != nil {
					t.Error("동시 proof 시험 전송 실패")
					return
				}
				defer response.Body.Close()
				io.Copy(io.Discard, response.Body)
				switch response.StatusCode {
				case http.StatusOK:
					success.Add(1)
					if refresh != nil && (response.Header.Get("Mcp-Access-Token") == "" || refresh.Refresh(t.Context(), response.Header) != nil) {
						t.Error("실제 동시 갱신 결합·헤더 검증 실패")
					}
				case http.StatusUnauthorized:
					rejected.Add(1)
				default:
					t.Error("동시 proof 시험 HTTP 상태 오류")
				}
			})
		}
		close(start)
		workers.Wait()
		return int(success.Load()), int(rejected.Load())
	}
	concurrent := request.Clone(t.Context())
	if _, err := manager.Apply(t.Context(), concurrent); err != nil {
		t.Fatal(err)
	}
	requests := make([]*http.Request, 100)
	for i := range requests {
		requests[i] = concurrent.Clone(t.Context())
		requests[i].Body = io.NopCloser(strings.NewReader(requestBody))
	}
	if success, rejected := parallel(requests, nil); success != 1 || rejected != 99 {
		t.Fatalf("동시·교차 인스턴스 재생: 성공=%d 거부=%d", success, rejected)
	}
	if os.Getenv("CLIENT_LIVE_RENEWAL") == "true" {
		nearExpiry, err := authorize.New(cfg, authorizationOptions)
		if err != nil {
			t.Fatal(err)
		}
		defer nearExpiry.Close()
		if _, err := nearExpiry.Credentials(t.Context(), authorize.Challenge{}); err != nil {
			t.Fatal(err)
		}
		identity, err := nearExpiry.Identity(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for i := range requests {
			requests[i] = request.Clone(t.Context())
			requests[i].Body = io.NopCloser(strings.NewReader(requestBody))
			if _, err := nearExpiry.Apply(t.Context(), requests[i]); err != nil {
				t.Fatal(err)
			}
		}
		if success, rejected := parallel(requests, nearExpiry); success != 100 || rejected != 0 {
			t.Fatalf("실제 동시 갱신: 성공=%d 거부=%d", success, rejected)
		}
		current, err := nearExpiry.Identity(t.Context())
		if err != nil || current != identity {
			t.Fatal("실제 동시 갱신에서 주체 변경")
		}
	}
	report := doctor.Run(t.Context(), func(key string) string { return values[key] }, doctor.Options{
		Authorization: authorizationOptions, HTTP: remote.HTTPOptions{Transport: transport},
	})
	if report.Status != "pass" {
		t.Fatal("실제 서비스 doctor 실패")
	}
	var diagnostic bytes.Buffer
	if err := report.Write(&diagnostic, true); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"access_token", "code_verifier", "state=", os.Getenv("CLIENT_LIVE_URL")} {
		if strings.Contains(diagnostic.String(), forbidden) {
			t.Fatal("실제 서비스 doctor 비밀·URL 노출")
		}
	}
	t.Log("호스트 stdio → 클라이언트 HTTP → 실제 서비스의 13종 결과 원문 보존 및 proof 재생·Bearer 하향 거부 확인")
}

func liveWrongKeyProof(t *testing.T, target, token string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoding := base64.RawURLEncoding
	header, err := json.Marshal(map[string]any{"typ": "dpop+jwt", "alg": "ES256", "jwk": map[string]string{"kty": "EC", "crv": "P-256", "x": encoding.EncodeToString(key.X.FillBytes(make([]byte, 32))), "y": encoding.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}})
	if err != nil {
		t.Fatal(err)
	}
	ath := sha256.Sum256([]byte(token))
	claims, err := json.Marshal(map[string]any{"htm": "POST", "htu": target, "iat": time.Now().UTC().Unix(), "jti": uuid.NewV7().String(), "ath": encoding.EncodeToString(ath[:])})
	if err != nil {
		t.Fatal(err)
	}
	unsigned := encoding.EncodeToString(header) + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return unsigned + "." + encoding.EncodeToString(signature)
}

type liveCapture struct {
	next   http.RoundTripper
	mu     sync.Mutex
	result map[string]jsontext.Value
}

func (c *liveCapture) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := c.next.RoundTrip(request)
	if err != nil || request.Header.Get("Mcp-Method") != "tools/call" || response.StatusCode != http.StatusOK {
		return response, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	var envelope struct {
		Result map[string]jsontext.Value `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		response.Body.Close()
		return nil, err
	}
	c.mu.Lock()
	c.result = envelope.Result
	c.mu.Unlock()
	return response, nil
}
