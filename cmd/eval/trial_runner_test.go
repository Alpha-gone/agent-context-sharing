package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type trialFailureReport struct {
	Trial    string                `json:"trial_id"`
	Mode     string                `json:"mode"`
	Status   string                `json:"status"`
	Code     string                `json:"cause_code"`
	Progress trialProgressSnapshot `json:"progress"`
}

// TestTrialErrorKeepsLocalCause는 로컬 실행 로그에 원인 코드와 내부 원인이 함께 남는지 확인한다.
func TestTrialErrorKeepsLocalCause(t *testing.T) {
	if got := failTrial("input_mismatch", errors.New("입력 checksum 불일치")).Error(); !strings.Contains(got, "input_mismatch") || !strings.Contains(got, "입력 checksum 불일치") {
		t.Fatalf("로컬 원인 = %q", got)
	}
	if got := failTrial("output_required", nil).Error(); got != "평가 실행 실패: output_required" {
		t.Fatalf("원인 없는 오류 = %q", got)
	}
}

// TestTrialDeadlineCodeRequiresTrialDeadline은 제공자 HTTP timeout처럼 DeadlineExceeded를 감싼
// 실패를 시험 기한이 남아 있으면 timeout_ms 소진으로 기록하지 않는지 확인한다.
func TestTrialDeadlineCodeRequiresTrialDeadline(t *testing.T) {
	for _, test := range []struct {
		name    string
		expired bool
		want    string
	}{{"provider_timeout", false, "execution_failed"}, {"trial_deadline", true, "deadline_exceeded"}} {
		t.Run(test.name, func(t *testing.T) {
			m := validManifest()
			m.TrialID = "deadline-" + strings.ReplaceAll(test.name, "_", "-")
			root := t.TempDir()
			trial, err := newManifestTrial(m, root)
			if err != nil {
				t.Fatal(err)
			}
			if test.expired {
				trial.Progress.deadline = time.Now().Add(-time.Second)
			}
			cause := failTrial("execution_failed", fmt.Errorf("임베딩 제공자 호출: %w", context.DeadlineExceeded))
			if err := trial.finish(cause); err == nil {
				t.Fatal("실패를 성공으로 공개했다")
			}
			var failure trialFailureReport
			readTrialFile(t, root, m.TrialID, "failure.json", &failure)
			if failure.Code != test.want {
				t.Fatalf("실패 코드 = %s, want %s", failure.Code, test.want)
			}
		})
	}
}

