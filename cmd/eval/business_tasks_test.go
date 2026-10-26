package main

import (
	json "encoding/json/v2"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func businessTasksFixture(experiment string) businessTaskInput {
	input := businessTaskInput{SchemaVersion: 1, Version: "tasks-v1", Provenance: "synthetic", Experiment: experiment,
		Controls: businessTaskControls{Agent: unitAgent{Version: "agent-v1", Model: "model-v1", PromptFamily: "prompt-v1"}, Tools: "tools-v1", Success: "criteria-v1", Preparation: "isolated-v1", Cache: "disabled-v1", TimeoutMS: new(int64(1000)), MaxTurns: new(20), Budget: new(4000)},
		Pricing:  businessPricing{Version: "synthetic-v1", Currency: "USD", AsOf: "2026-10-26", Allocation: "per-repeat-v1"}}
	for _, component := range businessComponents {
		rate := businessRate{Component: component, Unit: "tokens", UnitSize: 1000, Rates: map[string]*float64{}}
		if component == "search" || component == "storage" {
			rate.Unit = "request_or_byte_hour-v1"
		}
		for _, category := range businessCategories(component) {
			rate.Rates[category] = new(2.0)
		}
		input.Pricing.Rates = append(input.Pricing.Rates, rate)
	}
	for i, name := range businessConditionNames(experiment) {
		condition := businessTaskCondition{Code: name, Lookup: new(true), Irrelevant: new(false), Reduction: "original"}
		switch experiment {
		case "lookup":
			condition.Lookup = new(i == 1)
		case "irrelevant":
			condition.Irrelevant = new(i == 1)
		case "reduction":
			condition.Reduction = name
		}
		input.Conditions = append(input.Conditions, condition)
	}
	for taskIndex := range 3 {
		task := businessTask{Code: fmt.Sprintf("task-%d", taskIndex), Independent: fmt.Sprintf("unit-%d", taskIndex), Type: "fact", InputHash: fmt.Sprintf("%064x", taskIndex+1), ObservationHash: strings.Repeat("b", 64)}
		for repeat := 1; repeat <= 10; repeat++ {
			for i, condition := range input.Conditions {
				elapsed, turns, tokens := int64(100), 10, 100.0
				if i > 0 {
					elapsed, turns, tokens = 50, 5, 50
				}
				run := businessTaskRun{PreparationMS: new(int64(0)), ReindexMS: new(int64(0)), IndexState: "prepared", Condition: condition.Code, Repeat: repeat, Order: (i-(repeat-1)%len(input.Conditions)+len(input.Conditions))%len(input.Conditions) + 1, Trace: "aggregate-v1", Status: "success", ElapsedMS: new(elapsed), Turns: new(turns), Tools: new(0), Interventions: new(0), EvidenceRefetches: new(0), EvidenceAvailable: new(true)}
				for _, phase := range businessPhases {
					for _, component := range businessComponents {
						item := businessCostItem{Phase: phase, Component: component, Status: "not_applicable"}
						if (phase == "execution" && component == "agent") || (condition.Code == "summary" && component == "summary" && phase == "preparation") {
							item.Status, item.Allocation, item.Quantities = "measured", new(1.0), map[string]*float64{}
							for _, category := range businessTokenCategories {
								item.Quantities[category] = new(tokens)
							}
						}
						run.Costs = append(run.Costs, item)
					}
				}
				task.Runs = append(task.Runs, run)
			}
		}
		input.Tasks = append(input.Tasks, task)
	}
	sealBusinessTasks(&input)
	return input
}

func sealBusinessTasks(input *businessTaskInput) {
	for i, task := range input.Tasks {
		for j, run := range task.Runs {
			index := slices.IndexFunc(input.Conditions, func(c businessTaskCondition) bool { return c.Code == run.Condition })
			if index >= 0 {
				input.Tasks[i].Runs[j].InputHash = businessTaskHash(*input, task, input.Conditions[index])
			}
		}
	}
}

func writeBusinessTasks(t *testing.T, input businessTaskInput) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBusinessTasksIndependentExperiments(t *testing.T) {
	for _, experiment := range []string{"lookup", "irrelevant", "reduction"} {
		t.Run(experiment, func(t *testing.T) {
			input := businessTasksFixture(experiment)
			loaded, err := loadBusinessTasks(writeBusinessTasks(t, input))
			if err != nil {
				t.Fatal(err)
			}
			report := summarizeBusinessTasks(loaded)
			if report.Source != "imported_task_trace" || report.DefaultActivation || len(report.Costs) != 30*len(input.Conditions) {
				t.Fatalf("출처/반복/기본값: %+v", report)
			}
			for _, comparison := range report.Comparisons {
				if comparison.Decision != "deferred" || comparison.Reason != "synthetic_trace" || comparison.IndependentUnits != 3 {
					t.Fatalf("합성 효과를 채택했다: %+v", comparison)
				}
			}
			input.Provenance = "agent_trace"
			report = summarizeBusinessTasks(input)
			for _, comparison := range report.Comparisons {
				if comparison.Decision != "candidate" {
					t.Fatalf("실제 trace 효율 후보: %+v", comparison)
				}
			}
		})
	}
}

func TestBusinessTasksInvalidContracts(t *testing.T) {
	cases := map[string]func(*businessTaskInput){
		"schema":            func(i *businessTaskInput) { i.SchemaVersion = 2 },
		"source":            func(i *businessTaskInput) { i.Provenance = "raw_transcript" },
		"version":           func(i *businessTaskInput) { i.Version = "https://private.invalid" },
		"model":             func(i *businessTaskInput) { i.Controls.Agent.Model = "https://private.invalid" },
		"prompt":            func(i *businessTaskInput) { i.Controls.Agent.PromptFamily = "" },
		"tools":             func(i *businessTaskInput) { i.Controls.Tools = "" },
		"criteria":          func(i *businessTaskInput) { i.Controls.Success = "" },
		"cache":             func(i *businessTaskInput) { i.Controls.Cache = "" },
		"preparation":       func(i *businessTaskInput) { i.Controls.Preparation = "" },
		"timeout":           func(i *businessTaskInput) { i.Controls.TimeoutMS = nil },
		"turn_limit":        func(i *businessTaskInput) { i.Controls.MaxTurns = new(0) },
		"budget":            func(i *businessTaskInput) { i.Controls.Budget = nil },
		"negative_budget":   func(i *businessTaskInput) { i.Controls.Budget = new(-1) },
		"condition_lookup":  func(i *businessTaskInput) { i.Conditions[0].Lookup = nil },
		"multiple_axes":     func(i *businessTaskInput) { i.Conditions[1].Reduction = "masked" },
		"condition_order":   func(i *businessTaskInput) { slices.Reverse(i.Conditions) },
		"condition_missing": func(i *businessTaskInput) { i.Conditions = i.Conditions[:1] },
		"no_tasks":          func(i *businessTaskInput) { i.Tasks = nil },
		"duplicate_task":    func(i *businessTaskInput) { i.Tasks[1].Code = i.Tasks[0].Code },
		"unit":              func(i *businessTaskInput) { i.Tasks[0].Independent = "" },
		"unit_types": func(i *businessTaskInput) {
			i.Tasks[1].Independent = i.Tasks[0].Independent
			i.Tasks[1].Type = "resume"
		},
		"type":            func(i *businessTaskInput) { i.Tasks[0].Type = "personal_behavior" },
		"input_hash":      func(i *businessTaskInput) { i.Tasks[0].InputHash = "" },
		"observations":    func(i *businessTaskInput) { i.Tasks[0].ObservationHash = "" },
		"missing_run":     func(i *businessTaskInput) { i.Tasks[0].Runs = i.Tasks[0].Runs[:19] },
		"duplicate_run":   func(i *businessTaskInput) { i.Tasks[0].Runs[1] = i.Tasks[0].Runs[0] },
		"repeat":          func(i *businessTaskInput) { i.Tasks[0].Runs[0].Repeat = 11 },
		"order":           func(i *businessTaskInput) { i.Tasks[0].Runs[0].Order = 2 },
		"trace":           func(i *businessTaskInput) { i.Tasks[0].Runs[0].Trace = "" },
		"status":          func(i *businessTaskInput) { i.Tasks[0].Runs[0].Status = "excluded" },
		"time":            func(i *businessTaskInput) { i.Tasks[0].Runs[0].ElapsedMS = nil },
		"negative_time":   func(i *businessTaskInput) { i.Tasks[0].Runs[0].ElapsedMS = new(int64(-1)) },
		"turns":           func(i *businessTaskInput) { i.Tasks[0].Runs[0].Turns = nil },
		"over_turn_limit": func(i *businessTaskInput) { i.Tasks[0].Runs[0].Turns = new(21) },
		"tool_calls":      func(i *businessTaskInput) { i.Tasks[0].Runs[0].Tools = nil },
		"interventions":   func(i *businessTaskInput) { i.Tasks[0].Runs[0].Interventions = nil },
		"refetches":       func(i *businessTaskInput) { i.Tasks[0].Runs[0].EvidenceRefetches = nil },
		"evidence":        func(i *businessTaskInput) { i.Tasks[0].Runs[0].EvidenceAvailable = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			input := businessTasksFixture("lookup")
			change(&input)
			sealBusinessTasks(&input)
			if err := validateBusinessTasks(input); err == nil {
				t.Fatal("잘못된 입력을 허용했다")
			}
		})
	}
	input := businessTasksFixture("lookup")
	input.Controls.Success = "new-v2"
	if err := validateBusinessTasks(input); err == nil {
		t.Fatal("수정된 성공 조건의 과거 trace를 허용했다")
	}
}

