package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/diagnostic"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func arch06Configuration(t *testing.T, address, provider string) config.Config {
	t.Helper()
	base, err := os.MkdirTemp(".", ".arch06-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	directory, err := filepath.Abs(filepath.Join(base, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	values := serverConfiguration(t)
	for key, value := range map[string]string{
		"DATABASE_URL": address, "AGE_GRAPH_NAME": "agent_context", "EMBEDDING_BASE_URL": provider, "EMBEDDING_DIMENSION": "768", "EMBEDDING_VECTOR_TYPE": "vector",
		"TLS_TERMINATION": "direct", "TLS_CERT_FILE": "unused-cert.pem", "TLS_KEY_FILE": "unused-key.pem", "TRUSTED_PROXY_CIDRS": "",
		"BCRYPT_COST": "4", "DIAGNOSTIC_LOG_DIR": directory, "DIAGNOSTIC_INSTANCE_ID": "arch06", "DIAGNOSTIC_LOG_LEVEL": "INFO",
	} {
		values[key] = value
	}
	cfg, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func arch06IndexWorkers() int {
	data := make([]byte, 1<<20)
	stacks := string(data[:runtime.Stack(data, true)])
	count := 0
	for stack := range strings.SplitSeq(stacks, "\n\n") {
		if strings.Contains(stack, "agent_context_sharing/internal/index.(*Worker).Start.func") {
			count++
		}
	}
	return count
}

func arch06MCP(t *testing.T, server *httptest.Server, host, token, method, name string, args map[string]any) map[string]any {
	t.Helper()
	params := map[string]any{"_meta": map[string]any{sdk.MetaKeyProtocolVersion: "2026-07-28", sdk.MetaKeyClientCapabilities: map[string]any{}, sdk.MetaKeyClientInfo: map[string]any{"name": "arch06", "version": "1"}}}
	if method == "tools/call" {
		params["name"], params["arguments"] = name, args
	}
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "arch06", "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", method)
	if name != "" {
		request.Header.Set("Mcp-Name", name)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if response.StatusCode != 200 || json.Unmarshal(raw, &value) != nil || value["error"] != nil {
		t.Fatal("조립한 MCP 전송 실패", response.StatusCode, string(raw))
	}
	result := value["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatal("조립한 업무 처리 실패", string(raw))
	}
	return result
}

func TestARCH06AssemblyMCPProviderAndPlacementIntegration(t *testing.T) {
	for _, placement := range []config.ComponentPlacement{config.PlacementEmbedded, config.PlacementExternal} {
		t.Run(string(placement), func(t *testing.T) {
			_, _, address := newServerContractDatabase(t)
			var mu sync.Mutex
			var inputs []string
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				mu.Lock()
				inputs = append(inputs, string(data))
				mu.Unlock()
				if r.URL.Path != "/v1beta/models/gemini-embedding-2:embedContent" || r.Header.Get("x-goog-api-key") != "synthetic-key" {
					t.Error("제공자 구성 결합 불일치")
				}
				vector := make([]float64, 768)
				vector[0] = 1
				data, _ = json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
				w.Header().Set("Content-Type", "application/json")
				w.Write(data)
			}))
			defer provider.Close()
			cfg := arch06Configuration(t, address, provider.URL)
			cfg.IndexWorkerPlacement = placement
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer occupied.Close()
			cfg.HTTPAddr = occupied.Addr().String()
			before := serverWorkerGoroutines()
			a, err := assembleApplication(t.Context(), cfg, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := a.close(nil, true); err != nil {
					t.Error(err)
				}
			})
			if serverWorkerGoroutines() != before {
				t.Fatal("조립 중 업무 작업자를 시작했다")
			}
			if a.app.mcp == nil || a.app.web == nil || a.app.publicKeys == nil || a.app.oauthTokens == nil {
				t.Fatal("실제 처리기 조립 누락")
			}
			service, err := authz.New(a.database, authz.Config{Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: 4, OAuth: oauthConfiguration(cfg), AuthorizationCodes: a.database, AccessTokens: a.database, RefreshTokens: a.database, Connections: a.database})
			if err != nil {
				t.Fatal(err)
			}
			owner, err := service.Register(t.Context(), "arch06_owner", "correct password")
			if err != nil {
				t.Fatal(err)
			}
			client := "codex-cli-first-test"
			callback := cfg.OAuthClients[client].RedirectURIs[0]
			code, err := service.IssueAuthorizationCode(t.Context(), owner, authz.AuthorizationRequest{ClientID: client, RedirectURI: callback, Resource: cfg.MCPResourceURL.String(), ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
			if err != nil {
				t.Fatal(err)
			}
			tokens, err := service.ExchangeAuthorizationCode(t.Context(), code, client, callback, cfg.MCPResourceURL.String(), "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewTLSServer(a.app.handler())
			defer server.Close()
			call := func(method, name string, args map[string]any) map[string]any {
				return arch06MCP(t, server, cfg.MCPResourceURL.Host, tokens.AccessToken, method, name, args)
			}
			list := call("tools/list", "", nil)
			if len(list["tools"].([]any)) != 14 {
				t.Fatal("전체 14종 등록 누락")
			}
			registration := call("tools/call", "agent_register", map[string]any{"registration_key": "arch06_registration"})["structuredContent"].(map[string]any)
			if registration["agent_id"] == "" {
				t.Fatal("실제 등록 저장 누락")
			}
			created := call("tools/call", "graph_create", map[string]any{"request_id": "arch06_graph_request", "name": "조립 그래프"})["structuredContent"].(map[string]any)
			graph := created["graph"].(map[string]any)["graph_id"].(string)
			call("tools/call", "node_create", map[string]any{
				"request_id": "arch06_node_request", "graph_id": graph, "agent_id": registration["agent_id"], "judgment_input": "assembly judgment",
				"layer": "source", "body": "assembly body", "origin_kind": "user_utterance", "occurred_at": "2026-10-20T12:00:00Z",
				"source_ref": map[string]any{"channel": "file", "locator": "file:///arch06/source"},
			})
			call("tools/call", "graph_get", map[string]any{"graph_id": graph})
			call("tools/call", "context_flow_get", map[string]any{"graph_id": graph, "work_context": "assembly query", "scope": "local"})
			if _, err := a.indexer.EmbedDocument(diagnostic.WithTrace(t.Context(), diagnostic.Trace{}.NewJob()), "assembly body"); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			sent := strings.Join(inputs, "\n")
			mu.Unlock()
			if !strings.Contains(sent, "task: search result | query: assembly query") || !strings.Contains(sent, "title: none | text: assembly body") {
				t.Fatal("질의·본문 제공자 조립 누락")
			}
			baseline := arch06IndexWorkers()
			a.startWorkers(t.Context())
			if a.periodic.cancel == nil {
				t.Fatal("외부 배치에서 주기 작업을 누락했다")
			}
			if placement == config.PlacementEmbedded {
				deadline := time.Now().Add(time.Second)
				for arch06IndexWorkers() == baseline && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if arch06IndexWorkers() != baseline+1 {
					t.Fatal("내장 색인 작업자 시작 누락")
				}
			} else if arch06IndexWorkers() != baseline {
				t.Fatal("외부 배치가 소비 루프를 시작했다")
			}
			server.Close()
			if err := a.close(nil, true); err != nil {
				t.Fatal(err)
			}
			first := a.recorder.Stats()
			if err := a.close(nil, true); err != nil || a.recorder.Stats().Accepted != first.Accepted {
				t.Fatal("중복 종료가 다시 자원을 정리했다", err)
			}
			if err := a.database.Ping(t.Context()); err == nil {
				t.Fatal("정리 뒤 DB 풀이 열려 있다")
			}
			var boot string
			for _, kind := range []diagnostic.Kind{diagnostic.Request, diagnostic.Auth, diagnostic.Search, diagnostic.Index, diagnostic.Management, diagnostic.System} {
				name, _ := kind.Filename()
				data, err := os.ReadFile(filepath.Join(cfg.Diagnostic.Directory, cfg.Diagnostic.InstanceID, name))
				if err != nil || len(data) == 0 {
					t.Fatal("진단 종류 주입 누락", kind, err)
				}
				for _, secret := range []string{tokens.AccessToken, "assembly query", "assembly body", "synthetic-key"} {
					if bytes.Contains(data, []byte(secret)) {
						t.Fatal("조립 진단에 원문 복제", kind)
					}
				}
				for row := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
					var event diagnostic.Event
					if json.Unmarshal(row, &event) != nil || event.Validate() != nil || event.Kind != kind || event.InstanceID != cfg.Diagnostic.InstanceID {
						t.Fatal("진단 출처·분류 불일치")
					}
					if boot == "" {
						boot = event.BootID
					}
					if event.BootID != boot {
						t.Fatal("조립한 종류별 boot가 다르다")
					}
				}
			}
		})
	}
}

