package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 업무 처리기를 교체하지 않고 기동 경로 전체를 조립한다. 외부 제공자만 TLS 대역으로
// 503을 반환하며, 검색의 채널 부분 실패와 정상 절단도 공개 응답에 고정한다.
func TestMCP09AssembledServerContractIntegration(t *testing.T) {
	database, pool, address := newServerContractDatabase(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-embedding-2:embedContent" {
			t.Error("예상하지 않은 제공자 경로")
		}
		http.Error(w, "synthetic provider unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	cfg := arch06Configuration(t, address, provider.URL)
	service, err := authz.New(database, authz.Config{Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: 4, OAuth: oauthConfiguration(cfg), AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database, Connections: database})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := service.Register(t.Context(), "mcp09_owner", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := service.Register(t.Context(), "mcp09_viewer", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AccountPlans, err = plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"max_hop_nodes":1}}`, owner))
	if err != nil {
		t.Fatal(err)
	}
	issue := func(account model.ID) string {
		t.Helper()
		client := "codex-cli-first-test"
		callback := cfg.OAuthClients[client].RedirectURIs[0]
		code, err := service.IssueAuthorizationCode(t.Context(), account, authz.AuthorizationRequest{ClientID: client, RedirectURI: callback, Resource: cfg.MCPResourceURL.String(), ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := service.ExchangeAuthorizationCode(t.Context(), code, client, callback, cfg.MCPResourceURL.String(), "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
		if err != nil {
			t.Fatal(err)
		}
		return tokens.AccessToken
	}
	token, viewerToken := issue(owner), issue(viewer)
	servers := make([]*httptest.Server, 2)
	assemblies := make([]*serverAssembly, 2)
	for i := range servers {
		instance := cfg
		instance.Diagnostic.InstanceID = fmt.Sprintf("mcp09-%d", i)
		assemblies[i], err = assembleApplication(t.Context(), instance, provider.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		a := assemblies[i]
		t.Cleanup(func() {
			if err := a.close(nil, true); err != nil {
				t.Error(err)
			}
		})
		servers[i] = httptest.NewTLSServer(a.app.handler())
		t.Cleanup(servers[i].Close)
		servers[i].Client().Timeout = 10 * time.Second
		t.Cleanup(servers[i].Client().CloseIdleConnections)
	}
	aliases := map[string]string{owner.String(): "<owner>", viewer.String(): "<viewer>"}
	records := map[string]any{}
	requestBody := func(method, tool string, args map[string]any) map[string]any {
		params := map[string]any{"_meta": map[string]any{sdk.MetaKeyProtocolVersion: "2026-07-28", sdk.MetaKeyClientCapabilities: map[string]any{}}}
		if method == "tools/call" {
			params["name"], params["arguments"] = tool, args
		}
		return map[string]any{"jsonrpc": "2.0", "id": "mcp09", "method": method, "params": params}
	}
	exchange := func(t *testing.T, label string, index int, credential, verb string, body map[string]any, headers map[string]string, status int) map[string]any {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.NewRequestWithContext(t.Context(), verb, servers[index].URL+"/mcp", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = cfg.MCPResourceURL.Host
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2026-07-28")
		r.Header.Set("Mcp-Method", body["method"].(string))
		if tool, ok := body["params"].(map[string]any)["name"].(string); ok {
			r.Header.Set("Mcp-Name", tool)
		}
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		for key, value := range headers {
			if value == "" {
				r.Header.Del(key)
			} else {
				r.Header.Set(key, value)
			}
		}
		response, err := servers[index].Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil || response.StatusCode != status {
			t.Fatalf("%s: HTTP %d, expected %d: %s (%v)", label, response.StatusCode, status, raw, err)
		}
		if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Mcp-Session-Id") != "" || len(response.Cookies()) != 0 || response.Header.Get("Location") != "" {
			t.Fatal("캐시·무상태 응답 계약 위반", label)
		}
		if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
			t.Fatal("인증 challenge 계층 불일치", label)
		}
		var envelope map[string]any
		if len(raw) > 0 {
			mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" || json.Unmarshal(raw, &envelope) != nil {
				t.Fatal("JSON 외피 불일치", label)
			}
			if result, ok := envelope["result"].(map[string]any); ok {
				if result["resultType"] != "complete" || envelope["jsonrpc"] != "2.0" || envelope["id"] != "mcp09" {
					t.Fatal("완료 외피 불일치", label)
				}
				if structured, ok := result["structuredContent"].(map[string]any); ok {
					content, ok := result["content"].([]any)
					if !ok || len(content) != 1 {
						t.Fatal("단일 text content가 필요하다", label)
					}
					text := content[0].(map[string]any)
					var decoded map[string]any
					if text["type"] != "text" || json.Unmarshal([]byte(text["text"].(string)), &decoded) != nil || !reflect.DeepEqual(decoded, structured) {
						t.Fatal("text/structuredContent 의미 불일치", label)
					}
				}
			}
		}
		for _, secret := range []string{token, viewerToken, "synthetic-key", "synthetic provider unavailable"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatal("응답에 비공개 자격·제공자 오류 노출", label)
			}
		}
		records[label] = map[string]any{"status": status, "body": envelope, "contentType": response.Header.Get("Content-Type"), "cacheControl": response.Header.Get("Cache-Control"), "challenge": response.Header.Get("WWW-Authenticate"), "allow": response.Header.Get("Allow")}
		return envelope
	}
	successful := map[string]bool{}
	call := func(label, tool string, args map[string]any, code string) map[string]any {
		t.Helper()
		var value map[string]any
		if !t.Run(label, func(t *testing.T) {
			body := exchange(t, label, len(records)%2, token, "POST", requestBody("tools/call", tool, args), nil, 200)
			result := body["result"].(map[string]any)
			value = result["structuredContent"].(map[string]any)
			if (result["isError"] == true) != (code != "") || code != "" && value["code"] != code {
				t.Fatalf("도메인 결과 %s: %v", code, result)
			}
			if code == "" {
				successful[tool] = true
			}
		}) {
			t.FailNow()
		}
		return value
	}
	bind := func(label string, value any) string {
		t.Helper()
		id, ok := value.(string)
		parsed, err := model.ParseID(id)
		if !ok || err != nil || !parsed.IsV7() || aliases[id] != "" {
			t.Fatal("새 식별자의 UUIDv7·유일성 불일치", label)
		}
		aliases[id] = "<" + label + ">"
		return id
	}
	// 어느 인스턴스도 발견 이력 없이 첫 도구 호출을 처리한다.
	agent := bind("agent", call("agent_register", "agent_register", map[string]any{"registration_key": "mcp09_registration"}, "")["agent_id"])
	graph := bind("graph", call("graph_create", "graph_create", map[string]any{"name": "MCP09 graph", "description": "contract snapshot", "request_id": "mcp09_graph_request"}, "")["graph"].(map[string]any)["graph_id"])
	list := exchange(t, "tools_list", 0, token, "POST", requestBody("tools/list", "", nil), nil, 200)["result"].(map[string]any)
	discovery := exchange(t, "discover", 1, token, "POST", requestBody("server/discover", "", nil), nil, 200)["result"].(map[string]any)
	for _, result := range []map[string]any{list, discovery} {
		if result["ttlMs"] != float64(0) || result["cacheScope"] != "private" {
			t.Fatal("발견/목록 캐시 계약")
		}
	}
	if !reflect.DeepEqual(discovery["supportedVersions"], []any{"2026-07-28"}) {
		t.Fatal("고정 revision 계약")
	}
	actorArgs := func() map[string]any {
		return map[string]any{"graph_id": graph, "agent_id": agent, "judgment_input": "MCP09 judgment"}
	}
	create := func(label, layer, body string, attributes map[string]any) string {
		t.Helper()
		args := actorArgs()
		args["layer"], args["body"] = layer, body
		maps.Copy(args, attributes)
		return bind(label, call("node_create_"+label, "node_create", args, "")["node"].(map[string]any)["context_id"])
	}
	source := create("source", "source", "MCP09 source", map[string]any{"origin_kind": "user_utterance", "occurred_at": "2026-10-20T12:00:00Z", "source_ref": map[string]any{"channel": "file", "locator": "file:///mcp09/source"}})
	derived := create("derived", "derived", "MCP09 derived", map[string]any{"derived_from": []string{source}, "derivation_kind": "proposition", "evidence_state": "observation"})
	first := create("first_event", "event", "MCP09 first event", map[string]any{"member_refs": []string{source}, "event_start": "2026-10-20T11:00:00Z", "event_end": "2026-10-20T13:00:00Z"})
	secondSource := create("second_source", "source", "MCP09 second source", map[string]any{"origin_kind": "user_utterance", "occurred_at": "2026-10-20T15:00:00Z", "source_ref": map[string]any{"channel": "file", "locator": "file:///mcp09/second"}})
	second := create("second_event", "event", "MCP09 second event", map[string]any{"member_refs": []string{secondSource}, "event_start": "2026-10-20T14:00:00Z", "event_end": "2026-10-20T16:00:00Z"})
	call("graph_update", "graph_update", map[string]any{"graph_id": graph, "expected_version": 1, "description": "updated contract", "request_id": "mcp09_update_request"}, "")
	args := actorArgs()
	args["context_id"], args["expected_version"], args["body"] = derived, 1, "MCP09 revised derived"
	call("node_update", "node_update", args, "")
	call("node_discard", "node_discard", map[string]any{"graph_id": graph, "agent_id": agent, "context_id": derived}, "")
	call("node_restore", "node_restore", map[string]any{"graph_id": graph, "agent_id": agent, "context_id": derived}, "")
	args = actorArgs()
	args["relation_type"], args["from_context_id"], args["to_context_id"] = "precedes", first, second
	relation := bind("relation", call("relation_confirm", "relation_confirm", args, "")["relation"].(map[string]any)["relation_id"])
	call("graph_list", "graph_list", map[string]any{}, "")
	call("graph_get", "graph_get", map[string]any{"graph_id": graph}, "")
	call("node_get", "node_get", map[string]any{"graph_id": graph, "context_id": source, "hops": 0}, "")
	partial := call("node_get_truncated", "node_get", map[string]any{"graph_id": graph, "context_id": source, "hops": 1, "direction": "in", "traversal_filter": []string{"derived_from"}}, "")
	if partial["truncated"].(map[string]any)["result_truncated"] != true {
		t.Fatal("노드 상한의 정상 절단 누락")
	}
	call("relation_list", "relation_list", map[string]any{"graph_id": graph, "context_id": first, "state_filter": []string{"confirmed"}}, "")
	flowArgs := map[string]any{"graph_id": graph, "work_context": "MCP09", "scope": "local", "as_of": "2026-10-24T23:00:00Z"}
	flow := call("context_flow_get", "context_flow_get", flowArgs, "")
	if len(flow["contexts"].([]any)) == 0 || flow["channels"].(map[string]any)["semantic"].(map[string]any)["failure"] != "embedding_unavailable" {
		t.Fatal("실제 검색 채널 부분 실패 누락")
	}
	flowArgs["budget"] = 1
	flow = call("context_flow_get_truncated", "context_flow_get", flowArgs, "")
	if len(flow["contexts"].([]any)) != 0 || flow["truncated"].(map[string]any)["result_truncated"] != true {
		t.Fatal("문자 예산의 정상 빈 절단 누락")
	}
	call("relation_discard", "relation_discard", map[string]any{"graph_id": graph, "agent_id": agent, "relation_id": relation}, "")
	if len(list["tools"].([]any)) != 14 || len(successful) != 14 {
		t.Fatal("도구 목록·성공 응답 14종 대응 누락")
	}
	for _, raw := range list["tools"].([]any) {
		if !successful[raw.(map[string]any)["name"].(string)] {
			t.Fatal("목록 도구의 실제 성공 인수 누락")
		}
	}
	call("invalid_argument", "node_update", map[string]any{"graph_id": graph, "agent_id": agent, "context_id": source, "expected_version": 1, "body": "immutable"}, "invalid_argument")
	call("version_conflict", "graph_update", map[string]any{"graph_id": graph, "expected_version": 1, "name": "stale"}, "version_conflict")
	call("limit_exceeded", "node_get", map[string]any{"graph_id": graph, "context_id": source, "hops": 5}, "limit_exceeded")
	absent, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	aliases[absent.String()] = "<absent>"
	call("not_found", "graph_get", map[string]any{"graph_id": absent.String()}, "not_found")
	if err := database.GrantGraph(t.Context(), mustMCP09ID(t, graph), owner, viewer, store.GrantSubjectAccount, model.GraphGradeViewer); err != nil {
		t.Fatal(err)
	}
	denied := exchange(t, "permission_denied", 0, viewerToken, "POST", requestBody("tools/call", "graph_update", map[string]any{"graph_id": graph, "expected_version": 2, "name": "denied"}), nil, 200)["result"].(map[string]any)
	if denied["isError"] != true || denied["structuredContent"].(map[string]any)["code"] != "permission_denied" {
		t.Fatal("실제 열람자 등급 거부 누락")
	}
	if _, err := database.SetContextDeleted(t.Context(), mustMCP09ID(t, graph), mustMCP09ID(t, source), owner, true, 0); err != nil {
		t.Fatal(err)
	}
	call("not_supported", "node_restore", map[string]any{"graph_id": graph, "agent_id": agent, "context_id": source}, "not_supported")
	// 이 시험만 소유하는 DB에서 업무 조회 실패를 주입한다. 인증 저장소는 유지해
	// 도메인 internal이 HTTP 401·RPC 실패로 바뀌지 않는지 검사한다.
	func() {
		if _, err := pool.Exec(t.Context(), "ALTER TABLE public.graph_grant RENAME TO mcp09_unavailable"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.Exec(t.Context(), "ALTER TABLE public.mcp09_unavailable RENAME TO graph_grant"); err != nil {
				t.Error(err)
			}
		}()
		call("internal", "graph_get", map[string]any{"graph_id": graph}, "internal")
	}()
	call("recovered_graph_get", "graph_get", map[string]any{"graph_id": graph}, "")
	for _, sample := range []struct {
		name, header, value string
		status, code        int
	}{
		{"missing_revision_header", "MCP-Protocol-Version", "", 400, -32020},
		{"missing_method_header", "Mcp-Method", "", 400, -32020},
		{"missing_name_header", "Mcp-Name", "", 400, -32020},
		{"mismatched_name_header", "Mcp-Name", "node_get", 400, -32020},
		{"missing_version_meta", "", "", 400, -32602},
		{"missing_capabilities_meta", "", "", 400, -32602},
		{"old_revision", "MCP-Protocol-Version", "2025-11-25", 400, -32022},
		{"legacy_initialize", "", "", 404, -32601},
	} {
		t.Run(sample.name, func(t *testing.T) {
			body := requestBody("tools/call", "graph_get", map[string]any{"graph_id": graph})
			meta := body["params"].(map[string]any)["_meta"].(map[string]any)
			switch sample.name {
			case "missing_version_meta":
				delete(meta, sdk.MetaKeyProtocolVersion)
			case "missing_capabilities_meta":
				delete(meta, sdk.MetaKeyClientCapabilities)
			case "old_revision":
				meta[sdk.MetaKeyProtocolVersion] = sample.value
			case "legacy_initialize":
				body["method"] = "initialize"
			}
			headers := map[string]string{}
			if sample.header != "" {
				headers[sample.header] = sample.value
			}
			failure := exchange(t, sample.name, 0, token, "POST", body, headers, sample.status)["error"].(map[string]any)
			if failure["code"] != float64(sample.code) {
				t.Fatal("RPC 오류 계층 불일치", failure)
			}
		})
	}
	for _, verb := range []string{"GET", "DELETE"} {
		t.Run(verb, func(t *testing.T) {
			exchange(t, verb, 0, token, verb, requestBody("tools/list", "", nil), nil, 405)
		})
	}
	for _, sample := range []struct{ name, credential string }{{"missing_bearer", ""}, {"invalid_bearer", "invalid.synthetic.token"}} {
		t.Run(sample.name, func(t *testing.T) {
			exchange(t, sample.name, 0, sample.credential, "POST", requestBody("tools/list", "", nil), nil, 401)
		})
	}
	assemblies[1].database.Close()
	exchange(t, "authentication_unavailable", 1, token, "POST", requestBody("tools/list", "", nil), nil, 500)
	// 닫힌 인스턴스의 인증 장애가 다른 인스턴스에 전파되지 않는다.
	recovered := exchange(t, "other_instance_list", 0, token, "POST", requestBody("tools/list", "", nil), nil, 200)["result"]
	if !reflect.DeepEqual(recovered, list) {
		t.Fatal("인스턴스 장애 뒤 도구 목록 변경")
	}
	delete(records, "other_instance_list")
	assertMCP09Snapshot(t, records, aliases)
}

func mustMCP09ID(t *testing.T, value string) model.ID {
	t.Helper()
	id, err := model.ParseID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertMCP09Snapshot(t *testing.T, records map[string]any, aliases map[string]string) {
	t.Helper()
	// 알려진 UUID 결합, UTC 시각 형식과 비음수 latency를 검사한 뒤 변동값만 정규화한다.
	var normalize func(string, any) any
	normalize = func(key string, value any) any {
		switch value := value.(type) {
		case map[string]any:
			for key, item := range value {
				value[key] = normalize(key, item)
			}
		case []any:
			for i, item := range value {
				value[i] = normalize(key, item)
			}
		case string:
			if alias := aliases[value]; alias != "" {
				return alias
			}
			if parsed, err := model.ParseID(value); err == nil && parsed.IsV7() {
				t.Fatal("대응하지 않은 응답 식별자", key)
			}
			if slices.Contains([]string{"created_at", "last_activity_at", "recorded_at", "confirmed_at", "proposed_at", "deleted_at"}, key) && value != "" {
				stamp, err := time.Parse(time.RFC3339Nano, value)
				if err != nil || stamp.Location() != time.UTC {
					t.Fatal("동적 시각의 UTC 형식 불일치", key)
				}
				return "<utc-time>"
			}
			if key == "text" {
				var text any
				if err := json.Unmarshal([]byte(value), &text); err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(normalize("", text), json.Deterministic(true))
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
		case float64:
			if key == "latency_ms" {
				if value < 0 || value != float64(int64(value)) {
					t.Fatal("채널 latency의 비음수 정수 계약")
				}
				return "<latency-ms>"
			}
		}
		return value
	}
	data, err := json.Marshal(normalize("", records), json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", "mcp09_contract.json")
	if os.Getenv("UPDATE_MCP09_SNAPSHOT") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		actual := filepath.Join(t.TempDir(), "mcp09_actual.json")
		if err := os.WriteFile(actual, data, 0644); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("MCP-09 계약 스냅샷 불일치: %s (계약·격리 대응 검토 뒤 UPDATE_MCP09_SNAPSHOT=1로 갱신)", actual)
	}
	if strings.Contains(string(data), "Bearer eyJ") {
		t.Fatal("스냅샷 자격 증명 노출")
	}
}