func TestBusinessTasksCostArithmeticAndCompleteness(t *testing.T) {
	input := businessTasksFixture("lookup")
	run := input.Tasks[0].Runs[0]
	for i := range run.Costs {
		item := &run.Costs[i]
		item.Status, item.Allocation, item.Quantities = "measured", new(0.5), map[string]*float64{}
		for _, category := range businessCategories(item.Component) {
			item.Quantities[category] = new(1.0)
		}
	}
	if err := validateBusinessCosts(run.Costs, input.Pricing); err != nil {
		t.Fatal(err)
	}
	cost := businessRunCost("t", run, input.Pricing)
	if math.Abs(cost.Total.Total-0.042) > 1e-12 || math.Abs(cost.Phases["execution"].Total-0.014) > 1e-12 {
		t.Fatalf("금액 계산: %+v", cost)
	}
	tokens := businessRunTokens(run, "execution")
	for _, category := range businessTokenCategories {
		if tokens[category] != 3 {
			t.Fatalf("중복 토큰 또는 준비 토큰 혼입: %+v", tokens)
		}
	}
	for i := range run.Costs {
		run.Costs[i].Status = "measured"
		for category := range run.Costs[i].Quantities {
			run.Costs[i].Quantities[category] = new(0.0)
		}
	}
	if err := validateBusinessCosts(run.Costs, input.Pricing); err != nil {
		t.Fatal(err)
	}
	cost = businessRunCost("t", run, input.Pricing)
	if cost.Total.Total != 0 || cost.Total.Status != "measured" {
		t.Fatalf("실측 0: %+v", cost)
	}
	run.Costs[0].Status, run.Costs[0].Quantities, run.Costs[0].Allocation = "unmeasured", nil, nil
	cost = businessRunCost("t", run, input.Pricing)
	if cost.Total.Total != -1 || cost.Total.Status != "unmeasured" || cost.Total.KnownSubtotal != 0 || cost.Phases["execution"].Status != "measured" {
		t.Fatalf("불완전 총비용: %+v", cost)
	}
}

