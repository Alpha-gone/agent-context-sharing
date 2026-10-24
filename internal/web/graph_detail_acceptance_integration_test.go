package web_test

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

type web06Elements struct {
	Nodes []struct {
		Data struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"data"`
		Classes string `json:"classes"`
	} `json:"nodes"`
	Edges []struct {
		Data struct {
			Source string `json:"source"`
			Target string `json:"target"`
			Kind   string `json:"kind"`
		} `json:"data"`
		Classes string `json:"classes"`
	} `json:"edges"`
}

var web06Range = regexp.MustCompile(`id="hop-range" type="range" min="0" max="(\d+)" value="(\d+)"`)

// web06Detail은 그래프 상세의 시각화 데이터와 홉 슬라이더 상한을 읽는다.
func web06Detail(t *testing.T, html string) (web06Elements, int) {
	t.Helper()
	_, rest, found := strings.Cut(html, "const elements=")
	raw, _, ended := strings.Cut(rest, ";\n")
	if !found || !ended {
		t.Fatal("시각화 데이터가 없다")
	}
	var elements web06Elements
	if err := json.Unmarshal([]byte(raw), &elements); err != nil {
		t.Fatal(err)
	}
	match := web06Range.FindStringSubmatch(html)
	if match == nil || match[1] != match[2] {
		t.Fatal("홉 슬라이더의 상한과 초기값이 다르다")
	}
	maximum, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	// 모든 간선의 양 끝은 반환 노드여야 한다. 삭제 노드를 가리키는 선이 남으면 안 된다.
	nodes := map[string]bool{}
	for _, node := range elements.Nodes {
		nodes[node.Data.ID] = true
	}
	for _, edge := range elements.Edges {
		if !nodes[edge.Data.Source] || !nodes[edge.Data.Target] {
			t.Fatalf("반환 노드 밖을 가리키는 간선: %+v", edge.Data)
		}
	}
	return elements, maximum
}

func web06Has(elements web06Elements, id model.ID) bool {
	for _, node := range elements.Nodes {
		if node.Data.ID == id.String() {
			return true
		}
	}
	return false
}

// web06Event는 구성원 원천의 발생 시각을 포함하는 사건을 만든다.
func web06Event(t *testing.T, database *store.Store, graph, actor model.ID, member model.Context, body string) model.Context {
	t.Helper()
	at := member.Source.OccurredAt
	value := model.Context{ID: permissionID(t), GraphID: graph, Layer: model.LayerEvent, Body: body, RecordedAt: time.Now().UTC(), CreatedBy: actor, CreatedByAgent: actor, Version: 1,
		Event: &model.EventAttributes{MemberIDs: []model.ID{member.ID}, Start: at.Add(-time.Minute), End: new(at.Add(time.Minute))}}
	stored, err := database.CreateContext(t.Context(), graph, value, nil)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// TestWEB06GraphDetailIntegration은 실제 DB·서명 세션·TLS에서 시작점 선택, 한도 없음과
// 유한 홉·노드 상한의 슬라이더·절단 표시, 관계 종류 분류와 삭제 노드 제외를 확인한다.
func TestWEB06GraphDetailIntegration(t *testing.T) {
	database := permissionDatabase(t)
	owner, viewer := permissionAccount(t, database), permissionAccount(t, database)
	graph := web03Graph(t, database, owner, "상세 인수", time.Now().UTC()).ID
	web03Grant(t, database, graph, owner, viewer, store.GrantSubjectAccount, model.GraphGradeViewer)
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"max_hops":0,"max_hop_nodes":0},"%s":{"max_hops":0,"max_hop_nodes":2}}`, owner, viewer))
	if err != nil {
		t.Fatal(err)
	}
	ownerClient := web05Server(t, database, database, owner, plans)
	viewerClient := web05Server(t, database, database, viewer, plans)
	detail := "/graphs/" + graph.String()

	body := ownerClient.request(detail, nil, http.StatusOK, "")
	if elements, maximum := web06Detail(t, body); len(elements.Nodes) != 0 || maximum != 0 || !strings.Contains(body, "표시할 활성 컨텍스트가 없습니다.") || !strings.Contains(body, "활성 컨텍스트를 시작점으로") {
		t.Fatal("빈 그래프의 상세가 다르다")
	}

	first := web05Context(t, database, graph, owner, model.LayerSource, "web06 첫 원천", nil)
	second := web05Context(t, database, graph, owner, model.LayerSource, "web06 둘째 원천", nil)
	isolated := web05Context(t, database, graph, owner, model.LayerSource, "web06 고립 원천", nil)
	derived := web05Context(t, database, graph, owner, model.LayerDerived, "web06 파생", []model.ID{first.ID})
	// 첫 원천이 먼저 발생했으므로 앞 사건의 시작이 뒤 사건보다 늦지 않다.
	earlier := web06Event(t, database, graph, owner, first, "web06 앞 사건")
	later := web06Event(t, database, graph, owner, second, "web06 뒤 사건")
	confirmedAt := time.Now().UTC()
	if _, err := database.ConfirmRelation(t.Context(), graph, model.Relation{
		ID: permissionID(t), GraphID: graph, Type: model.RelationTypePrecedes, FromContextID: earlier.ID, ToContextID: later.ID,
		State: model.RelationStateConfirmed, ProposedBy: model.ProposalSourceAgent, ProposedAt: confirmedAt,
		ConfirmedBy: owner, ConfirmedByAgent: owner, ConfirmedAt: &confirmedAt,
	}, nil); err != nil {
		t.Fatal(err)
	}

	// 전역 요약이 없으면 활성 컨텍스트 전체가 시작점이다. 한도 없음의 슬라이더는 n-1이다.
	elements, maximum := web06Detail(t, ownerClient.request(detail, nil, http.StatusOK, ""))
	if len(elements.Nodes) != 6 || maximum != 5 || !web06Has(elements, isolated.ID) {
		t.Fatalf("활성 시작점 상세 = 노드 %d개, 슬라이더 %d", len(elements.Nodes), maximum)
	}
	kinds := map[string]string{}
	for _, edge := range elements.Edges {
		kinds[edge.Data.Kind] = edge.Classes
	}
	if kinds["derived_from"] != "reference" || kinds["has_member"] != "reference" || kinds["precedes"] != "relation" || len(elements.Edges) != 4 {
		t.Fatalf("간선 분류 = %v (%d개)", kinds, len(elements.Edges))
	}

	if _, err := database.SetContextDeleted(t.Context(), graph, second.ID, owner, true, 0); err != nil {
		t.Fatal(err)
	}
	elements, maximum = web06Detail(t, ownerClient.request(detail, nil, http.StatusOK, ""))
	if len(elements.Nodes) != 5 || maximum != 4 || web06Has(elements, second.ID) || len(elements.Edges) != 3 {
		t.Fatalf("삭제 뒤 상세 = 노드 %d개, 간선 %d개", len(elements.Nodes), len(elements.Edges))
	}

	summary := model.Context{ID: permissionID(t), GraphID: graph, Layer: model.LayerDerived, Body: "web06 전역 요약 <본문>", RecordedAt: time.Now().UTC(), CreatedBy: owner, CreatedByAgent: owner, Version: 1,
		Derived: &model.DerivedAttributes{Kind: model.DerivationKindSummary, SummaryScope: model.SummaryScopeGlobal, EvidenceState: model.EvidenceStateObservation}}
	if _, err := database.CreateContext(t.Context(), graph, summary, []model.ID{first.ID}); err != nil {
		t.Fatal(err)
	}
	body = ownerClient.request(detail, nil, http.StatusOK, "")
	elements, maximum = web06Detail(t, body)
	properties := body[strings.Index(body, "<h2>그래프 속성</h2>"):strings.Index(body, `id="context-graph"`)]
	if !strings.Contains(properties, "web06 전역 요약 &lt;본문&gt;") || strings.Contains(properties, "활성 컨텍스트를 시작점으로") {
		t.Fatal("전역 요약 영역이 다르다")
	}
	if !web06Has(elements, summary.ID) || !web06Has(elements, derived.ID) || web06Has(elements, isolated.ID) || !web06Has(elements, later.ID) || maximum != len(elements.Nodes)-1 {
		t.Fatalf("전역 요약 시작점 상세 = 노드 %d개, 슬라이더 %d", len(elements.Nodes), maximum)
	}

	body = viewerClient.request(detail, nil, http.StatusOK, "")
	elements, maximum = web06Detail(t, body)
	if len(elements.Nodes) != 2 || maximum != 1 || !strings.Contains(body, "홉 경계에서 잘렸습니다. 표시된 범위가 그래프 전체가 아닙니다.") {
		t.Fatalf("노드 상한 절단 = 노드 %d개, 슬라이더 %d", len(elements.Nodes), maximum)
	}
	finite, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"max_hops":1}}`, viewer))
	if err != nil {
		t.Fatal(err)
	}
	if _, maximum := web06Detail(t, web05Server(t, database, database, viewer, finite).request(detail, nil, http.StatusOK, "")); maximum != 1 {
		t.Fatalf("유한 홉 슬라이더 = %d", maximum)
	}
	if strings.Contains(ownerClient.request(detail+"/access", nil, http.StatusOK, ""), "cytoscape.min.js") {
		t.Fatal("상세 밖 화면이 시각화 자산을 읽었다")
	}

	ownerClient.request(detail+"/deletion", url.Values{"action": {"delete"}, "graph_name": {"상세 인수"}}, http.StatusSeeOther, "/graphs")
	ownerClient.request(detail, nil, http.StatusNotFound, "")
	web05Server(t, database, database, permissionAccount(t, database), plans).request(detail, nil, http.StatusNotFound, "")
}
