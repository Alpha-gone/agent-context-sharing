// 「관계·경로 오염 적대적 평가와 홉별 진단」을 실행한다. 기준선은 별도 평가 그래프에서 돌리고,
// 공격 표본·반복마다 새 평가 그래프를 만들어 공격 전, 공격 후, 복구 후를 같은 그래프에서 잰다.
//
// 진단은 검색 결과를 걸러 내거나 관계를 지우지 않는다. 복구는 표본이 지시한 관계 폐기,
// 컨텍스트 폐기, 관계 재확정만 쓰고 그래프 운영자 복구를 쓰지 않는다.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

// 홉별 진단의 경고 코드는 네 값만 쓴다. 그래프 불변식 규칙 식별자와 섞지 않는다.
const (
	warningMissingContext     = "missing_expected_context"
	warningUnexpectedContext  = "unexpected_context"
	warningMissingRelation    = "missing_expected_relation"
	warningUnexpectedRelation = "unexpected_relation"
)

// eventRelationKinds는 확정 사건 관계의 간선 종류다. 홉별 진단은 이 간선만 대조하고 파생 근거,
// 대체, 구성원 참조의 변화는 영향 질의 수에만 반영한다.
var eventRelationKinds = []string{"precedes", "causes", "part_of", "relates_to"}

type adversarialConditions struct {
	IndexTargets      string  `json:"index_targets"`
	Repeats           int     `json:"repeats"`
	GraphStage        string  `json:"graph_stage"`
	GlobalFallback    bool    `json:"global_fallback"`
	Budget            int     `json:"budget"`
	MaxHops           int     `json:"max_hops"`
	MaxHopNodes       int     `json:"max_hop_nodes"`
	Execution         string  `json:"channel_execution"`
	CandidateLimit    int     `json:"channel_candidate_limit"`
	SemanticThreshold float64 `json:"semantic_similarity_threshold"`
	FoldThreshold     float64 `json:"fold_threshold"`
	EmbeddingModel    string  `json:"embedding_model"`
}

// adversarialReport는 결과 파일이다. 질의문, 컨텍스트 본문, 원문 경로는 싣지 않고 데이터셋
// key와 관계 정체성, 집계값만 싣는다.
type adversarialReport struct {
	StartedAt      time.Time                     `json:"started_at"`
	ContextVersion string                        `json:"context_dataset_version"`
	QueryVersion   string                        `json:"query_dataset_version"`
	AttackVersion  string                        `json:"attack_dataset_version"`
	Conditions     adversarialConditions         `json:"conditions"`
	Overall        adversarialMetrics            `json:"overall"`
	AttackTypes    map[string]adversarialMetrics `json:"attack_types"`
	Samples        []sampleResult                `json:"samples"`
	Warnings       []hopWarning                  `json:"warnings"`
}

// metricSummary는 단위별 반복 평균의 평균과, 공격 전 같은 단위 값과의 대응 차이 95%
// 신뢰구간이다. 단위가 없으면 Value는 정의되지 않은 값이다.
type metricSummary struct {
	Units      int     `json:"units"`
	Value      float64 `json:"value"`
	Difference float64 `json:"paired_difference"`
	HalfWidth  float64 `json:"ci95_half_width"`
}

type adversarialMetrics struct {
	Samples       int           `json:"samples"`
	TargetQueries int           `json:"target_queries"`
	AttackSuccess metricSummary `json:"attack_success_rate"`
	// AffectedQueries 필드는 질의별 영향 여부의 반복 평균을 더한 값이다. 반복이 모두 같으면
	// 영향받은 고유 대상 질의 수와 같다.
	AffectedQueries float64       `json:"affected_queries"`
	Affected        metricSummary `json:"affected_query_ratio"`
	Contamination   metricSummary `json:"contamination_ratio"`
	Trace           metricSummary `json:"trace_completeness"`
	FalsePositive   metricSummary `json:"false_positive_rate"`
	Recovery        metricSummary `json:"recovery_completeness"`
}

