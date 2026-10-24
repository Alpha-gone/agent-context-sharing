package web_test

import (
	"context"
	"errors"
	"fmt"
	"html"
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

var (
	web08Row  = regexp.MustCompile(`<tr><td>([^<]+)</td><td>(웹 관리|관리 연산)</td><td>([^<]*)</td><td>([^<]*)</td><td><code>([^<]*)</code></td><td><code>([^<]*)</code></td><td>([^<]*)</td></tr>`)
	web08More = regexp.MustCompile(`<a href="([^"]+)">더 보기</a>`)
)

// web08Rows는 감사 화면의 모든 페이지를 따라가며 행과 다음 페이지 주소를 모은다.
func web08Rows(t *testing.T, client web05Client, path string) ([]string, []string) {
	t.Helper()
	var rows, pages []string
	for path != "" {
		body := client.request(path, nil, http.StatusOK, "")
		matches := web08Row.FindAllStringSubmatch(body, -1)
		if len(matches) > 2 {
			t.Fatalf("한 페이지의 행 수 = %d", len(matches))
		}
		for _, match := range matches {
			rows = append(rows, strings.Join(match[1:], "|"))
		}
		path = ""
		if more := web08More.FindStringSubmatch(body); more != nil {
			path = html.UnescapeString(more[1])
			pages = append(pages, path)
		}
	}
	return rows, pages
}

// web08Expected는 같은 범위를 저장소에서 한 번에 읽어 화면 행과 같은 형식으로 만든다.
func web08Expected(t *testing.T, database *store.Store, graph, owner model.ID, filter model.AuditFilter) []string {
	t.Helper()
	entries, next, err := database.ListAuditEntries(t.Context(), graph, owner, filter, "", 1000)
	if err != nil || next != "" {
		t.Fatalf("기준 감사 조회 = %v, 다음 %q", err, next)
	}
	rows := make([]string, 0, len(entries))
	for _, entry := range entries {
		source := "관리 연산"
		if entry.Source == "web" {
			source = "웹 관리"
		}
		rows = append(rows, strings.Join([]string{entry.OccurredAt.UTC().Format(time.RFC3339), source, entry.Kind, entry.Action, entry.ActorID.String(), entry.Target, html.EscapeString(entry.Detail)}, "|"))
	}
	return rows
}

// web08LostOwner는 화면 인가 뒤 감사 조회 직전에 행위자의 소유자 등급을 회수한다.
type web08LostOwner struct {
	*store.Store
	owner, actor model.ID
}

func (graphs web08LostOwner) ListAuditEntries(ctx context.Context, graph, actor model.ID, filter model.AuditFilter, cursor string, limit int) ([]model.AuditEntry, string, error) {
	if err := graphs.RevokeGraphGrantWithAudit(ctx, graph, graphs.owner, graphs.actor, store.GrantSubjectAccount); err != nil {
		return nil, "", err
	}
	return graphs.Store.ListAuditEntries(ctx, graph, actor, filter, cursor, limit)
}

// TestWEB08AuditIntegration은 실제 두 감사 기록을 플랜 페이지 크기로 합쳐 모든 페이지가
// 저장소의 한 번 조회와 같고, 기간·대상 필터와 커서·접근 경계가 유지되는지 확인한다.
func TestWEB08AuditIntegration(t *testing.T) {
	database := permissionDatabase(t)
	owner, viewer, coOwner := permissionAccount(t, database), permissionAccount(t, database), permissionAccount(t, database)
	graph := web03Graph(t, database, owner, "감사 인수", time.Now().UTC()).ID
	web03Grant(t, database, graph, owner, viewer, store.GrantSubjectAccount, model.GraphGradeViewer)
	source := model.Context{ID: permissionID(t), GraphID: graph, Layer: model.LayerSource, Body: "web08 원천", RecordedAt: time.Now().UTC(), CreatedBy: owner, CreatedByAgent: owner, Version: 1,
		Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelAPI, Locator: "api://web08/source"}, OccurredAt: time.Now().UTC(), OriginKind: model.OriginKindExternalContent}}
	if _, err := database.CreateContextWithOperation(t.Context(), graph, source, nil, &store.OperationRecord{Kind: store.OperationAdd, GraphID: graph, ContextID: source.ID, JudgmentInput: "web08 감사 인수 표본", AccountID: owner, AgentID: owner}, store.WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	other := web05Context(t, database, graph, owner, model.LayerSource, "web08 다른 원천", nil)
	for _, deleted := range []bool{true, false} {
		for _, target := range []model.ID{source.ID, other.ID} {
			if _, err := database.SetContextDeleted(t.Context(), graph, target, owner, deleted, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"graph_page":{"default":2,"maximum":3}}}`, owner))
	if err != nil {
		t.Fatal(err)
	}
	client := web05Server(t, database, database, owner, plans)
	base := "/graphs/" + graph.String() + "/audit"

	all := web08Expected(t, database, graph, owner, model.AuditFilter{})
	rows, pages := web08Rows(t, client, base)
	if len(all) < 6 || !slices.Equal(rows, all) || len(pages) != (len(all)-1)/2 || !slices.ContainsFunc(rows, func(row string) bool { return strings.Contains(row, "|관리 연산|") }) {
		t.Fatalf("전체 감사 페이지 = %d행/%d쪽, 기준 %d행", len(rows), len(pages), len(all))
	}
	targeted := web08Expected(t, database, graph, owner, model.AuditFilter{Target: source.ID})
	rows, pages = web08Rows(t, client, base+"?target="+source.ID.String())
	if len(targeted) != 3 || !slices.Equal(rows, targeted) {
		t.Fatalf("대상 감사 = %v, 기준 %v", rows, targeted)
	}
	for _, page := range pages {
		if parsed, err := url.Parse(page); err != nil || parsed.Query().Get("target") != source.ID.String() || parsed.Query().Get("cursor") == "" {
			t.Fatalf("대상 필터의 다음 페이지 = %q", page)
		}
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := today.Format(time.DateOnly)
	period := web08Expected(t, database, graph, owner, model.AuditFilter{From: &today, Until: new(today.AddDate(0, 0, 1))})
	if rows, _ := web08Rows(t, client, base+"?from="+day+"&until="+day); !slices.Equal(rows, period) {
		t.Fatal("오늘 하루의 감사 범위가 저장소와 다르다")
	}
	if rows, _ := web08Rows(t, client, base+"?until="+today.AddDate(0, 0, -1).Format(time.DateOnly)); len(rows) != 0 {
		t.Fatal("어제까지의 범위에 오늘 기록이 포함됐다")
	}
	body := client.request(base+"?target="+source.ID.String()+"&cursor=broken", nil, http.StatusBadRequest, "")
	if !strings.Contains(body, `value="`+source.ID.String()+`"`) {
		t.Fatal("커서 오류에서 필터가 유지되지 않았다")
	}
	client.request(base+"?from=2026-10-03&until=2026-10-02", nil, http.StatusBadRequest, "")
	web05Server(t, database, database, viewer, plans).request(base, nil, http.StatusForbidden, "")
	web05Server(t, database, database, permissionAccount(t, database), plans).request(base, nil, http.StatusNotFound, "")

	web03Grant(t, database, graph, owner, coOwner, store.GrantSubjectAccount, model.GraphGradeOwner)
	stale := web05Server(t, database, web08LostOwner{Store: database, owner: owner, actor: coOwner}, coOwner, plans)
	if body := stale.request(base, nil, http.StatusNotFound, ""); strings.Contains(body, source.ID.String()) {
		t.Fatal("등급을 잃은 행위자에게 감사 기록이 노출됐다")
	}
}

const (
	web08Resource = "https://permission.test/mcp"
	web08Callback = "http://127.0.0.1:49152/callback"
)

func web08Service(t *testing.T, database *store.Store) *authz.Service {
	t.Helper()
	service, err := authz.New(database, authz.Config{
		Issuer: "https://permission.test", BcryptCost: 4,
		OAuth: &authz.OAuthConfig{Resource: web08Resource, Clients: map[string]authz.OAuthClient{
			"first": {RedirectURIs: []string{web08Callback}, Resources: []string{web08Resource}, LoopbackPortPolicy: "exact"},
		}},
		AuthorizationCodes: database, AccessTokens: database, RefreshTokens: database, Connections: database,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// web08Grant는 정식 코드 교환으로 계정의 연결 계열과 자격을 발급한다.
func web08Grant(t *testing.T, service *authz.Service, account model.ID) (authz.OAuthTokens, model.ID) {
	t.Helper()
	code, err := service.IssueAuthorizationCode(t.Context(), account, authz.AuthorizationRequest{
		ClientID: "first", RedirectURI: web08Callback, Resource: web08Resource, Scope: "agent-context", ResponseType: "code",
		ChallengeMethod: "S256", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
	})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := service.ExchangeAuthorizationCode(t.Context(), code, "first", web08Callback, web08Resource, "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.VerifyBearer(t.Context(), tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	return tokens, principal.GrantID
}

func web08Connections(t *testing.T, database *store.Store, service *authz.Service, account model.ID) (web05Client, string) {
	t.Helper()
	token, err := service.WebSession(t.Context(), account, web.SessionAudience)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := web.New(permissionAuthentication{service}, database, web.Config{Connections: service, SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return web05Client{t: t, base: server.URL, client: client, token: token.Raw}, token.Raw
}

// TestWEB08ConnectionsIntegration은 본인 계열·등록만 보이고, 계열 폐기와 등록 폐기의 효과가
// 구분되며 웹 세션이 MCP 계열 폐기로 만료되지 않는지 실제 DB·TLS에서 확인한다.
func TestWEB08ConnectionsIntegration(t *testing.T) {
	database := permissionDatabase(t)
	service := web08Service(t, database)
	owner, other := permissionAccount(t, database), permissionAccount(t, database)
	revoked, revokedGrant := web08Grant(t, service, owner)
	kept, keptGrant := web08Grant(t, service, owner)
	foreign, foreignGrant := web08Grant(t, service, other)
	agent, err := database.RegisterAgent(t.Context(), owner, "first", "web08-owner-registration")
	if err != nil {
		t.Fatal(err)
	}
	foreignAgent, err := database.RegisterAgent(t.Context(), other, "first", "web08-other-registration")
	if err != nil {
		t.Fatal(err)
	}
	client, session := web08Connections(t, database, service, owner)

	body := client.request("/connections", nil, http.StatusOK, "")
	for _, want := range []string{revokedGrant.String(), keptGrant.String(), agent.String(), "first"} {
		if !strings.Contains(body, want) {
			t.Fatalf("연결 목록에 %q가 없다", want)
		}
	}
	for _, hidden := range []string{foreignGrant.String(), foreignAgent.String(), revoked.AccessToken, revoked.RefreshToken, kept.AccessToken, "web08-owner-registration", session} {
		if strings.Contains(body, hidden) {
			t.Fatal("다른 계정 대상 또는 자격이 연결 화면에 노출됐다")
		}
	}

	for _, form := range []url.Values{
		{"action": {"revoke_grant"}, "target_id": {foreignGrant.String()}},
		{"action": {"revoke_agent"}, "target_id": {foreignAgent.String()}},
	} {
		client.request("/connections", form, http.StatusNotFound, "")
	}
	if _, err := service.VerifyBearer(t.Context(), foreign.AccessToken); err != nil {
		t.Fatal("다른 계정의 계열이 폐기됐다")
	}
	if err := database.ValidateAgent(t.Context(), other, "first", foreignAgent); err != nil {
		t.Fatal("다른 계정의 등록이 폐기됐다")
	}

	client.request("/connections", url.Values{"action": {"revoke_grant"}, "target_id": {revokedGrant.String()}}, http.StatusSeeOther, "/connections")
	if _, err := service.VerifyBearer(t.Context(), revoked.AccessToken); !errors.Is(err, authz.ErrInvalidCredential) {
		t.Fatal("폐기한 계열의 접근 자격이 남았다")
	}
	if _, err := service.Refresh(t.Context(), revoked.RefreshToken, "first", web08Resource, ""); err == nil {
		t.Fatal("폐기한 계열의 갱신이 남았다")
	}
	if _, err := service.VerifyBearer(t.Context(), kept.AccessToken); err != nil {
		t.Fatal("다른 계열이 함께 폐기됐다")
	}
	if err := database.ValidateAgent(t.Context(), owner, "first", agent); err != nil {
		t.Fatal("계열 폐기가 에이전트 등록을 폐기했다")
	}
	if account, _, err := service.Verify(t.Context(), session, web.SessionAudience); err != nil || account != owner {
		t.Fatal("MCP 계열 폐기로 웹 세션이 만료됐다")
	}
	page, err := service.ListConnections(t.Context(), owner, model.ID{})
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(page.Grants, func(grant store.OAuthGrantSummary) bool { return grant.ID == revokedGrant })
	if index < 0 || page.Grants[index].RevokedAt == nil {
		t.Fatal("폐기 시각이 기록되지 않았다")
	}
	first := *page.Grants[index].RevokedAt
	client.request("/connections", url.Values{"action": {"revoke_grant"}, "target_id": {revokedGrant.String()}}, http.StatusSeeOther, "/connections")
	if page, err = service.ListConnections(t.Context(), owner, model.ID{}); err != nil || !page.Grants[index].RevokedAt.Equal(first) {
		t.Fatal("반복 폐기가 최초 폐기 시각을 바꿨다")
	}
	if body := client.request("/connections", nil, http.StatusOK, ""); !strings.Contains(body, "폐기됨") {
		t.Fatal("폐기 상태가 표시되지 않았다")
	}

	client.request("/connections", url.Values{"action": {"revoke_agent"}, "target_id": {agent.String()}}, http.StatusSeeOther, "/connections")
	if err := database.ValidateAgent(t.Context(), owner, "first", agent); err == nil {
		t.Fatal("폐기한 등록이 유효하다")
	}
	if _, err := service.VerifyBearer(t.Context(), kept.AccessToken); err != nil {
		t.Fatal("등록 폐기가 연결 계열을 폐기했다")
	}
	if body := client.request("/connections", nil, http.StatusOK, ""); strings.Contains(body, agent.String()) || !strings.Contains(body, "활성 에이전트 등록이 없습니다.") {
		t.Fatal("폐기한 등록이 활성 목록에 남았다")
	}
}
