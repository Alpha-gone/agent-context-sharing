package main

import (
	"context"
	"crypto/sha256"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func trialFixture(t *testing.T, experiment string) (executionManifest, trialBindings, businessTaskInput) {
	t.Helper()
	input := businessTasksFixture(experiment)
	m := validManifest()
	m.TrialID = "trial-" + experiment
	m.Mode = "business_tasks"
	m.Provenance = input.Provenance
	m.Models.AgentVersion, m.Models.AgentModel, m.Models.PromptVersion = input.Controls.Agent.Version, input.Controls.Agent.Model, input.Controls.Agent.PromptFamily
	m.Search.Budget = input.Controls.Budget
	m.Mapping = manifestMapping{Preprocessing: "direct-v1", Split: "direct-v1", Answers: "direct-v1", Evidence: "direct-v1", AnswerPolicy: "evaluation_only"}
	m.Execution.Preparation, m.Execution.Cache, m.Execution.TimeoutMS = input.Controls.Preparation, input.Controls.Cache, int(*input.Controls.TimeoutMS)
	m.Execution.Seeds = []manifestSeed{{Component: "agent", Supported: new(false)}}
	path := writeBusinessTasks(t, input)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Inputs = []manifestInput{{Code: "tasks", Dataset: "tasks", Role: "business_tasks", Version: input.Version, Commit: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}}
	m.Units = nil
	for _, task := range input.Tasks {
		m.Units = append(m.Units, manifestUnit{Dataset: "tasks", Query: task.Code, Task: task.Independent, Type: task.Type, Independent: task.Independent})
	}
	m.Exclusions = manifestExclusions{Policy: "direct-v1", Total: len(m.Units), Items: []manifestExclusion{}}
	m.Cost.PricingVersion, m.Cost.AsOf, m.Cost.Currency, m.Cost.Allocation = input.Pricing.Version, input.Pricing.AsOf, input.Pricing.Currency, input.Pricing.Allocation
	tmp := manifestTrial{Manifest: m}
	r := summarizeBusinessTasks(input)
	m.Cost.Items = tmp.resultCost(&r).Items
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	return m, trialBindings{"tasks": path}, input
}

func readTrialFile(t *testing.T, root, id, name string, target any) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, id, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target, json.RejectUnknownMembers(true)); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return raw
}

func assertTrialFiles(t *testing.T, root, id string, success bool) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, id))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("공개 파일 권한/종류: %v %v", info, err)
		}
	}
	want := []string{"cost.json", "manifest.json", "preparation.json", "summary.json"}
	if !success {
		want = []string{"failure.json", "manifest.json"}
	}
	if !slices.Equal(names, want) {
		t.Fatalf("불완전/비공개 파일 공개: %v", names)
	}
	remaining, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Name() != id {
		t.Fatalf("임시 파일/잠금 누수: %v", remaining)
	}
}

// TestManifestTrialBusinessCostMismatch는 실제 비용을 산정하는 확장 업무 과업이 측정 뒤 산정
// 금액과 다른 선언을 cost_mismatch로 거부하는 사후 대조를 유지하는지 확인한다.
func TestManifestTrialBusinessCostMismatch(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EVAL_TEST_COMMAND_PROCESS", "1")
	t.Setenv("DATABASE_URL", "private-do-not-connect")
	m, bindings, _ := trialFixture(t, "lookup")
	index := slices.IndexFunc(m.Cost.Items, func(item manifestCostItem) bool { return item.Status == "measured" })
	if index < 0 {
		t.Fatal("측정 비용 항목이 없다")
	}
	m.Cost.Items[index].Value = new(*m.Cost.Items[index].Value + 1)
	root := filepath.Join(t.TempDir(), "results")
	logs, err := exec.CommandContext(t.Context(), binary, "-test.run=^TestEvalCommandProcess$", "--", "-manifest="+writeManifestFixture(t, m), "-results="+root, "-trial-input=tasks="+bindings["tasks"]).CombinedOutput()
	if err == nil {
		t.Fatalf("산정 금액과 다른 선언을 허용했다: %s", logs)
	}
	assertTrialFiles(t, root, m.TrialID, false)
	var failure trialFailureReport
	readTrialFile(t, root, m.TrialID, "failure.json", &failure)
	if failure.Code != "cost_mismatch" {
		t.Fatalf("실패 코드 = %s", failure.Code)
	}
}