type sampleResult struct {
	SampleID   string             `json:"sample_id"`
	AttackType string             `json:"attack_type"`
	Targets    []string           `json:"target_query_ids"`
	Controls   int                `json:"control_queries"`
	Metrics    adversarialMetrics `json:"metrics"`
	// ControlContaminated와 ControlPathWarnings 필드는 오탐률을 원인별로 나눈 반복 평균이다.
	// 앞은 통제 질의가 오염 식별자를 반환한 비율, 뒤는 통제 질의의 정상 기대 경로에 경고가 난
	// 비율이다. 두 원인이 겹칠 수 있어 합이 오탐률보다 클 수 있다.
	ControlContaminated float64 `json:"control_contaminated_ratio"`
	ControlPathWarnings float64 `json:"control_path_warning_ratio"`
	// IsolationMismatches 필드는 공격 전 응답이 기준선 그래프 응답과 달랐던 질의·반복 수다.
	// 0이 아니면 앞선 표본이나 반복의 상태가 넘어왔다는 뜻이다.
	IsolationMismatches int `json:"isolation_mismatches"`
}

type hopWarning struct {
	SampleID string      `json:"sample_id"`
	QueryID  string      `json:"query_id"`
	PathID   string      `json:"path_id"`
	HopIndex int         `json:"hop_index"`
	Code     string      `json:"warning_code"`
	Expected []attackRef `json:"expected_refs"`
	Actual   []attackRef `json:"actual_refs"`
}

// response는 한 질의의 평면 응답을 데이터셋 key 공간으로 옮긴 것이다. 그래프마다 식별자가
// 다르므로 기준선·공격 전후·복구 후를 key와 관계 정체성으로 견준다.
type response struct {
	Contexts []string
	Ranks    map[string]int
	Edges    []relationSpec
	Entries  []string
}

func (r response) has(ref attackRef) bool {
	if ref.Key != "" {
		_, ok := r.Ranks[ref.Key]
		return ok
	}
	return slices.Contains(r.Edges, ref.Relation.normalized())
}

// signature는 영향 판정에 쓰는 응답 요약이다. 컨텍스트 순위와 모든 간선을 담으므로 참조
// 반환 변화도 여기에서 드러난다.
func (r response) signature() string {
	parts := make([]string, 0, len(r.Contexts)+len(r.Edges))
	for _, key := range r.Contexts {
		parts = append(parts, key+"#"+strconv.Itoa(r.Ranks[key]))
	}
	edges := make([]string, 0, len(r.Edges))
	for _, edge := range r.Edges {
		edges = append(edges, edge.Type+"|"+edge.From+"|"+edge.To)
	}
	slices.Sort(edges)
	return strings.Join(append(parts, edges...), ",")
}

// baseShape는 기준 그래프의 컨텍스트 계층과 확정 사건 관계다. 홉별 진단이 기대 밖의
// 컨텍스트와 관계를 가를 때 쓴다.
type baseShape struct {
	layers    map[string]string
	relations map[relationSpec]struct{}
}

func newBaseShape(contexts contextSet) baseShape {
	shape := baseShape{layers: make(map[string]string, len(contexts.Contexts)), relations: make(map[relationSpec]struct{}, len(contexts.Relations))}
	for _, spec := range contexts.Contexts {
		shape.layers[spec.Key] = spec.Layer
	}
	for _, relation := range contexts.Relations {
		shape.relations[relation.normalized()] = struct{}{}
	}
	return shape
}

func meetsCriteria(r response, criteria successCriteria) bool {
	for _, ref := range criteria.MustReturn {
		if !r.has(ref) {
			return false
		}
	}
	for _, ref := range criteria.MustOmit {
		if r.has(ref) {
			return false
		}
	}
	return true
}

func returnsContaminated(r response, contaminated []attackRef) bool {
	return slices.ContainsFunc(contaminated, r.has)
}

// contaminationRatio는 반환한 컨텍스트·참조·관계 중 오염 식별자와 일치하는 항목의 비율이다.
// 반환 항목이 없으면 0이다.
func contaminationRatio(r response, contaminated []attackRef) float64 {
	total := len(r.Contexts) + len(r.Edges)
	if total == 0 {
		return 0
	}
	matched := 0
	for _, key := range r.Contexts {
		if slices.ContainsFunc(contaminated, func(ref attackRef) bool { return ref.Key == key }) {
			matched++
		}
	}
	for _, edge := range r.Edges {
		if slices.ContainsFunc(contaminated, func(ref attackRef) bool { return ref.Relation != nil && ref.Relation.normalized() == edge }) {
			matched++
		}
	}
	return float64(matched) / float64(total)
}