func TestBusinessTasksCostRejectsInvalidMeasurement(t *testing.T) {
	cases := map[string]func(*businessTaskInput){
		"currency":             func(i *businessTaskInput) { i.Pricing.Currency = "usd" },
		"date":                 func(i *businessTaskInput) { i.Pricing.AsOf = "2026-02-30" },
		"pricing":              func(i *businessTaskInput) { i.Pricing.Version = "" },
		"allocation_version":   func(i *businessTaskInput) { i.Pricing.Allocation = "" },
		"duplicate_rate":       func(i *businessTaskInput) { i.Pricing.Rates[1] = i.Pricing.Rates[0] },
		"missing_rate":         func(i *businessTaskInput) { delete(i.Pricing.Rates[0].Rates, "input") },
		"negative_rate":        func(i *businessTaskInput) { i.Pricing.Rates[0].Rates["input"] = new(-1.0) },
		"nan_rate":             func(i *businessTaskInput) { i.Pricing.Rates[0].Rates["input"] = new(math.NaN()) },
		"units":                func(i *businessTaskInput) { i.Pricing.Rates[0].Unit = "chars" },
		"zero_denominator":     func(i *businessTaskInput) { i.Pricing.Rates[0].UnitSize = 0 },
		"missing_phase":        func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs = i.Tasks[0].Runs[0].Costs[1:] },
		"duplicate_item":       func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[1] = i.Tasks[0].Runs[0].Costs[0] },
		"unknown_with_zero":    func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[0].Allocation = new(0.0) },
		"missing_quantity":     func(i *businessTaskInput) { delete(i.Tasks[0].Runs[0].Costs[10].Quantities, "input") },
		"null_quantity":        func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[10].Quantities["input"] = nil },
		"fractional_tokens":    func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[10].Quantities["input"] = new(0.5) },
		"negative_tokens":      func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[10].Quantities["input"] = new(-1.0) },
		"infinite_tokens":      func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[10].Quantities["input"] = new(math.Inf(1)) },
		"unknown_status":       func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[10].Status = "free" },
		"allocation_above_one": func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[10].Allocation = new(2.0) },
		"zero_allocation":      func(i *businessTaskInput) { i.Tasks[0].Runs[0].Costs[10].Allocation = new(0.0) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			input := businessTasksFixture("lookup")
			change(&input)
			sealBusinessTasks(&input)
			if err := validateBusinessTasks(input); err == nil {
				t.Fatal("잘못된 비용을 허용했다")
			}
		})
	}
}