func TestARCH06AssemblyFailureReleasesResourcesIntegration(t *testing.T) {
	_, _, address := newServerContractDatabase(t)
	for _, stage := range []string{"진단 수준 준비", "임베딩 DB 구성 확인", "검색 실행기 준비", "인가 서버 준비", "MCP 전송 준비"} {
		t.Run(stage, func(t *testing.T) {
			cfg := arch06Configuration(t, address, "https://provider.invalid")
			switch stage {
			case "진단 수준 준비":
				cfg.Diagnostic.Level = "TRACE"
			case "임베딩 DB 구성 확인":
				cfg.EmbeddingDimension = 128
			case "검색 실행기 준비":
				cfg.SearchExecution = "unsupported"
			case "인가 서버 준비":
				cfg.BcryptCost = 0
			case "MCP 전송 준비":
				cfg.MCPAllowedOrigins = nil
			}
			before := serverWorkerGoroutines()
			a, err := assembleApplication(t.Context(), cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if a != nil || err == nil || !strings.Contains(err.Error(), stage) {
				t.Fatal("부분 조립 오류 위치 유실", err)
			}
			if serverWorkerGoroutines() != before {
				t.Fatal("부분 조립 뒤 작업자 누수")
			}
			source, _ := diagnostic.NewSource(cfg.Diagnostic.InstanceID)
			reopened, err := diagnostic.NewRecorder(t.Context(), cfg.Diagnostic.Directory, source)
			if err != nil {
				t.Fatal("기동 실패 뒤 기록자 잠금 누수", err)
			}
			if err := reopened.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg := arch06Configuration(t, address, "https://provider.invalid")
	if a, err := assembleApplication(ctx, cfg, nil, nil); a != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("조립 취소 원인 유실", err)
	}
}