// traceable은 반환된 첫 오염 식별자까지 진입점에서 반환 간선만으로 경로를 복원할 수 있는지
// 본다. 첫 항목은 순위가 앞선 컨텍스트, 없으면 응답 순서가 앞선 관계다.
func traceable(r response, contaminated []attackRef) bool {
	var target attackRef
	for _, key := range r.Contexts {
		if slices.ContainsFunc(contaminated, func(ref attackRef) bool { return ref.Key == key }) {
			target = attackRef{Key: key}
			break
		}
	}
	if target.Key == "" {
		for _, edge := range r.Edges {
			if slices.ContainsFunc(contaminated, func(ref attackRef) bool { return ref.Relation != nil && ref.Relation.normalized() == edge }) {
				target = attackRef{Relation: &edge}
				break
			}
		}
	}
	if target.Key == "" && target.Relation == nil {
		return false
	}
	adjacent := map[string][]string{}
	for _, edge := range r.Edges {
		adjacent[edge.From] = append(adjacent[edge.From], edge.To)
		adjacent[edge.To] = append(adjacent[edge.To], edge.From)
	}
	reached := map[string]bool{}
	queue := slices.Clone(r.Entries)
	for _, entry := range queue {
		reached[entry] = true
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacent[current] {
			if !reached[next] {
				reached[next] = true
				queue = append(queue, next)
			}
		}
	}
	if target.Key != "" {
		return reached[target.Key]
	}
	return reached[target.Relation.From] || reached[target.Relation.To]
}

// diagnosePath는 정상 기대 경로를 앞에서부터 응답과 대조해 첫 불일치 하나를 돌려준다.
//
// 사건 단계에서는 그 사건이 반환됐는지, 반환된 확정 사건 관계로 기준에 없는 사건이 붙었는지
// 본다. 관계 단계에서는 기대 관계가 반환됐는지, 바로 앞 사건에서 기준에 없는 관계가 기준
// 사건으로 뻗었는지 본다. 기준 그래프에 원래 있던 관계와 사건은 기대 밖으로 세지 않으므로
// 기준선 응답에서는 경고가 나지 않는다.
func diagnosePath(r response, path expectedPath, base baseShape) (hopWarning, bool) {
	events := make([]relationSpec, 0, len(r.Edges))
	for _, edge := range r.Edges {
		if slices.Contains(eventRelationKinds, edge.Type) {
			events = append(events, edge)
		}
	}
	// incident는 사건 하나에 닿은 반환 확정 사건 관계 중 기준 그래프에 없는 것이다.
	incident := func(key string) []relationSpec {
		var found []relationSpec
		for _, edge := range events {
			if _, known := base.relations[edge]; !known && (edge.From == key || edge.To == key) {
				found = append(found, edge)
			}
		}
		return found
	}
	warn := func(index int, code string, expected attackRef, actual []attackRef) (hopWarning, bool) {
		return hopWarning{QueryID: path.QueryID, PathID: path.PathID, HopIndex: index, Code: code, Expected: []attackRef{expected}, Actual: actual}, true
	}
	previous := ""
	for index, step := range path.Steps {
		if step.Key != "" {
			if _, ok := r.Ranks[step.Key]; !ok {
				return warn(index, warningMissingContext, step, []attackRef{})
			}
			var foreign []attackRef
			for _, edge := range incident(step.Key) {
				other := edge.From
				if other == step.Key {
					other = edge.To
				}
				if _, known := base.layers[other]; !known && !slices.Contains(foreign, attackRef{Key: other}) {
					foreign = append(foreign, attackRef{Key: other})
				}
			}
			if len(foreign) > 0 {
				slices.SortFunc(foreign, func(left, right attackRef) int { return strings.Compare(left.Key, right.Key) })
				return warn(index, warningUnexpectedContext, step, foreign)
			}
			previous = step.Key
			continue
		}
		expected := step.Relation.normalized()
		extras := incident(previous)
		if !slices.Contains(events, expected) {
			return warn(index, warningMissingRelation, attackRef{Relation: &expected}, relationRefs(extras))
		}
		var unexpected []relationSpec
		for _, edge := range extras {
			_, fromKnown := base.layers[edge.From]
			_, toKnown := base.layers[edge.To]
			if fromKnown && toKnown {
				unexpected = append(unexpected, edge)
			}
		}
		if len(unexpected) > 0 {
			return warn(index, warningUnexpectedRelation, attackRef{Relation: &expected}, relationRefs(unexpected))
		}
	}
	return hopWarning{}, false
}

func relationRefs(relations []relationSpec) []attackRef {
	refs := make([]attackRef, 0, len(relations))
	for _, relation := range relations {
		refs = append(refs, attackRef{Relation: &relation})
	}
	slices.SortFunc(refs, func(left, right attackRef) int {
		return strings.Compare(left.Relation.Type+left.Relation.From+left.Relation.To, right.Relation.Type+right.Relation.From+right.Relation.To)
	})
	return refs
}

