package store

import (
	"reflect"
	"testing"
	"time"
)

// invariantFixture는 규칙을 모두 만족하는 작은 그래프다. 원천 둘, 둘을 근거로 한 파생,
// 파생을 대체한 파생, 원천을 구성원으로 한 사건 둘과 확정 precedes 하나다.
func invariantFixture() auditSnapshot {
	const graph = "g"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	later := start.Add(time.Hour)
	contexts := map[string]auditContext{
		"s1": {id: "s1", graphID: graph, layer: "source"},
		"s2": {id: "s2", graphID: graph, layer: "source"},
		"d1": {id: "d1", graphID: graph, layer: "derived"},
		"d2": {id: "d2", graphID: graph, layer: "derived"},
		"e1": {id: "e1", graphID: graph, layer: "event", start: start, hasTime: true},
		"e2": {id: "e2", graphID: graph, layer: "event", start: later, hasTime: true},
	}
	edges := []auditEdge{
		{label: "DERIVED_FROM", graphID: graph, from: "d1", to: "s1"},
		{label: "DERIVED_FROM", graphID: graph, from: "d1", to: "s2"},
		{label: "DERIVED_FROM", graphID: graph, from: "d2", to: "s1"},
		{label: "SUPERSEDES", graphID: graph, from: "d2", to: "d1"},
		{label: "HAS_MEMBER", graphID: graph, from: "e1", to: "s1"},
		{label: "HAS_MEMBER", graphID: graph, from: "e2", to: "s2"},
		{label: "PRECEDES", graphID: graph, relationID: "r1", state: "confirmed", from: "e1", to: "e2"},
	}
	snapshot := auditSnapshot{graphID: graph, contexts: contexts, edges: edges,
		embeddings: []auditEmbedding{{contextID: "s1", graphID: graph, modelID: "m:vector:4", dimension: 4}},
		expect:     EmbeddingExpectation{ModelID: "m:vector:4", Dimension: 4}}
	snapshot.pathExists = memoryPaths(snapshot.edges)
	return snapshot
}

func TestInvariantRulesAcceptValidGraph(t *testing.T) {
	violations, err := runInvariantRules(invariantFixture())
	if err != nil || len(violations) != 0 {
		t.Fatalf("정상 표본 위반 = %+v, %v", violations, err)
	}
}

// TestInvariantRulesReportSingleViolation은 규칙마다 한 규칙만 깨진 표본을 만들어 그 규칙과
// 위치만 보고되는지 확인한다.
func TestInvariantRulesReportSingleViolation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*auditSnapshot)
		want   InvariantViolation
	}{
		"다른 그래프의 간선": {func(s *auditSnapshot) { s.edges[4].graphID = "other" },
			InvariantViolation{Rule: RuleGraphMembership, GraphID: "g", Target: TargetReference, Label: "has_member", IDs: []string{"e1", "s1"}}},
		"사건을 근거로 둔 파생": {func(s *auditSnapshot) { s.edges[2].to = "e1" },
			InvariantViolation{Rule: RuleEndpointLayer, GraphID: "g", Target: TargetReference, Label: "derived_from", IDs: []string{"d2", "e1"}}},
		"근거 없는 파생": {func(s *auditSnapshot) {
			s.contexts["d3"] = auditContext{id: "d3", graphID: "g", layer: "derived"}
		}, InvariantViolation{Rule: RuleReferenceCardinality, GraphID: "g", Target: TargetContext, IDs: []string{"d3"}}},
		"역방향 relates_to": {func(s *auditSnapshot) {
			s.edges = append(s.edges, auditEdge{label: "RELATES_TO", graphID: "g", relationID: "r2", state: "confirmed", from: "e2", to: "e1"})
		}, InvariantViolation{Rule: RuleRelationIdentity, GraphID: "g", Target: TargetRelation, Label: "relates_to", IDs: []string{"r2", "e2", "e1"}}},
		"확정 순환": {func(s *auditSnapshot) {
			s.edges = append(s.edges, auditEdge{label: "CAUSES", graphID: "g", relationID: "r3", state: "confirmed", from: "e1", to: "e1"})
		}, InvariantViolation{Rule: RuleRelationAcyclic, GraphID: "g", Target: TargetRelation, Label: "causes", IDs: []string{"r3", "e1", "e1"}}},
		"시간을 거스르는 precedes": {func(s *auditSnapshot) { s.edges[6].from, s.edges[6].to = "e2", "e1" },
			InvariantViolation{Rule: RuleRelationTime, GraphID: "g", Target: TargetRelation, Label: "precedes", IDs: []string{"r1", "e2", "e1"}}},
		"다른 모델의 임베딩": {func(s *auditSnapshot) { s.embeddings[0].modelID = "old:vector:4" },
			InvariantViolation{Rule: RuleEmbeddingLink, GraphID: "g", Target: TargetEmbedding, IDs: []string{"s1"}}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := invariantFixture()
			test.mutate(&snapshot)
			snapshot.pathExists = memoryPaths(snapshot.edges)
			violations, err := runInvariantRules(snapshot)
			if err != nil {
				t.Fatalf("규칙 실행: %v", err)
			}
			if !reflect.DeepEqual(violations, []InvariantViolation{test.want}) {
				t.Fatalf("위반 = %+v, want %+v", violations, test.want)
			}
		})
	}
}

// TestInvariantRulesLimitContextRulesToScope는 쓰기 검사에서 범위 밖 이웃의 기수성을 판정하지
// 않는지 확인한다. 이웃의 간선은 일부만 읽었으므로 판정하면 거짓 위반이 난다.
func TestInvariantRulesLimitContextRulesToScope(t *testing.T) {
	snapshot := invariantFixture()
	snapshot.scope = map[string]struct{}{"s1": {}}
	snapshot.edges = snapshot.edges[:1]
	violations, err := runInvariantRules(snapshot)
	if err != nil || len(violations) != 0 {
		t.Fatalf("범위 밖 이웃을 판정했다: %+v, %v", violations, err)
	}
}
