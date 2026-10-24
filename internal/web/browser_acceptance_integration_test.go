package web_test

import (
	"crypto/sha256"
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

const web09DefaultChrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

// web09Browser는 실제 브라우저 인수에 쓸 Chrome과 Node.js를 찾는다. 필수 실행 환경에서는
// 둘 중 하나라도 없으면 실패하고, 그 밖에서는 건너뜀을 명시한다.
func web09Browser(t *testing.T) (string, string) {
	t.Helper()
	chrome := os.Getenv("WEB_BROWSER_CHROME")
	if chrome == "" {
		chrome = web09DefaultChrome
	}
	node, nodeErr := exec.LookPath("node")
	if _, err := os.Stat(chrome); err != nil || nodeErr != nil {
		if os.Getenv("WEB_BROWSER_REQUIRED") != "" {
			t.Fatalf("필수 브라우저 인수의 Chrome 또는 Node.js가 없다: chrome=%v node=%v", err, nodeErr)
		}
		t.Skip("Chrome 또는 Node.js가 없어 실제 브라우저 인수를 건너뛴다")
	}
	return chrome, node
}

var web09Attack = template.Must(template.New("attack").Parse(`<!doctype html><html lang="ko"><head><meta charset="utf-8"><title>교차 출처 표본</title></head><body>
<form id="attack" method="post" action="{{.Action}}"><input name="action" value="delete_context"><input name="context_id" value="{{.Context}}"></form>
<script>document.getElementById('attack').submit()</script></body></html>`))

// TestWEB09BrowserAcceptance는 실제 DB·서명 세션·TLS 서버를 headless Chrome으로 조작해
// 로그인부터 목록·시각화·팀·삭제/복구·감사·연결·출처·작은 화면·로그아웃까지 확인한다.
func TestWEB09BrowserAcceptance(t *testing.T) {
	chrome, node := web09Browser(t)
	database := permissionDatabase(t)
	service := web08Service(t, database)
	const password = "web09-browser-password"
	owner, err := service.Register(t.Context(), "web09_owner", password)
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.Register(t.Context(), "web09_member", "web09-member-password")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	detail := web03Graph(t, database, owner, "브라우저 상세", now).ID
	var listed []string
	var deleteGraph model.Graph
	for index := range 3 {
		deleteGraph = web03Graph(t, database, owner, fmt.Sprintf("브라우저 목록 %d", index+1), now.Add(time.Duration(index+1)*time.Second))
		listed = append(listed, deleteGraph.Name)
	}
	viewed := web03Graph(t, database, member, "열람 그래프", now)
	web03Grant(t, database, viewed.ID, member, owner, store.GrantSubjectAccount, model.GraphGradeViewer)
	isolated := web05Context(t, database, detail, owner, model.LayerSource, "고립 원천", nil)
	source := web05Context(t, database, detail, owner, model.LayerSource, "근거 원천", nil)
	derived := web05Context(t, database, detail, owner, model.LayerDerived, "선택 파생", []model.ID{source.ID})
	descendant := web05Context(t, database, detail, owner, model.LayerDerived, "파생 후손", []model.ID{derived.ID})
	event := web05Context(t, database, detail, owner, model.LayerEvent, "사건 노드", []model.ID{source.ID})
	for _, deleted := range []bool{true, false} {
		if _, err := database.SetContextDeleted(t.Context(), detail, isolated.ID, owner, deleted, 0); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := database.RegisterAgent(t.Context(), owner, "first", "web09-registration-key")
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"graph_page":{"default":2,"maximum":3},"max_hops":0}}`, owner))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := web.New(permissionAuthentication{service}, database, web.Config{Connections: service, Plans: plans, SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	attacker := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = web09Attack.Execute(writer, map[string]string{"Action": server.URL + "/graphs/" + detail.String() + "/deletion", "Context": isolated.ID.String()})
	}))
	t.Cleanup(attacker.Close)

	// 오류 무시 대신 시험 인증서의 공개 키만 신뢰한다. 인증서 오류 상태의 응답은 브라우저가
	// 캐시하지 않아 자산 재검증을 관찰할 수 없다.
	spki := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	evidence := os.Getenv("WEB_BROWSER_EVIDENCE")
	if evidence == "" {
		evidence = t.TempDir()
	}
	config, err := json.Marshal(map[string]any{
		"chrome": chrome, "spki": base64.StdEncoding.EncodeToString(spki[:]), "profile": t.TempDir(), "evidence": evidence, "base": server.URL, "attacker": attacker.URL,
		"login": "web09_owner", "password": password, "memberLogin": "web09_member", "teamName": "브라우저 팀",
		"detailGraph": detail.String(), "listGraphs": listed, "viewerGraph": viewed.Name,
		"deleteGraph": map[string]string{"id": deleteGraph.ID.String(), "name": deleteGraph.Name}, "agent": agent.String(),
		"contexts": map[string]string{"isolated": isolated.ID.String(), "derived": derived.ID.String(), "descendant": descendant.ID.String(), "event": event.ID.String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), node, "browser_acceptance_test.cjs")
	command.Env = append(os.Environ(), "WEB09_CONFIG="+string(config))
	command.WaitDelay = 10 * time.Second
	output, err := command.CombinedOutput()
	t.Logf("브라우저 인수 출력 (증거 %s):\n%s", evidence, output)
	if err != nil {
		t.Fatalf("브라우저 인수 실패: %v", err)
	}
	if !strings.Contains(string(output), "WEB09 {") {
		t.Fatal("브라우저 인수 결과 요약이 없다")
	}

	// 브라우저 조작의 결과를 저장소 상태로 대조한다.
	if web05Stored(t, database, detail, event.ID).DeletedAt != nil || web05Graph(t, database, deleteGraph.ID).DeletedAt != nil {
		t.Fatal("브라우저에서 삭제 뒤 복구한 대상이 활성 상태가 아니다")
	}
	if web05Stored(t, database, detail, isolated.ID).DeletedAt != nil {
		t.Fatal("교차 출처 요청이 노드를 삭제했다")
	}
	teams, err := database.ListManagedTeams(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 1 || teams[0].Name != "브라우저 팀" || len(teams[0].MemberLoginIDs) != 1 || teams[0].MemberLoginIDs[0] != "web09_member" {
		t.Fatalf("브라우저에서 만든 팀 = %+v", teams)
	}
	if err := database.ValidateAgent(t.Context(), owner, "first", agent); err == nil {
		t.Fatal("브라우저에서 폐기한 에이전트 등록이 유효하다")
	}
	entries, _, err := database.ListAuditEntries(t.Context(), detail, owner, model.AuditFilter{Target: event.ID}, "", 50)
	if err != nil || len(entries) != 2 || entries[0].Action != "restore" || entries[1].Action != "delete" || entries[0].ActorID != owner {
		t.Fatalf("브라우저 삭제·복구 감사 = %+v, %v", entries, err)
	}
}