// answersRestored는 정답 key의 반환 여부와 순위가 공격 전과 같은지 본다.
func answersRestored(after, before response, answers []string) bool {
	for _, key := range answers {
		if after.Ranks[key] != before.Ranks[key] {
			return false
		}
	}
	return true
}

// targetRun은 대상 질의 하나의 한 반복 관측값이다. Traced는 공격에 성공하지 않은 반복에서
// 정의되지 않은 값이다.
type targetRun struct {
	PreSuccess, Success             float64
	Affected                        float64
	PreContamination, Contamination float64
	Traced                          float64
	AttackRecovered, Recovered      float64
}

// sampleRuns는 표본 하나의 반복 관측값이다. 통제 값은 반복마다 경고가 난 통제 질의 비율이다.
type sampleRuns struct {
	sample                    attackSample
	targets                   map[string][]targetRun
	controlsPre, controlsPost []float64
	// controlsContaminated와 controlsWarned에는 반복마다 오탐 원인별 통제 질의 비율을 둔다.
	controlsContaminated, controlsWarned []float64
	controls                             int
	isolationMismatches                  int
}

// observeTarget은 대상 질의 하나의 공격 전·공격 후·복구 후 응답을 관측값으로 바꾼다. 복구
// 판정은 성공 조건이 거짓이고, 오염 식별자가 없고, 정답 순위가 공격 전과 같고, 정상 기대
// 경로에 경고가 없어야 참이다.
func observeTarget(sample attackSample, query querySpec, paths []expectedPath, before, attacked, recovered response, base baseShape) targetRun {
	criteria := sample.SuccessCriteria[slices.IndexFunc(sample.SuccessCriteria, func(item successCriteria) bool { return item.QueryID == query.ID })]
	restored := func(r response) float64 {
		if meetsCriteria(r, criteria) || returnsContaminated(r, sample.ContaminatedRefs) || !answersRestored(r, before, query.Answers) {
			return 0
		}
		for _, path := range paths {
			if _, warned := diagnosePath(r, path, base); warned {
				return 0
			}
		}
		return 1
	}
	run := targetRun{
		PreSuccess:       indicator(meetsCriteria(before, criteria)),
		Success:          indicator(meetsCriteria(attacked, criteria)),
		Affected:         indicator(before.signature() != attacked.signature()),
		PreContamination: contaminationRatio(before, sample.ContaminatedRefs),
		Contamination:    contaminationRatio(attacked, sample.ContaminatedRefs),
		Traced:           undefinedMetric,
		AttackRecovered:  restored(attacked),
		Recovered:        restored(recovered),
	}
	if run.Success == 1 {
		run.Traced = indicator(traceable(attacked, sample.ContaminatedRefs))
	}
	return run
}

// controlFlagged는 통제 질의에서 오염 식별자가 반환되거나 그 질의의 정상 기대 경로에 경고가
// 났는지 본다.
func controlFlagged(r response, contaminated []attackRef, paths []expectedPath, base baseShape) bool {
	if returnsContaminated(r, contaminated) {
		return true
	}
	return slices.ContainsFunc(paths, func(path expectedPath) bool {
		_, warned := diagnosePath(r, path, base)
		return warned
	})
}