func TestManifestTrialActualBusinessCommands(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EVAL_TEST_COMMAND_PROCESS", "1")
	t.Setenv("DATABASE_URL", "private-do-not-connect")
	for _, experiment := range []string{"lookup", "irrelevant", "reduction"} {
		t.Run(experiment, func(t *testing.T) {
			m, bindings, input := trialFixture(t, experiment)
			root := filepath.Join(t.TempDir(), "results")
			manifest := writeManifestFixture(t, m)
			logs, err := exec.CommandContext(t.Context(), binary, "-test.run=^TestEvalCommandProcess$", "--", "-manifest="+manifest, "-results="+root, "-trial-input=tasks="+bindings["tasks"]).CombinedOutput()
			if err != nil {
				t.Fatalf("manifest CLI: %v %s", err, logs)
			}
			assertTrialFiles(t, root, m.TrialID, true)
			var summary trialSummary
			raw := readTrialFile(t, root, m.TrialID, "summary.json", &summary)
			if summary.Source != "imported_aggregate_trace" || summary.DefaultActivation || summary.Status != "completed" {
				t.Fatalf("수입/실행 상태: %+v", summary)
			}
			for _, d := range summary.Decisions {
				if d.Status != "deferred" {
					t.Fatalf("합성 채택: %+v", d)
				}
			}
			for _, private := range []string{bindings["tasks"], input.Tasks[0].ObservationHash, "private-do-not-connect"} {
				if strings.Contains(string(raw), private) || strings.Contains(string(logs), private) {
					t.Fatal("비공개 값 노출")
				}
			}
			var cost trialCost
			readTrialFile(t, root, m.TrialID, "cost.json", &cost)
			if cost.Total.Status != "measured" || cost.Total.Total <= 0 || len(cost.Details) != len(input.Tasks)*len(input.Conditions)*10 {
				t.Fatalf("실제 산정 비용 누락: %+v", cost.Total)
			}
			var prep trialPreparation
			readTrialFile(t, root, m.TrialID, "preparation.json", &prep)
			if prep.Readiness != "not_applicable" || prep.Pending != -1 || prep.VectorBytes != -1 || prep.Costs["execution"].Status != "measured" {
				t.Fatal("수입 기록을 실제 색인 계측으로 표시했다")
			}
			if len(prep.Checks) != 1 || prep.Units != 3 || prep.Independent != 3 || prep.Progress.Cleanup != "not_applicable" || prep.Progress.Requests != 0 {
				t.Fatalf("가져온 기록을 재실행으로 표시했다: %+v", prep)
			}
		})
	}
}

func TestManifestTrialRejectsMismatchedEvidence(t *testing.T) {
	cases := map[string]func(*executionManifest, trialBindings){
		"checksum":        func(m *executionManifest, b trialBindings) { m.Inputs[0].SHA256 = strings.Repeat("0", 64) },
		"version":         func(m *executionManifest, b trialBindings) { m.Inputs[0].Version = "v2" },
		"unit":            func(m *executionManifest, b trialBindings) { m.Units[0].Type = "resume" },
		"agent":           func(m *executionManifest, b trialBindings) { m.Models.AgentVersion = "agent-v2" },
		"model":           func(m *executionManifest, b trialBindings) { m.Models.AgentModel = "model-v2" },
		"prompt":          func(m *executionManifest, b trialBindings) { m.Models.PromptVersion = "prompt-v2" },
		"budget":          func(m *executionManifest, b trialBindings) { m.Search.Budget = new(10) },
		"cache":           func(m *executionManifest, b trialBindings) { m.Execution.Cache = "cache-v2" },
		"preparation":     func(m *executionManifest, b trialBindings) { m.Execution.Preparation = "prepared-v2" },
		"pricing_version": func(m *executionManifest, b trialBindings) { m.Cost.PricingVersion = "v2" },
		"currency":        func(m *executionManifest, b trialBindings) { m.Cost.Currency = "EUR" },
		"allocation":      func(m *executionManifest, b trialBindings) { m.Cost.Allocation = "v2" },
		"mapping":         func(m *executionManifest, b trialBindings) { m.Mapping.Answers = "v2" },
		"seed": func(m *executionManifest, b trialBindings) {
			m.Execution.Seeds[0].Supported = new(true)
			m.Execution.Seeds[0].Value = new(int64(0))
		},
		"missing_binding": func(m *executionManifest, b trialBindings) { delete(b, "tasks") },
		"unused_binding":  func(m *executionManifest, b trialBindings) { b["unused"] = "private-secret-input" },
		"cost_value":      func(m *executionManifest, b trialBindings) { m.Cost.Items[0].Value = new(*m.Cost.Items[0].Value + 1) },
		"cost_availability": func(m *executionManifest, b trialBindings) {
			m.Cost.Items[0].Status = "unmeasured"
			m.Cost.Items[0].Value = nil
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m, b, _ := trialFixture(t, "lookup")
			change(&m, b)
			root := t.TempDir()
			err := runManifestTrial(writeManifestFixture(t, m), root, b, "")
			if err == nil {
				t.Fatal("불일치 입력을 실행했다")
			}
			assertTrialFiles(t, root, m.TrialID, false)
			raw, err := os.ReadFile(filepath.Join(root, m.TrialID, "failure.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "private-secret-input") || strings.Contains(string(raw), "tasks.json") {
				t.Fatal("실패 보고에 입력 경로를 공개했다")
			}
		})
	}
}

