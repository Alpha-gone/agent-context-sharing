package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type connectionStub struct {
	page                    store.ConnectionPage
	err                     error
	account, before, target model.ID
	action                  string
	lists, revokes          int
	cancel                  context.CancelFunc
}

func (s *connectionStub) ListConnections(_ context.Context, account, before model.ID) (store.ConnectionPage, error) {
	s.lists++
	s.account, s.before = account, before
	if s.cancel != nil {
		s.cancel()
	}
	return s.page, s.err
}
func (s *connectionStub) RevokeConnection(_ context.Context, account model.ID, action string, target model.ID) error {
	s.revokes++
	s.account, s.action, s.target = account, action, target
	if s.cancel != nil {
		s.cancel()
	}
	return s.err
}

func TestConnectionsHTTPBoundary(t *testing.T) {
	account, target := testID(t), testID(t)
	valid := "action=revoke_grant&target_id=" + target.String()
	for _, tc := range []struct {
		name, method, path, body, origin string
		status, calls                    int
	}{
		{"grant", "POST", "/connections", valid, "", 303, 1},
		{"agent", "POST", "/connections", strings.Replace(valid, "revoke_grant", "revoke_agent", 1), "", 303, 1},
		{"cross origin", "POST", "/connections", valid, "https://other.test", 403, 0},
		{"unknown action", "POST", "/connections", "action=remove&target_id=" + target.String(), "", 400, 0},
		{"duplicate action", "POST", "/connections", valid + "&action=revoke_agent", "", 400, 0},
		{"duplicate target", "POST", "/connections", valid + "&target_id=" + target.String(), "", 400, 0},
		{"account spoof", "POST", "/connections", valid + "&account_id=" + target.String(), "", 400, 0},
		{"client spoof", "POST", "/connections", valid + "&client_id=other", "", 400, 0},
		{"query credentials", "POST", "/connections?" + valid, "", "", 400, 0},
		{"query merged", "POST", "/connections?action=revoke_agent", valid, "", 400, 0},
		{"malformed escape", "POST", "/connections", valid + "%", "", 400, 0},
		{"missing", "POST", "/connections", "action=revoke_grant", "", 400, 0},
		{"invalid uuid", "POST", "/connections", "action=revoke_grant&target_id=none", "", 400, 0},
		{"too large", "POST", "/connections", strings.Repeat("x", authFormLimit+1), "", 413, 0},
		{"method", "DELETE", "/connections", valid, "", 405, 0},
		{"get mutation", "GET", "/connections?" + valid, "", "", 400, 0},
		{"duplicate cursor", "GET", "/connections?before=" + target.String() + "&before=" + target.String(), "", "", 400, 0},
		{"invalid cursor", "GET", "/connections?before=bad", "", "", 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &connectionStub{}
			server := newTestServer(t, account, model.ID{})
			server.config.Connections = source
			request := sessionRequest(tc.method, tc.path, nil)
			request.Body = http.NoBody
			if tc.body != "" {
				request = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "web-session"})
			}
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != tc.status || source.revokes != tc.calls || source.lists != 0 {
				t.Fatalf("boundary status=%d calls=%d lists=%d", response.Code, source.revokes, source.lists)
			}
			if tc.calls > 0 && (source.account != account || source.target != target || response.Header().Get("Location") != "/connections") {
				t.Fatal("session binding or redirect")
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Referrer-Policy") != "no-referrer" || response.Header().Get("X-Frame-Options") != "DENY" || len(response.Result().Cookies()) != 0 {
				t.Fatal("privacy boundary")
			}
		})
	}
}

func TestConnectionsListAndFailures(t *testing.T) {
	account, target, revoked := testID(t), testID(t), testID(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, mode := range []string{"list", "empty", "missing session", "session outage", "missing service", "list outage", "not found", "revoke outage", "canceled list", "canceled revoke", "bad media", "stream limit"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			source := &connectionStub{page: store.ConnectionPage{Grants: []store.OAuthGrantSummary{
				{ID: target, ClientID: "<script>private()</script>", AuthorizedAt: now, LastRotatedAt: now, AbsoluteExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(-time.Second)},
				{ID: revoked, ClientID: "revoked", RevokedAt: new(now), AbsoluteExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(time.Hour)},
			}, Agents: []store.AgentSummary{{ID: target, ClientID: "first", CreatedAt: now}}, Next: target}}
			server := newTestServer(t, account, model.ID{})
			server.config.Connections = source
			request := sessionRequest("GET", "/connections?before="+target.String(), nil).WithContext(ctx)
			expected := 200
			switch mode {
			case "empty":
				source.page = store.ConnectionPage{}
			case "missing session":
				request.Header.Del("Cookie")
				expected = 303
			case "session outage":
				server.auth = &sessionBoundaryAuthentication{verification: ErrAuthenticationUnavailable}
				expected = 500
			case "missing service":
				server.config.Connections = nil
				expected = 503
			case "list outage":
				source.err = errors.New("synthetic-connection-secret")
				expected = 500
			case "not found", "revoke outage", "canceled revoke", "bad media", "stream limit":
				request = sessionRequest("POST", "/connections", url.Values{"action": {"revoke_grant"}, "target_id": {target.String()}}).WithContext(ctx)
				if mode == "not found" {
					source.err = model.ErrNotFound
					expected = 404
				}
				if mode == "revoke outage" {
					source.err = errors.Join(ErrAuthenticationUnavailable, model.ErrNotFound, errors.New("synthetic-connection-secret"))
					expected = 500
				}
				if mode == "canceled revoke" {
					source.cancel = cancel
				}
				if mode == "bad media" {
					request.Header.Set("Content-Type", "application/json")
					expected = 400
				}
				if mode == "stream limit" {
					request.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", authFormLimit+1)))
					request.ContentLength = -1
					expected = 413
				}
			case "canceled list":
				source.cancel = cancel
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != expected || strings.Contains(body, "synthetic-connection-secret") || strings.Contains(body, "<script>private()") || len(response.Result().Cookies()) != 0 {
				t.Fatalf("%s status=%d", mode, response.Code)
			}
			if strings.HasPrefix(mode, "canceled") && (body != "" || response.Header().Get("Location") != "") {
				t.Fatal("cancel produced success")
			}
			if mode == "list" {
				for _, text := range []string{"갱신 만료", "폐기됨", "&lt;script&gt;", "aria-describedby=\"grant-revoke-help\"", "method=\"post\" action=\"/connections\"", "before=" + target.String()} {
					if !strings.Contains(body, text) {
						t.Fatal("missing rendered state: " + text)
					}
				}
				if source.account != account || source.before != target || strings.Count(body, "value=\"revoke_grant\"") != 1 {
					t.Fatal("paging or revoked form")
				}
			}
		})
	}
}
