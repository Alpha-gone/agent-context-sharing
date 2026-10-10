package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

const continualBudget = 512

var continualAsOf = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

type continualReport struct {
	StartedAt  time.Time            `json:"started_at"`
	Repeats    int                  `json:"repeats"`
	Conditions continualConditions  `json:"conditions"`
	Runs       []continualRun       `json:"runs"`
	Judgements []continualJudgement `json:"judgements"`
}

type continualConditions struct {
	Budget            int     `json:"budget"`
	MaxHops           int     `json:"max_hops"`
	MaxHopNodes       int     `json:"max_hop_nodes"`
	Execution         string  `json:"channel_execution"`
	CandidateLimit    int     `json:"channel_candidate_limit"`
	SemanticThreshold float64 `json:"semantic_similarity_threshold"`
	FoldThreshold     float64 `json:"fold_threshold"`
	EmbeddingModel    string  `json:"embedding_model"`
}

type continualRun struct {
	Scenario         string       `json:"scenario"`
	Repeat           int          `json:"repeat"`
	Baseline         queryMetrics `json:"baseline"`
	Current          queryMetrics `json:"current"`
	StateVerified    bool         `json:"state_verified"`
	JudgmentVerified bool         `json:"judgment_verified"`
}

type continualJudgement struct {
	Scenario      string       `json:"scenario"`
	Comparisons   []comparison `json:"comparisons"`
	StatePassed   bool         `json:"state_passed"`
	RecordsPassed bool         `json:"records_passed"`
	Passed        bool         `json:"passed"`
}

type continualScenario struct {
	name       string
	contexts   contextSet
	query      querySpec
	graphStage search.GraphStage
	judgments  []string
}

func runContinual(outPath string, repeats, maxHops, maxHopNodes int) error {
	settings, err := loadSettings()
	if err != nil {
		return err
	}
	ctx := context.Background()
	database, err := store.New(ctx, settings.databaseURL, settings.graphName, nil, nil, store.IndexTargetsAllLayers)
	if err != nil {
		return fmt.Errorf("데이터베이스 풀 준비: %w", err)
	}
	defer database.Close()
	if err := database.CheckEmbeddingSchema(ctx, settings.index.VectorType, settings.index.Dimension); err != nil {
		return err
	}
	worker, err := index.New(database, settings.index, nil, slog.Default())
	if err != nil {
		return fmt.Errorf("색인 작업자 준비: %w", err)
	}

	result := continualReport{
		StartedAt: time.Now().UTC(),
		Repeats:   repeats,
		Conditions: continualConditions{
			Budget: continualBudget, MaxHops: maxHops, MaxHopNodes: maxHopNodes,
			Execution: string(settings.execution), CandidateLimit: settings.candidateLimit,
			SemanticThreshold: settings.semanticThreshold, FoldThreshold: settings.foldThreshold,
			EmbeddingModel: worker.ModelID(),
		},
	}
	for _, scenario := range continualScenarios() {
		for repeat := 1; repeat <= repeats; repeat++ {
			run, err := runContinualScenario(ctx, database, worker, settings, scenario, repeat, maxHops, maxHopNodes)
			if err != nil {
				return fmt.Errorf("시나리오 %q 회차 %d: %w", scenario.name, repeat, err)
			}
			result.Runs = append(result.Runs, run)
			slog.Info("지속 평가 회차 완료", "scenario", scenario.name, "repeat", repeat)
		}
	}
	result.Judgements = judgeContinual(result.Runs)
	return writeReport(outPath, result)
}

