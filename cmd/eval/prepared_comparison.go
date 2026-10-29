package main

import (
	"cmp"
	json "encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

type comparisonDataset struct {
	Code     string
	Contexts contextSet
	Queries  querySet
}

type comparisonInput struct {
	Datasets                     []comparisonDataset
	Preparation                  *preparationReport
	ContextVersion, QueryVersion string
}

// 준비 checksum을 실제로 대조한 뒤 평가 주석의 근거 key만 검색 정답에 연결한다.
// 원본 매핑과 정답 문자열은 검색 대상 기억에 넣지 않는다.
func loadPreparedComparison(directory string) (comparisonInput, error) {
	var report preparationReport
	if err := readPreparedReport(filepath.Join(directory, "preparation.json"), &report); err != nil {
		return comparisonInput{}, err
	}
	if report.Version != preparationVersion || !manifestCode.MatchString(report.Dataset) {
		return comparisonInput{}, fmt.Errorf("준비 판/데이터셋 코드 오류")
	}
	if report.Pin != nil {
		found := false
		for _, format := range []string{"longmemeval", "locomo"} {
			pin, _ := publicPin(format)
			found = found || *report.Pin == pin
		}
		if !found || report.Pin.Dataset != report.Dataset {
			return comparisonInput{}, fmt.Errorf("고정 공개 데이터 판 불일치")
		}
	}
	var memory preparedMemory
	var evaluation preparedEvaluation
	for _, file := range []struct {
		name  string
		value any
	}{{"memory.json", &memory}, {"evaluation-private.json", &evaluation}} {
		checksum := report.Files[file.name]
		if !manifestSHA256.MatchString(checksum) {
			return comparisonInput{}, fmt.Errorf("준비 파일 checksum 누락")
		}
		if err := readPinnedPublic(filepath.Join(directory, file.name), publicDataPin{SHA256: checksum}, file.value); err != nil {
			return comparisonInput{}, err
		}
	}
	if memory.Version != preparationVersion || evaluation.Version != preparationVersion {
		return comparisonInput{}, fmt.Errorf("준비 파일 판 불일치")
	}
	rebuilt := preparedData{Memory: memory, Evaluation: evaluation, Report: report}
	rebuilt.Report.Contexts = 0
	if err := rebuilt.finish(); err != nil {
		return comparisonInput{}, err
	}
	if rebuilt.Report.Included != report.Included || rebuilt.Report.Graphs != report.Graphs || rebuilt.Report.Contexts != report.Contexts || rebuilt.Report.Independent != report.Independent {
		return comparisonInput{}, fmt.Errorf("준비 집계 불일치")
	}
	if report.Mapping != (manifestMapping{Preprocessing: preparationVersion, Split: preparationVersion, Answers: preparationVersion, Evidence: preparationVersion, AnswerPolicy: "evaluation-only-separated"}) {
		return comparisonInput{}, fmt.Errorf("준비 매핑 판 불일치")
	}
	if report.Pin == nil && report.Dataset != "own-supplement-v1" {
		return comparisonInput{}, fmt.Errorf("공개 데이터의 고정 판이 없다")
	}
	units := map[string]manifestUnit{}
	groups, independent := map[string]string{}, map[string]string{}
	for _, unit := range report.Units {
		if _, duplicate := units[unit.Query]; duplicate || unit.Dataset != report.Dataset || !manifestCode.MatchString(unit.Query) || !manifestCode.MatchString(unit.Independent) {
			return comparisonInput{}, fmt.Errorf("준비 독립 단위 오류")
		}
		if (unit.Conversation == "") == (unit.Task == "") {
			return comparisonInput{}, fmt.Errorf("준비 대화/과업 매핑 오류")
		}
		group := "conversation-" + unit.Conversation
		if unit.Task != "" {
			group = "task-" + unit.Task
		}
		if (unit.Conversation != "" && !manifestCode.MatchString(unit.Conversation)) || (unit.Task != "" && !manifestCode.MatchString(unit.Task)) || (groups[group] != "" && groups[group] != unit.Independent) || (independent[unit.Independent] != "" && independent[unit.Independent] != group) {
			return comparisonInput{}, fmt.Errorf("준비 독립 단위 묶음 불일치")
		}
		groups[group], independent[unit.Independent] = unit.Independent, group
		units[unit.Query] = unit
	}
	if len(units) != len(evaluation.Queries) {
		return comparisonInput{}, fmt.Errorf("준비 독립 단위 수 불일치")
	}
	byGroup := map[string][]querySpec{}
	types, evidence, reasons := map[string]int{}, map[string]int{}, map[string]int{}
	for _, query := range evaluation.Queries {
		unit := units[query.Code]
		if unit.Independent != query.Unit || unit.Type != query.Type || !manifestCode.MatchString(query.Group) || !manifestCode.MatchString(query.Type) || query.AsOf.IsZero() {
			return comparisonInput{}, fmt.Errorf("준비 질의의 그룹/독립 단위/시각 불일치")
		}
		useCase, err := preparedUseCase(query.Type)
		if err != nil {
			return comparisonInput{}, err
		}
		if !slices.Contains([]string{"annotated", "unmeasured", "not_applicable"}, query.EvidenceStatus) {
			return comparisonInput{}, fmt.Errorf("준비 근거 주석 상태 오류")
		}
		types[query.Type]++
		evidence[query.EvidenceStatus]++
		if query.EvidenceReason != "" {
			reasons[query.EvidenceReason]++
		}
		byGroup[query.Group] = append(byGroup[query.Group], querySpec{ID: query.Code, IndependentUnit: query.Unit, DatasetType: query.Type,
			WorkContext: query.Question, UseCase: useCase, Scope: "auto", AsOf: new(query.AsOf), Answers: slices.Clone(query.Evidence),
			Required: slices.Clone(query.Evidence), unmeasuredAnswers: query.EvidenceStatus != "annotated", evidenceStatus: query.EvidenceStatus})
	}
	if !maps.Equal(types, report.Types) || !maps.Equal(evidence, report.Evidence) || !maps.Equal(reasons, report.EvidenceReasons) {
		return comparisonInput{}, fmt.Errorf("준비 유형/주석 분모 불일치")
	}
	input := comparisonInput{Preparation: &report, ContextVersion: report.Dataset + "-" + report.Version, QueryVersion: report.Dataset + "-" + report.Version}
	for _, group := range memory.Groups {
		queries := byGroup[group.Code]
		if len(queries) == 0 {
			continue
		}
		input.Datasets = append(input.Datasets, comparisonDataset{Code: group.Code, Contexts: group.Contexts, Queries: querySet{Version: input.QueryVersion, Queries: queries}})
	}
	slices.SortFunc(input.Datasets, func(a, b comparisonDataset) int { return cmp.Compare(a.Code, b.Code) })
	if len(input.Datasets) == 0 {
		return comparisonInput{}, fmt.Errorf("측정할 준비 그룹이 없다")
	}
	return input, nil
}

func preparedUseCase(kind string) (string, error) {
	switch kind {
	case "multi-session", "multi-hop", "event":
		return useCaseAssociative, nil
	case "single-session-assistant", "single-session-preference", "single-session-user", "temporal-reasoning", "knowledge-update", "abstention", "single-hop", "temporal", "commonsense", "evidence-state", "isolation", "recovery", "contamination", "handover":
		return useCaseFact, nil
	default:
		return "", fmt.Errorf("준비 문항 유형을 지원하지 않는다")
	}
}

// 그룹별 측정값은 먼저 같은 회차로 합치고 독립 단위 통계는 기존 집계기에 맡긴다.
func mergeMeasurements(parts []measurement) measurement {
	result := measurement{UseCases: map[string]useCaseMetrics{}, Recovered: map[string]map[string]struct{}{}, Signals: nil}
	channels := map[string]map[string]int{}
	for _, part := range parts {
		result.QuerySamples = append(result.QuerySamples, part.QuerySamples...)
		maps.Copy(result.Recovered, part.Recovered)
		for useCase, metrics := range part.UseCases {
			total := result.UseCases[useCase]
			if total.Failures == nil {
				total.Failures = map[string]int{}
				channels[useCase] = map[string]int{}
			}
			for failure, count := range metrics.Failures {
				total.Failures[failure] += count
			}
			for channel, count := range part.channelCounts[useCase] {
				channels[useCase][channel] += count
			}
			weight := float64(metrics.Queries)
			total.Queries += metrics.Queries
			total.SelectionCandidates += metrics.SelectionCandidates * weight
			total.SelectionSelected += metrics.SelectionSelected * weight
			total.SelectionConnectors += metrics.SelectionConnectors * weight
			total.SelectionUnreachable += metrics.SelectionUnreachable * weight
			result.UseCases[useCase] = total
		}
	}
	slices.SortFunc(result.QuerySamples, func(a, b queryMetrics) int { return cmp.Compare(a.ID, b.ID) })
	for useCase, total := range result.UseCases {
		var samples []queryMetrics
		var hits, scoredBudget int
		for _, sample := range result.QuerySamples {
			if sample.UseCase == useCase {
				samples = append(samples, sample)
				if !sample.unmeasuredAnswers {
					hits += sample.hits
					scoredBudget += sample.BudgetUsed
				}
			}
		}
		total.Recall = definedMean(samples, func(q queryMetrics) float64 { return q.Recall })
		total.ReciprocalRank = definedMean(samples, func(q queryMetrics) float64 { return q.ReciprocalRank })
		total.BudgetUsed = definedMean(samples, func(q queryMetrics) float64 { return float64(q.BudgetUsed) })
		total.BudgetPerHit = undefinedMetric
		if hits > 0 {
			total.BudgetPerHit = float64(scoredBudget) / float64(hits)
		}
		// 미측정값을 평균의 분모에 넣지 않는다.
		for _, field := range []struct {
			target *float64
			value  func(queryMetrics) float64
		}{
			{&total.EvidenceCompleteness, func(q queryMetrics) float64 { return q.EvidenceCompleteness }}, {&total.PathContinuity, func(q queryMetrics) float64 { return q.PathContinuity }},
			{&total.DisconnectedRatio, func(q queryMetrics) float64 { return q.DisconnectedRatio }}, {&total.HubConcentration, func(q queryMetrics) float64 { return q.HubConcentration }},
			{&total.DuplicateRatio, func(q queryMetrics) float64 { return q.DuplicateRatio }}, {&total.LatencyMS, func(q queryMetrics) float64 { return q.LatencyMS }}, {&total.MisrouteRate, func(q queryMetrics) float64 { return q.Misrouted }},
		} {
			*field.target = definedMean(samples, field.value)
		}
		total.LatencyP95MS, total.Routes, total.RouteReasons, total.Channels = latencyP95(samples), routeRatio(samples), routeReasons(samples), contributionRatio(channels[useCase])
		weight := float64(total.Queries)
		total.SelectionCandidates /= weight
		total.SelectionSelected /= weight
		total.SelectionConnectors /= weight
		total.SelectionUnreachable /= weight
		result.UseCases[useCase] = total
	}
	return result
}

// 메타데이터는 대형 기억 파일보다 작은 읽기 상한을 적용한다.
func readPreparedReport(path string, report *preparationReport) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("준비 메타데이터 열기 실패")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil || len(raw) > maxManifestBytes {
		return fmt.Errorf("준비 메타데이터 크기/읽기 오류")
	}
	if err := json.Unmarshal(raw, report, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("준비 메타데이터 JSON 오류")
	}
	return nil
}
