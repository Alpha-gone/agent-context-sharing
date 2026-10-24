package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 같은 DB의 실제 조립 서버를 모두 닫고 새 조립으로 교체한다. 발견 호출이나
// 이전 SDK 객체 없이 공유 자격·등록·업무 결과가 이어지는지 공개 HTTP로 확인한다.
func TestARCH08SharedStateAndReplacementIntegration(t *testing.T) {
	database, pool, address := newServerContractDatabase(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vector := make([]float64, 768)
		vector[0] = 1
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, map[string]any{"embedding": map[string]any{"values": vector}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(provider.Close)
	cfg := arch06Configuration(t, address, provider.URL)
	service, err := authz.New(database, authz.Config{Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: 4, OAuth: oauthConfiguration(cfg), AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database, Connections: database})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := service.Register(t.Context(), "arch08_owner", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := service.Register(t.Context(), "arch08_viewer", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	assemblies := make([]*serverAssembly, 2)
	servers := make([]*httptest.Server, 2)
	// 교체된 서버도 다음 하위 시나리오에서 사용하므로 최상위 시험이 소유한다.
	lifetime := t
	start := func(t *testing.T, slot int, placement config.ComponentPlacement) {
		t.Helper()
		instance := cfg
		instance.IndexWorkerPlacement = placement
		instance.Diagnostic.InstanceID = fmt.Sprintf("arch08-%d", slot)
		a, err := assembleApplication(lifetime.Context(), instance, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewTLSServer(a.app.handler())
		s.Client().Timeout = 5 * time.Second
		s.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		lifetime.Cleanup(func() {
			s.Close()
			s.Client().CloseIdleConnections()
			if err := a.close(nil, true); err != nil {
				t.Error(err)
			}
		})
		assemblies[slot], servers[slot] = a, s
	}
	stop := func(t *testing.T, slot int) {
		t.Helper()
		servers[slot].Close()
		servers[slot].Client().CloseIdleConnections()
		if err := assemblies[slot].close(nil, true); err != nil {
			t.Fatal(err)
		}
		if err := assemblies[slot].database.Ping(t.Context()); err == nil {
			t.Fatal("이전 조립의 DB 풀이 닫히지 않았다")
		}
	}
	start(t, 0, config.PlacementEmbedded)
	start(t, 1, config.PlacementExternal)
	request := func(t *testing.T, slot int, path, mediaType, body, bearer, cookie string, status int) []byte {
		t.Helper()
		method := "POST"
		if mediaType == "" {
			method = "GET"
		}
		r, err := http.NewRequestWithContext(t.Context(), method, servers[slot].URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = cfg.MCPResourceURL.Host
		if mediaType != "" {
			r.Header.Set("Content-Type", mediaType)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "agent_context_session", Value: cookie})
		}
		if path == "/mcp" {
			var envelope struct {
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if err := json.Unmarshal([]byte(body), &envelope); err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Accept", "application/json, text/event-stream")
			r.Header.Set("MCP-Protocol-Version", "2026-07-28")
			r.Header.Set("Mcp-Method", "tools/call")
			r.Header.Set("Mcp-Name", envelope.Params.Name)
		}
		response, err := servers[slot].Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Mcp-Session-Id") != "" {
			t.Fatalf("%s: HTTP %d, expected %d; 읽기 오류=%v", path, response.StatusCode, status, err)
		}
		return raw
	}
	client := "codex-cli-first-test"
	credentials := []string{"ARCH08 private body", "ARCH08 private judgment", "synthetic-key"}
	tokenRequest := func(t *testing.T, slot int, values url.Values) authz.OAuthTokens {
		t.Helper()
		values.Set("client_id", client)
		values.Set("resource", cfg.MCPResourceURL.String())
		raw := request(t, slot, "/token", "application/x-www-form-urlencoded", values.Encode(), "", "", 200)
		var tokens authz.OAuthTokens
		if json.Unmarshal(raw, &tokens) != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" {
			t.Fatal("공유 인가의 자격 교환 실패")
		}
		credentials = append(credentials, tokens.AccessToken, tokens.RefreshToken)
		return tokens
	}
	issue := func(t *testing.T, account model.ID, slot int) authz.OAuthTokens {
		t.Helper()
		callback := cfg.OAuthClients[client].RedirectURIs[0]
		code, err := service.IssueAuthorizationCode(t.Context(), account, authz.AuthorizationRequest{ClientID: client, RedirectURI: callback, Resource: cfg.MCPResourceURL.String(), ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
		if err != nil {
			t.Fatal(err)
		}
		return tokenRequest(t, slot, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback}, "code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"}})
	}
	call := func(t *testing.T, slot int, token, tool string, args map[string]any, expected string) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "arch08", "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args, "_meta": map[string]any{sdk.MetaKeyProtocolVersion: "2026-07-28", sdk.MetaKeyClientCapabilities: map[string]any{}}}})
		if err != nil {
			t.Fatal(err)
		}
		raw := request(t, slot, "/mcp", "application/json", string(body), token, "", 200)
		var envelope map[string]any
		if json.Unmarshal(raw, &envelope) != nil || envelope["error"] != nil {
			t.Fatal("MCP 교환 실패")
		}
		result := envelope["result"].(map[string]any)
		value := result["structuredContent"].(map[string]any)
		if result["resultType"] != "complete" || (result["isError"] == true) != (expected != "") || expected != "" && value["code"] != expected {
			t.Fatalf("조립 서버의 업무 결과 계층 불일치 %s: code=%v, expected=%s", tool, value["code"], expected)
		}
		return value
	}
	step := func(name string, run func(*testing.T)) {
		t.Helper()
		if !t.Run(name, run) {
			t.FailNow()
		}
	}
	tokens, foreign := issue(t, owner, 0), issue(t, viewer, 1)
	var agent, graph string
	create := map[string]any{"request_id": "arch08_graph_request", "name": "ARCH08 private graph"}
	var created, node map[string]any
	step("registration_and_idempotency_across_instances", func(t *testing.T) {
		agent = call(t, 0, tokens.AccessToken, "agent_register", map[string]any{"registration_key": "arch08_registration"}, "")["agent_id"].(string)
		if call(t, 1, tokens.AccessToken, "agent_register", map[string]any{"registration_key": "arch08_registration"}, "")["agent_id"] != agent {
			t.Fatal("등록이 인스턴스별로 갈라졌다")
		}
		created = call(t, 0, tokens.AccessToken, "graph_create", create, "")
		graph = created["graph"].(map[string]any)["graph_id"].(string)
		if !reflect.DeepEqual(created, call(t, 1, tokens.AccessToken, "graph_create", create, "")) {
			t.Fatal("같은 키의 최초 결과가 공유되지 않았다")
		}
	})
	graphID := mustMCP09ID(t, graph)
	nodeArgs := map[string]any{"request_id": "arch08_source_request", "graph_id": graph, "agent_id": agent, "judgment_input": "ARCH08 private judgment", "layer": "source", "body": "ARCH08 private body", "origin_kind": "user_utterance", "occurred_at": "2026-10-20T12:00:00Z", "source_ref": map[string]any{"channel": "file", "locator": "file:///arch08/source"}}
	step("current_permission_is_not_cached", func(t *testing.T) {
		call(t, 1, foreign.AccessToken, "graph_get", map[string]any{"graph_id": graph}, "not_found")
		if err := assemblies[0].database.GrantGraph(t.Context(), graphID, owner, viewer, store.GrantSubjectAccount, model.GraphGradeViewer); err != nil {
			t.Fatal(err)
		}
		call(t, 1, foreign.AccessToken, "graph_get", map[string]any{"graph_id": graph}, "")
		if err := assemblies[0].database.RevokeGraphGrantWithAudit(t.Context(), graphID, owner, viewer, store.GrantSubjectAccount); err != nil {
			t.Fatal(err)
		}
		call(t, 1, foreign.AccessToken, "graph_get", map[string]any{"graph_id": graph}, "not_found")
	})
	step("stored_context_and_pending_index", func(t *testing.T) {
		node = call(t, 1, tokens.AccessToken, "node_create", nodeArgs, "")
		call(t, 0, tokens.AccessToken, "node_get", map[string]any{"graph_id": graph, "context_id": node["node"].(map[string]any)["context_id"], "hops": 0}, "")
	})
	session, err := service.WebSession(t.Context(), owner, "web")
	if err != nil {
		t.Fatal(err)
	}
	credentials = append(credentials, session.Raw)
	step("shared_web_session_and_signing_rotation", func(t *testing.T) {
		request(t, 0, "/graphs", "", "", "", session.Raw, 200)
		request(t, 1, "/graphs", "", "", "", session.Raw, 200)
		if err := service.RotateSigningKey(t.Context()); err != nil {
			t.Fatal(err)
		}
		fresh := issue(t, owner, 1)
		// 정상 키 목록 적재 뒤의 부재 재조회 제한은 1초다. 로컬 캐시를
		// 건드리지 않고 실제 시간이 지난 뒤 다른 인스턴스의 새 kid를 검사한다.
		time.Sleep(time.Second)
		call(t, 0, fresh.AccessToken, "graph_get", map[string]any{"graph_id": graph}, "")
		call(t, 1, tokens.AccessToken, "graph_get", map[string]any{"graph_id": graph}, "")
	})
	step("refresh_on_other_instance", func(t *testing.T) {
		previous := tokens.RefreshToken
		tokens = tokenRequest(t, 1, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {previous}})
		if tokens.RefreshToken == previous {
			t.Fatal("갱신 자격을 회전하지 않았다")
		}
		call(t, 0, tokens.AccessToken, "graph_get", map[string]any{"graph_id": graph}, "")
	})
	var auditBefore, queueBefore int
	counts := func(t *testing.T) (int, int) {
		t.Helper()
		var audit, queue int
		if err := pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM public.operation_log WHERE graph_id=$1), (SELECT count(*) FROM public.index_task WHERE graph_id=$1)", graph).Scan(&audit, &queue); err != nil {
			t.Fatal(err)
		}
		return audit, queue
	}
	step("replace_all_servers_and_swap_placement", func(t *testing.T) {
		auditBefore, queueBefore = counts(t)
		if queueBefore != 1 {
			t.Fatal("소비 전 색인 작업이 공유 저장소에 없다")
		}
		stop(t, 0)
		stop(t, 1)
		start(t, 0, config.PlacementExternal)
		start(t, 1, config.PlacementEmbedded)
		// 새 SDK의 첫 호출부터 이전 등록과 결과를 찾는다. discover/list는 호출하지 않는다.
		if call(t, 0, tokens.AccessToken, "agent_register", map[string]any{"registration_key": "arch08_registration"}, "")["agent_id"] != agent || !reflect.DeepEqual(created, call(t, 1, tokens.AccessToken, "graph_create", create, "")) || !reflect.DeepEqual(node, call(t, 0, tokens.AccessToken, "node_create", nodeArgs, "")) {
			t.Fatal("서버 교체가 등록·최초 결과를 잃었다")
		}
		if audit, queue := counts(t); audit != auditBefore || queue != queueBefore {
			t.Fatal("재생이 감사·색인 작업을 다시 추가했다")
		}
		request(t, 1, "/graphs", "", "", "", session.Raw, 200)
		call(t, 1, foreign.AccessToken, "graph_get", map[string]any{"graph_id": graph}, "not_found")
		tokens = tokenRequest(t, 0, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens.RefreshToken}})
	})
	step("registration_revocation_survives_replacement", func(t *testing.T) {
		if err := assemblies[1].database.RevokeAgent(t.Context(), owner, mustMCP09ID(t, agent)); err != nil {
			t.Fatal(err)
		}
		call(t, 0, tokens.AccessToken, "node_create", nodeArgs, "invalid_argument")
		call(t, 1, tokens.AccessToken, "agent_register", map[string]any{"registration_key": "arch08_registration"}, "invalid_argument")
	})
	step("shared_web_and_oauth_revocation", func(t *testing.T) {
		if err := service.Revoke(t.Context(), session); err != nil {
			t.Fatal(err)
		}
		// 웹 폐기 목록의 지역 캐시는 10초 간격이다. Bearer 공유 폐기의
		// 즉시 대조와 나누어 확인하며 제품 캐시나 시계를 시험에서 바꾸지 않는다.
		select {
		case <-time.After(10 * time.Second):
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
		request(t, 0, "/graphs", "", "", "", session.Raw, 303)
		request(t, 1, "/graphs", "", "", "", session.Raw, 303)
		principal, err := service.VerifyBearer(t.Context(), tokens.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		if err := assemblies[1].database.RevokeOAuthGrant(t.Context(), owner, principal.GrantID); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "arch08", "method": "tools/call", "params": map[string]any{"name": "graph_get", "arguments": map[string]any{"graph_id": graph}, "_meta": map[string]any{sdk.MetaKeyProtocolVersion: "2026-07-28", sdk.MetaKeyClientCapabilities: map[string]any{}}}})
		if err != nil {
			t.Fatal(err)
		}
		for slot := range 2 {
			request(t, slot, "/mcp", "application/json", string(body), tokens.AccessToken, "", 401)
		}
	})
	step("diagnostic_boot_and_credential_isolation", func(t *testing.T) {
		stop(t, 0)
		stop(t, 1)
		for slot := range 2 {
			bootIDs := map[string]bool{}
			for _, kind := range []diagnostic.Kind{diagnostic.Request, diagnostic.Auth, diagnostic.Search, diagnostic.Index, diagnostic.Management, diagnostic.System} {
				filename, _ := kind.Filename()
				data, err := os.ReadFile(filepath.Join(cfg.Diagnostic.Directory, fmt.Sprintf("arch08-%d", slot), filename))
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range credentials {
					if bytes.Contains(data, []byte(secret)) {
						t.Fatal("진단 파일에 자격·업무 원문 노출")
					}
				}
				for row := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
					if len(row) == 0 {
						continue
					}
					var event diagnostic.Event
					if json.Unmarshal(row, &event) != nil || event.Validate() != nil || event.InstanceID != fmt.Sprintf("arch08-%d", slot) {
						t.Fatal("진단 출처·필드 계약 불일치")
					}
					bootIDs[event.BootID] = true
				}
			}
			if len(bootIDs) != 2 {
				t.Fatal("재시작 전후의 boot가 분리되지 않았다")
			}
		}
	})
}