func runContinualScenario(ctx context.Context, database *store.Store, worker *index.Worker, settings settings, scenario continualScenario, repeat, maxHops, maxHopNodes int) (result continualRun, err error) {
	graph, err := loadGraph(ctx, database, scenario.contexts)
	if err != nil {
		return continualRun{}, err
	}
	defer func() { err = errors.Join(err, dropGraph(ctx, database, graph)) }()
	if err := drainIndexQueue(ctx, database, worker, graph.GraphID, len(scenario.contexts.Contexts)*4+64); err != nil {
		return continualRun{}, err
	}
	service, err := search.New(database, worker, search.Config{
		Execution: settings.execution, CandidateLimit: settings.candidateLimit,
		SemanticThreshold: settings.semanticThreshold, FoldThreshold: settings.foldThreshold,
		GraphStage: scenario.graphStage, GlobalFallback: true,
	}, slog.Default())
	if err != nil {
		return continualRun{}, err
	}
	baseline, baselineFlow, err := measureContinualQuery(ctx, service, graph, scenario.query, maxHops, maxHopNodes)
	if err != nil {
		return continualRun{}, err
	}
	if err := applyContinualMutation(ctx, database, graph, scenario); err != nil {
		return continualRun{}, err
	}
	if err := drainIndexQueue(ctx, database, worker, graph.GraphID, 64); err != nil {
		return continualRun{}, err
	}
	current, currentFlow, err := measureContinualQuery(ctx, service, graph, scenario.query, maxHops, maxHopNodes)
	if err != nil {
		return continualRun{}, err
	}
	stateVerified, err := verifyContinualState(ctx, database, graph, scenario, baselineFlow, currentFlow)
	if err != nil {
		return continualRun{}, err
	}
	judgmentVerified := true
	for _, judgment := range scenario.judgments {
		found, err := database.HasOperationJudgment(ctx, graph.GraphID, judgment)
		if err != nil {
			return continualRun{}, err
		}
		judgmentVerified = judgmentVerified && found
	}
	return continualRun{Scenario: scenario.name, Repeat: repeat, Baseline: baseline, Current: current, StateVerified: stateVerified, JudgmentVerified: judgmentVerified}, nil
}

func measureContinualQuery(ctx context.Context, service *search.Service, graph loadedGraph, query querySpec, maxHops, maxHopNodes int) (queryMetrics, search.Flow, error) {
	flow, err := service.Flow(ctx, search.Input{GraphID: graph.GraphID, WorkContext: query.WorkContext, AsOf: query.AsOf.UTC(), Budget: continualBudget, Scope: query.Scope, MaxHops: maxHops, MaxHopNodes: maxHopNodes})
	if err != nil {
		return queryMetrics{}, search.Flow{}, err
	}
	hits, best := scoreFlow(flow, answerIDs(query, graph.Keys))
	metrics := queryMetrics{ID: query.ID, UseCase: query.UseCase, Recall: float64(hits) / float64(len(query.Answers)), BudgetPerHit: undefinedMetric,
		EvidenceCompleteness: undefinedMetric, PathContinuity: undefinedMetric, DisconnectedRatio: undefinedMetric,
		HubConcentration: undefinedMetric, DuplicateRatio: undefinedMetric, LatencyMS: undefinedMetric, Misrouted: undefinedMetric}
	if best > 0 {
		metrics.ReciprocalRank = 1 / float64(best)
	}
	if hits > 0 {
		metrics.BudgetPerHit = float64(flow.BudgetUsed) / float64(hits)
	}
	return metrics, flow, nil
}

func judgeContinual(runs []continualRun) []continualJudgement {
	grouped := map[string][]continualRun{}
	for _, run := range runs {
		grouped[run.Scenario] = append(grouped[run.Scenario], run)
	}
	judgements := make([]continualJudgement, 0, len(grouped))
	for _, scenario := range continualScenarios() {
		values := grouped[scenario.name]
		if len(values) == 0 {
			continue
		}
		judgement := continualJudgement{Scenario: scenario.name, StatePassed: true, RecordsPassed: true, Passed: true}
		for _, run := range values {
			judgement.StatePassed = judgement.StatePassed && run.StateVerified
			judgement.RecordsPassed = judgement.RecordsPassed && run.JudgmentVerified
		}
		for _, metric := range metricNames {
			if !slices.Contains([]string{"recall", "reciprocal_rank", "budget_per_hit"}, metric.name) {
				continue
			}
			baseline := make([]float64, 0, len(values))
			current := make([]float64, 0, len(values))
			for _, run := range values {
				before := metric.sample(run.Baseline)
				after := metric.sample(run.Current)
				if before < 0 || after < 0 {
					continue
				}
				baseline = append(baseline, before)
				current = append(current, after)
			}
			if len(baseline) == 0 {
				continue
			}
			difference, interval := pairedInterval(baseline, current)
			improved := difference > 0
			degraded := difference < -interval
			if metric.lowerIsBetter {
				improved = difference < 0
				degraded = difference > interval
			}
			judgement.Comparisons = append(judgement.Comparisons, comparison{Metric: metric.name, Samples: len(baseline), Baseline: mean(baseline), Current: mean(current), Difference: difference, Interval: interval, Improved: improved, Significant: interval >= 0 && math.Abs(difference) > interval})
			judgement.Passed = judgement.Passed && interval >= 0 && !degraded
		}
		judgement.Passed = judgement.Passed && judgement.StatePassed && judgement.RecordsPassed
		judgements = append(judgements, judgement)
	}
	return judgements
}