func indicator(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// summarizeMetrics는 표본 묶음의 여섯 지표를 낸다. 반복값을 먼저 단위별로 평균하고 단위
// 사이의 대응 차이로 신뢰구간을 계산한다. 반복 회차를 독립 표본으로 쓰지 않는다.
func summarizeMetrics(runs []sampleRuns) adversarialMetrics {
	var success, preSuccess, affected, contamination, preContamination, traced, recovered, attackRecovered, controls, preControls []float64
	for _, sample := range runs {
		for _, queryID := range slices.Sorted(maps.Keys(sample.targets)) {
			series := sample.targets[queryID]
			average := func(value func(targetRun) float64) float64 {
				total := 0.0
				for _, run := range series {
					total += value(run)
				}
				return total / float64(len(series))
			}
			success = append(success, average(func(run targetRun) float64 { return run.Success }))
			preSuccess = append(preSuccess, average(func(run targetRun) float64 { return run.PreSuccess }))
			affected = append(affected, average(func(run targetRun) float64 { return run.Affected }))
			contamination = append(contamination, average(func(run targetRun) float64 { return run.Contamination }))
			preContamination = append(preContamination, average(func(run targetRun) float64 { return run.PreContamination }))
			recovered = append(recovered, average(func(run targetRun) float64 { return run.Recovered }))
			attackRecovered = append(attackRecovered, average(func(run targetRun) float64 { return run.AttackRecovered }))
			total, count := 0.0, 0
			for _, run := range series {
				if run.Traced >= 0 {
					total += run.Traced
					count++
				}
			}
			if count > 0 {
				traced = append(traced, total/float64(count))
			}
		}
		if sample.controls > 0 {
			controls = append(controls, mean(sample.controlsPost))
			preControls = append(preControls, mean(sample.controlsPre))
		}
	}
	metrics := adversarialMetrics{
		Samples:       len(runs),
		TargetQueries: len(success),
		AttackSuccess: summarizeMetric(preSuccess, success),
		Affected:      summarizeMetric(make([]float64, len(affected)), affected),
		Contamination: summarizeMetric(preContamination, contamination),
		Trace:         summarizeMetric(make([]float64, len(traced)), traced),
		FalsePositive: summarizeMetric(preControls, controls),
		Recovery:      summarizeMetric(attackRecovered, recovered),
	}
	for _, value := range affected {
		metrics.AffectedQueries += value
	}
	return metrics
}

func summarizeMetric(before, after []float64) metricSummary {
	if len(after) == 0 {
		return metricSummary{Value: undefinedMetric, Difference: undefinedMetric, HalfWidth: undefinedMetric}
	}
	difference, halfWidth := pairedInterval(before, after)
	return metricSummary{Units: len(after), Value: mean(after), Difference: difference, HalfWidth: halfWidth}
}

// runAdversarial은 기준선을 검증하고 표본·반복마다 공격 그래프를 새로 만들어 잰다.
func runAdversarial(ctx context.Context, database *store.Store, worker *index.Worker, service *search.Service, contexts contextSet, queries querySet, attacks attackSet, conditions adversarialConditions, expect store.EmbeddingExpectation) (adversarialReport, error) {
	base := newBaseShape(contexts)
	pathsByQuery := map[string][]expectedPath{}
	for _, sample := range attacks.Samples {
		for _, path := range sample.ExpectedPaths {
			pathsByQuery[path.QueryID] = append(pathsByQuery[path.QueryID], path)
		}
	}
	baseline, err := runBaseline(ctx, database, worker, service, contexts, queries, attacks, conditions, base, pathsByQuery)
	if err != nil {
		return adversarialReport{}, err
	}
	result := adversarialReport{
		StartedAt: time.Now().UTC(), ContextVersion: contexts.Version, QueryVersion: queries.Version, AttackVersion: attacks.Version,
		Conditions: conditions, AttackTypes: map[string]adversarialMetrics{}, Warnings: []hopWarning{},
	}
	seen := map[string]struct{}{}
	all := make([]sampleRuns, 0, len(attacks.Samples))
	for _, sample := range attacks.Samples {
		runs := sampleRuns{sample: sample, targets: map[string][]targetRun{}}
		for repeat := 1; repeat <= conditions.Repeats; repeat++ {
			warnings, err := runAttackRepeat(ctx, database, worker, service, contexts, queries, sample, conditions, expect, base, pathsByQuery, baseline, &runs)
			if err != nil {
				return adversarialReport{}, fmt.Errorf("표본 %q 회차 %d: %w", sample.SampleID, repeat, err)
			}
			for _, warning := range warnings {
				key := fmt.Sprintf("%s|%s|%d|%s", warning.QueryID, warning.PathID, warning.HopIndex, warning.Code)
				if _, dup := seen[sample.SampleID+"|"+key]; dup {
					continue
				}
				seen[sample.SampleID+"|"+key] = struct{}{}
				warning.SampleID = sample.SampleID
				result.Warnings = append(result.Warnings, warning)
			}
			slog.Info("공격 회차 완료", "sample", sample.SampleID, "repeat", repeat)
		}
		all = append(all, runs)
		result.Samples = append(result.Samples, sampleResult{
			SampleID: sample.SampleID, AttackType: sample.AttackType, Targets: sample.TargetQueryIDs, Controls: runs.controls,
			Metrics: summarizeMetrics([]sampleRuns{runs}), IsolationMismatches: runs.isolationMismatches,
			ControlContaminated: mean(runs.controlsContaminated), ControlPathWarnings: mean(runs.controlsWarned),
		})
	}
	for _, attackType := range attackTypes {
		var matched []sampleRuns
		for _, runs := range all {
			if runs.sample.AttackType == attackType {
				matched = append(matched, runs)
			}
		}
		if len(matched) > 0 {
			result.AttackTypes[attackType] = summarizeMetrics(matched)
		}
	}
	result.Overall = summarizeMetrics(all)
	return result, nil
}

// runBaseline은 공격 없는 평가 그래프에서 질의를 반복 실행한다. 어느 반복에서든 표본의 성공
// 조건이 이미 성립하거나 정상 기대 경로가 성립하지 않으면 그 표본을 거부한다. 돌려주는
// 값은 첫 반복의 응답이며 표본 그래프의 공격 전 응답과 견줘 상태 누출을 찾는 데 쓴다.
func runBaseline(ctx context.Context, database *store.Store, worker *index.Worker, service *search.Service, contexts contextSet, queries querySet, attacks attackSet, conditions adversarialConditions, base baseShape, pathsByQuery map[string][]expectedPath) (first map[string]response, err error) {
	graph, err := loadAdversarialGraph(ctx, database, worker, contexts)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dropGraph(ctx, database, graph)) }()
	for repeat := 1; repeat <= conditions.Repeats; repeat++ {
		responses, err := runQueries(ctx, service, graph, queries, conditions)
		if err != nil {
			return nil, fmt.Errorf("기준선 회차 %d: %w", repeat, err)
		}
		for _, sample := range attacks.Samples {
			for _, criteria := range sample.SuccessCriteria {
				if meetsCriteria(responses[criteria.QueryID], criteria) {
					return nil, fmt.Errorf("표본 %q: 질의 %q의 성공 조건이 기준선에서 이미 성립한다", sample.SampleID, criteria.QueryID)
				}
			}
		}
		for queryID, paths := range pathsByQuery {
			for _, path := range paths {
				if warning, warned := diagnosePath(responses[queryID], path, base); warned {
					return nil, fmt.Errorf("정상 기대 경로 %q가 기준선에서 성립하지 않는다: 홉 %d %s", path.PathID, warning.HopIndex, warning.Code)
				}
			}
		}
		if first == nil {
			first = responses
		}
	}
	return first, nil
}

