package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

type web05Client struct {
	t      *testing.T
	base   string
	client *http.Client
	token  string
}

func web05Server(t *testing.T, database *store.Store, graphs web.GraphStore, actor model.ID, plans plan.AccountPlans) web05Client {
	t.Helper()
	service, err := authz.New(database, authz.Config{Issuer: "https://permission.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(t.Context(), actor, web.SessionAudience)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := web.New(permissionAuthentication{service}, graphs, web.Config{Plans: plans, SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return web05Client{t: t, base: server.URL, client: client, token: token.Raw}
}

// request는 GET 또는 POST를 보내고 상태·이동 위치를 대조한 뒤 본문을 돌려준다.
func (client web05Client) request(path string, form url.Values, status int, location string) string {
	client.t.Helper()
	method := http.MethodGet
	if form != nil {
		method = http.MethodPost
	}
	request, err := http.NewRequestWithContext(client.t.Context(), method, client.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		client.t.Fatal(err)
	}
	request.Header.Set("Origin", client.base)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "agent_context_session", Value: client.token})
	response, err := client.client.Do(request)
	if err != nil {
		client.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		client.t.Fatal(err)
	}
	if response.StatusCode != status {
		client.t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, status, body)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		client.t.Fatal("보호 화면의 캐시 경계")
	}
	if response.Header.Get("Location") != location {
		client.t.Fatalf("%s %s 이동 = %q, want %q", method, path, response.Header.Get("Location"), location)
	}
	return string(body)
}