func TestManifestTrialAcceptedAndIncompleteCost(t *testing.T) {
	for _, outcome := range []string{"accepted", "unmeasured", "success_regression"} {
		t.Run(outcome, func(t *testing.T) {
			m, b, input := trialFixture(t, "lookup")
			input.Provenance = "agent_trace"
			m.Provenance = "agent_trace"
			if outcome == "unmeasured" {
				input.Pricing.Rates[0].Rates["input"] = nil
			}
			if outcome == "success_regression" {
				for i := range input.Tasks {
					for j := range input.Tasks[i].Runs {
						if input.Tasks[i].Runs[j].Condition == "with_context" {
							input.Tasks[i].Runs[j].Status = "failure"
						}
					}
				}
			}
			sealBusinessTasks(&input)
			b["tasks"] = writeBusinessTasks(t, input)
			raw, _ := os.ReadFile(b["tasks"])
			m.Inputs[0].SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
			r := summarizeBusinessTasks(input)
			tmp := manifestTrial{Manifest: m}
			m.Cost.Items = tmp.resultCost(&r).Items
			root := t.TempDir()
			if err := runManifestTrial(writeManifestFixture(t, m), root, b, ""); err != nil {
				t.Fatal(err)
			}
			var summary trialSummary
			readTrialFile(t, root, m.TrialID, "summary.json", &summary)
			want := map[string]string{"accepted": "accepted", "unmeasured": "deferred", "success_regression": "not_accepted"}[outcome]
			for _, d := range summary.Decisions {
				if d.Status != want {
					t.Fatalf("판정: %+v", d)
				}
			}
			if summary.DefaultActivation {
				t.Fatal("운영 기본값 변경")
			}
		})
	}
}

func TestManifestTrialNeverClobbersExistingID(t *testing.T) {
	for _, kind := range []string{"empty_directory", "previous_results", "symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			m, b, _ := trialFixture(t, "lookup")
			root := t.TempDir()
			target := filepath.Join(root, m.TrialID)
			sentinel := []byte("previous-results")
			switch kind {
			case "empty_directory":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			case "previous_results":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "summary.json"), sentinel, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), target); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(target, sentinel, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := runManifestTrial(writeManifestFixture(t, m), root, b, ""); err == nil {
				t.Fatal("기존 ID 덮어쓰기")
			}
			if kind == "previous_results" {
				raw, _ := os.ReadFile(filepath.Join(target, "summary.json"))
				if string(raw) != string(sentinel) {
					t.Fatal("기존 결과 변경")
				}
			}
		})
	}
	from, to := filepath.Join(t.TempDir(), "from"), filepath.Join(t.TempDir(), "to")
	if err := os.Mkdir(from, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(to, 0700); err != nil {
		t.Fatal(err)
	}
	if err := renameTrialExclusive(from, to); err == nil {
		t.Fatal("빈 기존 디렉터리도 교체했다")
	}
}

func TestManifestTrialConcurrentReservation(t *testing.T) {
	m, _, _ := trialFixture(t, "lookup")
	root := t.TempDir()
	var mu sync.Mutex
	var winners []*manifestTrial
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			trial, err := newManifestTrial(m, root)
			if err == nil {
				mu.Lock()
				winners = append(winners, trial)
				mu.Unlock()
			}
		})
	}
	group.Wait()
	if len(winners) != 1 {
		t.Fatalf("같은 ID 동시 실행 %d개", len(winners))
	}
	if err := winners[0].finish(errors.New("private-provider-secret")); err == nil {
		t.Fatal("실패 상태 소실")
	}
	assertTrialFiles(t, root, m.TrialID, false)
}