func applyContinualMutation(ctx context.Context, database *store.Store, graph loadedGraph, scenario continualScenario) error {
	operation := func(kind store.OperationKind, contextID model.ID, judgment string) *store.OperationRecord {
		return &store.OperationRecord{Kind: kind, GraphID: graph.GraphID, ContextID: contextID, JudgmentInput: judgment, AccountID: graph.AccountID, AgentID: graph.AccountID}
	}
	switch scenario.name {
	case "online_update":
		for index := range 3 {
			judgment := scenario.judgments[index]
			if _, err := createContinualSource(ctx, database, graph, fmt.Sprintf("online-noise-%d", index), longBody("무관한 온라인 누적 배경", 80), operation(store.OperationAdd, model.ID{}, judgment)); err != nil {
				return err
			}
		}
	case "replay":
		id := graph.Keys["answer"]
		value, err := database.Context(ctx, graph.GraphID, id)
		if err != nil {
			return err
		}
		if _, err := database.KeepContext(ctx, graph.GraphID, id, value.Version, operation(store.OperationKeep, id, scenario.judgments[0])); err != nil {
			return err
		}
	case "transfer":
		source, err := createContinualSource(ctx, database, graph, "transfer-target", longBody("새 사건의 별도 배경", 80), operation(store.OperationAdd, model.ID{}, scenario.judgments[0]))
		if err != nil {
			return err
		}
		return createContinualEvent(ctx, database, graph, source, operation(store.OperationAdd, model.ID{}, scenario.judgments[1]))
	case "recovery":
		id := graph.Keys["answer"]
		discarded, err := database.DiscardContext(ctx, graph.GraphID, id, operation(store.OperationDiscard, id, scenario.judgments[0]), store.WriteLimits{})
		if err != nil {
			return err
		}
		if discarded.DeletedAt == nil {
			return fmt.Errorf("폐기 뒤 삭제 표시가 없다")
		}
		if _, err := database.RestoreContext(ctx, graph.GraphID, id, operation(store.OperationUpdate, id, scenario.judgments[1]), store.WriteLimits{}); err != nil {
			return err
		}
	case "forgetting":
		id := graph.Keys["expiring"]
		value, err := database.Context(ctx, graph.GraphID, id)
		if err != nil {
			return err
		}
		value.Version++
		value.Derived.ValidTo = new(time.Now().UTC().Add(-time.Hour))
		if _, err := database.UpdateContextWithOperation(ctx, graph.GraphID, value.Version-1, value, operation(store.OperationUpdate, id, scenario.judgments[0]), store.WriteLimits{}); err != nil {
			return err
		}
	case "conflict_resolution":
		id := graph.Keys["disputed"]
		value, err := database.Context(ctx, graph.GraphID, id)
		if err != nil {
			return err
		}
		value.Version++
		value.Body = "해소된 예전 의견"
		value.Derived.ConfidenceState = model.ConfidenceStateUncertain
		if _, err := database.UpdateContextWithOperation(ctx, graph.GraphID, value.Version-1, value, operation(store.OperationUpdate, id, scenario.judgments[0]), store.WriteLimits{}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("알 수 없는 지속 평가 시나리오 %q", scenario.name)
	}
	return nil
}

func verifyContinualState(ctx context.Context, database *store.Store, graph loadedGraph, scenario continualScenario, baseline, current search.Flow) (bool, error) {
	switch scenario.name {
	case "online_update", "replay":
		return flowContains(current, graph.Keys["answer"]), nil
	case "transfer":
		answer, evidence := graph.Keys["answer"], graph.Keys["evidence"]
		hops, err := database.HopContexts(ctx, graph.GraphID, answer, 1, "out", []string{"derived_from"}, 10)
		if err != nil {
			return false, err
		}
		for _, edge := range hops.Edges {
			if edge.FromID == answer && edge.ToID == evidence && edge.Kind == "derived_from" {
				return true, nil
			}
		}
		return false, nil
	case "recovery":
		value, err := database.Context(ctx, graph.GraphID, graph.Keys["answer"])
		return err == nil && value.DeletedAt == nil && flowContains(current, value.ID), err
	case "forgetting":
		expiring := graph.Keys["expiring"]
		if flowContains(current, expiring) {
			return false, nil
		}
		hops, err := database.HopContexts(ctx, graph.GraphID, graph.Keys["answer"], 1, "in", []string{"derived_from"}, 10)
		if err != nil {
			return false, err
		}
		for _, value := range hops.Contexts {
			if value.ID == expiring {
				return true, nil
			}
		}
		return false, nil
	case "conflict_resolution":
		before, after := confidenceInFlow(baseline, graph.Keys["disputed"]), confidenceInFlow(current, graph.Keys["disputed"])
		return before == model.ConfidenceStateDisputed && after == model.ConfidenceStateUncertain, nil
	default:
		return false, nil
	}
}

func createContinualSource(ctx context.Context, database *store.Store, graph loadedGraph, locator, body string, operation *store.OperationRecord) (model.Context, error) {
	id, err := model.NewID()
	if err != nil {
		return model.Context{}, err
	}
	operation.ContextID = id
	value := model.Context{ID: id, GraphID: graph.GraphID, Layer: model.LayerSource, Body: body, RecordedAt: time.Now().UTC(), CreatedBy: graph.AccountID, CreatedByAgent: graph.AccountID, Version: 1, Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelOther, Locator: "eval://continual/" + locator}, OccurredAt: continualAsOf.Add(-24 * time.Hour), OriginKind: model.OriginKindAgentOutput}}
	return database.CreateContextWithOperation(ctx, graph.GraphID, value, nil, operation, store.WriteLimits{})
}