// runAttackRepeat은 표본 하나의 한 반복이다. 새 그래프에 기준 집합을 올려 공격 전 응답을
// 기준선과 견주고, 공격을 적용해 불변식 감사를 통과하는지 본 뒤 공격 후 응답을 잰다. 같은
// 그래프에서 복구를 적용하고 다시 감사한 뒤 복구 후 응답을 잰다.
func runAttackRepeat(ctx context.Context, database *store.Store, worker *index.Worker, service *search.Service, contexts contextSet, queries querySet, sample attackSample, conditions adversarialConditions, expect store.EmbeddingExpectation, base baseShape, pathsByQuery map[string][]expectedPath, baseline map[string]response, runs *sampleRuns) (warnings []hopWarning, err error) {
	graph, err := loadAdversarialGraph(ctx, database, worker, contexts)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dropGraph(ctx, database, graph)) }()
	if err := auditClean(ctx, database, graph, expect, "공격 전"); err != nil {
		return nil, err
	}
	before, err := runQueries(ctx, service, graph, queries, conditions)
	if err != nil {
		return nil, fmt.Errorf("공격 전: %w", err)
	}
	for queryID, answer := range before {
		if answer.signature() != baseline[queryID].signature() {
			runs.isolationMismatches++
		}
	}

	if err := createContexts(ctx, database, graph.GraphID, graph.AccountID, sample.AddContexts, graph.Keys); err != nil {
		return nil, fmt.Errorf("공격 컨텍스트 추가: %w", err)
	}
	if err := drainIndexQueue(ctx, worker, graph.GraphID, len(sample.AddContexts)*4+64); err != nil {
		return nil, err
	}
	if err := confirmRelations(ctx, database, graph.GraphID, graph.AccountID, contextSet{Relations: sample.AddRelations}, graph.Keys); err != nil {
		return nil, fmt.Errorf("공격 관계 추가: %w", err)
	}
	for _, relation := range sample.DiscardRelations {
		if err := discardRelation(ctx, database, graph, relation); err != nil {
			return nil, fmt.Errorf("정상 관계 폐기: %w", err)
		}
	}
	if err := auditClean(ctx, database, graph, expect, "공격 후"); err != nil {
		return nil, err
	}
	attacked, err := runQueries(ctx, service, graph, queries, conditions)
	if err != nil {
		return nil, fmt.Errorf("공격 후: %w", err)
	}

	// 오염 관계를 먼저 폐기해야 공격 컨텍스트를 폐기할 때 관계 끝점을 잃지 않는다. 사건이
	// 구성원을 참조하므로 컨텍스트는 추가한 역순으로 폐기한다.
	for _, relation := range sample.AddRelations {
		if err := discardRelation(ctx, database, graph, relation); err != nil {
			return nil, fmt.Errorf("복구 관계 폐기: %w", err)
		}
	}
	for _, spec := range slices.Backward(sample.AddContexts) {
		if _, err := database.DiscardContext(ctx, graph.GraphID, graph.Keys[spec.Key], nil, store.WriteLimits{}); err != nil {
			return nil, fmt.Errorf("복구 컨텍스트 %q 폐기: %w", spec.Key, err)
		}
	}
	if err := confirmRelations(ctx, database, graph.GraphID, graph.AccountID, contextSet{Relations: sample.DiscardRelations}, graph.Keys); err != nil {
		return nil, fmt.Errorf("복구 관계 재확정: %w", err)
	}
	if err := drainIndexQueue(ctx, worker, graph.GraphID, len(sample.AddContexts)*4+64); err != nil {
		return nil, err
	}
	if err := auditClean(ctx, database, graph, expect, "복구 후"); err != nil {
		return nil, err
	}
	recovered, err := runQueries(ctx, service, graph, queries, conditions)
	if err != nil {
		return nil, fmt.Errorf("복구 후: %w", err)
	}

	flaggedBefore, flaggedAfter, contaminatedAfter, warnedAfter, controls := 0, 0, 0, 0, 0
	for _, query := range queries.Queries {
		if slices.Contains(sample.TargetQueryIDs, query.ID) {
			var paths []expectedPath
			for _, path := range sample.ExpectedPaths {
				if path.QueryID == query.ID {
					paths = append(paths, path)
					if warning, warned := diagnosePath(attacked[query.ID], path, base); warned {
						warnings = append(warnings, warning)
					}
				}
			}
			runs.targets[query.ID] = append(runs.targets[query.ID], observeTarget(sample, query, paths, before[query.ID], attacked[query.ID], recovered[query.ID], base))
			continue
		}
		controls++
		if controlFlagged(before[query.ID], sample.ContaminatedRefs, pathsByQuery[query.ID], base) {
			flaggedBefore++
		}
		if controlFlagged(attacked[query.ID], sample.ContaminatedRefs, pathsByQuery[query.ID], base) {
			flaggedAfter++
		}
		if returnsContaminated(attacked[query.ID], sample.ContaminatedRefs) {
			contaminatedAfter++
		}
		warned := false
		for _, path := range pathsByQuery[query.ID] {
			if warning, found := diagnosePath(attacked[query.ID], path, base); found {
				warnings = append(warnings, warning)
				warned = true
			}
		}
		if warned {
			warnedAfter++
		}
	}
	runs.controls = controls
	if controls > 0 {
		runs.controlsPre = append(runs.controlsPre, float64(flaggedBefore)/float64(controls))
		runs.controlsPost = append(runs.controlsPost, float64(flaggedAfter)/float64(controls))
		runs.controlsContaminated = append(runs.controlsContaminated, float64(contaminatedAfter)/float64(controls))
		runs.controlsWarned = append(runs.controlsWarned, float64(warnedAfter)/float64(controls))
	}
	return warnings, nil
}

