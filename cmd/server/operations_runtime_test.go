//go:build operations_runtime

package main

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 명시한 태그에서 전용 소유 라벨·실제 접속 포트를 대조한 뒤 실행한다.
// Docker 정지·복귀는 이 시험 전용의 합성 DB에만 적용한다.
func TestOPS09DatabaseRecoveryRuntimeIntegration(t *testing.T) {
	id := os.Getenv("TEST_OPS09_CONTAINER")
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 32 {
		t.Fatal("TEST_OPS09_CONTAINER에는 새 합성 DB의 전체 ID가 필요합니다")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 360*time.Second)
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
	database, pool, isolated := newServerContractDatabase(t)
	// 0=503, 1=정상, 2=응답 지연. 외부 API 대신 동일한 로컬 TLS 제공자를 제어한다.
	var providerMode atomic.Int32
	providerStarted := make(chan struct{}, 1)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 1<<20))
		switch providerMode.Load() {
		case 0:
			http.Error(w, "synthetic provider unavailable", http.StatusServiceUnavailable)
		case 1:
			vector := make([]float64, 768)
			vector[0] = 1
			w.Header().Set("Content-Type", "application/json")
			if err := json.MarshalWrite(w, map[string]any{"embedding": map[string]any{"values": vector}}); err != nil {
				t.Error("합성 제공자 응답 실패", err)
			}
		case 2:
			select {
			case providerStarted <- struct{}{}:
			default:
			}
			<-request.Context().Done()
		}
	}))
	t.Cleanup(provider.Close)
	cfg := arch06Configuration(t, isolated, provider.URL)
	assembly, err := assembleApplication(ctx, cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
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
	registration := call("agent_register", map[string]any{"registration_key": "ops09_registration"})
	created := call("node_create", map[string]any{
		"request_id": "ops09-node-create-001", "graph_id": graphID, "agent_id": registration["agent_id"], "judgment_input": "ops09 audit judgment",
		"layer": "source", "body": "ops09 recovery evidence", "origin_kind": "user_utterance", "occurred_at": "2026-10-27T12:00:00Z",
		"source_ref": map[string]any{"channel": "file", "locator": "file:///ops09/source"},
	})
	nodeID := created["node"].(map[string]any)["context_id"].(string)
	flowArgs := map[string]any{"graph_id": graphID, "work_context": "ops09 recovery", "scope": "local"}
	assertPartial := func() {
		t.Helper()
		flow := call("context_flow_get", flowArgs)
		channels := flow["channels"].(map[string]any)
		if channels["semantic"].(map[string]any)["failure"] != "embedding_unavailable" || channels["keyword"].(map[string]any)["failure"] != "" || channels["time"].(map[string]any)["failure"] != "" || len(flow["contexts"].([]any)) != 1 {
			t.Fatal("제공자 장애가 정상 채널의 부분 응답을 훼손했습니다", channels)
		}
	}
	assertPartial()
	if found, err := assembly.indexer.RunOnceInGraph(ctx, parsedGraph); err != nil || !found {
		t.Fatal("503 제공자 색인 회차 실패", err)
	}
	var attempts, pending, embeddings int
	var retryAt time.Time
	if err := pool.QueryRow(ctx, `SELECT attempts, next_attempt_at FROM public.index_task WHERE graph_id=$1 AND state='pending' AND last_error IS NOT NULL`, graphID).Scan(&attempts, &retryAt); err != nil || attempts != 1 || !retryAt.After(time.Now()) {
		t.Fatal("제공자 장애의 재시도 상태 보존 실패", err)
	}
	// 열린 파일의 실제 권한을 바꾼다. 기록기 주입 함수나 가짜 Sink를 사용하지 않는다.
	requestLog := filepath.Join(cfg.Diagnostic.Directory, cfg.Diagnostic.InstanceID, "request.jsonl")
	wait := func(limit time.Duration, condition func() bool) {
		t.Helper()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(limit)
		defer deadline.Stop()
		for !condition() {
			select {
			case <-ticker.C:
			case <-deadline.C:
				t.Fatal("운영 장애 상태 전이 대기 초과")
			case <-ctx.Done():
				t.Fatal("운영 장애 인수 취소")
			}
		}
	}
	stats := func() diagnostic.RecorderStats {
		wait(5*time.Second, func() bool {
			value := assembly.recorder.Stats()
			return value.QueuedEvents == 0 && !value.Writing
		})
		return assembly.recorder.Stats()
	}
	beforeFault := stats()
	if err := os.Chmod(requestLog, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(requestLog, 0o600) })
	preserved := call("graph_create", map[string]any{"name": "OPS09 diagnostic fault commit", "request_id": "ops09-diag-create-001"})["graph"].(map[string]any)
	afterFault := stats()
	if afterFault.Dropped <= beforeFault.Dropped {
		t.Fatal("실제 진단 파일 장애의 손실 계수 누락")
	}
	if err := os.Chmod(requestLog, 0o600); err != nil {
		t.Fatal(err)
	}
	call("graph_get", map[string]any{"graph_id": preserved["graph_id"]})
	afterRecovery := stats()
	if afterRecovery.Written <= afterFault.Written || afterRecovery.Dropped != afterFault.Dropped {
		t.Fatal("진단 파일 복구 뒤 기록 재개 실패")
	}
	// 시험 전용 DB의 테이블 이름만 잠시 바꿔 실제 SQL 채널을 실패시킨다.
	// 원본 행·권한·색인 큐는 변경하지 않으며 실패 경로에서도 이름을 복구한다.
	if _, err := pool.Exec(ctx, `ALTER TABLE agent_context."Context" RENAME TO "OPS09ContextUnavailable"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = pool.Exec(cleanup, `ALTER TABLE IF EXISTS agent_context."OPS09ContextUnavailable" RENAME TO "Context"`)
	})
	failed := arch06MCPResult(t, server, cfg.MCPResourceURL.Host, tokens.AccessToken, "tools/call", "context_flow_get", flowArgs)
	if failed["isError"] != true || failed["structuredContent"].(map[string]any)["code"] != "internal" || probe("/readyz") != 200 {
		t.Fatal("전체 검색 채널 장애의 분류 또는 DB 준비 상태 불일치", failed)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE agent_context."OPS09ContextUnavailable" RENAME TO "Context"`); err != nil {
		t.Fatal(err)
	}
	assertPartial()
	t.Log("OPS-09 stage=combined_faults provider=503 index_pending=true partial_search=true all_entry_channels_failed=internal diagnostic_file_fault_preserves_commit=true diagnostic_recording_resumed=true")
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
	// DB 중단으로 무효화된 관리 연결을 재사용하지 않는다.
	recoveredPool, err := pgxpool.New(ctx, isolated)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(recoveredPool.Close)
	if err := recoveredPool.QueryRow(ctx, `SELECT count(*) FROM public.index_task WHERE graph_id=$1 AND state='pending' AND attempts=1`, graphID).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("DB 복귀 뒤 재시도 작업 보존 실패", err)
	}
	assertPartial()
	// 실제 1분 백오프와 2분 리스를 기다린다. DB/호스트 시각이나 큐 기한을 조작하지 않는다.
	wait(70*time.Second, func() bool { return !time.Now().Before(retryAt) })
	providerMode.Store(2)
	workContext, interrupt := context.WithCancel(ctx)
	workDone := make(chan error, 1)
	go func() {
		_, err := assembly.indexer.RunOnceInGraph(workContext, parsedGraph)
		workDone <- err
	}()
	select {
	case <-providerStarted:
	case <-ctx.Done():
		t.Fatal("지연 제공자 도착 실패")
	}
	interrupt()
	select {
	case err := <-workDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("작업자 중단의 취소 원인 누락", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("지연 제공자 호출의 취소 미회수")
	}
	var leasedUntil time.Time
	if err := recoveredPool.QueryRow(ctx, `SELECT next_attempt_at FROM public.index_task WHERE graph_id=$1 AND state='pending'`, graphID).Scan(&leasedUntil); err != nil || !leasedUntil.After(time.Now()) {
		t.Fatal("중단된 작업자의 리스 보존 실패", err)
	}
	providerMode.Store(1)
	if found, err := assembly.indexer.RunOnceInGraph(ctx, parsedGraph); err != nil || found {
		t.Fatal("만료 전 리스를 다른 회차가 재확보했습니다", err)
	}
	t.Log("OPS-09 stage=worker_interrupted provider_delay_cancelled=true pending_lease_preserved=true waiting_for_natural_lease_expiration=true")
	wait(130*time.Second, func() bool { return !time.Now().Before(leasedUntil) })
	if found, err := assembly.indexer.RunOnceInGraph(ctx, parsedGraph); err != nil || !found {
		t.Fatal("리스 자연 만료 뒤 색인 재확보 실패", err)
	}
	if err := assembly.database.CheckGraphIndexReady(ctx, parsedGraph, assembly.indexer.ModelID()); err != nil {
		t.Fatal("제공자·DB·작업자 복구 뒤 색인 미완료", err)
	}
	if err := recoveredPool.QueryRow(ctx, `SELECT count(*) FROM public.context_embedding WHERE graph_id=$1 AND context_id=$2`, graphID, nodeID).Scan(&embeddings); err != nil || embeddings != 1 {
		t.Fatal("복구 임베딩의 단일 공개 실패", err)
	}
	flow := call("context_flow_get", flowArgs)
	channels := flow["channels"].(map[string]any)
	if channels["semantic"].(map[string]any)["failure"] != "" || fmt.Sprint(channels["semantic"].(map[string]any)["candidates"]) != "1" || len(flow["contexts"].([]any)) != 1 {
		t.Fatal("복구 뒤 실제 의미 검색 재합류 실패", channels)
	}
	// 정상 파일 권한을 되돌려도 과거 손실은 최종 종료 오류로 남아야 한다.
	closeErr := assembly.close(nil, true)
	closed = true
	if closeErr == nil || assembly.recorder.Stats().Dropped != afterFault.Dropped {
		t.Fatal("공통 정리가 실제 진단 손실을 누락했습니다")
	}
	if err := assembly.database.Ping(ctx); err == nil {
		t.Fatal("공통 정리 뒤 DB 풀이 열려 있습니다")
	}
	t.Log("OPS-09 stage=combined_recovered original_node_preserved=true pending_index=0 current_embedding=1 semantic_channel_rejoined=true cleanup_closed_pool=true diagnostic_loss_reported=true")
	t.Log("OPS-09 stage=database_recovered healthz=200 readiness_during_outage=503 authorization_during_outage=500 readiness_after_restart=200 original_credentials_and_graph_preserved=true")
}