func web05Context(t *testing.T, database *store.Store, graph, actor model.ID, layer model.Layer, body string, references []model.ID) model.Context {
	t.Helper()
	now := time.Now().UTC()
	value := model.Context{ID: permissionID(t), GraphID: graph, Layer: layer, Body: body, RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1}
	var derivedFrom []model.ID
	switch layer {
	case model.LayerSource:
		value.Source = &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelAPI, Locator: "api://web05/" + value.ID.String()}, OccurredAt: now, OriginKind: model.OriginKindExternalContent}
	case model.LayerDerived:
		value.Derived = &model.DerivedAttributes{Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateObservation}
		derivedFrom = references
	case model.LayerEvent:
		value.Event = &model.EventAttributes{MemberIDs: references, Start: now.Add(-time.Minute), End: new(now.Add(time.Minute))}
	}
	stored, err := database.CreateContext(t.Context(), graph, value, derivedFrom)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func web05Stored(t *testing.T, database *store.Store, graph, contextID model.ID) model.Context {
	t.Helper()
	value, err := database.Context(t.Context(), graph, contextID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func web05Graph(t *testing.T, database *store.Store, graph model.ID) model.Graph {
	t.Helper()
	value, err := database.Graph(t.Context(), graph)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func web05Grade(t *testing.T, database *store.Store, graph, account model.ID) model.GraphGrade {
	t.Helper()
	grade, found, err := database.EffectiveGrade(t.Context(), graph, account)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return ""
	}
	return grade
}

// web05LostOwner는 화면 인가 뒤 저장 변경 직전에 행위자의 소유자 등급을 회수한다.
type web05LostOwner struct {
	*store.Store
	owner, actor model.ID
}

func (graphs web05LostOwner) revoke(ctx context.Context, graph model.ID) error {
	return graphs.RevokeGraphGrantWithAudit(ctx, graph, graphs.owner, graphs.actor, store.GrantSubjectAccount)
}
func (graphs web05LostOwner) SetGraphDeleted(ctx context.Context, graph, actor model.ID, deleted bool) error {
	if err := graphs.revoke(ctx, graph); err != nil {
		return err
	}
	return graphs.Store.SetGraphDeleted(ctx, graph, actor, deleted)
}
func (graphs web05LostOwner) SetContextDeleted(ctx context.Context, graph, contextID, actor model.ID, deleted bool, limit int64) (model.Context, error) {
	if err := graphs.revoke(ctx, graph); err != nil {
		return model.Context{}, err
	}
	return graphs.Store.SetContextDeleted(ctx, graph, contextID, actor, deleted, limit)
}

// TestWEB05DeletionRestoreIntegration은 실제 DB·서명 세션·TLS에서 영향 안내, 노드·그래프
// 삭제/복구, 저장량 한도, 삭제된 그래프의 노드 변경 거부와 소유자 재판정을 확인한다.
func TestWEB05DeletionRestoreIntegration(t *testing.T) {
	database := permissionDatabase(t)
	owner, viewer := permissionAccount(t, database), permissionAccount(t, database)
	graph := web03Graph(t, database, owner, `삭제 인수 <"그래프">`, time.Now().UTC()).ID
	web03Grant(t, database, graph, owner, viewer, store.GrantSubjectAccount, model.GraphGradeViewer)
	source := web05Context(t, database, graph, owner, model.LayerSource, "web05 원천 본문 표본", nil)
	derived := web05Context(t, database, graph, owner, model.LayerDerived, "web05 파생 본문", []model.ID{source.ID})
	event := web05Context(t, database, graph, owner, model.LayerEvent, "web05 사건 본문", []model.ID{source.ID})
	stored := web05Graph(t, database, graph).StoredChars
	limited, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"stored_characters_per_graph":%d}}`, owner, stored-1))
	if err != nil {
		t.Fatal(err)
	}
	ownerLimited := web05Server(t, database, database, owner, limited)
	ownerClient := web05Server(t, database, database, owner, plan.AccountPlans{})
	base := "/graphs/" + graph.String() + "/deletion"

	body := ownerClient.request(base, nil, http.StatusOK, "")
	if !strings.Contains(body, "활성 컨텍스트 3개, 확정 관계 0개, 접근이 차단될 계정 2개") || !strings.Contains(body, "삭제 인수 &lt;&#34;그래프&#34;&gt;") {
		t.Fatal("그래프 삭제 영향·이름 표시가 다르다")
	}
	body = ownerClient.request(base+"?context_id="+source.ID.String(), nil, http.StatusOK, "")
	if !strings.Contains(body, "활성 파생 1개, 구성원으로 갖는 활성 사건 1개") {
		t.Fatal("원천 삭제 영향이 다르다")
	}
	body = ownerClient.request(base+"?context_id="+event.ID.String(), nil, http.StatusOK, "")
	if !strings.Contains(body, "이 사건에 붙은 확정 관계 0개") {
		t.Fatal("사건 삭제 영향이 다르다")
	}

	// 삭제는 저장량을 줄이므로 한도를 넘은 계정에서도 통과하고 복구만 거부된다.
	ownerLimited.request(base, url.Values{"action": {"delete_context"}, "context_id": {source.ID.String()}}, http.StatusSeeOther, base)
	if web05Stored(t, database, graph, source.ID).DeletedAt == nil || !web05Stored(t, database, graph, derived.ID).Derived.EvidenceInvalidated || web05Stored(t, database, graph, event.ID).DeletedAt != nil {
		t.Fatal("원천 삭제의 근거 무효·비연쇄 상태가 다르다")
	}
	if body := ownerClient.request(base, nil, http.StatusOK, ""); strings.Contains(body, source.ID.String()) {
		t.Fatal("삭제 보기에 삭제된 원천이 남았다")
	}
	if body := ownerClient.request(base+"?tab=restore", nil, http.StatusOK, ""); !strings.Contains(body, source.ID.String()) || !strings.Contains(body, "web05 원천 본문 표본") {
		t.Fatal("복구 보기에 삭제된 원천이 없다")
	}
	body = ownerLimited.request(base, url.Values{"action": {"restore_context"}, "context_id": {source.ID.String()}}, http.StatusConflict, "")
	if !strings.Contains(body, "그래프 저장량 한도를 초과합니다.") || web05Stored(t, database, graph, source.ID).DeletedAt == nil {
		t.Fatal("저장량 한도 초과 복구가 거부되지 않았다")
	}
	ownerClient.request(base, url.Values{"action": {"restore_context"}, "context_id": {source.ID.String()}}, http.StatusSeeOther, base+"?tab=restore")
	if web05Stored(t, database, graph, source.ID).DeletedAt != nil || web05Graph(t, database, graph).StoredChars != stored {
		t.Fatal("노드 복구 상태·저장량이 다르다")
	}
	ownerClient.request(base, url.Values{"action": {"restore_context"}, "context_id": {source.ID.String()}}, http.StatusConflict, "")

	viewerClient := web05Server(t, database, database, viewer, plan.AccountPlans{})
	viewerClient.request(base, nil, http.StatusForbidden, "")
	viewerClient.request(base, url.Values{"action": {"delete"}, "graph_name": {`삭제 인수 <"그래프">`}}, http.StatusForbidden, "")

	ownerClient.request(base, url.Values{"action": {"delete"}, "graph_name": {"삭제 인수"}}, http.StatusBadRequest, "")
	if web05Graph(t, database, graph).DeletedAt != nil {
		t.Fatal("다른 이름으로 그래프가 삭제됐다")
	}
	ownerClient.request(base, url.Values{"action": {"delete"}, "graph_name": {`삭제 인수 <"그래프">`}}, http.StatusSeeOther, "/graphs")
	deleted := web05Graph(t, database, graph)
	if deleted.DeletedAt == nil || deleted.GraceStartedAt != nil {
		t.Fatal("직접 그래프 삭제 상태가 다르다")
	}
	if body := ownerClient.request(base, nil, http.StatusOK, ""); !strings.Contains(body, "삭제된 그래프입니다.") || strings.Contains(body, "delete_context") {
		t.Fatal("삭제된 그래프의 삭제 보기가 다르다")
	}
	if body := ownerClient.request(base+"?tab=restore", nil, http.StatusOK, ""); strings.Contains(body, "restore_context") || !strings.Contains(body, `value="restore"`) {
		t.Fatal("삭제된 그래프의 복구 보기가 다르다")
	}
	ownerClient.request(base, url.Values{"action": {"delete_context"}, "context_id": {event.ID.String()}}, http.StatusConflict, "")
	if web05Stored(t, database, graph, event.ID).DeletedAt != nil {
		t.Fatal("삭제된 그래프의 노드가 바뀌었다")
	}
	ownerClient.request(base, url.Values{"action": {"restore"}}, http.StatusSeeOther, "/graphs")
	if web05Graph(t, database, graph).DeletedAt != nil {
		t.Fatal("그래프 복구 상태가 다르다")
	}

	// 화면 인가 뒤 저장 변경 전에 등급을 잃으면 잠금 뒤 재판정이 404로 거부한다.
	coOwner := permissionAccount(t, database)
	for _, form := range []url.Values{
		{"action": {"delete_context"}, "context_id": {event.ID.String()}},
		{"action": {"delete"}, "graph_name": {`삭제 인수 <"그래프">`}},
	} {
		web03Grant(t, database, graph, owner, coOwner, store.GrantSubjectAccount, model.GraphGradeOwner)
		stale := web05Server(t, database, web05LostOwner{Store: database, owner: owner, actor: coOwner}, coOwner, plan.AccountPlans{})
		stale.request(base, form, http.StatusNotFound, "")
		if web05Stored(t, database, graph, event.ID).DeletedAt != nil || web05Graph(t, database, graph).DeletedAt != nil || web05Grade(t, database, graph, coOwner) != "" {
			t.Fatal("등급을 잃은 행위자의 변경이 적용됐다")
		}
	}
}

// web05AutomaticGraph는 본문 표식을 가진 그래프를 소유자 이력과 함께 자동 삭제 상태로 만든다.
func web05AutomaticGraph(t *testing.T, database *store.Store, creator, former model.ID, marker string) model.ID {
	t.Helper()
	graph := web03Graph(t, database, creator, "자동 삭제 인수", time.Now().UTC()).ID
	web05Context(t, database, graph, creator, model.LayerSource, marker, nil)
	web03Grant(t, database, graph, creator, former, store.GrantSubjectAccount, model.GraphGradeOwner)
	if err := database.RevokeGraphGrantWithAudit(t.Context(), graph, creator, former, store.GrantSubjectAccount); err != nil {
		t.Fatal(err)
	}
	team, err := database.CreateTeam(t.Context(), creator, "만료를 위한 팀")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), team.ID, creator, creator); err != nil {
		t.Fatal(err)
	}
	web03Grant(t, database, graph, creator, team.ID, store.GrantSubjectTeam, model.GraphGradeOwner)
	if err := database.RevokeGraphGrantWithAudit(t.Context(), graph, creator, creator, store.GrantSubjectAccount); err != nil {
		t.Fatal(err)
	}
	if err := database.RemoveTeamMemberWithAudit(t.Context(), team.ID, creator, creator); err != nil {
		t.Fatal(err)
	}
	permissionExpireGrace(t, database, graph)
	if value := web05Graph(t, database, graph); value.DeletedAt == nil || value.GraceStartedAt == nil {
		t.Fatal("자동 삭제 상태를 만들지 못했다")
	}
	return graph
}

// TestWEB05OperatorRestoreIntegration은 요청 없는 운영자 복구가 불가능하고, 처리 결과가
// 가장 오래된 요청자에게만 직접 소유자 등급을 복원하며 운영자는 등급·본문을 얻지 않는지 확인한다.
func TestWEB05OperatorRestoreIntegration(t *testing.T) {
	database := permissionDatabaseWithGrace(t, func(model.ID) int { return 1 })
	creator, former, operator := permissionAccount(t, database), permissionAccount(t, database), permissionAccount(t, database)
	marker := "web05-operator-body-marker"
	automatic := web05AutomaticGraph(t, database, creator, former, marker)
	direct := web03Graph(t, database, creator, "직접 삭제 인수", time.Now().UTC()).ID
	if err := database.SetGraphDeleted(t.Context(), direct, creator, true); err != nil {
		t.Fatal(err)
	}
	operatorClient := web05Server(t, database, database, operator, plan.AccountPlans{})
	operatorClient.request("/operator/restores", url.Values{"graph_id": {automatic.String()}}, http.StatusNotFound, "")

	web05Server(t, database, database, former, plan.AccountPlans{}).request("/graphs", url.Values{"restore_graph_id": {automatic.String()}}, http.StatusSeeOther, "/graphs")
	web05Server(t, database, database, creator, plan.AccountPlans{}).request("/graphs", url.Values{"restore_graph_id": {automatic.String()}}, http.StatusSeeOther, "/graphs")

	body := operatorClient.request("/operator/restores", nil, http.StatusOK, "")
	if !strings.Contains(body, automatic.String()) || !strings.Contains(body, former.String()) || !strings.Contains(body, creator.String()) || strings.Contains(body, direct.String()) || strings.Contains(body, marker) {
		t.Fatal("대기 목록의 대상·요청 계정·본문 비노출이 다르다")
	}
	body = operatorClient.request("/operator/restores?graph_id="+automatic.String(), nil, http.StatusOK, "")
	confirm := body[strings.Index(body, "복구 처리 확인"):strings.Index(body, "</section>")]
	if !strings.Contains(confirm, former.String()) || strings.Contains(confirm, creator.String()) || strings.Contains(body, marker) {
		t.Fatal("처리 확인이 가장 오래된 요청을 가리키지 않거나 본문을 노출했다")
	}
	operatorClient.request("/operator/restores?graph_id="+direct.String(), nil, http.StatusNotFound, "")
	operatorClient.request("/operator/restores", url.Values{"graph_id": {direct.String()}}, http.StatusNotFound, "")
	if web05Graph(t, database, direct).DeletedAt == nil {
		t.Fatal("요청 없는 직접 삭제 그래프가 운영자 경로로 복구됐다")
	}

	operatorClient.request("/operator/restores", url.Values{"graph_id": {automatic.String()}}, http.StatusSeeOther, "/operator/restores")
	restored := web05Graph(t, database, automatic)
	if restored.DeletedAt != nil || restored.GraceStartedAt != nil {
		t.Fatal("자동 삭제·유예 상태가 해제되지 않았다")
	}
	if web05Grade(t, database, automatic, former) != model.GraphGradeOwner || web05Grade(t, database, automatic, operator) != "" || web05Grade(t, database, automatic, creator) != "" {
		t.Fatal("복구 뒤 등급이 요청자 직접 소유자 하나가 아니다")
	}
	grants, err := database.ListGraphGrants(t.Context(), automatic, former)
	if err != nil {
		t.Fatal(err)
	}
	directOwners := 0
	for _, grant := range grants {
		if !grant.Inherited && grant.Type == string(store.GrantSubjectAccount) && grant.Grade == model.GraphGradeOwner {
			directOwners++
			if grant.ID != former {
				t.Fatal("요청자가 아닌 계정에 직접 소유자 등급이 생겼다")
			}
		}
	}
	if directOwners != 1 {
		t.Fatalf("직접 소유자 수 = %d", directOwners)
	}
	entries, _, err := database.ListAuditEntries(t.Context(), automatic, former, model.AuditFilter{}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, entry := range entries {
		audited = audited || entry.Kind == "operator_restore" && entry.ActorID == operator
	}
	if !audited {
		t.Fatal("운영자 복구 감사가 없다")
	}

	if body := operatorClient.request("/operator/restores", nil, http.StatusOK, ""); strings.Contains(body, automatic.String()) {
		t.Fatal("처리된 그래프의 요청이 대기 목록에 남았다")
	}
	operatorClient.request("/operator/restores", url.Values{"graph_id": {automatic.String()}}, http.StatusNotFound, "")
	operatorClient.request("/graphs/"+automatic.String(), nil, http.StatusNotFound, "")
	web05Server(t, database, database, former, plan.AccountPlans{}).request("/graphs/"+automatic.String()+"/deletion", nil, http.StatusOK, "")
}