func createContinualEvent(ctx context.Context, database *store.Store, graph loadedGraph, member model.Context, operation *store.OperationRecord) error {
	id, err := model.NewID()
	if err != nil {
		return err
	}
	operation.ContextID = id
	end := member.Source.OccurredAt.Add(time.Minute)
	value := model.Context{ID: id, GraphID: graph.GraphID, Layer: model.LayerEvent, Body: longBody("전이 대상 새 사건", 80), RecordedAt: time.Now().UTC(), CreatedBy: graph.AccountID, CreatedByAgent: graph.AccountID, Version: 1, Event: &model.EventAttributes{MemberIDs: []model.ID{member.ID}, Start: member.Source.OccurredAt, End: &end}}
	_, err = database.CreateContextWithOperation(ctx, graph.GraphID, value, nil, operation, store.WriteLimits{})
	return err
}

func continualScenarios() []continualScenario {
	return []continualScenario{
		newContinualScenario("online_update", search.GraphStageBaseline, []string{"continual:online_update:add:1", "continual:online_update:add:2", "continual:online_update:add:3"}),
		newContinualScenario("replay", search.GraphStageBaseline, []string{"continual:replay:keep"}),
		newContinualScenario("transfer", search.GraphStageReferences, []string{"continual:transfer:add_source", "continual:transfer:add_event"}),
		newContinualScenario("recovery", search.GraphStageBaseline, []string{"continual:recovery:discard", "continual:recovery:restore"}),
		newContinualScenario("forgetting", search.GraphStageBaseline, []string{"continual:forgetting:expire"}),
		newContinualScenario("conflict_resolution", search.GraphStageBaseline, []string{"continual:conflict_resolution:update"}),
	}
}

