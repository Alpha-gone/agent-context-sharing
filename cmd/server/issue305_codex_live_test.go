//go:build codex_live

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

type issue305LiveCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Result    map[string]any `json:"result"`
	Failed    bool           `json:"failed"`
}

// 실제 모델 사용량을 소비한다. 기본 회귀에서는 컴파일·실행하지 않으며,
// 모델 응답 대역이나 미리 만든 업무 입력으로 실제 생성 호출을 대신하지 않는다.
func TestIssue305CodexLiveMCP(t *testing.T) {
	if os.Getenv("TEST_CODEX_LIVE") != "1" {
		t.Skip("TEST_CODEX_LIVE=1로 실제 모델 호출을 명시해야 합니다")
	}
	binary, liveModel := os.Getenv("TEST_CODEX_BINARY"), os.Getenv("TEST_CODEX_LIVE_MODEL")
	if binary == "" || liveModel == "" {
		t.Fatal("TEST_CODEX_BINARY·TEST_CODEX_LIVE_MODEL이 필요합니다")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil || !strings.HasPrefix(string(version), "codex-cli ") {
		t.Fatal("Codex CLI 판을 확인하지 못했습니다")
	}
	database, _, address := newServerContractDatabase(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic embedding unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	certificate, rootPEM := test08Certificate(t)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	origin := "https://" + server.Listener.Addr().String()
	cfg := arch06Configuration(t, address, provider.URL)
	cfg.MCPResourceURL, _ = url.Parse(origin + "/mcp")
	cfg.AuthorizationServerURL, _ = url.Parse(origin)
	cfg.MCPAllowedOrigins = []string{origin}
	service, err := authz.New(database, authz.Config{Issuer: origin, BcryptCost: 4, OAuth: oauthTestConfiguration(t, cfg, database), AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database, Connections: database})
	if err != nil {
		t.Fatal(err)
	}
	account, err := service.Register(ctx, "issue305_live", "synthetic password")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AccountPlans, err = plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"writes_per_minute":100}}`, account))
	if err != nil {
		t.Fatal(err)
	}
	client, callback := "codex-cli-first-test", "http://127.0.0.1:49152/callback"
	code, err := service.IssueAuthorizationCode(ctx, account, authz.AuthorizationRequest{ClientID: client, RedirectURI: callback, Resource: cfg.MCPResourceURL.String(), ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := service.ExchangeAuthorizationCode(ctx, code, client, callback, cfg.MCPResourceURL.String(), "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := assembleApplication(ctx, cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := assembly.close(nil, true); err != nil {
			t.Error(err)
		}
	})
	var mu sync.Mutex
	var calls []issue305LiveCall
	next := assembly.app.handler()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "probe read failed", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var request struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(data, &request)
		response := httptest.NewRecorder()
		next.ServeHTTP(response, r)
		for name, values := range response.Header() {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
		if request.Method == "tools/call" {
			var returned struct {
				Result struct {
					IsError bool           `json:"isError"`
					Value   map[string]any `json:"structuredContent"`
				} `json:"result"`
				Error any `json:"error"`
			}
			decodeErr := json.Unmarshal(response.Body.Bytes(), &returned)
			mu.Lock()
			calls = append(calls, issue305LiveCall{Tool: request.Params.Name, Arguments: request.Params.Arguments, Result: returned.Result.Value, Failed: response.Code != 200 || decodeErr != nil || returned.Error != nil || returned.Result.IsError})
			mu.Unlock()
		}
	})
	server.StartTLS()
	work := t.TempDir()
	// 공개 시스템 CA를 함께 넣어 모델 서버의 정상 TLS 검증도 유지한다.
	if systemRoots, err := os.ReadFile("/etc/ssl/cert.pem"); err == nil {
		rootPEM = append(rootPEM, systemRoots...)
	}
	ca := filepath.Join(work, "ca.pem")
	if err := os.WriteFile(ca, rootPEM, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"exec", "--ignore-user-config", "--skip-git-repo-check", "--ephemeral", "--json", "--sandbox", "read-only", "--model", liveModel}
	for _, override := range []string{
		`model_provider="openai"`, `model_reasoning_effort="low"`, `approval_policy="never"`, `project_doc_max_bytes=0`,
		`features.apps=false`, `features.plugins=false`, `features.multi_agent=false`, `features.shell_tool=false`, `features.unified_exec=false`, `features.shell_snapshot=false`,
		`features.mcp_2026_07_28=true`, `analytics.enabled=false`, `check_for_update_on_startup=false`,
		fmt.Sprintf(`mcp_servers.issue305.url=%q`, origin+"/mcp"), `mcp_servers.issue305.bearer_token_env_var="ACS_ISSUE305_MCP_TOKEN"`,
		`mcp_servers.issue305.required=true`, `mcp_servers.issue305.default_tools_approval_mode="approve"`,
		`mcp_servers.issue305.startup_timeout_sec=15`, `mcp_servers.issue305.tool_timeout_sec=30`,
	} {
		args = append(args, "-c", override)
	}
	args = append(args, issue305LivePrompt)
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = work
	command.Env = append(os.Environ(), "CODEX_CA_CERTIFICATE="+ca, "ACS_ISSUE305_MCP_TOKEN="+tokens.AccessToken)
	command.WaitDelay = 3 * time.Second
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	var events, stderr bytes.Buffer
	command.Stdout, command.Stderr = &events, &stderr
	runErr := command.Run()
	artifacts := os.Getenv("TEST_CODEX_LIVE_ARTIFACT_DIR")
	if artifacts == "" {
		artifacts = t.TempDir()
	}
	if err := os.MkdirAll(artifacts, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"events.jsonl": events.Bytes(), "stderr.log": stderr.Bytes()} {
		if err := os.WriteFile(filepath.Join(artifacts, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	observed := slices.Clone(calls)
	mu.Unlock()
	data, _ := json.Marshal(observed)
	if err := os.WriteFile(filepath.Join(artifacts, "mcp-calls.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("CLI=%s model=%s artifacts=%s calls=%d", strings.TrimSpace(string(version)), liveModel, artifacts, len(observed))
	if runErr != nil {
		t.Fatalf("실제 Codex 실행 실패: %v (비공개 stderr·events 참조)", runErr)
	}
	completed := false
	for line := range bytes.SplitSeq(events.Bytes(), []byte{'\n'}) {
		var event struct {
			Type  string         `json:"type"`
			Usage map[string]any `json:"usage"`
			Item  struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if len(line) == 0 || json.Unmarshal(line, &event) != nil {
			continue
		}
		if event.Item.Type == "command_execution" || event.Item.Type == "file_change" || event.Item.Type == "web_search" {
			t.Fatal("MCP 외 도구를 사용했습니다", event.Item.Type)
		}
		if event.Type == "turn.completed" {
			completed = true
			t.Logf("actual model usage=%v", event.Usage)
		}
	}
	if !completed {
		t.Fatal("실제 모델 turn.completed가 없습니다")
	}
	var graphID model.ID
	ids := map[model.ID]bool{}
	counts := map[string]int{}
	failures := 0
	for _, call := range observed {
		counts[call.Tool]++
		if call.Failed {
			failures++
			continue
		}
		if graph, ok := call.Result["graph"].(map[string]any); ok && call.Tool == "graph_create" {
			graphID = mustMCP09ID(t, graph["graph_id"].(string))
		}
		if node, ok := call.Result["node"].(map[string]any); ok && call.Tool == "node_create" {
			ids[mustMCP09ID(t, node["context_id"].(string))] = true
		}
	}
	t.Logf("tools=%v rejected_calls=%d", counts, failures)
	if !graphID.IsV7() || len(ids) != 6 || counts["node_update"] == 0 || counts["node_discard"] == 0 || counts["node_restore"] == 0 {
		t.Fatal("요청한 생성·수정·폐기·복구 호출을 수행하지 않았습니다")
	}
	var sources, eventsStored []model.Context
	var proposition, summary model.Context
	for id := range ids {
		node, err := database.Context(ctx, graphID, id)
		if err != nil || node.DeletedAt != nil {
			t.Fatal("실제 저장 노드가 활성 상태가 아닙니다", err)
		}
		switch node.Layer {
		case model.LayerSource:
			if node.Source.ConversationID != "issue305-codex-live" || node.Source.TurnIndex == nil {
				t.Fatal("원천의 대화·턴 저장 누락")
			}
			sources = append(sources, node)
		case model.LayerEvent:
			if node.Event.ConversationID != "issue305-codex-live" {
				t.Fatal("사건 대화 저장 누락")
			}
			eventsStored = append(eventsStored, node)
		case model.LayerDerived:
			if node.Derived.Kind == model.DerivationKindSummary {
				summary = node
			} else {
				proposition = node
			}
		}
	}
	if len(sources) != 2 || len(eventsStored) != 2 || !summary.ID.IsV7() || !proposition.ID.IsV7() {
		t.Fatal("세 계층의 실제 저장 개수 불일치")
	}
	for _, source := range sources {
		turn := *source.Source.TurnIndex
		if turn < 0 || turn > 1 || source.Body != []string{"영문 도구 설명으로 노드를 생성한다.", "파생 노드를 수정한 뒤 폐기·복구한다."}[turn] || !slices.Contains(summary.Derived.DerivedFrom, source.ID) {
			t.Fatal("원문·턴 순서·요약 근거가 요청과 다릅니다")
		}
	}
	if proposition.Body != "검증 명제 수정 완료" || proposition.Version < 4 {
		t.Fatal("명제 수정·폐기·복구의 저장 판 불일치")
	}
	if err := issue305LiveLifecycle(observed, graphID, proposition.ID); err != nil {
		t.Fatal(err)
	}
	for _, event := range eventsStored {
		if !slices.Contains(summary.Derived.NavigationRefs, event.ID) {
			t.Fatal("요약 안내 참조 누락")
		}
	}
	graph, err := database.Graph(ctx, graphID)
	if err != nil || graph.EntryContextID == nil || *graph.EntryContextID != summary.ID {
		t.Fatal("대표 요약 저장 불일치", err)
	}
	slices.SortFunc(eventsStored, func(a, b model.Context) int { return a.Event.Start.Compare(b.Event.Start) })
	relations, _, err := database.ListRelations(ctx, graphID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, relation := range relations {
		linked = linked || relation.Type == "precedes" && relation.State == "confirmed" && relation.FromContextID == eventsStored[0].ID && relation.ToContextID == eventsStored[1].ID
	}
	if !linked {
		t.Fatal("사건 흐름의 확정 관계가 없습니다")
	}
}

// 최종 활성 상태만으로는 폐기·복구 성공을 입증할 수 없으므로 실제 응답을 대조한다.
func issue305LiveLifecycle(calls []issue305LiveCall, graphID, contextID model.ID) error {
	want := []string{"node_update", "node_discard", "node_restore"}
	stage := 0
	for _, call := range calls {
		if stage == len(want) || call.Failed || call.Tool != want[stage] || call.Arguments["graph_id"] != graphID.String() || call.Arguments["context_id"] != contextID.String() {
			continue
		}
		node, ok := call.Result["node"].(map[string]any)
		if !ok || node["graph_id"] != graphID.String() || node["context_id"] != contextID.String() || node["body"] != "검증 명제 수정 완료" || node["version"] != float64(stage+2) {
			return fmt.Errorf("%s의 대상·본문·결과 판 불일치", call.Tool)
		}
		deleted, present := node["deleted_at"]
		if !present || (stage != 1 && deleted != nil) {
			return fmt.Errorf("%s의 활성 상태 불일치", call.Tool)
		}
		if stage == 1 {
			value, ok := deleted.(string)
			if !ok || value == "" {
				return fmt.Errorf("폐기 성공 응답의 시각 누락")
			}
		}
		stage++
	}
	if stage != len(want) {
		return fmt.Errorf("같은 명제의 성공한 수정→폐기→복구 호출 누락")
	}
	return nil
}

const issue305LivePrompt = `이 연결은 검증 전용 새 DB입니다. 연결된 공유 컨텍스트 MCP 도구와 도구 검색만 사용하세요. 셸·파일·웹·다른 에이전트는 사용하지 마세요. 쓰기마다 고유한 재시도 키를 사용하고 중간에 질문하지 말고 실제로 저장·조회하세요.
1. issue305-codex-live라는 새 그래프 하나를 만드세요. 대화 식별자는 issue305-codex-live입니다.
2. 아래 사용자 원문 두 개를 원문 그대로 대화 출처 원천으로 기록하세요. 턴은 각각 0, 1입니다.
   첫 원문: 영문 도구 설명으로 노드를 생성한다. / 출처 chat://issue305/turn0 / 발생 UTC 2026-11-04T12:00:00Z
   둘째 원문: 파생 노드를 수정한 뒤 폐기·복구한다. / 출처 chat://issue305/turn1 / 발생 UTC 2026-11-04T12:10:00Z
3. 첫 원천을 근거로 관찰 성격의 명제 파생 하나(본문: 검증 명제)를 만드세요.
4. 각 원천을 구성원으로 하는 사건 두 개를 같은 대화에 기록하세요. 첫 사건은 UTC 11:59~12:01, 둘째는 12:09~12:11(모두 2026-11-04)입니다. 둘째 사건을 만드는 호출에서 앞 사건과의 확정 순서도 함께 저장하세요.
5. 두 원천을 근거로 관찰 성격의 국소 요약 파생 하나(본문: 검증 요약)를 만들고, 두 사건을 안내 참조로 연결하세요. 이 요약을 그래프 대표 진입점으로 지정하세요.
6. 명제의 본문을 검증 명제 수정 완료로 수정한 다음 그 명제를 한 번 폐기하고 복구하세요.
7. 마지막 상태와 사건 순서를 MCP 도구로 조회해 확인하고, 그래프·여섯 노드 ID와 검증 결과를 한국어로 간단히 보고하세요.`
