package web_test

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

func web03Graph(t *testing.T, database *store.Store, owner model.ID, name string, at time.Time) model.Graph {
	t.Helper()
	graph, err := database.CreateGraphWithOwner(t.Context(), model.Graph{ID: permissionID(t), Name: name, Description: "목록 인수", CreatedBy: owner, CreatedAt: at, LastActivityAt: at, Version: 1}, store.WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func web03Grant(t *testing.T, database *store.Store, graph, owner, subject model.ID, kind store.GrantSubjectType, grade model.GraphGrade) {
	t.Helper()
	if err := database.GrantGraph(t.Context(), graph, owner, subject, kind, grade); err != nil {
		t.Fatal(err)
	}
}

func web03AutomaticGraph(t *testing.T, database *store.Store, creator, former, viewer model.ID, name string) model.Graph {
	t.Helper()
	graph := web03Graph(t, database, creator, name, time.Now().UTC())
	for _, grant := range []struct {
		account model.ID
		grade   model.GraphGrade
	}{{former, model.GraphGradeOwner}, {viewer, model.GraphGradeViewer}} {
		if !grant.account.IsV7() {
			continue
		}
		web03Grant(t, database, graph.ID, creator, grant.account, store.GrantSubjectAccount, grant.grade)
		if err := database.RevokeGraphGrantWithAudit(t.Context(), graph.ID, creator, grant.account, store.GrantSubjectAccount); err != nil {
			t.Fatal(err)
		}
	}
	team, err := database.CreateTeam(t.Context(), creator, "만료를 위한 팀")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), team.ID, creator, creator); err != nil {
		t.Fatal(err)
	}
	web03Grant(t, database, graph.ID, creator, team.ID, store.GrantSubjectTeam, model.GraphGradeOwner)
	if err := database.RevokeGraphGrantWithAudit(t.Context(), graph.ID, creator, creator, store.GrantSubjectAccount); err != nil {
		t.Fatal(err)
	}
	if err := database.RemoveTeamMemberWithAudit(t.Context(), team.ID, creator, creator); err != nil {
		t.Fatal(err)
	}
	permissionExpireGrace(t, database, graph.ID)
	return graph
}

