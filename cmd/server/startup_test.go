package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent_context_sharing/internal/config"
)

// startupLogDirectory는 진단 경로 정책을 지키도록 작업 디렉터리 아래에 시험 전용 경로를 만든다.
func startupLogDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(".", ".startup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	absolute, err := filepath.Abs(filepath.Join(directory, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

// TestAssemblySignalStopsStartupWhileDatabaseHangs는 DB가 연결을 받고 응답하지 않는 조립 중에
// 종료 신호를 받으면 기동을 기한 안에 멈추는지 확인한다. 신호는 조립이 DB 접속을 시작한 뒤,
// 즉 신호 등록이 끝난 뒤에만 보내 시험 프로세스의 기본 종료 동작에 닿지 않게 한다.
func TestAssemblySignalStopsStartupWhileDatabaseHangs(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	var mu sync.Mutex
	var connections []net.Conn
	defer func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, connection := range connections {
			connection.Close()
		}
	}()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, connection)
			mu.Unlock()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	cfg := config.Config{
		Diagnostic:  config.DiagnosticConfig{Directory: startupLogDirectory(t), InstanceID: "startup_signal", Level: "INFO"},
		DatabaseURL: "postgres://synthetic:synthetic@" + listener.Addr().String() + "/synthetic?sslmode=disable", GraphName: "agent_context",
		EmbeddingProvider: "gemini", GeminiAPIKey: "synthetic-key", EmbeddingBaseURL: &url.URL{Scheme: "https", Host: "embedding.test"},
		EmbeddingModel: "gemini-embedding-2", EmbeddingVectorType: "vector", EmbeddingDimension: 768,
		RelationAdjacencyWindow: time.Hour, RelationSimilarityThreshold: .8, RelationProposalLimit: 10,
		IndexWorkerPlacement: config.PlacementExternal,
	}
	done := make(chan error, 1)
	go func() { done <- runConfigured(cfg) }()
	select {
	case <-accepted:
	case err := <-done:
		t.Fatalf("DB 접속 전에 기동이 끝났다: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("조립이 DB 접속을 시작하지 않았다")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("조립 중 종료 신호의 기동 결과 = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DB 무응답 조립 중 종료 신호를 무시했다")
	}
}

// TestDirectTLSCertificateCheckedBeforeAssembly는 직접 TLS 인증서를 진단 파일·DB 등 자원을
// 열기 전에 읽어, 잘못된 인증서가 작업자 시작 뒤에야 드러나지 않는지 확인한다.
func TestDirectTLSCertificateCheckedBeforeAssembly(t *testing.T) {
	logs := startupLogDirectory(t)
	missing := filepath.Join(filepath.Dir(logs), "missing")
	cfg := config.Config{
		TLSMode: config.TLSModeDirect, TLSCertFile: missing + ".crt", TLSKeyFile: missing + ".key",
		Diagnostic: config.DiagnosticConfig{Directory: logs, InstanceID: "startup_tls", Level: "INFO"},
	}
	if err := runConfigured(cfg); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("없는 인증서의 기동 = %v", err)
	}
	if _, err := os.Stat(logs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("인증서 검증 전에 진단 파일을 준비했다: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"service.test"}}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TLSCertFile, cfg.TLSKeyFile = filepath.Join(filepath.Dir(logs), "server.crt"), filepath.Join(filepath.Dir(logs), "server.key")
	if err := os.WriteFile(cfg.TLSCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.TLSKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0o600); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := serverTLSConfig(cfg)
	if err != nil || tlsConfig == nil || len(tlsConfig.Certificates) != 1 {
		t.Fatalf("직접 TLS 구성 = %#v, %v", tlsConfig, err)
	}
	cfg.TLSMode = config.TLSModeProxy
	if tlsConfig, err := serverTLSConfig(cfg); err != nil || tlsConfig != nil {
		t.Fatalf("프록시 종단의 TLS 구성 = %#v, %v", tlsConfig, err)
	}
}
