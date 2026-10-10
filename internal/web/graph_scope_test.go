package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// TestGraphDetailScopeControls는 국소 보기에서도 복귀와 범위 밖 노드 선택 수단이
// 남아 있는지 확인하고, 실제 렌더링된 스크립트로 시각화 상태 전이를 검사한다.
func TestGraphDetailScopeControls(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "disconnected"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			accountID, graphID := testID(t), testID(t)
			graphs := &scopeGraphStore{fakeGraphStore: &fakeGraphStore{accountID: accountID, graphID: graphID}}
			if !empty {
				for _, body := range []string{"고립 노드", "근거 원천", "선택 파생", "파생 후손", "사건 노드"} {
					graphs.result.Contexts = append(graphs.result.Contexts, model.Context{
						ID: testID(t), GraphID: graphID, Layer: model.LayerSource, Body: body,
					})
				}
				graphs.result.Contexts[2].Layer = model.LayerDerived
				graphs.result.Contexts[3].Layer = model.LayerDerived
				graphs.result.Contexts[4].Layer = model.LayerEvent
				nodes := graphs.result.Contexts
				graphs.result.Edges = []store.HopEdge{
					{FromID: nodes[2].ID, ToID: nodes[1].ID, Kind: "derived_from"},
					{FromID: nodes[3].ID, ToID: nodes[2].ID, Kind: "derived_from"},
					{FromID: nodes[4].ID, ToID: nodes[1].ID, Kind: "member_refs"},
				}
			}
			server, err := New(fakeAuthentication{accountID: accountID}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graphID.String(), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("그래프 상세 응답 = %d", response.Code)
			}
			html := response.Body.String()
			for _, markup := range []string{
				`id="full-view" type="button">전체 보기</button>`,
				`<label for="hop-range">`,
				`id="scope-status" role="status" aria-atomic="true">전체 보기</p>`,
				`id="node-details-heading" tabindex="-1"`,
				`id="details-link" href="#node-details-heading"`,
				`id="node-evidence"`,
				"빈 영역을 누르거나 Esc를 누르면 전체 보기로 돌아갑니다.",
			} {
				if !strings.Contains(html, markup) {
					t.Errorf("국소 보기 조작 수단이 없다: %s", markup)
				}
			}
			for _, node := range graphs.result.Contexts {
				if !strings.Contains(html, `type="button" data-context-id="`+node.ID.String()+`" aria-label="`+node.ID.String()+` 노드 선택" aria-pressed="false" aria-controls="node-details node-evidence" aria-describedby="scope-help"`) {
					t.Errorf("목록에 노드 %s의 선택 수단이 없다", node.ID)
				}
			}
			t.Run("javascript", func(t *testing.T) {
				node, err := exec.LookPath("node")
				if err != nil {
					t.Skip("Node.js가 없어 Cytoscape 상태 전이 시험을 실행하지 못했다")
				}
				command := exec.CommandContext(t.Context(), node, "graph_scope_test.cjs")
				command.Stdin = strings.NewReader(html)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("Cytoscape 상태 전이: %v\n%s", err, output)
				}
				t.Log(string(output))
			})
		})
	}
}

type scopeGraphStore struct {
	*fakeGraphStore
	result store.HopResult
}

func (graphs *scopeGraphStore) GraphVisualization(context.Context, model.ID, int, int) (store.HopResult, error) {
	return graphs.result, nil
}