func TestWEB03GraphListIntegration(t *testing.T) {
	database := permissionDatabaseWithGrace(t, func(model.ID) int { return 1 })
	actor, other := permissionAccount(t, database), permissionAccount(t, database)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	ownerA := web03Graph(t, database, actor, "대상 %_+ A", at)
	ownerB := web03Graph(t, database, actor, "대상 %_+ B", at)
	editor := web03Graph(t, database, other, "대상 %_+ C", at.Add(-time.Minute))
	viewer := web03Graph(t, database, other, "대상 %_+ D", at.Add(-2*time.Minute))
	web03Graph(t, database, other, "SECRET 다른 계정", at)
	web03Grant(t, database, viewer.ID, other, actor, store.GrantSubjectAccount, model.GraphGradeViewer)
	web03Grant(t, database, editor.ID, other, actor, store.GrantSubjectAccount, model.GraphGradeViewer)
	team, err := database.CreateTeam(t.Context(), other, "상속 편집자")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), team.ID, other, actor); err != nil {
		t.Fatal(err)
	}
	web03Grant(t, database, editor.ID, other, team.ID, store.GrantSubjectTeam, model.GraphGradeEditor)
	direct := web03Graph(t, database, actor, "직접 삭제", at)
	if err := database.SetGraphDeleted(t.Context(), direct.ID, actor, true); err != nil {
		t.Fatal(err)
	}
	creatorAuto := web03AutomaticGraph(t, database, actor, model.ID{}, model.ID{}, "생성자 자동 삭제")
	formerAuto := web03AutomaticGraph(t, database, other, actor, model.ID{}, "과거 소유자 자동 삭제")
	strangerAuto := web03AutomaticGraph(t, database, other, model.ID{}, actor, "SECRET 과거 열람자 자동 삭제")
	service, err := authz.New(database, authz.Config{Issuer: "https://permission.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(t.Context(), actor, "web")
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"graph_page":{"default":2,"maximum":3}}}`, actor.String()))
	if err != nil {
		t.Fatal(err)
	}
	app, err := web.New(permissionAuthentication{service}, database, web.Config{Plans: plans, SecureCookie: func(r *http.Request) bool { return r.TLS != nil }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(app)
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	exchange := func(t *testing.T, path string, form url.Values, want int) string {
		t.Helper()
		method, body := "GET", ""
		if form != nil {
			method, body = "POST", form.Encode()
		}
		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: "__Host-agent_context_session", Value: token.Raw})
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", server.URL)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil || response.StatusCode != want || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("TLS 목록 응답·캐시가 다르다", response.StatusCode, err)
		}
		if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), strangerAuto.ID.String()) {
			t.Fatal("다른 계정·열람자 이력만 있는 자동 삭제 대상이 노출됐다")
		}
		return string(raw)
	}
	ids := func(body string) []string {
		var values []string
		for _, match := range regexp.MustCompile(`<a href="/graphs/([0-9a-f-]+)">`).FindAllStringSubmatch(body, -1) {
			values = append(values, match[1])
		}
		return values
	}
	t.Run("pages_and_filters", func(t *testing.T) {
		query := url.Values{"name": {"  대상 %_+  "}}
		page := exchange(t, "/graphs?"+query.Encode(), nil, 200)
		first := ids(page)
		if len(first) != 2 {
			t.Fatal("계정 플랜의 페이지 크기를 적용하지 않았다")
		}
		link := regexp.MustCompile(`<a href="([^"]+)">더 보기</a>`).FindStringSubmatch(page)
		if len(link) != 2 {
			t.Fatal("다음 페이지가 없다")
		}
		next := exchange(t, html.UnescapeString(link[1]), nil, 200)
		want := []string{ownerA.ID.String(), ownerB.ID.String()}
		slices.Sort(want)
		slices.Reverse(want)
		want = append(want, editor.ID.String(), viewer.ID.String())
		if !slices.Equal(append(first, ids(next)...), want) || strings.Contains(next, ">더 보기</a>") {
			t.Fatal("활동 시각·ID 정렬 또는 마지막 페이지가 다르다")
		}
		for _, sample := range []struct {
			grades []string
			want   []string
		}{{[]string{"owner"}, want[:2]}, {[]string{"editor"}, []string{editor.ID.String()}}, {[]string{"viewer"}, []string{viewer.ID.String()}}, {[]string{"viewer", "editor", "viewer", ""}, []string{editor.ID.String(), viewer.ID.String()}}} {
			query["grade"] = sample.grades
			if got := ids(exchange(t, "/graphs?"+query.Encode(), nil, 200)); !slices.Equal(got, sample.want) {
				t.Fatal("현재 직접/상속 유효 등급 필터가 다르다", sample.grades, got)
			}
		}
		if err := database.RevokeGraphGrantWithAudit(t.Context(), viewer.ID, other, actor, store.GrantSubjectAccount); err != nil {
			t.Fatal(err)
		}
		if got := ids(exchange(t, html.UnescapeString(link[1]), nil, 200)); !slices.Equal(got, []string{editor.ID.String()}) {
			t.Fatal("커서 다음 요청에서 회수된 등급을 재검사하지 않았다")
		}
		exchange(t, "/graphs?cursor=bad", nil, 400)
	})
	t.Run("restore_entry_and_request", func(t *testing.T) {
		body := exchange(t, "/graphs", nil, 200)
		for _, graph := range []model.Graph{creatorAuto, formerAuto, direct} {
			if !strings.Contains(body, graph.Name) || !strings.Contains(body, graph.ID.String()) {
				t.Fatal("본인 복구 진입점이 없다", graph.Name)
			}
		}
		for _, graph := range []model.Graph{creatorAuto, formerAuto} {
			exchange(t, "/graphs", url.Values{"restore_graph_id": {graph.ID.String()}}, 303)
		}
		exchange(t, "/graphs", url.Values{"restore_graph_id": {strangerAuto.ID.String()}}, 404)
		exchange(t, "/graphs", url.Values{"restore_graph_id": {direct.ID.String()}}, 404)
		pending, err := database.PendingRestoreRequests(t.Context())
		if err != nil || len(pending) != 2 {
			t.Fatal("복구 요청의 저장·범위가 다르다", err)
		}
		for _, request := range pending {
			if request.RequestedBy != actor || !slices.Contains([]model.ID{creatorAuto.ID, formerAuto.ID}, request.GraphID) {
				t.Fatal("복구 요청 주체·대상이 세션과 다르다")
			}
		}
	})
}
