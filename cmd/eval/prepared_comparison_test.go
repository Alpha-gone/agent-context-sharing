package main

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func preparedComparisonFixture(t *testing.T) preparedData {
	t.Helper()
	data := newPreparedData("own-supplement-v1", nil, 3)
	for _, code := range []string{"group1", "group2", "unused"} {
		data.Memory.Groups = append(data.Memory.Groups, preparedGroup{Code: code, Contexts: lifecycleInput(t)})
	}
	for i, spec := range []struct{ group, unit, status, evidence string }{{"group1", "conversation1", "annotated", "source"}, {"group1", "conversation1", "annotated", "derived"}, {"group2", "conversation2", "unmeasured", ""}} {
		query := preparedQuery{Code: []string{"q1", "q2", "q3"}[i], Group: spec.group, Unit: spec.unit, Type: "single-hop", Question: "private-question-marker", Answer: []byte(`"private-answer-marker"`), AnswerStatus: "provided", EvidenceStatus: spec.status, AsOf: time.Now().UTC()}
		if spec.evidence != "" {
			query.Evidence = []string{spec.evidence}
		} else {
			query.EvidenceReason = "missing_annotation"
		}
		data.addQuery(query, spec.unit, "")
	}
	if err := data.finish(); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPreparedComparisonMappingAndPrivateSeparation(t *testing.T) {
	data := preparedComparisonFixture(t)
	dir := filepath.Join(t.TempDir(), "prepared")
	if err := writePreparedData(dir, data); err != nil {
		t.Fatal(err)
	}
	// 원본 대응표는 비교 입력에 사용하지 않는다.
	if err := os.Remove(filepath.Join(dir, "mapping-private.json")); err != nil {
		t.Fatal(err)
	}
	input, err := loadPreparedComparison(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Datasets) != 2 || input.Preparation.Graphs != 3 || input.Preparation.Independent != 2 {
		t.Fatalf("독립 단위: %+v", input.Preparation)
	}
	left, right := input.Datasets[0].Queries.Queries, input.Datasets[1].Queries.Queries
	if len(left) != 2 || left[0].IndependentUnit != left[1].IndependentUnit || right[0].IndependentUnit == left[0].IndependentUnit || !right[0].unmeasuredAnswers || len(right[0].Answers) != 0 {
		t.Fatal("질의/독립 단위/미측정 연결 오류")
	}
	if len(left[0].Answers) != 1 || left[0].Answers[0] != "source" || left[0].DatasetType != "single-hop" {
		t.Fatal("근거 key 또는 유형이 바뀌었다")
	}
	for _, group := range input.Datasets {
		for _, context := range group.Contexts.Contexts {
			if context.Body == "private-question-marker" || context.Body == "private-answer-marker" {
				t.Fatal("평가 주석을 기억에 유출했다")
			}
		}
	}
}

func TestPreparedComparisonRejectsChecksumAndMappingChanges(t *testing.T) {
	for _, failure := range []string{"checksum", "unit", "count", "type", "metadata"} {
		t.Run(failure, func(t *testing.T) {
			data := preparedComparisonFixture(t)
			switch failure {
			case "unit":
				data.Report.Units[0].Independent = "wrong"
			case "count":
				data.Report.Contexts++
			case "type":
				data.Evaluation.Queries[0].Type = "unknown"
			}
			dir := filepath.Join(t.TempDir(), "prepared")
			if err := writePreparedData(dir, data); err != nil {
				t.Fatal(err)
			}
			if failure == "checksum" {
				file, err := os.OpenFile(filepath.Join(dir, "memory.json"), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString(" ")
				_ = file.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if failure == "metadata" {
				raw, err := os.ReadFile(filepath.Join(dir, "preparation.json"))
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err = json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				fields["body"] = "private"
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, "preparation.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadPreparedComparison(dir); err == nil {
				t.Fatal("잘못된 준비 입력을 수용했다")
			}
		})
	}
}

func TestPreparedComparisonPreservesNotApplicable(t *testing.T) {
	data := preparedComparisonFixture(t)
	data.Evaluation.Queries[2].EvidenceStatus = "not_applicable"
	delete(data.Report.Evidence, "unmeasured")
	data.Report.Evidence["not_applicable"] = 1
	dir := filepath.Join(t.TempDir(), "prepared")
	if err := writePreparedData(dir, data); err != nil {
		t.Fatal(err)
	}
	input, err := loadPreparedComparison(dir)
	if err != nil {
		t.Fatal(err)
	}
	query := input.Datasets[1].Queries.Queries[0]
	if !query.unmeasuredAnswers || query.evidenceStatus != "not_applicable" || len(query.Answers) != 0 {
		t.Fatal("비해당 주석을 정답 없음으로 바꾸었다")
	}
}

func TestPreparedMeasurementMergePreservesUnmeasuredDenominators(t *testing.T) {
	parts := []measurement{
		{UseCases: map[string]useCaseMetrics{useCaseFact: {Queries: 1, SelectionCandidates: 2, Failures: map[string]int{"semantic": 1}}}, QuerySamples: []queryMetrics{{ID: "q1", UseCase: useCaseFact, Recall: 1, ReciprocalRank: 1, BudgetUsed: 100, BudgetPerHit: 100, hits: 1}}, channelCounts: map[string]map[string]int{useCaseFact: {"keyword": 1}}},
		{UseCases: map[string]useCaseMetrics{useCaseFact: {Queries: 1, SelectionCandidates: 4}}, QuerySamples: []queryMetrics{{ID: "q2", UseCase: useCaseFact, Recall: -1, ReciprocalRank: -1, BudgetUsed: 1000, BudgetPerHit: -1, unmeasuredAnswers: true}}, channelCounts: map[string]map[string]int{useCaseFact: {"semantic": 9}}},
	}
	merged := mergeMeasurements(parts)
	metrics := merged.UseCases[useCaseFact]
	if metrics.Recall != 1 || metrics.ReciprocalRank != 1 || metrics.BudgetPerHit != 100 || metrics.BudgetUsed != 550 || metrics.Queries != 2 || metrics.SelectionCandidates != 3 || metrics.Channels["semantic"] != 0.9 || metrics.Failures["semantic"] != 1 {
		t.Fatalf("분모 합산: %+v", metrics)
	}
}
