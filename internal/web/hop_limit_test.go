package web

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

// TestGraphDetailHopLimits는 한도 없음과 0홉을 구분하고 슬라이더에는 유한한 범위를
// 주는지 확인한다. 시작점이 여럿이면 모든 노드의 조회 거리가 0이어도 간선은 존재한다.
func TestGraphDetailHopLimits(t *testing.T) {
	for _, test := range []struct {
		name         string
		override     string
		nodes        int
		allStarts    bool
		wantDepth    int
		wantLimit    int
		wantNodes    int
		wantRange    int
		wantBoundary string
	}{
		{name: "default", nodes: 4, wantDepth: plan.Default().MaxHops, wantLimit: plan.Default().MaxHopNodes, wantNodes: 4, wantRange: plan.Default().MaxHops},
		{name: "finite", override: `"max_hops":1`, nodes: 4, wantDepth: 1, wantLimit: plan.Default().MaxHopNodes, wantNodes: 2, wantRange: 1},
		{name: "unlimited", override: `"max_hops":0`, nodes: 4, wantDepth: math.MaxInt, wantLimit: plan.Default().MaxHopNodes, wantNodes: 4, wantRange: 3},
		{name: "unlimited_multiple_starts", override: `"max_hops":0`, nodes: 4, allStarts: true, wantDepth: math.MaxInt, wantLimit: plan.Default().MaxHopNodes, wantNodes: 4, wantRange: 3},
		{name: "unlimited_capped_nodes", override: `"max_hops":0,"max_hop_nodes":2`, nodes: 4, wantDepth: math.MaxInt, wantLimit: 2, wantNodes: 2, wantRange: 1, wantBoundary: "2홉 경계에서 잘렸습니다"},
		{name: "unlimited_nodes", override: `"max_hops":0,"max_hop_nodes":0`, nodes: 4, wantDepth: math.MaxInt, wantLimit: 0, wantNodes: 4, wantRange: 3},
		{name: "empty", override: `"max_hops":0`, wantDepth: math.MaxInt, wantLimit: plan.Default().MaxHopNodes},
		{name: "single", override: `"max_hops":0`, nodes: 1, wantDepth: math.MaxInt, wantLimit: plan.Default().MaxHopNodes, wantNodes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			accountID, graphID := testID(t), testID(t)
			plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{%s}}`, accountID, test.override))
			if err != nil {
				t.Fatalf("계정 플랜: %v", err)
			}
			graphs := &hopLimitGraphStore{
				fakeGraphStore: &fakeGraphStore{accountID: accountID, graphID: graphID},
				allStarts:      test.allStarts,
			}
			for range test.nodes {
				graphs.nodes = append(graphs.nodes, model.Context{ID: testID(t), GraphID: graphID, Layer: model.LayerSource, Body: "홉 범위 표본"})
			}
			server, err := New(fakeAuthentication{accountID: accountID}, graphs, Config{
				SecureCookie: func(*http.Request) bool { return true }, Plans: plans,
			})
			if err != nil {
				t.Fatalf("웹 서버 생성: %v", err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+graphID.String(), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("그래프 상세 응답 = %d", response.Code)
			}
			if graphs.depth != test.wantDepth || graphs.limit != test.wantLimit {
				t.Errorf("조회 깊이·노드 상한 = %d·%d, want %d·%d", graphs.depth, graphs.limit, test.wantDepth, test.wantLimit)
			}
			body := response.Body.String()
			slider := fmt.Sprintf(`id="hop-range" type="range" min="0" max="%d" value="%d"`, test.wantRange, test.wantRange)
			if !strings.Contains(body, slider) {
				t.Errorf("홉 슬라이더에 예상 범위가 없다: %s", slider)
			}
			for index, node := range graphs.nodes {
				if displayed := strings.Contains(body, node.ID.String()); displayed != (index < test.wantNodes) {
					t.Errorf("노드 %d 표시 = %v, want %v", index, displayed, index < test.wantNodes)
				}
			}
			if edges := strings.Count(body, `"source":`); edges != max(0, test.wantNodes-1) {
				t.Errorf("시각화 간선 수 = %d, want %d", edges, max(0, test.wantNodes-1))
			}
			if test.wantBoundary != "" && !strings.Contains(body, test.wantBoundary) {
				t.Error("노드 상한의 절단 경계 표시가 없다")
			}
		})
	}
}

// hopLimitGraphStore는 조회 깊이와 노드 상한을 기록하고 사슬 그래프의 부분 결과를 준다.
// 0홉에서는 간선을 주지 않아 실제 홉 조회처럼 시작점만 화면에 남는 회귀를 재현한다.
type hopLimitGraphStore struct {
	*fakeGraphStore
	nodes     []model.Context
	allStarts bool
	depth     int
	limit     int
}

func (graphs *hopLimitGraphStore) GraphVisualization(_ context.Context, _ model.ID, depth, limit int) (store.HopResult, error) {
	graphs.depth, graphs.limit = depth, limit
	result := store.HopResult{Distances: make(map[model.ID]int)}
	for index, node := range graphs.nodes {
		distance := index
		if graphs.allStarts {
			distance = 0
		}
		if distance > depth {
			break
		}
		if limit > 0 && len(result.Contexts) == limit {
			result.Truncated, result.Boundary = true, distance
			break
		}
		result.Contexts = append(result.Contexts, node)
		result.Distances[node.ID] = distance
		if depth > 0 && index > 0 {
			result.Edges = append(result.Edges, store.HopEdge{FromID: graphs.nodes[index-1].ID, ToID: node.ID, Kind: "derived_from"})
		}
	}
	return result, nil
}