func TestBusinessTasksAllOutcomesAndIndependentUnits(t *testing.T) {
	input := businessTasksFixture("lookup")
	for i := range input.Tasks {
		for j := range input.Tasks[i].Runs {
			run := &input.Tasks[i].Runs[j]
			run.Status = []string{"success", "failure", "timeout", "cancelled"}[(run.Repeat-1)%4]
			if run.Status == "timeout" {
				run.ElapsedMS = new(int64(1100))
			}
		}
	}
	if err := validateBusinessTasks(input); err != nil {
		t.Fatal(err)
	}
	report := summarizeBusinessTasks(input)
	summary := report.Summaries[0]
	if summary.Attempts != 30 || summary.StatusCounts["success"] != 9 || summary.StatusCounts["failure"] != 9 || summary.StatusCounts["timeout"] != 6 || summary.StatusCounts["cancelled"] != 6 || summary.SuccessRate != 0.3 {
		t.Fatalf("분모: %+v", summary)
	}
	if summary.All["elapsed_ms"].Mean != 300 || summary.SuccessOnly["elapsed_ms"].Mean != 100 {
		t.Fatalf("전체/성공 시간: %+v", summary)
	}
	input.Tasks[1].Independent = input.Tasks[0].Independent
	input.Tasks[1].Runs[0].ElapsedMS = new(int64(200))
	sealBusinessTasks(&input)
	report = summarizeBusinessTasks(input)
	if report.Comparisons[0].Metrics[0].Samples != 2 {
		t.Fatal("반복/과업을 독립 단위로 부풀렸다")
	}
	if got := report.Summaries[0].All["elapsed_ms"].Mean; got != 302.5 {
		t.Fatalf("과업→독립 단위 평균: %v", got)
	}
	for i := range input.Tasks {
		for j := range input.Tasks[i].Runs {
			input.Tasks[i].Runs[j].Status = "failure"
		}
	}
	if got := summarizeBusinessTasks(input).Summaries[0].SuccessOnly["elapsed_ms"]; got.Mean != -1 || got.Samples != 0 {
		t.Fatalf("성공 없음: %+v", got)
	}
}

