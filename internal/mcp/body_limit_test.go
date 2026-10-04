package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// countedBody는 처리기가 실제로 읽은 바이트와 스트림 종료를 관찰한다.
type countedBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (body *countedBody) Read(buffer []byte) (int, error) {
	n, err := body.reader.Read(buffer)
	body.read += n
	return n, err
}

func (body *countedBody) Close() error {
	body.closed = true
	if closer, ok := body.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func TestMCPRequestBodyLimit(t *testing.T) {
	const limit = 1 << 20
	for _, test := range []struct {
		name    string
		size    int
		length  int64
		auth    bool
		status  int
		maxRead int
	}{
		{"상한 미만", limit - 1, limit - 1, true, http.StatusOK, limit - 1},
		{"정확한 상한", limit, limit, true, http.StatusOK, limit},
		{"미인증 상한 이내", limit, -1, false, http.StatusUnauthorized, limit},
		{"알려진 초과 길이", limit + 1, limit + 1, true, http.StatusRequestEntityTooLarge, 0},
		{"길이 미상 1바이트 초과", limit + 1, -1, true, http.StatusRequestEntityTooLarge, limit + 1},
		{"미인증 대용량", 8 * limit, -1, false, http.StatusRequestEntityTooLarge, limit + 1},
		{"작게 선언한 대용량", 8 * limit, 1, true, http.StatusRequestEntityTooLarge, limit + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			verified, called := 0, 0
			server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
				called++
				return ToolResult{Content: []Content{}}, nil
			})
			verify := server.verify
			server.verify = func(ctx context.Context, authentication Authentication) (model.ID, error) {
				verified++
				return verify(ctx, authentication)
			}
			request := toolCallRequest(t, "graph_list", map[string]any{})
			payload, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			request.Body.Close()
			body := &countedBody{reader: io.MultiReader(strings.NewReader(string(payload)), strings.NewReader(strings.Repeat(" ", test.size-len(payload))))}
			request.Body = body
			request.ContentLength = test.length
			request.Header.Set("Authorization", "DPoP valid-token")
			if !test.auth {
				request.Header.Del("Authorization")
				request.Header.Del("DPoP")
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("상태 = %d, want %d", response.Code, test.status)
			}
			if body.read > test.maxRead || !body.closed {
				t.Fatalf("본문 읽기 = %d (상한 %d), 닫힘 = %t", body.read, test.maxRead, body.closed)
			}
			if test.status == http.StatusOK {
				if verified != 1 || called != 1 || body.read != test.size {
					t.Fatalf("정상 요청 검증 = %d, 실행 = %d, 읽기 = %d", verified, called, body.read)
				}
			} else if verified != 0 || called != 0 {
				t.Fatalf("거부된 요청이 인증·도메인에 도달했다: %d, %d", verified, called)
			}
			if test.status == http.StatusRequestEntityTooLarge {
				assertRPCError(t, response, http.StatusRequestEntityTooLarge, -32600)
				var result struct {
					ID    any      `json:"id"`
					Error rpcError `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				data, ok := result.Error.Data.(map[string]any)
				if !ok || result.ID != nil || result.Error.Message != "Request body too large" || data["max_body_bytes"] != float64(limit) {
					t.Fatalf("크기 초과 응답 = %#v", result)
				}
				if response.Header().Get("WWW-Authenticate") != "" {
					t.Fatal("크기 초과가 인증 도전으로 응답됐다")
				}
			}
		})
	}
}

func TestMCPBodyLimitPreservesEarlierValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"Origin", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.test") }, http.StatusForbidden},
		{"메서드", func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed},
		{"헤더", func(r *http.Request) { r.Header.Del("Mcp-Method") }, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := toolCallRequest(t, "graph_list", map[string]any{})
			body := &countedBody{reader: strings.NewReader("읽지 않아야 하는 본문")}
			request.Body = body
			request.ContentLength = 2 << 20
			test.change(request)
			response := httptest.NewRecorder()
			testServer(t, nil).ServeHTTP(response, request)
			if response.Code != test.status || body.read != 0 {
				t.Fatalf("선행 검증 상태 = %d, 본문 읽기 = %d", response.Code, body.read)
			}
		})
	}
}

func TestMCPBodyLimitAppliesToEveryMethod(t *testing.T) {
	for _, method := range []string{"server/discover", "tools/list", "tools/call"} {
		t.Run(method, func(t *testing.T) {
			request := mcpRequest(t, method, map[string]any{"name": "graph_list", "arguments": map[string]any{}})
			if method == "tools/call" {
				request.Header.Set("Mcp-Name", "graph_list")
			}
			body := &countedBody{reader: request.Body}
			request.Body = body
			request.ContentLength = (1 << 20) + 1
			response := httptest.NewRecorder()
			testServer(t, nil).ServeHTTP(response, request)
			assertRPCError(t, response, http.StatusRequestEntityTooLarge, -32600)
			if body.read != 0 || !body.closed {
				t.Fatalf("초과 선언 본문 읽기 = %d, 닫힘 = %t", body.read, body.closed)
			}
		})
	}
}

func TestMCPBodyLimitStopsLargeArguments(t *testing.T) {
	for _, arguments := range []string{
		`{"body":"` + strings.Repeat("a", 2<<20) + `"}`,
		`{"traversal_filter":[` + strings.Repeat(`"derived_from",`, 200_000) + `"derived_from"]}`,
	} {
		server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
			t.Fatal("크기 초과 인자가 도메인 처리기로 넘어왔다")
			return ToolResult{}, nil
		})
		server.verify = func(context.Context, Authentication) (model.ID, error) {
			t.Fatal("크기 초과 인자가 인증 단계로 넘어왔다")
			return model.ID{}, nil
		}
		request := toolCallRequest(t, "graph_list", map[string]any{})
		request.Header.Set("Authorization", "DPoP valid-token")
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		request.Body.Close()
		raw := strings.Replace(string(payload), `"arguments":{}`, `"arguments":`+arguments, 1)
		body := &countedBody{reader: strings.NewReader(raw)}
		request.Body = body
		request.ContentLength = -1
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		assertRPCError(t, response, http.StatusRequestEntityTooLarge, -32600)
		if body.read != (1<<20)+1 || !body.closed {
			t.Fatalf("큰 JSON 인자 읽기 = %d, 닫힘 = %t", body.read, body.closed)
		}
	}
}

func TestMCPBodyLimitAcceptsMaximumEscapedEvent(t *testing.T) {
	id := newTestID(t)
	members := make([]string, maxMembers)
	for i := range members {
		members[i] = id
	}
	text := strings.Repeat("😀", maxBodyRunes)
	request := toolCallRequest(t, "node_create", map[string]any{
		"graph_id": id, "created_by_agent": id, "layer": "event",
		"body": text, "judgment_input": text, "member_refs": members,
	})
	request.Header.Set("Authorization", "DPoP valid-token")
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	request.Body.Close()
	var escapedID strings.Builder
	for _, char := range id {
		escapedID.WriteString(fmt.Sprintf(`\u%04x`, char))
	}
	raw := strings.ReplaceAll(string(payload), "😀", `\ud83d\ude00`)
	raw = strings.ReplaceAll(raw, id, escapedID.String())
	if len(raw) >= 1<<20 {
		t.Fatalf("최대 인자의 이스케이프 표현이 상한을 넘었다: %d", len(raw))
	}
	request.Body = io.NopCloser(strings.NewReader(raw))
	request.ContentLength = int64(len(raw))
	called := false
	server := testServer(t, func(_ context.Context, _ model.ID, _ string, arguments map[string]any) (ToolResult, error) {
		called = true
		if arguments["body"] != text || len(arguments["member_refs"].([]any)) != maxMembers {
			t.Fatal("최대 크기 입력이 정확히 해석되지 않았다")
		}
		return ToolResult{Content: []Content{}}, nil
	})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !called {
		t.Fatalf("최대 인자 처리 = %d, 실행 = %t", response.Code, called)
	}
	t.Logf("최대 텍스트 두 개·참조 1000개의 이스케이프 본문: %d바이트", len(raw))
}

func TestMCPMalformedBodyStillUsesParseError(t *testing.T) {
	request := toolCallRequest(t, "graph_list", map[string]any{})
	body := &countedBody{reader: strings.NewReader(`{"jsonrpc":`)}
	request.Body = body
	request.ContentLength = -1
	response := httptest.NewRecorder()
	testServer(t, nil).ServeHTTP(response, request)
	assertRPCError(t, response, http.StatusBadRequest, -32700)
	if !body.closed {
		t.Fatal("해석 실패 뒤 본문이 닫히지 않았다")
	}
}

func TestMCPBodyLimitOverHTTP(t *testing.T) {
	server := httptest.NewServer(testServer(t, nil))
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	for _, test := range []struct {
		name    string
		size    int
		chunked bool
		status  int
	}{
		{"상한 이내", 1 << 20, false, http.StatusUnauthorized},
		{"Content-Length 초과", 2 << 20, false, http.StatusRequestEntityTooLarge},
		{"chunked 초과", 2 << 20, true, http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := toolCallRequest(t, "graph_list", map[string]any{})
			payload, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			request.Body.Close()
			raw := string(payload) + strings.Repeat(" ", test.size-len(payload))
			request = request.WithContext(t.Context())
			request.URL = mustURL(t, server.URL+"/mcp")
			request.RequestURI = ""
			request.Header.Del("DPoP")
			request.Body = io.NopCloser(strings.NewReader(raw))
			request.ContentLength = int64(len(raw))
			if test.chunked {
				request.ContentLength = -1
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("HTTP 응답 = %d, want %d", response.StatusCode, test.status)
			}
			if test.status == http.StatusRequestEntityTooLarge {
				var result rpcErrorResponse
				if err := json.UnmarshalRead(response.Body, &result); err != nil {
					t.Fatal(err)
				}
				if string(result.ID) != "null" || result.Error.Code != -32600 {
					t.Fatalf("HTTP 크기 초과 오류 = %#v", result)
				}
			}
		})
	}
}

func TestFilterArrayLimitsMatchPublishedSchema(t *testing.T) {
	for _, test := range []struct {
		tool      string
		field     string
		values    []string
		arguments map[string]any
	}{
		{"graph_list", "grade_filter", []string{"owner", "editor", "viewer"}, map[string]any{}},
		{"node_get", "traversal_filter", []string{"derived_from", "has_member", "supersedes", "precedes", "causes", "part_of", "relates_to"}, map[string]any{"graph_id": newTestID(t), "context_id": newTestID(t), "hops": float64(0)}},
		{"relation_list", "state_filter", []string{"proposed", "confirmed", "discarded"}, map[string]any{"graph_id": newTestID(t), "context_id": newTestID(t)}},
		{"relation_list", "type_filter", []string{"precedes", "causes", "part_of", "relates_to"}, map[string]any{"graph_id": newTestID(t), "context_id": newTestID(t)}},
	} {
		t.Run(test.field, func(t *testing.T) {
			items := make([]any, len(test.values))
			for i, value := range test.values {
				items[i] = value
			}
			for _, values := range [][]any{nil, items, {test.values[0], test.values[0]}} {
				test.arguments[test.field] = values
				if err := validateToolCall(test.tool, test.arguments); err != nil {
					t.Fatalf("정상 필터 거부: %v", err)
				}
			}
			called := false
			server := testServer(t, func(context.Context, model.ID, string, map[string]any) (ToolResult, error) {
				called = true
				return ToolResult{}, nil
			})
			test.arguments[test.field] = append(items, test.values[0])
			response := httptest.NewRecorder()
			request := toolCallRequest(t, test.tool, test.arguments)
			request.Header.Set("Authorization", "DPoP valid-token")
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK || called {
				t.Fatalf("초과 필터 상태 = %d, 처리기 실행 = %t", response.Code, called)
			}
			assertDomainCode(t, response, "invalid_argument")
			assertDomainField(t, response, "field", test.field)
			for _, tool := range toolDefinitions() {
				if tool.Name == test.tool {
					properties := tool.InputSchema["properties"].(map[string]any)
					maximum := properties[test.field].(map[string]any)["maxItems"]
					if maximum != len(test.values) {
						t.Fatalf("공개 스키마 상한 = %v, 서버 기준 = %d", maximum, len(test.values))
					}
				}
			}
		})
	}
}
