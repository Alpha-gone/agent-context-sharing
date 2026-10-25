package main

import (
	"bytes"
	json "encoding/json/v2"
	"os"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

func TestContinualScenarioContractsIntegration(t *testing.T) {
	database, worker, connection := evalDatabase(t, nil)
	settings := settings{execution: search.ExecutionSequential, candidateLimit: 50, semanticThreshold: 0.7, foldThreshold: 0.95}
	for _, scenario := range continualScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			scenario.contexts.Version += "-" + id.String()
			var before int
			if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM public.context_graph WHERE name=$1`, "eval "+scenario.contexts.Version).Scan(&before); err != nil {
				t.Fatal(err)
			}
			run, err := runContinualScenario(t.Context(), database, worker, settings, scenario, 1, 2, 100)
			if err != nil {
				t.Fatal(err)
			}
			if !run.StateVerified || !run.JudgmentVerified || len(run.StateChecks) < 2 || len(run.JudgmentChecks) != len(scenario.judgments) || run.IndependentUnit != scenario.name || run.GraphStage != string(scenario.graphStage) {
				t.Fatalf("시나리오 상태·기록: %+v", run)
			}
			if run.Baseline.Recall != 1 || run.Current.Recall != 1 || run.Baseline.BudgetUsed <= 0 || run.Current.BudgetUsed <= 0 {
				t.Fatalf("전후 검색: %+v", run)
			}
			var after, active int
			if err := connection.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM public.context_graph WHERE name=$1`, "eval "+scenario.contexts.Version).Scan(&after, &active); err != nil {
				t.Fatal(err)
			}
			if after-before != 1 || active != 0 {
				t.Fatalf("독립 그래프·정리: before=%d after=%d active=%d", before, after, active)
			}
		})
	}
}