func TestBusinessTasksDecisionAndDeterministicAggregation(t *testing.T) {
	for _, name := range []string{"regression", "missing_cost", "missing_tokens", "no_improvement", "insufficient", "evidence", "summary_cost"} {
		t.Run(name, func(t *testing.T) {
			input := businessTasksFixture("reduction")
			input.Provenance = "agent_trace"
			want := "deferred"
			switch name {
			case "regression":
				for i := range input.Tasks {
					for j := range input.Tasks[i].Runs {
						if input.Tasks[i].Runs[j].Condition == "summary" {
							input.Tasks[i].Runs[j].Status = "failure"
						}
					}
				}
				want = "rejected"
			case "missing_cost":
				input.Tasks[0].Runs[1].Costs[4].Status = "unmeasured"
			case "missing_tokens":
				item := &input.Tasks[0].Runs[1].Costs[10]
				item.Status, item.Quantities, item.Allocation = "unmeasured", nil, nil
			case "no_improvement":
				for i := range input.Tasks {
					for j := range input.Tasks[i].Runs {
						run := &input.Tasks[i].Runs[j]
						run.ElapsedMS, run.Turns = new(int64(100)), new(10)
						for k := range run.Costs {
							for category := range run.Costs[k].Quantities {
								run.Costs[k].Quantities[category] = new(100.0)
							}
						}
					}
				}
			case "insufficient":
				for i := range input.Tasks {
					input.Tasks[i].Independent = "shared"
				}
			case "evidence":
				input.Tasks[0].Runs[1].EvidenceAvailable = new(false)
			case "summary_cost":
				for i := range input.Tasks[0].Runs[1].Costs {
					item := &input.Tasks[0].Runs[1].Costs[i]
					if item.Component == "summary" {
						item.Status, item.Quantities, item.Allocation = "not_applicable", nil, nil
					}
				}
				sealBusinessTasks(&input)
				if validateBusinessTasks(input) == nil {
					t.Fatal("요약 비용 누락 허용")
				}
				return
			}
			sealBusinessTasks(&input)
			if err := validateBusinessTasks(input); err != nil {
				t.Fatal(err)
			}
			report := summarizeBusinessTasks(input)
			if report.Comparisons[0].Decision != want {
				t.Fatalf("판정: %+v", report.Comparisons[0])
			}
			if name == "missing_cost" && report.Summaries[1].All["total_cost"].Status != "partially_measured" {
				t.Fatal("일부 단위의 미측정 비용을 완전한 관측으로 표시했다")
			}
		})
	}
	input := businessTasksFixture("lookup")
	want := summarizeBusinessTasks(input)
	slices.Reverse(input.Tasks)
	for i := range input.Tasks {
		slices.Reverse(input.Tasks[i].Runs)
	}
	if got := summarizeBusinessTasks(input); !reflect.DeepEqual(got, want) {
		t.Fatal("입력 순서만 바뀌어 집계가 달라졌다")
	}
}

