package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/web"
)

func TestWEB05RestoreRequestBoundsIntegration(t *testing.T) {
	database := permissionDatabaseWithGrace(t, func(model.ID) int { return 1 })
	actor := permissionAccount(t, database)
	service, err := authz.New(database, authz.Config{Issuer: "https://permission.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(t.Context(), actor, "web")
	if err != nil {
		t.Fatal(err)
	}
	app, err := web.New(permissionAuthentication{service}, database, web.Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	exchange := func(path string, form url.Values, status int) string {
		t.Helper()
		method, body := http.MethodGet, ""
		if form != nil {
			method, body = http.MethodPost, form.Encode()
		}
		request := httptest.NewRequestWithContext(t.Context(), method, "https://permission.test"+path, strings.NewReader(body))
		request.AddCookie(&http.Cookie{Name: "__Host-agent_context_session", Value: token.Raw})
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", "https://permission.test")
		}
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s = %d want=%d: %s", method, path, response.Code, status, response.Body.String())
		}
		if status == http.StatusSeeOther && response.Header().Get("Location") != "/graphs" && response.Header().Get("Location") != "/operator/restores" {
			t.Fatalf("unexpected redirect: %s", response.Header().Get("Location"))
		}
		return response.Body.String()
	}
	link := regexp.MustCompile(`/operator/restores\?graph_id=([0-9a-f-]+)`)
	var graphs []string
	for i := range 101 {
		graph := web03AutomaticGraph(t, database, actor, model.ID{}, model.ID{}, "복구 대기")
		graphs = append(graphs, graph.ID.String())
		exchange("/graphs", url.Values{"restore_graph_id": {graph.ID.String()}}, http.StatusSeeOther)
		if i == 0 {
			for range 3 {
				exchange("/graphs", url.Values{"restore_graph_id": {graph.ID.String()}}, http.StatusSeeOther)
			}
			pending, err := database.PendingRestoreRequests(t.Context())
			if err != nil || len(pending) != 1 {
				t.Fatalf("repeated HTTP requests=%+v: %v", pending, err)
			}
		}
		if i == 98 || i == 99 || i == 100 {
			body := exchange("/operator/restores", nil, http.StatusOK)
			var actual []string
			for _, match := range link.FindAllStringSubmatch(body, -1) {
				actual = append(actual, match[1])
			}
			if !slices.Equal(actual, graphs[:min(i+1, 100)]) {
				t.Fatalf("total=%d listed=%d: wrong bounds/order", i+1, len(actual))
			}
			if !strings.Contains(body, "오래된 순서로 최대 100건") {
				t.Fatal("missing queue limit notice")
			}
		}
	}
	// 상한 밖의 그래프는 확인 화면에도 싣지 않으며 처리 후 다음 대기 요청이 나타난다.
	exchange("/operator/restores?graph_id="+graphs[100], nil, http.StatusNotFound)
	exchange("/operator/restores?graph_id="+graphs[0], nil, http.StatusOK)
	exchange("/operator/restores", url.Values{"graph_id": {graphs[0]}}, http.StatusSeeOther)
	body := exchange("/operator/restores", nil, http.StatusOK)
	var actual []string
	for _, match := range link.FindAllStringSubmatch(body, -1) {
		actual = append(actual, match[1])
	}
	if !slices.Equal(actual, graphs[1:]) {
		t.Fatalf("queue did not advance after restore: listed=%d", len(actual))
	}
	exchange("/operator/restores?graph_id="+graphs[100], nil, http.StatusOK)
}