func loadAdversarialGraph(ctx context.Context, database *store.Store, worker *index.Worker, contexts contextSet) (loadedGraph, error) {
	graph, err := loadGraph(ctx, database, contexts)
	if err != nil {
		return loadedGraph{}, err
	}
	if err := drainIndexQueue(ctx, worker, graph.GraphID, len(contexts.Contexts)*4+64); err != nil {
		return loadedGraph{}, errors.Join(err, dropGraph(ctx, database, graph))
	}
	return graph, nil
}

// auditClean은 그래프 불변식 전체 감사가 위반 없이 끝나는지 본다. 공격은 구조적으로 유효한
// 의미 오염이어야 하므로 위반은 표본 오류다.
func auditClean(ctx context.Context, database *store.Store, graph loadedGraph, expect store.EmbeddingExpectation, phase string) error {
	violations, err := database.AuditGraph(ctx, graph.GraphID, expect)
	if err != nil {
		return fmt.Errorf("%s 불변식 감사: %w", phase, err)
	}
	if len(violations) > 0 {
		return fmt.Errorf("%s 불변식 위반 %d건: 첫 규칙 %s", phase, len(violations), violations[0].Rule)
	}
	return nil
}

// discardRelation은 관계 정체성으로 확정 관계를 찾아 폐기한다.
func discardRelation(ctx context.Context, database *store.Store, graph loadedGraph, relation relationSpec) error {
	fromID, toID := graph.Keys[relation.From], graph.Keys[relation.To]
	cursor := ""
	for {
		page, next, err := database.ListContextRelations(ctx, graph.GraphID, fromID, []model.RelationState{model.RelationStateConfirmed}, []model.RelationType{model.RelationType(relation.Type)}, cursor, 100)
		if err != nil {
			return err
		}
		for _, stored := range page {
			if (stored.FromContextID == fromID && stored.ToContextID == toID) || (relation.Type == string(model.RelationTypeRelatesTo) && stored.FromContextID == toID && stored.ToContextID == fromID) {
				_, err := database.DiscardRelation(ctx, graph.GraphID, stored.ID, nil)
				return err
			}
		}
		if next == "" {
			return fmt.Errorf("확정 관계 %s(%s→%s)가 없다", relation.Type, relation.From, relation.To)
		}
		cursor = next
	}
}

