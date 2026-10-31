package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
)

func TestOPS06RestoreSessionHTTPIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	cfg := arch06Configuration(t, f.pool.Config().ConnString(), "https://provider.invalid")
	a, err := assembleApplication(t.Context(), cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.close(nil, true); err != nil {
			t.Error(err)
		}
	})
	service, err := authz.New(a.database, authz.Config{Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	operator, err := service.Register(t.Context(), "ops06_operator", "synthetic password")
	if err != nil {
		t.Fatal(err)
	}
	owner := f.principal.AccountID
	cookies := make(map[model.ID]*http.Cookie)
	for _, id := range []model.ID{owner, operator} {
		session, err := service.WebSession(t.Context(), id, "web")
		if err != nil {
			t.Fatal(err)
		}
		cookies[id] = &http.Cookie{Name: "__Host-agent_context_session", Value: session.Raw}
	}
	const secret = "ops06-private-context-marker"
	args := f.args("node_create")
	args["layer"], args["body"], args["origin_kind"], args["occurred_at"] = "source", secret, "user_utterance", "2026-10-23T00:00:00Z"
	args["source_ref"] = map[string]any{"channel": "file", "locator": "file:///ops06/private"}
	f.id(t, "node_create", args, "node", "context_id")
	// 시험이 소유한 그래프만 DB 시계 기준의 접근 소멸 상태로 옮기고 실제 유예 만료를 실행한다.
	if _, err := f.pool.Exec(t.Context(), `DELETE FROM public.graph_grant WHERE graph_id=$1`, f.graph.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE public.context_graph SET grace_started_at=clock_timestamp()-interval '1 day',grace_expires_at=clock_timestamp() WHERE graph_id=$1`, f.graph.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.database.ExpireGrace(t.Context()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(a.app.handler())
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(client.CloseIdleConnections)
	do := func(method, path string, cookie *http.Cookie, form url.Values, origin string, want int) string {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != want || strings.Contains(string(body), secret) {
			t.Fatalf("복구 HTTP 상태·내용 격리: %s %s / %d / %v", method, path, response.StatusCode, err)
		}
		return string(body)
	}
	restore := url.Values{"graph_id": {f.graph.String()}, "requester_account_id": {operator.String()}}
	request := url.Values{"restore_graph_id": {f.graph.String()}, "requester_account_id": {owner.String()}}
	do("POST", "/operator/restores", nil, restore, server.URL, 303)
	do("POST", "/operator/restores", cookies[operator], restore, "https://cross.test", 403)
	do("POST", "/operator/restores", cookies[operator], restore, server.URL, 404)
	do("POST", "/graphs", cookies[operator], request, server.URL, 404)
	request.Set("requester_account_id", operator.String())
	do("POST", "/graphs", cookies[owner], request, server.URL, 303)
	pending, err := a.database.PendingRestoreRequests(t.Context())
	if err != nil || len(pending) != 1 || pending[0].RequestedBy != owner {
		t.Fatalf("복구 요청 주체를 양식에서 바꿨다: %+v / %v", pending, err)
	}
	body := do("GET", "/operator/restores", cookies[operator], nil, "", 200)
	if !strings.Contains(body, f.graph.String()) {
		t.Fatal("운영자 대기 목록에 요청이 없다")
	}
	do("GET", "/graphs/"+f.graph.String(), cookies[operator], nil, "", 404)
	do("POST", "/operator/restores", cookies[operator], restore, server.URL, 303)
	do("POST", "/operator/restores", cookies[operator], restore, server.URL, 404)
	do("GET", "/graphs/"+f.graph.String(), cookies[operator], nil, "", 404)
	if grade, found, err := a.database.EffectiveGrade(t.Context(), f.graph, owner); err != nil || !found || grade != model.GraphGradeOwner {
		t.Fatalf("실제 요청자의 복원 등급: %s / %t / %v", grade, found, err)
	}
	if _, found, err := a.database.EffectiveGrade(t.Context(), f.graph, operator); err != nil || found {
		t.Fatal("처리 운영자에게 등급을 부여했다", err)
	}
	if pending, err := a.database.PendingRestoreRequests(t.Context()); err != nil || len(pending) != 0 {
		t.Fatal("완료한 HTTP 요청이 대기에 남았다", err)
	}
}
