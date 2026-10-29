package web

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// web06Store는 그래프 상세의 그래프 메타데이터와 시각화 결과를 고정한다.
type web06Store struct {
	*fakeGraphStore
	graph  model.Graph
	result store.HopResult
	err    error
	cancel context.CancelFunc
}

func (s *web06Store) Graph(_ context.Context, graphID model.ID) (model.Graph, error) {
	graph := s.graph
	graph.ID = graphID
	return graph, nil
}
func (s *web06Store) GraphVisualization(context.Context, model.ID, int, int) (store.HopResult, error) {
	if s.cancel != nil {
		s.cancel()
	}
	return s.result, s.err
}

// web06Elements는 렌더링된 스크립트에서 서버가 만든 시각화 데이터를 읽는다.
func web06Elements(t *testing.T, html string) visualizationData {
	t.Helper()
	_, rest, found := strings.Cut(html, "const elements=")
	raw, _, ended := strings.Cut(rest, ";\n")
	if !found || !ended {
		t.Fatal("시각화 데이터가 없다")
	}
	var elements visualizationData
	if err := json.Unmarshal([]byte(raw), &elements); err != nil {
		t.Fatal(err)
	}
	return elements
}

func web06Detail(t *testing.T, backend *web06Store) string {
	t.Helper()
	server, err := New(fakeAuthentication{accountID: backend.accountID}, backend, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+backend.graphID.String(), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("그래프 상세 = %d", response.Code)
	}
	return response.Body.String()
}

// TestWEB06GraphDetailLayout은 왼쪽 그래프 속성·전역 요약, 가운데 시각화, 오른쪽 선택
// 상세 순서와 전역 요약이 없을 때의 시작점 안내를 확인한다.
func TestWEB06GraphDetailLayout(t *testing.T) {
	for _, summary := range []bool{true, false} {
		t.Run(fmt.Sprintf("summary=%t", summary), func(t *testing.T) {
			account, graph := testID(t), testID(t)
			created := time.Date(2026, 10, 1, 2, 3, 4, 0, time.UTC)
			source := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerSource, Body: "원천 본문"}
			global := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerDerived, Body: "<전역 요약 본문>", Derived: &model.DerivedAttributes{Kind: model.DerivationKindSummary, SummaryScope: model.SummaryScopeGlobal}}
			local := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerDerived, Body: "국소 요약 본문", Derived: &model.DerivedAttributes{Kind: model.DerivationKindSummary, SummaryScope: "local"}}
			contexts := []model.Context{source, local}
			edges := []store.HopEdge{{FromID: local.ID, ToID: source.ID, Kind: "derived_from"}}
			if summary {
				contexts = append(contexts, global)
				edges = append(edges, store.HopEdge{FromID: global.ID, ToID: source.ID, Kind: "derived_from"})
			}
			backend := &web06Store{
				fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph},
				graph:          model.Graph{Name: "상세 그래프", CreatedAt: created, LastActivityAt: created.Add(time.Hour)},
				result:         store.HopResult{Contexts: contexts, Edges: edges},
			}
			html := web06Detail(t, backend)
			left := strings.Index(html, "<h2>그래프 속성</h2>")
			center := strings.Index(html, `id="context-graph"`)
			right := strings.Index(html, `id="node-details-heading"`)
			if left < 0 || left > center || center > right {
				t.Fatal("그래프 속성·시각화·선택 상세의 배치 순서가 다르다")
			}
			properties := html[left:center]
			for _, want := range []string{graph.String(), "<dd>없음</dd>", "2026-10-01T02:03:04Z", "2026-10-01T03:03:04Z", fmt.Sprintf("노드 %d개 · 연결 %d개", len(contexts), len(edges))} {
				if !strings.Contains(properties, want) {
					t.Fatalf("그래프 속성에 %q가 없다", want)
				}
			}
			if strings.Contains(properties, "국소 요약 본문") {
				t.Fatal("전역이 아닌 요약을 전역 요약으로 표시했다")
			}
			if summary != strings.Contains(properties, "&lt;전역 요약 본문&gt;") || summary == strings.Contains(properties, "활성 컨텍스트를 시작점으로") {
				t.Fatal("전역 요약 또는 시작점 안내가 다르다")
			}
		})
	}
}