// runQueries는 질의를 한 번씩 돌려 key 공간의 응답으로 바꾼다.
func runQueries(ctx context.Context, service *search.Service, graph loadedGraph, queries querySet, conditions adversarialConditions) (map[string]response, error) {
	names := make(map[model.ID]string, len(graph.Keys))
	for key, id := range graph.Keys {
		names[id] = key
	}
	responses := make(map[string]response, len(queries.Queries))
	for _, query := range queries.Queries {
		input := search.Input{GraphID: graph.GraphID, WorkContext: query.WorkContext, Budget: conditions.Budget, Scope: query.Scope, MaxHops: conditions.MaxHops, MaxHopNodes: conditions.MaxHopNodes}
		if query.AsOf != nil {
			input.AsOf = query.AsOf.UTC()
		}
		flow, err := service.Flow(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("질의 %q 검색: %w", query.ID, err)
		}
		responses[query.ID] = toResponse(flow, names)
	}
	return responses, nil
}

// toResponse는 검색 흐름을 key 공간으로 옮긴다. 적재하지 않은 식별자는 나오지 않아야 하지만,
// 나오더라도 원문 식별자 대신 표식만 남긴다.
func toResponse(flow search.Flow, names map[model.ID]string) response {
	name := func(id model.ID) string {
		if key, ok := names[id]; ok {
			return key
		}
		return "unknown"
	}
	result := response{Ranks: make(map[string]int, len(flow.Contexts))}
	ordered := slices.Clone(flow.Contexts)
	slices.SortStableFunc(ordered, func(left, right search.Context) int { return left.Rank - right.Rank })
	for _, item := range ordered {
		key := name(item.Value.ID)
		result.Contexts = append(result.Contexts, key)
		result.Ranks[key] = item.Rank
	}
	for _, edge := range flow.Edges {
		result.Edges = append(result.Edges, relationSpec{Type: edge.Kind, From: name(edge.FromID), To: name(edge.ToID)}.normalized())
	}
	for _, id := range flow.EntryPoints {
		result.Entries = append(result.Entries, name(id))
	}
	return result
}

// runAdversarialCommand는 공격 평가 검색 실행기를 만들고 결과를 기록한다. 그래프 단계는 관계
// 경로 확장이 열린 relations로 고정한다. 운영 기본값인 baseline은 사건 관계를 따라가지 않아
// 관계·경로 공격이 검색에 닿지 않는다.
func runAdversarialCommand(ctx context.Context, database *store.Store, worker *index.Worker, settings settings, contexts contextSet, queries querySet, attacks attackSet, conditions adversarialConditions, outPath string) error {
	service, err := search.New(database, worker, search.Config{
		Execution:         settings.execution,
		CandidateLimit:    settings.candidateLimit,
		SemanticThreshold: settings.semanticThreshold,
		FoldThreshold:     settings.foldThreshold,
		GraphStage:        search.GraphStage(conditions.GraphStage),
		GlobalFallback:    conditions.GlobalFallback,
		Consistency:       search.ConsistencySnapshot,
	}, slog.Default())
	if err != nil {
		return fmt.Errorf("공격 평가 검색 실행기 준비: %w", err)
	}
	expect := store.EmbeddingExpectation{ModelID: worker.ModelID(), Dimension: settings.index.Dimension}
	result, err := runAdversarial(ctx, database, worker, service, contexts, queries, attacks, conditions, expect)
	if err != nil {
		return err
	}
	return writeReport(outPath, result)
}