func TestManifestTrialAtomicFailureAndMalformedResult(t *testing.T) {
	for _, failure := range []string{"partial_publication", "missing_fields", "invalid_json", "cancelled", "expired", "cleanup_failed"} {
		t.Run(failure, func(t *testing.T) {
			m, b, input := trialFixture(t, "lookup")
			root := t.TempDir()
			trial, err := newManifestTrial(m, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := trial.bindInputs(b, ""); err != nil {
				t.Fatal(err)
			}
			trial.Progress.setStage("publication")
			r := summarizeBusinessTasks(input)
			r.StartedAt = time.Now().UTC()
			if err := writeReport(filepath.Join(trial.Private, "result.json"), r); err != nil {
				t.Fatal(err)
			}
			var cause error
			switch failure {
			case "partial_publication":
				if err := os.Mkdir(filepath.Join(trial.Bundle, "cost.json"), 0700); err != nil {
					t.Fatal(err)
				}
			case "missing_fields":
				if err := os.WriteFile(filepath.Join(trial.Private, "result.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid_json":
				if err := os.WriteFile(filepath.Join(trial.Private, "result.json"), []byte(`{"private-body":"secret"`), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cause = context.Canceled
			case "expired":
				trial.Progress.deadline = time.Now().Add(-time.Second)
			case "cleanup_failed":
				id, _ := model.NewID()
				trial.Progress.active[id] = true
				trial.Progress.value.Graphs = 1
				trial.Progress.value.CleanupFailures = 1
			}
			err = trial.finish(cause)
			if err == nil {
				t.Fatal("실패/정리 누락을 성공 처리했다")
			}
			if failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("취소 원인 소실")
			}
			assertTrialFiles(t, root, m.TrialID, false)
			raw, _ := os.ReadFile(filepath.Join(root, m.TrialID, "failure.json"))
			if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), trial.Private) {
				t.Fatal("실패 원문 노출")
			}
		})
	}
	if err := writeTrialJSON(t.TempDir(), "nan.json", math.NaN()); err == nil {
		t.Fatal("불완전 JSON 공개")
	}
}

func TestManifestTrialProgressAndRequestBudget(t *testing.T) {
	p := newTrialProgress(manifestExecution{RequestLimit: 2, IndexRoundLimit: 3, TimeoutMS: 1000})
	ctx := context.WithValue(t.Context(), trialContextKey{}, p)
	for range 2 {
		if err := trialRequest(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := trialRequest(ctx); err == nil {
		t.Fatal("요청 예산 무시")
	}
	if trialIndexLimit(ctx, 10) != 3 {
		t.Fatal("색인 예산 무시")
	}
	id, _ := model.NewID()
	trialGraphCreated(ctx, id)
	trialRoundDone(ctx, "b", "sample", 2)
	trialRoundDone(ctx, "a", "sample", 1)
	trialGraphCleanup(ctx, id, nil)
	s := p.snapshot()
	if !s.LimitExceeded || s.Requests != 2 || s.Graphs != 1 || s.ActiveGraphs != 0 || s.Cleanup != "completed" || s.Completed[0].Condition != "a" {
		t.Fatalf("실제 진행/정리: %+v", s)
	}
	trialGraphCreated(ctx, id)
	trialGraphCleanup(ctx, id, context.Canceled)
	if p.snapshot().Cleanup != "failed" {
		t.Fatal("정리 실패를 숨겼다")
	}
	if err := trialRequest(context.WithoutCancel(context.Background())); err != nil {
		t.Fatal(err)
	}
}

func TestManifestTrialPreflightAndFollowup(t *testing.T) {
	for _, args := range [][]string{{"-manifest=missing"}, {"-manifest=missing", "-results=unused", "-business=missing"}, {"-trial-input=tasks=missing"}, {"-results=unused"}, {"-manifest=missing", "-results=unused", "-out=unused"}} {
		if err := runEvaluation(args, nil); err == nil {
			t.Fatalf("사전 거부 없음: %v", args)
		}
	}
	if err := runEvaluation([]string{"-manifest=", "-business=missing"}, nil); err == nil || !strings.Contains(err.Error(), "manifest 실행") {
		t.Fatal("빈 manifest 옵션을 무시하고 다른 모드를 실행했다")
	}
	var bindings trialBindings
	if bindings.Set("bad-code") == nil {
		t.Fatal("잘못된 연결 허용")
	}
	if err := bindings.Set("tasks=path"); err != nil {
		t.Fatal(err)
	}
	if bindings.Set("tasks=other") == nil {
		t.Fatal("중복 연결 허용")
	}
	m, _, _ := trialFixture(t, "lookup")
	m.Followup = &manifestFollowup{Parent: "previous", Purpose: "additional_independent_tasks", Samples: len(m.Units), Repeats: 10}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*manifestFollowup){func(f *manifestFollowup) { f.Parent = m.TrialID }, func(f *manifestFollowup) { f.Purpose = "" }, func(f *manifestFollowup) { f.Repeats = 3 }, func(f *manifestFollowup) { f.Samples++ }} {
		f := *m.Followup
		change(&f)
		copy := m
		copy.Followup = &f
		if copy.validate() == nil {
			t.Fatal("사전 고정 없는 추가 시험 허용")
		}
	}
	before := reflect.ValueOf(m).Interface()
	if !reflect.DeepEqual(before, m) {
		t.Fatal("검사가 입력을 바꿨다")
	}
}