// TestTrialReservationPrecedesExistingCheck는 같은 ID의 예약을 먼저 확인하고, 예약을 얻은 뒤
// 기존 결과를 발견하면 자기 예약만 해제하는지 확인한다.
func TestTrialReservationPrecedesExistingCheck(t *testing.T) {
	m := validManifest()
	root := t.TempDir()
	lock := filepath.Join(root, "."+m.TrialID+".lock")
	if err := os.Mkdir(filepath.Join(root, m.TrialID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := newManifestTrial(m, root); err == nil || !strings.Contains(err.Error(), "trial_id_locked") {
		t.Fatalf("다른 실행의 예약 확인 = %v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("다른 실행의 예약을 지웠다: %v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if _, err := newManifestTrial(m, root); err == nil || !strings.Contains(err.Error(), "trial_id_exists") {
		t.Fatalf("기존 결과 확인 = %v", err)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("기존 결과 확인 뒤 예약이 남았다: %v", err)
	}
}

// TestManifestTrialCancellationCleansUp은 입력 스냅숏 뒤 종료 신호가 오면 측정을 시작하지 않고
// 예약·비공개 입력 사본을 정리한 실패 묶음만 남기는지 확인한다.
func TestManifestTrialCancellationCleansUp(t *testing.T) {
	m, bindings, _ := trialFixture(t, "lookup")
	root := t.TempDir()
	signals, cancel := context.WithCancel(t.Context())
	cancel()
	err := runManifestTrialContext(signals, writeManifestFixture(t, m), root, bindings, "")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("취소된 시험 = %v", err)
	}
	var failure trialFailureReport
	readTrialFile(t, root, m.TrialID, "failure.json", &failure)
	if failure.Code != "cancelled" || failure.Progress.Stage != "input" {
		t.Fatalf("취소 실패 보고 = %+v", failure)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != m.TrialID {
		t.Fatalf("예약·임시 입력 정리 = %v", entries)
	}
}

// TestConvertKeepsExistingOutputsOnRejection은 출력 경로 중복이나 검증 실패가 기존 유효 파일을
// 바꾸지 않고 임시 파일도 남기지 않는지 확인한다.
func TestConvertKeepsExistingOutputsOnRejection(t *testing.T) {
	directory := t.TempDir()
	contexts, queries := filepath.Join(directory, "contexts.json"), filepath.Join(directory, "queries.json")
	if err := runConvert(convertOwn, "", "kept", 2, "", contexts, queries, ""); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, path := range []string{contexts, queries} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = raw
	}
	if err := runConvert(convertOwn, "", "duplicate", 2, "", contexts, filepath.Join(directory, ".", "contexts.json"), ""); err == nil || !strings.Contains(err.Error(), "중복") {
		t.Fatalf("같은 출력 경로 = %v", err)
	}
	duplicateQuestions := `[
 {"id": "q1", "question": "첫 질의", "answerable": true, "paragraphs": [{"idx": 0, "title": "A", "paragraph_text": "첫 번째 문단이다.", "is_supporting": true}], "question_decomposition": []},
 {"id": "q1", "question": "같은 id 질의", "answerable": true, "paragraphs": [{"idx": 0, "title": "B", "paragraph_text": "두 번째 문단이다.", "is_supporting": true}], "question_decomposition": []}
]`
	source := writeFile(t, "duplicate.json", duplicateQuestions)
	if err := runConvert(convertHippoRAG, source, "duplicate", 0, "", contexts, queries, ""); err == nil {
		t.Fatal("중복 질의 id 변환을 허용했다")
	}
	for path, raw := range before {
		current, err := os.ReadFile(path)
		if err != nil || string(current) != string(raw) {
			t.Fatalf("거부한 변환이 기존 출력을 바꿨다: %s %v", path, err)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("임시 파일이 남았다: %s", entry.Name())
		}
	}
}

// TestBusinessTasksReindexOmissionIsNotUnmeasured는 허용 입력인 재색인 시간 생략만으로 확장
// 과업 판정을 미측정 보류하지 않고, 준비 시간 미측정은 계속 보류하는지 확인한다.
func TestBusinessTasksReindexOmissionIsNotUnmeasured(t *testing.T) {
	input := businessTaskInput{Provenance: "agent_trace", Experiment: "lookup"}
	entry := businessTaskComparison{Tasks: minimumRepeats, IndependentUnits: 3}
	for _, metric := range businessTaskMetrics {
		samples := entry.IndependentUnits
		if metric == "reindex_ms" {
			samples = 0
		}
		entry.Metrics = append(entry.Metrics, comparison{Metric: metric, Samples: samples, Baseline: 1, Current: 1})
	}
	if decision, reason := judgeBusinessTasks(input, entry); decision != "deferred" || reason != "no_significant_task_efficiency_improvement" {
		t.Fatalf("재색인 생략 판정 = %s/%s", decision, reason)
	}
	for index := range entry.Metrics {
		if entry.Metrics[index].Metric == "preparation_ms" {
			entry.Metrics[index].Samples = 0
		}
	}
	if _, reason := judgeBusinessTasks(input, entry); reason != "usage_or_cost_unmeasured" {
		t.Fatalf("준비 시간 미측정 판정 = %s", reason)
	}
}

// TestAdaptiveOutcomesAverageDefinedQualityOnly는 일부 회차에서만 미정의(-1)인 품질 지표를
// 정의된 회차 값만으로 평균하고, 모두 미정의면 미정의로 남기는지 확인한다.
func TestAdaptiveOutcomesAverageDefinedQualityOnly(t *testing.T) {
	sample := func(repeat int, continuity float64) runMetrics {
		return runMetrics{Repeat: repeat, QuerySamples: []queryMetrics{{ID: "q", UseCase: useCaseAssociative, EvidenceCompleteness: 1, PathContinuity: continuity}}}
	}
	result := outcomes([]runMetrics{sample(1, 0.5), sample(2, undefinedMetric)}, querySet{Queries: []querySpec{{ID: "q"}}})
	if quality := result["q"].quality; len(quality) != 2 || quality[0] != 1 || quality[1] != 0.5 {
		t.Fatalf("정의된 회차 평균 = %v", quality)
	}
	result = outcomes([]runMetrics{sample(1, undefinedMetric), sample(2, undefinedMetric)}, querySet{Queries: []querySpec{{ID: "q"}}})
	if quality := result["q"].quality; quality[1] != undefinedMetric {
		t.Fatalf("모두 미정의인 지표 = %v", quality)
	}
}