func newContinualScenario(name string, stage search.GraphStage, judgments []string) continualScenario {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	source := func(key, body string, offset int) contextSpec {
		return contextSpec{Key: key, Layer: "source", Body: body, Source: &sourceSpec{Channel: "other", Locator: "eval://continual/" + name + "/" + key, OccurredAt: base.Add(time.Duration(offset) * time.Minute), OriginKind: "agent_output"}}
	}
	query := querySpec{ID: name, UseCase: useCaseFact, Scope: "local", AsOf: new(continualAsOf)}
	var contexts []contextSpec
	switch name {
	case "online_update":
		query.WorkContext, query.Answers = "온라인 청록표식 유지 지식", []string{"answer"}
		contexts = []contextSpec{source("answer", shortBody(query.WorkContext), 1), source("noise", longBody("기준선 무관 배경", 80), 2)}
	case "replay":
		query.WorkContext, query.Answers = "재생 호박표식 과거 해결", []string{"answer"}
		contexts = []contextSpec{source("answer", shortBody(query.WorkContext), 1)}
	case "transfer":
		query.WorkContext, query.Answers = "전이 자주표식 공통 규칙", []string{"answer"}
		contexts = []contextSpec{
			source("evidence", shortBody("자주표식 규칙을 첫 사건에서 확인했다"), 1),
			{Key: "answer", Layer: "derived", Body: shortBody(query.WorkContext), Derived: &derivedSpec{Kind: "proposition", DerivedFrom: []string{"evidence"}, EvidenceState: "experience"}},
			source("noise", longBody("기준선 무관 배경", 80), 2),
		}
	case "recovery":
		query.WorkContext, query.Answers = "복구 남색표식 정상 지식", []string{"answer"}
		contexts = []contextSpec{source("answer", shortBody(query.WorkContext), 1)}
	case "forgetting":
		query.WorkContext, query.Answers = "망각 은색표식 현재 근거", []string{"answer"}
		contexts = []contextSpec{
			source("answer", shortBody(query.WorkContext), 1),
			{Key: "expiring", Layer: "derived", Body: shortBody(query.WorkContext + " 예전 판"), Derived: &derivedSpec{Kind: "proposition", DerivedFrom: []string{"answer"}, EvidenceState: "observation", ValidFrom: new(base)}},
		}
	case "conflict_resolution":
		query.WorkContext, query.Answers = "상충 금색표식 현재 판단", []string{"answer"}
		contexts = []contextSpec{
			source("evidence-a", shortBody("금색표식 근거 A"), 1), source("evidence-b", shortBody("금색표식 근거 B"), 2),
			{Key: "disputed", Layer: "derived", Body: shortBody(query.WorkContext + " 반대 의견"), Derived: &derivedSpec{Kind: "reflection", DerivedFrom: []string{"evidence-b"}, EvidenceState: "opinion", ConfidenceState: "disputed"}},
			{Key: "answer", Layer: "derived", Body: shortBody(query.WorkContext), Derived: &derivedSpec{Kind: "reflection", DerivedFrom: []string{"evidence-a"}, EvidenceState: "opinion", ConfidenceState: "supported"}},
		}
	}
	return continualScenario{name: name, contexts: contextSet{Version: "continual-" + name, Contexts: contexts}, query: query, graphStage: stage, judgments: judgments}
}

func flowContains(flow search.Flow, id model.ID) bool {
	for _, value := range flow.Contexts {
		if value.Value.ID == id {
			return true
		}
	}
	return false
}

func confidenceInFlow(flow search.Flow, id model.ID) model.ConfidenceState {
	for _, value := range flow.Contexts {
		if value.Value.ID == id && value.Value.Derived != nil {
			return value.Value.Derived.ConfidenceState
		}
	}
	return ""
}

func shortBody(prefix string) string { return prefix + strings.Repeat(" 근거", 12) }
func longBody(prefix string, repeats int) string {
	return prefix + strings.Repeat(" 무관한배경설명", repeats)
}
