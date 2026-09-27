package main

import (
	"math"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

func structureID(t *testing.T, value string) model.ID {
	t.Helper()
	id, err := model.ParseID(value)
	if err != nil {
		t.Fatalf("식별자 해석: %v", err)
	}
	return id
}

func structureContext(id model.ID, layer model.Layer, degree int, channels ...string) search.Context {
	return search.Context{Value: model.Context{ID: id, Layer: layer}, MatchedChannels: channels, CandidateDegree: degree}
}

// TestMeasureStructure는 필수 근거·기대 경로가 끊긴 응답에서 다섯 구조 지표를 확인한다.
// 경로 a-b-c 중 b-c 간선이 응답에 없고, 고립된 d는 진입점에 닿지 못한다.
func TestMeasureStructure(t *testing.T) {
	a := structureID(t, "019a0000-0000-7000-8000-000000000201")
	b := structureID(t, "019a0000-0000-7000-8000-000000000202")
	c := structureID(t, "019a0000-0000-7000-8000-000000000203")
	d := structureID(t, "019a0000-0000-7000-8000-000000000204")
	keys := map[string]model.ID{"a": a, "b": b, "c": c, "d": d}
	flow := search.Flow{
		Contexts: []search.Context{
			structureContext(a, model.LayerEvent, 1, "graph"),
			structureContext(b, model.LayerEvent, 2, "graph"),
			structureContext(c, model.LayerEvent, 1, "keyword"),
			structureContext(d, model.LayerEvent, 0, "graph"),
		},
		Edges:     []store.HopEdge{{FromID: b, ToID: a, Kind: "precedes"}},
		Channels:  map[string]search.Channel{"keyword": {Candidates: 3}, "graph": {Candidates: 5}},
		Selection: search.Selection{Candidates: 6},
	}
	query := querySpec{Required: []string{"a", "c"}, Paths: [][]string{{"a", "b", "c"}}}
	got := measureStructure(flow, query, keys)
	// 필수 컨텍스트 둘과 관계 a-b는 있고 관계 b-c는 없다.
	if got.completeness != 0.75 {
		t.Errorf("근거 완전성 = %v, want 0.75", got.completeness)
	}
	if got.continuity != 0 {
		t.Errorf("경로 연속성 = %v, want 0", got.continuity)
	}
	// 비진입 a·b·d 모두 진입점 c에 닿지 못한다.
	if got.disconnected != 1 {
		t.Errorf("단절 근거 비율 = %v, want 1", got.disconnected)
	}
	// 차수 1,2,1,0의 지니 계수는 차이 합 12 / (2·4·4) = 0.375다.
	if math.Abs(got.hub-0.375) > 1e-9 {
		t.Errorf("허브 편중 = %v, want 0.375", got.hub)
	}
	if got.duplicate != 0.25 {
		t.Errorf("중복 컨텍스트 비율 = %v, want 0.25", got.duplicate)
	}

	flow.Edges = append(flow.Edges, store.HopEdge{FromID: b, ToID: c, Kind: "part_of"})
	got = measureStructure(flow, query, keys)
	if got.completeness != 1 || got.continuity != 1 || math.Abs(got.disconnected-1.0/3) > 1e-9 {
		t.Errorf("이어진 경로 = %+v", got)
	}
}

func TestMeasureStructureLeavesUndefinedEvidence(t *testing.T) {
	got := measureStructure(search.Flow{}, querySpec{}, nil)
	if got.completeness != undefinedMetric || got.continuity != undefinedMetric {
		t.Fatalf("기대 근거 없는 질의 = %+v", got)
	}
	if got.disconnected != 0 || got.hub != 0 || got.duplicate != 0 {
		t.Fatalf("빈 응답의 분모 0 지표 = %+v", got)
	}
}

func TestStageConfigAddsEvidenceSelectionToGlobal(t *testing.T) {
	stages, err := parseStages("global,evidence")
	if err != nil || len(stages) != 2 {
		t.Fatalf("단계 해석 = %v, %v", stages, err)
	}
	if stage, evidence := stageConfig(stageEvidence); stage != search.GraphStageGlobal || !evidence {
		t.Fatalf("근거 경로 단계 = %q %v", stage, evidence)
	}
	if stage, evidence := stageConfig("global"); stage != search.GraphStageGlobal || evidence {
		t.Fatalf("전역 단계 = %q %v", stage, evidence)
	}
}

func TestMarginalsCountRecoveredAndCost(t *testing.T) {
	run := func(stage string, chars int, latency float64, recovered ...string) runMetrics {
		found := map[string]struct{}{}
		for _, key := range recovered {
			found[key] = struct{}{}
		}
		return runMetrics{Stage: stage, QuerySamples: []queryMetrics{{ID: "q", UseCase: useCaseAssociative, BudgetUsed: chars, LatencyMS: latency}}, recovered: map[string]map[string]struct{}{"q": found}}
	}
	runs := map[string][]runMetrics{
		"global":   {run("global", 100, 10, "a", "b")},
		"evidence": {run("evidence", 160, 14, "a", "c", "d")},
	}
	got := marginals([]string{"global", "evidence"}, runs)
	if len(got) != 1 {
		t.Fatalf("한계 기여 = %+v", got)
	}
	if entry := got[0]; entry.Recovered != 2 || entry.Lost != 1 || entry.AddedChars != 60 || entry.AddedLatencyMS != 4 || entry.UseCase != useCaseAssociative {
		t.Fatalf("한계 기여 = %+v", entry)
	}
}

func TestLoadQuerySetRejectsInvalidEvidence(t *testing.T) {
	contexts, err := loadContextSet(writeFile(t, "contexts.json", validContexts))
	if err != nil {
		t.Fatalf("컨텍스트 집합 읽기: %v", err)
	}
	for name, body := range map[string]string{
		"없는 필수 근거": `{"version":"q-1","queries":[{"id":"q1","use_case":"fact","work_context":"질의","answers":["s1"],"required":["없음"]}]}`,
		"한 노드 경로":  `{"version":"q-1","queries":[{"id":"q1","use_case":"fact","work_context":"질의","answers":["s1"],"paths":[["s1"]]}]}`,
		"없는 경로 노드": `{"version":"q-1","queries":[{"id":"q1","use_case":"fact","work_context":"질의","answers":["s1"],"paths":[["s1","없음"]]}]}`,
	} {
		if _, err := loadQuerySet(writeFile(t, "bad.json", body), contexts); err == nil {
			t.Errorf("%s을 받아들였다", name)
		}
	}
}