// TestWEB06VisualizationClasses는 참조와 사건 관계, 계층과 상태 표식을 서버 데이터의
// 클래스로 구분하고 범례가 같은 표현을 설명하는지 확인한다.
func TestWEB06VisualizationClasses(t *testing.T) {
	account, graph := testID(t), testID(t)
	source := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerSource, Body: "원천"}
	derived := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerDerived, Body: "파생", Derived: &model.DerivedAttributes{EvidenceInvalidated: true, ConfidenceState: model.ConfidenceStateDisputed}}
	first := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerEvent, Body: "앞 사건"}
	second := model.Context{ID: testID(t), GraphID: graph, Layer: model.LayerEvent, Body: "뒤 사건"}
	kinds := []string{"derived_from", "supersedes", "has_member", "precedes", "causes", "part_of", "relates_to"}
	var edges []store.HopEdge
	for index, kind := range kinds {
		from, to := derived.ID, source.ID
		if index >= 2 {
			from, to = first.ID, second.ID
		}
		if kind == "has_member" {
			to = source.ID
		}
		edges = append(edges, store.HopEdge{FromID: from, ToID: to, Kind: kind})
	}
	backend := &web06Store{fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph}, graph: model.Graph{Name: "분류"}, result: store.HopResult{Contexts: []model.Context{source, derived, first, second}, Edges: edges}}
	html := web06Detail(t, backend)
	elements := web06Elements(t, html)
	for index, edge := range elements.Edges {
		want := "reference"
		if index >= 3 {
			want = "relation"
		}
		if edge.Data.Kind != kinds[index] || edge.Classes != want {
			t.Fatalf("%s 간선 분류 = %q, want %q", edge.Data.Kind, edge.Classes, want)
		}
	}
	if elements.Nodes[1].Classes != "derived evidence-invalidated disputed" || elements.Nodes[0].Classes != "source" || elements.Nodes[2].Classes != "event" {
		t.Fatalf("노드 표식 = %+v", elements.Nodes)
	}
	for _, want := range []string{`{selector:'edge.relation',style:{'line-style':'dashed'}}`, "참조 · 실선 화살표", "사건 관계 · 점선 화살표", "근거 경로 · 굵은 빨간 선", "marks.push('상충')", "marks.push('근거 무효')"} {
		if !strings.Contains(html, want) {
			t.Fatalf("시각 표현 또는 범례에 %q가 없다", want)
		}
	}
}

// TestWEB06VisualizationReadBoundary는 시각화 조회 장애를 고정 500으로, 취소를 무응답으로
// 끝내는지 확인한다.
func TestWEB06VisualizationReadBoundary(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			account, graph := testID(t), testID(t)
			backend := &web06Store{fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph}, err: errors.New("private visualization detail")}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				backend.cancel = cancel
			}
			server, err := New(fakeAuthentication{accountID: account}, backend, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graph.String(), nil).WithContext(ctx))
			if strings.Contains(response.Body.String(), "private visualization") || canceled && response.Body.Len() != 0 || !canceled && response.Code != http.StatusInternalServerError {
				t.Fatalf("시각화 조회 경계 = %d", response.Code)
			}
		})
	}
}

// TestWEB07SelectionAccessibilityMarkup은 키보드 선택 수단이 기본 버튼이고, 선택이 포커스를
// 강제로 옮기거나 양의 tabindex·캔버스용 위젯 역할을 추가하지 않는지 확인한다.
func TestWEB07SelectionAccessibilityMarkup(t *testing.T) {
	account, graph := testID(t), testID(t)
	nodes := []model.Context{
		{ID: testID(t), GraphID: graph, Layer: model.LayerSource, Body: "원천"},
		{ID: testID(t), GraphID: graph, Layer: model.LayerDerived, Body: "파생"},
	}
	backend := &web06Store{fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph}, graph: model.Graph{Name: "접근성"}, result: store.HopResult{Contexts: nodes, Edges: []store.HopEdge{{FromID: nodes[1].ID, ToID: nodes[0].ID, Kind: "derived_from"}}}}
	html := web06Detail(t, backend)
	script := html[strings.Index(html, "const elements="):]
	if strings.Count(html, `<span class="node-selection-mark" aria-hidden="true">✓ 선택됨</span>`) != len(nodes) {
		t.Fatal("선택 표식은 노드별로 제공하고 접근성 이름과 분리해야 한다")
	}
	if regexp.MustCompile(`tabindex="[1-9]`).MatchString(html) || regexp.MustCompile(`role="(application|grid|tab|tablist|listbox|option|button)"`).MatchString(html) {
		t.Fatal("양의 tabindex 또는 불필요한 위젯 역할이 있다")
	}
	if strings.Contains(script, ".focus(") || strings.Count(html, `role="status"`) != 1 {
		t.Fatal("선택이 포커스를 옮기거나 상태 안내 영역이 하나가 아니다")
	}
	for _, node := range nodes {
		button := `<button type="button" data-context-id="` + node.ID.String() + `" aria-label="` + node.ID.String() + ` 노드 선택" aria-pressed="false" aria-controls="node-details node-evidence"`
		if !strings.Contains(html, button) {
			t.Fatalf("노드 %s의 기본 선택 버튼이 없다", node.ID)
		}
	}
	// 버튼 이름은 선택 전후에 바꾸지 않고 상태는 aria-pressed만 바꾼다.
	if strings.Contains(script, "setAttribute('aria-label'") || strings.Contains(script, ".textContent='노드") || !strings.Contains(script, "setAttribute('aria-pressed'") {
		t.Fatal("선택 버튼의 이름 또는 상태 갱신 방식이 다르다")
	}
	for _, want := range []string{`<a id="details-link" href="#node-details-heading">`, `<h2 id="node-details-heading" tabindex="-1">`, "event.key==='Escape'"} {
		if !strings.Contains(html, want) {
			t.Fatalf("키보드 경로 %q가 없다", want)
		}
	}
}
