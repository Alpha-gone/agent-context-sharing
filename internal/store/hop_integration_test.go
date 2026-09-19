package store

import (
	"testing"

	"agent_context_sharing/internal/model"
)

// TestHopDirectionsKeepEdgeOrientationIntegration은 세 탐색 방향이 같은 간선을 같은
// 방향으로 돌려주는지 확인한다. 양방향은 무방향 패턴 한 질의로 묻고 간선의 방향을 기준
// 정점이 간선의 시작인지로 판정하므로, 방향마다 질의를 나누던 때와 결과가 같아야 한다.
func TestHopDirectionsKeepEdgeOrientationIntegration(t *testing.T) {
	store := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, store, actorID)
	graphID := createTestGraph(t, store, actorID)

	source, err := store.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "api://hop-direction/"+newTestID(t).String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	derived, err := store.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}

	// 간선은 파생에서 근거로 나간다. 근거에서 보면 역방향, 파생에서 보면 정방향이다.
	cases := []struct {
		name      string
		startID   model.ID
		direction string
		wantID    model.ID
	}{
		{name: "근거에서 역방향", startID: source.ID, direction: "in", wantID: derived.ID},
		{name: "근거에서 양방향", startID: source.ID, direction: "both", wantID: derived.ID},
		{name: "파생에서 정방향", startID: derived.ID, direction: "out", wantID: source.ID},
		{name: "파생에서 양방향", startID: derived.ID, direction: "both", wantID: source.ID},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := store.HopContexts(t.Context(), graphID, testCase.startID, 1, testCase.direction, []string{"derived_from"}, 10)
			if err != nil {
				t.Fatalf("홉 조회: %v", err)
			}
			if len(result.Contexts) != 2 || result.Distances[testCase.wantID] != 1 {
				t.Fatalf("확장 결과 = %d개, 거리 %#v", len(result.Contexts), result.Distances)
			}
			if len(result.Edges) != 1 {
				t.Fatalf("간선 수 = %d개", len(result.Edges))
			}
			edge := result.Edges[0]
			if edge.FromID != derived.ID || edge.ToID != source.ID || edge.Kind != "derived_from" {
				t.Fatalf("간선 방향이 뒤집혔다: %#v", edge)
			}
		})
	}

	// 반대쪽 방향에서는 같은 간선이 걸리지 않아야 한다.
	for _, testCase := range []struct {
		name      string
		startID   model.ID
		direction string
	}{
		{name: "근거에서 정방향", startID: source.ID, direction: "out"},
		{name: "파생에서 역방향", startID: derived.ID, direction: "in"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := store.HopContexts(t.Context(), graphID, testCase.startID, 1, testCase.direction, []string{"derived_from"}, 10)
			if err != nil {
				t.Fatalf("홉 조회: %v", err)
			}
			if len(result.Contexts) != 1 || len(result.Edges) != 0 {
				t.Fatalf("반대 방향에서 확장됐다: %d개 노드, %d개 간선", len(result.Contexts), len(result.Edges))
			}
		})
	}
}