func TestContinualAppliedJudgmentScopeIntegration(t *testing.T) {
	database, worker, _ := evalDatabase(t, nil)
	scenario := continualScenarios()[1]
	err := withEvaluationGraph(t.Context(), database, worker, scenario.contexts, 32, func(graph loadedGraph) error {
		checks, err := verifyContinualJudgments(t.Context(), database, graph, scenario)
		if err != nil || allContinualChecks(checks) {
			t.Fatalf("없는 기록: %+v %v", checks, err)
		}
		operation := store.OperationRecord{Kind: store.OperationKeep, GraphID: graph.GraphID, ContextID: graph.Keys["answer"], TargetVersion: 1, JudgmentInput: scenario.judgments[0], AccountID: graph.AccountID, AgentID: graph.AccountID}
		if err := database.RecordRejectedOperation(t.Context(), operation, "validation"); err != nil {
			t.Fatal(err)
		}
		checks, err = verifyContinualJudgments(t.Context(), database, graph, scenario)
		if err != nil || allContinualChecks(checks) {
			t.Fatalf("거부 기록을 성공으로 처리했다: %+v %v", checks, err)
		}
		if found, err := database.HasOperationJudgment(t.Context(), graph.GraphID, scenario.judgments[0]); err != nil || !found {
			t.Fatalf("일반 기록 조회 계약: %t %v", found, err)
		}
		if err := withEvaluationGraph(t.Context(), database, worker, scenario.contexts, 32, func(other loadedGraph) error {
			op := operation
			op.GraphID, op.ContextID, op.AccountID, op.AgentID = other.GraphID, other.Keys["answer"], other.AccountID, other.AccountID
			if _, err := database.KeepContext(t.Context(), other.GraphID, op.ContextID, 1, &op); err != nil {
				return err
			}
			found, err := database.HasAppliedOperationJudgment(t.Context(), graph.GraphID, op.JudgmentInput)
			if err != nil || found {
				t.Fatalf("다른 그래프 적용 기록 누출: %t %v", found, err)
			}
			return nil
		}); err != nil {
			return err
		}
		if _, err := database.KeepContext(t.Context(), graph.GraphID, operation.ContextID, 1, &operation); err != nil {
			return err
		}
		checks, err = verifyContinualJudgments(t.Context(), database, graph, scenario)
		if err != nil || !allContinualChecks(checks) {
			t.Fatalf("적용 기록 확인: %+v %v", checks, err)
		}
		if _, err := database.HasAppliedOperationJudgment(t.Context(), model.ID{}, "invalid"); err == nil {
			t.Fatal("잘못된 그래프가 조회됐다")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContinualTransferRequiresReturnedEvidenceIntegration(t *testing.T) {
	database, worker, _ := evalDatabase(t, nil)
	scenario := continualScenarios()[2]
	err := withEvaluationGraph(t.Context(), database, worker, scenario.contexts, 32, func(graph loadedGraph) error {
		service, err := search.New(database, worker, search.Config{Execution: search.ExecutionSequential, CandidateLimit: 50, FoldThreshold: 0.95, GraphStage: scenario.graphStage}, nil)
		if err != nil {
			return err
		}
		_, before, err := measureContinualQuery(t.Context(), service, graph, scenario.query, 2, 100)
		if err != nil {
			return err
		}
		if err := applyContinualMutation(t.Context(), database, graph, scenario, service, 2, 100); err != nil {
			return err
		}
		if err := drainIndexQueue(t.Context(), database, worker, graph.GraphID, 32); err != nil {
			return err
		}
		_, after, err := measureContinualQuery(t.Context(), service, graph, scenario.query, 2, 100)
		if err != nil {
			return err
		}
		after.Edges = nil
		checks, err := verifyContinualState(t.Context(), database, graph, scenario, before, after)
		if err != nil || checks["current_evidence_path"] || allContinualChecks(checks) {
			t.Fatalf("반환 간선 없이 전이가 통과했다: %+v %v", checks, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContinualPartialChannelFailureStopsMeasurementIntegration(t *testing.T) {
	database, worker, connection := evalDatabase(t, nil)
	scenario := continualScenarios()[0]
	err := withEvaluationGraph(t.Context(), database, worker, scenario.contexts, 32, func(graph loadedGraph) error {
		failing := failingEvalWorker(t, database, connection, 400, 0)
		service, err := search.New(database, failing, search.Config{Execution: search.ExecutionSequential, CandidateLimit: 50, FoldThreshold: 0.95, GraphStage: search.GraphStageBaseline}, nil)
		if err != nil {
			return err
		}
		if _, _, err := measureContinualQuery(t.Context(), service, graph, scenario.query, 2, 100); err == nil {
			t.Fatal("부분 채널 실패를 품질 표본으로 사용했다")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func requireContinualReport(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report continualReport
	if err := json.Unmarshal(raw, &report, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if len(report.Runs) != 60 || len(report.Judgements) != 6 || report.Repeats != 10 || report.Conditions.AsOf != continualAsOf || report.Conditions.IndexTargets != "all_layers" {
		t.Fatalf("지속 보고서 구성: %+v", report.Conditions)
	}
	seen := map[string]map[int]bool{}
	for _, run := range report.Runs {
		if seen[run.Scenario] == nil {
			seen[run.Scenario] = map[int]bool{}
		}
		if seen[run.Scenario][run.Repeat] || run.Repeat < 1 || run.Repeat > 10 || !run.StateVerified || !run.JudgmentVerified || run.IndependentUnit != run.Scenario {
			t.Fatalf("지속 회차 상태: %+v", run)
		}
		seen[run.Scenario][run.Repeat] = true
	}
	for _, scenario := range continualScenarios() {
		if len(seen[scenario.name]) != 10 {
			t.Fatalf("시나리오 반복 누락: %s", scenario.name)
		}
		if bytes.Contains(raw, []byte(scenario.query.WorkContext)) {
			t.Fatal("질의 원문이 결과에 노출됐다")
		}
		for _, context := range scenario.contexts.Contexts {
			if bytes.Contains(raw, []byte(context.Body)) {
				t.Fatal("컨텍스트 본문이 결과에 노출됐다")
			}
		}
	}
	for _, judgement := range report.Judgements {
		if judgement.Passed || !judgement.StatePassed || !judgement.RecordsPassed || judgement.Status != "deferred" || judgement.Reason != "insufficient_independent_units" || len(judgement.Comparisons) != 3 {
			t.Fatalf("반복을 독립 표본으로 처리했다: %+v", judgement)
		}
		for _, comparison := range judgement.Comparisons {
			if comparison.Samples != 1 || comparison.Interval != -1 || comparison.Significant {
				t.Fatalf("독립 표본·구간: %+v", comparison)
			}
		}
	}
}