func TestBusinessTasksStrictJSONAndPrivacy(t *testing.T) {
	input := businessTasksFixture("lookup")
	raw, _ := json.Marshal(input)
	for _, field := range []string{`"model_turns":10`, `"tool_calls":0`, `"input":100`, `"elapsed_ms":100`} {
		t.Run(field, func(t *testing.T) {
			key, _, _ := strings.Cut(field, ":")
			for _, replacement := range []string{key + ":null", `"private_body":"secret-content",` + field} {
				path := filepath.Join(t.TempDir(), "input.json")
				if err := os.WriteFile(path, []byte(strings.Replace(string(raw), field, replacement, 1)), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := loadBusinessTasks(path); err == nil || strings.Contains(err.Error(), "secret-content") || strings.Contains(err.Error(), "private_body") {
					t.Fatalf("필수 숫자/안전 오류: %v", err)
				}
			}
		})
	}
}

func TestBusinessTasksActualCommand(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EVAL_TEST_COMMAND_PROCESS", "1")
	t.Setenv("DATABASE_URL", "invalid-must-not-connect")
	for _, experiment := range []string{"lookup", "irrelevant", "reduction"} {
		t.Run(experiment, func(t *testing.T) {
			input := businessTasksFixture(experiment)
			path := writeBusinessTasks(t, input)
			out := filepath.Join(t.TempDir(), "report.json")
			output, err := exec.CommandContext(t.Context(), binary, "-test.run=^TestEvalCommandProcess$", "--", "-business-tasks="+path, "-out="+out).CombinedOutput()
			if err != nil {
				t.Fatalf("CLI: %v %s", err, output)
			}
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			var report businessTasksReport
			if err := json.Unmarshal(raw, &report, json.RejectUnknownMembers(true)); err != nil {
				t.Fatal(err)
			}
			if report.StartedAt.IsZero() || report.Source != "imported_task_trace" || report.DefaultActivation {
				t.Fatalf("잘못된 보고서: %+v", report)
			}
			if strings.Contains(string(raw), input.Tasks[0].ObservationHash) || strings.Contains(string(raw), input.Tasks[0].InputHash) {
				t.Fatal("원문 식별 해시가 보고서에 노출되었다")
			}
		})
	}
	for _, extra := range [][]string{{"-business=missing"}, {"-continual"}, {"-unit-comparison=missing"}, {"-repeat=10"}, {"-budget=10"}, {"-prepared-input=missing"}} {
		t.Run(strings.Join(extra, ""), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "must-not-exist.json")
			args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-business-tasks=missing", "-out=" + out}
			output, err := exec.CommandContext(t.Context(), binary, append(args, extra...)...).CombinedOutput()
			if err == nil || strings.Contains(string(output), "읽") || strings.Contains(string(output), "invalid-must-not-connect") {
				t.Fatalf("모드 사전 거부: %v %s", err, output)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("잘못된 모드가 파일을 생성했다")
			}
		})
	}
}

func TestBusinessTasksPreparationAndUnknownPricing(t *testing.T) {
	input := businessTasksFixture("lookup")
	input.Pricing.Rates[0].Rates["input"] = nil
	input.Tasks[0].Runs[0].PreparationMS = nil
	input.Tasks[0].Runs[0].ReindexMS = nil
	for i := range input.Tasks {
		for j := range input.Tasks[i].Runs {
			if input.Tasks[i].Runs[j].Repeat == 1 {
				input.Tasks[i].Runs[j].IndexState = "initial"
			}
		}
	}
	sealBusinessTasks(&input)
	if err := validateBusinessTasks(input); err != nil {
		t.Fatal(err)
	}
	report := summarizeBusinessTasks(input)
	cost := businessRunCost(input.Tasks[0].Code, input.Tasks[0].Runs[0], input.Pricing)
	if cost.Total.Total != -1 || cost.TimesMS["preparation"] != -1 || cost.TimesMS["reindex"] != -1 {
		t.Fatalf("미측정 비용/시간을 0으로 채웠다: %+v", cost)
	}
	if report.Summaries[0].All["total_tokens"].Mean != 400 || report.Summaries[0].All["total_cost"].Mean != -1 || report.Summaries[0].IndexStates["initial"] != 3 || report.Summaries[0].IndexStates["prepared"] != 27 {
		t.Fatalf("단가/사용량·실행 구분: %+v", report.Summaries[0])
	}
	for _, change := range []func(*businessTaskInput){
		func(i *businessTaskInput) { i.Tasks[0].Runs[0].IndexState = "unknown" },
		func(i *businessTaskInput) { i.Tasks[0].Runs[0].IndexState = "prepared" },
		func(i *businessTaskInput) { i.Tasks[0].Runs[0].PreparationMS = new(int64(-1)) },
		func(i *businessTaskInput) { i.Tasks[1].InputHash = i.Tasks[0].InputHash },
	} {
		copy := businessTasksFixture("lookup")
		for i := range copy.Tasks {
			for j := range copy.Tasks[i].Runs {
				if copy.Tasks[i].Runs[j].Repeat == 1 {
					copy.Tasks[i].Runs[j].IndexState = "initial"
				}
			}
		}
		change(&copy)
		sealBusinessTasks(&copy)
		if validateBusinessTasks(copy) == nil {
			t.Fatal("준비 통제/독립 단위 위반 허용")
		}
	}
}

