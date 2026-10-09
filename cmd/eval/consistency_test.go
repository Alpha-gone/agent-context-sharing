package main

import (
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

// TestMixedStatesFlagsGraphOnlyContext는 그래프 경로 채널만 낸 컨텍스트를 두 상태가 섞인
// 응답으로 판정하는지 확인한다. 진입 채널과 함께 나온 컨텍스트는 판정하지 않는다.
func TestMixedStatesFlagsGraphOnlyContext(t *testing.T) {
	consistent := search.Flow{Contexts: []search.Context{{MatchedChannels: []string{"time"}}, {MatchedChannels: []string{"graph", "time"}}}}
	if mixedStates(consistent) {
		t.Fatal("진입 채널이 함께 낸 컨텍스트를 섞인 상태로 봤다")
	}
	mixed := search.Flow{Contexts: []search.Context{{MatchedChannels: []string{"time"}}, {MatchedChannels: []string{"graph"}}}}
	if !mixedStates(mixed) {
		t.Fatal("그래프 경로 채널만 낸 컨텍스트를 놓쳤다")
	}
}

// TestFlowSignatureIgnoresMeasurementsOnly는 측정값만 다르면 같은 서명이고 응답 구성 요소가
// 다르면 다른 서명인지 확인한다.
func TestFlowSignatureIgnoresMeasurementsOnly(t *testing.T) {
	first, err := model.NewID()
	if err != nil {
		t.Fatalf("식별자 생성: %v", err)
	}
	second, err := model.NewID()
	if err != nil {
		t.Fatalf("식별자 생성: %v", err)
	}
	base := search.Flow{
		Contexts: []search.Context{{Value: model.Context{ID: first}, Rank: 1, MatchedChannels: []string{"time"}}},
		Edges:    []store.HopEdge{{FromID: first, ToID: second, Kind: "derived_from"}},
		Channels: map[string]search.Channel{"time": {Candidates: 1}, "graph": {Failure: "disabled"}},
	}
	measured := base
	measured.Read = store.ReadStats{Connections: 6, PeakConnections: 4}
	measured.Channels = map[string]search.Channel{"graph": {Failure: "disabled"}, "time": {Candidates: 1, Latency: 5}}
	if flowSignature(base) != flowSignature(measured) {
		t.Fatal("측정값만 다른 응답을 다르게 봤다")
	}
	reordered := base
	reordered.Contexts = []search.Context{{Value: model.Context{ID: first}, Rank: 2, MatchedChannels: []string{"time"}}}
	if flowSignature(base) == flowSignature(reordered) {
		t.Fatal("순위가 다른 응답을 같게 봤다")
	}
}

func TestSummarizeLatency(t *testing.T) {
	got := summarize([]float64{5, 1, 3, 2, 4})
	if got.Mean != 3 || got.P50 != 3 || got.P95 != 5 || got.Max != 5 {
		t.Fatalf("지연 요약 = %+v", got)
	}
	if summarize(nil).Mean != undefinedMetric {
		t.Fatal("빈 표본의 요약이 미측정 값이 아니다")
	}
}
