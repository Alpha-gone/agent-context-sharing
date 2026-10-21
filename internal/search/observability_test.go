package search

import (
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"context"
	"encoding/json/v2"
	"strings"
	"sync"
	"testing"
	"time"
)

type obs05Events struct {
	mu     sync.Mutex
	events []diagnostic.Event
}

func (s *obs05Events) Emit(_ context.Context, e diagnostic.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return false
}
func TestOBS05SearchChannelsAndSelection(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000001")
	node := testContext(t, graph, "019a0000-0000-7000-8000-000000000002", "sensitive body", 1)
	backend := &fakeStore{semantic: []model.SearchCandidate{{Context: node}}, keyword: []model.SearchCandidate{{Context: node}}}
	service := testService(t, backend, ExecutionParallel)
	sink := &obs05Events{}
	source, _ := diagnostic.NewSource("obs05")
	reporter, _ := diagnostic.NewReporter(diagnostic.Search, sink, source)
	service.SetDiagnostics(reporter)
	trace := diagnostic.NewRequestTrace()
	ctx := diagnostic.WithTrace(t.Context(), trace)
	flow, err := service.Flow(ctx, Input{GraphID: graph, WorkContext: "secret query https://private.example/", AsOf: time.Now().UTC(), Budget: 5})
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]bool{}
	channels := map[string]uint64{}
	for _, e := range sink.events {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		if e.CorrelationID != trace.CorrelationID() {
			t.Fatal("검색 상관 ID 손실")
		}
		stages[e.Stage] = true
		if e.Stage == "channel" {
			channels[e.Fields.Search.Channel] = e.Fields.Search.Candidates
		}
		if e.Stage == "budget" && (e.Fields.Search.Characters != uint64(flow.BudgetUsed) || e.Fields.Search.Truncated != (flow.Truncation != nil)) {
			t.Fatal("실제 예산 집계 불일치")
		}
	}
	for _, stage := range []string{"channel", "routing", "selection", "budget", "flow"} {
		if !stages[stage] {
			t.Fatal("단계 누락", stage)
		}
	}
	if channels["semantic"] != 1 || channels["keyword"] != 1 {
		t.Fatal("결합 전 채널 수 손실")
	}
	raw, _ := json.Marshal(sink.events)
	for _, secret := range []string{"sensitive body", "secret query", "private.example"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("검색 원문 누출")
		}
	}
}