func TestBusinessTasksTypesAndAdverseMetrics(t *testing.T) {
	input := businessTasksFixture("lookup")
	task := businessTasksFixture("lookup").Tasks[0]
	task.Code, task.Independent, task.InputHash = "task-3", "unit-3", strings.Repeat("f", 64)
	input.Tasks = append(input.Tasks, task)
	for i, kind := range []string{"resume", "handoff", "fact", "multi_evidence"} {
		input.Tasks[i].Type = kind
	}
	// 전체 시간 개선과 다른 지표 악화를 함께 보고한다.
	for i := range input.Tasks {
		for j := range input.Tasks[i].Runs {
			if input.Tasks[i].Runs[j].Condition == "with_context" {
				input.Tasks[i].Runs[j].Tools = new(10)
				input.Tasks[i].Runs[j].Interventions = new(2)
			}
		}
	}
	sealBusinessTasks(&input)
	if err := validateBusinessTasks(input); err != nil {
		t.Fatal(err)
	}
	report := summarizeBusinessTasks(input)
	if len(report.Summaries) != 10 || len(report.Comparisons) != 5 {
		t.Fatal("과업 유형을 합쳤다")
	}
	for _, metric := range report.Comparisons[0].Metrics {
		if metric.Metric == "tool_calls" && (!metric.Significant || metric.Improved || metric.Difference != 10) {
			t.Fatalf("악화를 숨겼다: %+v", metric)
		}
	}
	for _, entry := range report.Comparisons[1:] {
		if entry.Decision != "deferred" || entry.IndependentUnits != 1 {
			t.Fatalf("유형별 표본 부족: %+v", entry)
		}
	}
}

func TestBusinessLegacyRequiredNumbers(t *testing.T) {
	raw := `{"completion_time_ms":100,"rework_count":0,"resume_interactions":0,"collaboration_usage":0}`
	for _, field := range []string{`"completion_time_ms":100`, `"rework_count":0`, `"resume_interactions":0`, `"collaboration_usage":0`} {
		t.Run(field, func(t *testing.T) {
			key, _, _ := strings.Cut(field, ":")
			for _, replacement := range []string{key + ":null", key + `:"0"`} {
				var metric businessMetrics
				if err := json.Unmarshal([]byte(strings.Replace(raw, field, replacement, 1)), &metric); err == nil {
					t.Fatal("필수 숫자에 null/문자열을 허용했다")
				}
			}
			missing := strings.Replace(raw, field+",", "", 1)
			if missing == raw {
				missing = strings.Replace(raw, ","+field, "", 1)
			}
			var metric businessMetrics
			if err := json.Unmarshal([]byte(missing), &metric); err == nil {
				t.Fatal("누락을 측정 0으로 처리했다")
			}
		})
	}
	var zero businessMetrics
	if err := json.Unmarshal([]byte(raw), &zero); err != nil {
		t.Fatal(err)
	}
	if err := validateBusinessMetrics(1, "with_context", zero); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessTasksInputLimitAndReportFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((32 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBusinessTasks(path); err == nil {
		t.Fatal("입력 크기 상한을 무시했다")
	}
	input := writeBusinessTasks(t, businessTasksFixture("lookup"))
	if err := runBusinessTasks(input, filepath.Join(t.TempDir(), "missing", "report.json")); err == nil {
		t.Fatal("출력 실패를 성공으로 처리했다")
	}
}
