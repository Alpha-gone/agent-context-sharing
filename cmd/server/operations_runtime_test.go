//go:build operations_runtime

package main

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
)

// 명시한 태그에서 전용 소유 라벨·실제 접속 포트를 대조한 뒤 실행한다.
// Docker 정지·복귀는 이 시험 전용의 합성 DB에만 적용한다.
func TestOPS09DatabaseRecoveryRuntimeIntegration(t *testing.T) {
	id := os.Getenv("TEST_OPS09_CONTAINER")
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 32 {
		t.Fatal("TEST_OPS09_CONTAINER에는 새 합성 DB의 전체 ID가 필요합니다")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	docker := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", args...).Output()
	}
	label, err := docker("inspect", "--format", `{{index .Config.Labels "acs.task"}}`, id)
	if err != nil || strings.TrimSpace(string(label)) != "ops09-runtime" {
		t.Fatal("OPS-09 전용 컨테이너 소유 라벨 불일치")
	}
	portData, err := docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", id)
	configuredData, configuredErr := docker("inspect", "--format", "{{json .HostConfig.PortBindings}}", id)
	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	var configured map[string][]struct {
		HostPort string `json:"HostPort"`
	}
	address, addressErr := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || configuredErr != nil || addressErr != nil || json.Unmarshal(portData, &ports) != nil || json.Unmarshal(configuredData, &configured) != nil || len(configured["5432/tcp"]) != 1 || len(ports["5432/tcp"]) != 1 || configured["5432/tcp"][0].HostPort != ports["5432/tcp"][0].HostPort || ports["5432/tcp"][0].HostIP != "127.0.0.1" || address.Hostname() != "127.0.0.1" || address.Port() != ports["5432/tcp"][0].HostPort {
		t.Fatal("정지 대상과 TEST_DATABASE_URL의 loopback 포트 불일치")
	}
	database, _, isolated := newServerContractDatabase(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic provider unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	cfg := arch06Configuration(t, isolated, provider.URL)
	assembly, err := assembleApplication(ctx, cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := assembly.close(nil, true); err != nil {
			t.Error(err)
		}
	})
	service, err := authz.New(database, authz.Config{Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: 4, OAuth: oauthConfiguration(cfg), AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database, Connections: database})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := service.Register(ctx, "ops09_owner", "ops09-synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	client := "codex-cli-first-test"
	callback := cfg.OAuthClients[client].RedirectURIs[0]
	code, err := service.IssueAuthorizationCode(ctx, owner, authz.AuthorizationRequest{ClientID: client, RedirectURI: callback, Resource: cfg.MCPResourceURL.String(), ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := service.ExchangeAuthorizationCode(ctx, code, client, callback, cfg.MCPResourceURL.String(), "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(assembly.app.handler())
	t.Cleanup(server.Close)
	server.Client().Timeout = 5 * time.Second
	call := func(name string, args map[string]any) map[string]any {
		return arch06MCP(t, server, cfg.MCPResourceURL.Host, tokens.AccessToken, "tools/call", name, args)["structuredContent"].(map[string]any)
	}
	graph := call("graph_create", map[string]any{"name": "OPS09 preserved graph", "request_id": "ops09-graph-create-001"})["graph"].(map[string]any)
	graphID := graph["graph_id"].(string)
	parsedGraph, err := model.ParseID(graphID)
	if err != nil {
		t.Fatal("시험 그래프 식별 실패")
	}
	grade, _, err := assembly.database.EffectiveGrade(ctx, parsedGraph, owner)
	if err != nil || grade != model.GraphGradeOwner {
		t.Fatal("DB 중단 전 소유자 등급 확인 실패")
	}
	probe := func(path string) int {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal("상태 확인 요청 준비 실패")
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal("상태 확인 TLS 요청 실패")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		return response.StatusCode
	}
	if probe("/healthz") != 200 || probe("/readyz") != 200 {
		t.Fatal("DB 중단 전 생존/준비 확인 실패")
	}
	// DB가 내려간 실패 경로에서도 다른 cleanup보다 먼저 복귀시켜
	// 이 시험이 만든 개별 DB와 저장소를 정상 정리할 수 있게 한다.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := exec.CommandContext(cleanup, "docker", "start", id).Run(); err != nil {
			t.Error("시험 소유 DB의 정리 전 복귀 실패")
		}
	})
	if _, err := docker("stop", "--time", "5", id); err != nil {
		t.Fatal("시험 소유 DB 정지 실패")
	}
	if probe("/healthz") != 200 || probe("/readyz") != 503 {
		t.Fatal("실제 DB 중단의 생존/준비 상태 분리 실패")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"ops09-outage","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = cfg.MCPResourceURL.Host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/list")
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal("DB 중단의 실제 인증 요청 실패")
	}
	var failure map[string]any
	decodeErr := json.UnmarshalRead(io.LimitReader(response.Body, 64<<10), &failure)
	response.Body.Close()
	if response.StatusCode != 500 || decodeErr != nil || failure["error"] != "server_error" || response.Header.Get("WWW-Authenticate") != "" {
		t.Fatal("DB 장애가 인증 실패나 재인가 도전으로 위장되었습니다")
	}
	if _, err := docker("start", id); err != nil {
		t.Fatal("시험 소유 DB 재시작 실패")
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	recovered := false
	for limit := time.Now().Add(20 * time.Second); time.Now().Before(limit); {
		if probe("/readyz") == 200 {
			recovered = true
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("DB 복귀 대기 취소")
		}
	}
	if !recovered || probe("/healthz") != 200 {
		t.Fatal("동일 서버의 DB 준비 상태 회복 실패")
	}
	restored := call("graph_get", map[string]any{"graph_id": graphID})["graph"].(map[string]any)
	if restored["graph_id"] != graphID || restored["name"] != "OPS09 preserved graph" || fmt.Sprint(restored["version"]) != "1" {
		t.Fatal("DB 중단/복귀 뒤 그래프·권한·기존 자격 보존 실패")
	}
	grade, _, err = assembly.database.EffectiveGrade(ctx, parsedGraph, owner)
	if err != nil || grade != model.GraphGradeOwner {
		t.Fatal("DB 복귀 뒤 소유자 등급 보존 실패")
	}
	t.Log("OPS-09 stage=database_recovered healthz=200 readiness_during_outage=503 authorization_during_outage=500 readiness_after_restart=200 original_credentials_and_graph_preserved=true")
}
