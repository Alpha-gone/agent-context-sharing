package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

func TestNetworkUsesProxyRouteAndVerifiesTLS(t *testing.T) {
	var heads, connects atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "" || r.Header.Get("DPoP") != "" {
			t.Errorf("진단이 보호 도구 요청을 실행함: %s %s", r.Method, r.URL.Path)
		}
		heads.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "proxy-only.invalid:443" {
			t.Errorf("프록시 CONNECT: %s %s", r.Method, r.Host)
			w.WriteHeader(400)
			return
		}
		connects.Add(1)
		upstream, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := buffered.Flush(); err != nil {
			t.Error(err)
			return
		}
		var copies sync.WaitGroup
		copies.Go(func() { io.Copy(upstream, buffered); upstream.Close() })
		io.Copy(connection, upstream)
		connection.Close()
		copies.Wait()
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transport.Proxy = http.ProxyURL(proxyURL)
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: target.Certificate().DNSNames[0]}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != proxyURL.Host {
			return nil, errors.New("직접 연결 금지")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := checkNetwork(ctx, "https://proxy-only.invalid/mcp", transport); err != nil {
		t.Fatalf("프록시 전용 환경의 진단 실패: %v", err)
	}
	if heads.Load() != 1 || connects.Load() != 1 {
		t.Fatal("프록시 경로 미사용")
	}
	// ProxyFromEnvironment는 프로세스별로 환경을 캐시하므로 독립 프로세스로 검사한다.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, executable, "-test.run=^TestNetworkProxyEnvironmentHelper$")
	child.Env = append(os.Environ(), "CLIENT_TEST_PROXY_CHILD=1", "HTTPS_PROXY="+proxy.URL, "https_proxy=", "NO_PROXY=", "no_proxy=", "HTTP_PROXY=", "http_proxy=", "CLIENT_TEST_CERT="+base64.StdEncoding.EncodeToString(target.Certificate().Raw), "CLIENT_TEST_TLS_NAME="+target.Certificate().DNSNames[0])
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("HTTPS_PROXY 환경 진단: %v\n%s", err, output)
	}
	if heads.Load() != 2 || connects.Load() != 2 {
		t.Fatal("HTTPS_PROXY 환경 경로 미사용")
	}
	transport.TLSClientConfig.ServerName = "wrong.invalid"
	if err := checkNetwork(ctx, "https://proxy-only.invalid/mcp", transport); !errors.Is(err, contract.ErrTransport) {
		t.Fatalf("프록시 경로의 잘못된 TLS 호스트 승인: %v", err)
	}
	if heads.Load() != 2 || connects.Load() != 3 {
		t.Fatal("TLS 검증 전 HEAD 전송")
	}
}

func TestNetworkProxyEnvironmentHelper(t *testing.T) {
	if os.Getenv("CLIENT_TEST_PROXY_CHILD") != "1" {
		return
	}
	der, err := base64.StdEncoding.DecodeString(os.Getenv("CLIENT_TEST_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: os.Getenv("CLIENT_TEST_TLS_NAME")}
	if err := checkNetwork(t.Context(), "https://proxy-only.invalid/mcp", transport); err != nil {
		t.Fatal(err)
	}
}
