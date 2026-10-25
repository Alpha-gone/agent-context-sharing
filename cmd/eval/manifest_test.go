package main

import (
	json "encoding/json/v2"
	"flag"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func validManifest() executionManifest {
	// checksum·commit은 형식 검증용 합성 값이며 실제 데이터 판으로 사용하지 않는다.
	return executionManifest{
		SchemaVersion: 1, TrialID: "synthetic-01", Mode: "basic",
		Inputs: []manifestInput{
			{Code: "input-01", Dataset: "synthetic", Role: "contexts", Version: "v1", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64)},
			{Code: "queries-01", Dataset: "synthetic", Role: "queries", Version: "v1", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("c", 64)},
		},
		Mapping: manifestMapping{Preprocessing: "v1", Split: "v1", Answers: "v1", Evidence: "v1", AnswerPolicy: "evaluation_only"},
		Units: []manifestUnit{
			{Dataset: "synthetic", Query: "q1", Conversation: "c1", Type: "fact", Independent: "i1"},
			{Dataset: "synthetic", Query: "q2", Conversation: "c1", Type: "time", Independent: "i1"},
			{Dataset: "synthetic", Query: "q3", Task: "t1", Type: "handoff", Independent: "i2"},
		},
		Models:     manifestModels{AgentVersion: "agent-v1", AgentModel: "vendor/model:1", PromptVersion: "prompt-v1", Embedding: "gemini:gemini-embedding-2:retrieval-v1:vector:768", Dimension: 768, VectorType: "vector", Compression: "none"},
		Search:     manifestSearch{Stages: []string{"baseline", "references"}, IndexTargets: "all_layers", ChannelExecution: "parallel", EvidenceSelection: new(false), GlobalFallback: new(false), Budget: new(0), MaxHops: new(2), MaxHopNodes: new(0), CandidateLimit: 25, SemanticThreshold: new(0.5), FoldThreshold: new(0.9)},
		Execution:  manifestExecution{Repeats: 10, Order: "round_robin", RequestLimit: 100, IndexRoundLimit: 100, TimeoutMS: 30000, Preparation: "warm-v1", Cache: "same-warm-v1", Seeds: []manifestSeed{{Component: "agent", Supported: new(true), Value: new(int64(0))}, {Component: "embedding", Supported: new(false)}}},
		Exclusions: manifestExclusions{Policy: "v1", Total: 3, Items: []manifestExclusion{}},
		Cost: manifestCost{PricingVersion: "synthetic-v1", AsOf: "2026-10-25", Currency: "USD", Allocation: "per-task-v1", Items: []manifestCostItem{
			{Code: "agent", Status: "measured", Value: new(0.0)},
			{Code: "embedding", Status: "unmeasured"}, {Code: "summary", Status: "not_applicable"},
			{Code: "search", Status: "unmeasured"}, {Code: "storage", Status: "unmeasured"},
		}},
	}
}

func TestManifestModesAndMeasuredZero(t *testing.T) {
	for _, mode := range []string{"basic", "convert", "business", "continual", "adaptive", "consistency", "adversarial"} {
		t.Run(mode, func(t *testing.T) {
			m := validManifest()
			m.Mode = mode
			m.Exclusions.Total++
			m.Exclusions.Items = []manifestExclusion{{Dataset: "synthetic", Query: "q4", Reason: "image-only"}}
			switch mode {
			case "convert":
				m.Conversion = &manifestConversion{Format: "own-adversarial"}
				m.Inputs[0].Role = "source"
			case "business":
				m.Inputs[0].Role = "business"
			case "adversarial":
				input := m.Inputs[0]
				input.Code, input.Role = "attacks-01", "attacks"
				m.Inputs = append(m.Inputs, input)
			case "adaptive":
				m.Search.DirectThresholds, m.Search.DirectMargins = []float64{0, 1}, []float64{0, 1}
			case "consistency":
				m.Consistency = &manifestConsistency{Stage: "references", Requests: 1, LoadRequests: 1, Concurrency: []int{1}, WritePauses: []int{0}}
			}
			path := writeManifestFixture(t, m)
			loaded, err := loadManifest(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Cost.Items[0].Value == nil || *loaded.Cost.Items[0].Value != 0 || loaded.Cost.Items[1].Value != nil || loaded.Execution.Seeds[0].Value == nil || *loaded.Execution.Seeds[0].Value != 0 {
				t.Fatal("측정한 0과 미측정을 구분하지 못했다")
			}
		})
	}
}

func TestManifestInvalidContracts(t *testing.T) {
	cases := map[string]func(*executionManifest){
		"schema":              func(m *executionManifest) { m.SchemaVersion = 2 },
		"mode":                func(m *executionManifest) { m.Mode = "unknown" },
		"unsafe_code":         func(m *executionManifest) { m.TrialID = "https://private.invalid" },
		"no_inputs":           func(m *executionManifest) { m.Inputs = nil },
		"missing_query_input": func(m *executionManifest) { m.Inputs = m.Inputs[:1] },
		"sha256":              func(m *executionManifest) { m.Inputs[0].SHA256 = "not-measured" },
		"unpinned_commit":     func(m *executionManifest) { m.Inputs[0].Commit = "main" },
		"input_duplicate":     func(m *executionManifest) { m.Inputs = append(m.Inputs, m.Inputs[0]) },
		"annotation_policy":   func(m *executionManifest) { m.Mapping.AnswerPolicy = "memory" },
		"mapping_version":     func(m *executionManifest) { m.Mapping.Evidence = "" },
		"unknown_dataset":     func(m *executionManifest) { m.Units[0].Dataset = "unknown" },
		"query_duplicate":     func(m *executionManifest) { m.Units[1].Query = "q1" },
		"unit_split":          func(m *executionManifest) { m.Units[1].Independent = "i3" },
		"unit_merged":         func(m *executionManifest) { m.Units[2].Independent = "i1" },
		"both_group_types":    func(m *executionManifest) { m.Units[0].Task = "t2" },
		"no_group":            func(m *executionManifest) { m.Units[0].Conversation = "" },
		"model_missing":       func(m *executionManifest) { m.Models.AgentModel = "" },
		"prompt_missing":      func(m *executionManifest) { m.Models.PromptVersion = "" },
		"dimension":           func(m *executionManifest) { m.Models.Dimension = 2001 },
		"invalid_stage":       func(m *executionManifest) { m.Search.Stages = []string{"baseline", ""} },
		"duplicate_stage":     func(m *executionManifest) { m.Search.Stages = []string{"baseline", "baseline"} },
		"null_budget":         func(m *executionManifest) { m.Search.Budget = nil },
		"negative_budget":     func(m *executionManifest) { m.Search.Budget = new(-1) },
		"null_policy":         func(m *executionManifest) { m.Search.EvidenceSelection = nil },
		"nan_threshold":       func(m *executionManifest) { m.Search.SemanticThreshold = new(math.NaN()) },
		"zero_fold":           func(m *executionManifest) { m.Search.FoldThreshold = new(0.0) },
		"execution_mode":      func(m *executionManifest) { m.Search.ChannelExecution = "unknown" },
		"repeats":             func(m *executionManifest) { m.Execution.Repeats = 3 },
		"order":               func(m *executionManifest) { m.Execution.Order = "by-condition" },
		"unbounded_index":     func(m *executionManifest) { m.Execution.IndexRoundLimit = 0 },
		"unbounded_time":      func(m *executionManifest) { m.Execution.TimeoutMS = 0 },
		"no_cache_policy":     func(m *executionManifest) { m.Execution.Cache = "" },
		"no_seed_declaration": func(m *executionManifest) { m.Execution.Seeds = nil },
		"missing_seed":        func(m *executionManifest) { m.Execution.Seeds[0].Value = nil },
		"unsupported_seed":    func(m *executionManifest) { m.Execution.Seeds[1].Value = new(int64(0)) },
		"null_seed_support":   func(m *executionManifest) { m.Execution.Seeds[0].Supported = nil },
		"exclusion_total":     func(m *executionManifest) { m.Exclusions.Total = 4 },
		"null_exclusions":     func(m *executionManifest) { m.Exclusions.Items = nil },
		"exclusion_overlap": func(m *executionManifest) {
			m.Exclusions.Total++
			m.Exclusions.Items = []manifestExclusion{{Dataset: "synthetic", Query: "q1", Reason: "image"}}
		},
		"cost_missing":          func(m *executionManifest) { m.Cost.Items = m.Cost.Items[:1] },
		"cost_null_zero":        func(m *executionManifest) { m.Cost.Items[0].Value = nil },
		"cost_unknown_zero":     func(m *executionManifest) { m.Cost.Items[1].Value = new(0.0) },
		"cost_negative":         func(m *executionManifest) { m.Cost.Items[0].Value = new(-1.0) },
		"cost_date":             func(m *executionManifest) { m.Cost.AsOf = "2026-02-30" },
		"cost_currency":         func(m *executionManifest) { m.Cost.Currency = "usd" },
		"conversion_other_mode": func(m *executionManifest) { m.Conversion = &manifestConversion{Format: "own"} },
		"conversion_missing":    func(m *executionManifest) { m.Mode = "convert" },
		"adversarial_filter": func(m *executionManifest) {
			m.Mode = "convert"
			m.Conversion = &manifestConversion{Format: "own-adversarial", UseCase: "fact"}
		},
		"adaptive_missing":    func(m *executionManifest) { m.Mode = "adaptive" },
		"adaptive_other_mode": func(m *executionManifest) { m.Search.DirectThresholds = []float64{0.5} },
		"consistency_missing": func(m *executionManifest) { m.Mode = "consistency" },
		"consistency_candidate": func(m *executionManifest) {
			m.Mode = "consistency"
			m.Search.CandidateLimit = 24
			m.Consistency = &manifestConsistency{Stage: "references", Requests: 1, LoadRequests: 1, Concurrency: []int{1}, WritePauses: []int{0}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			change(&m)
			if err := m.validate(); err == nil {
				t.Fatal("잘못된 통제 명세를 허용했다")
			}
		})
	}
}

func writeManifestFixture(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManifestStrictJSONAndSize(t *testing.T) {
	valid, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"top_unknown":      strings.Replace(string(valid), `"schema_version":1`, `"schema_version":1,"secret":"private"`, 1),
		"nested_unknown":   strings.Replace(string(valid), `"agent_version":`, `"body":"private","agent_version":`, 1),
		"duplicate":        strings.Replace(string(valid), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"required_missing": strings.Replace(string(valid), `"budget":0,`, ``, 1),
		"required_null":    strings.Replace(string(valid), `"budget":0`, `"budget":null`, 1),
		"null_object":      "null", "trailing": string(valid) + " {}", "oversize": strings.Repeat(" ", maxManifestBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadManifest(path); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("JSON 거부/안전 오류: %v", err)
			}
		})
	}
}

func invokeEval(t *testing.T, args ...string) error {
	t.Helper()
	originalFlags, originalArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine, os.Args = originalFlags, originalArgs }()
	flag.CommandLine = flag.NewFlagSet("eval", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"eval"}, args...)
	return run()
}

func TestEvaluationPreflightRejectsBeforeDataIO(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://127.0.0.1:1/no-db")
	t.Setenv("SEARCH_CHANNEL_CANDIDATE_LIMIT", "25")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-repeat=3"}, "반복"},
		{[]string{"-budget=-1"}, "예산"},
		{[]string{"-index-targets=unknown"}, "색인"},
		{[]string{"-consistency"}, "최대 활성 컨텍스트"},
		{[]string{"-consistency", "-consistency-requests=0"}, "일관성"},
		{[]string{"-consistency", "-consistency-load=0"}, "정수"},
		{[]string{"-consistency", "-consistency-write-pauses=-1"}, "정수"},
		{[]string{"-consistency-stage=global"}, "모드"},
		{[]string{"-direct-thresholds=NaN"}, "모드"},
		{[]string{"-adaptive", "-direct-thresholds=NaN"}, "후보"},
		{[]string{"-convert=unknown"}, "변환"},
		{[]string{"-convert=hipporag", "-convert-source=missing", "-convert-use-case=unknown"}, "사용 사례"},
		{[]string{"-convert=own-adversarial", "-convert-use-case=fact"}, "own-adversarial"},
		{[]string{"-adversarial"}, "attacks"},
		{[]string{"-unknown"}, "flag"},
		{[]string{"-check-manifest=missing"}, "모드"},
	}
	for _, tt := range cases {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			if tt.want == "최대 활성 컨텍스트" {
				t.Setenv("SEARCH_CHANNEL_CANDIDATE_LIMIT", "24")
			}
			dir := t.TempDir()
			args := append([]string{"-contexts=" + filepath.Join(dir, "contexts.json"), "-queries=" + filepath.Join(dir, "queries.json"), "-out=" + filepath.Join(dir, "result.json")}, tt.args...)
			if err := invokeEval(t, args...); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("사전 검증: %v, 기대 %s", err, tt.want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("거부한 실행에 파일이 생겼다: %v %v", entries, err)
			}
		})
	}
}

func TestEvaluationDatasetRejectsUnknownMembers(t *testing.T) {
	contexts, queries, err := convertOwnSet("strict-v1", 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{contexts, queries} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(string(raw), `"version":`, `"unapproved":true,"version":`, 1)
		path := filepath.Join(t.TempDir(), "dataset.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		var destination any
		switch value.(type) {
		case contextSet:
			destination = new(contextSet)
		case querySet:
			destination = new(querySet)
		}
		if err := readJSON(path, destination); err == nil {
			t.Fatal("허용하지 않은 데이터셋 필드를 수락했다")
		}
	}
}

func TestManifestCommandReadOnlyProcess(t *testing.T) {
	path := writeManifestFixture(t, validManifest())
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-test.run=^TestEvalCommandProcess$", "--", "-check-manifest="+path)
	command.Env = append(os.Environ(), "EVAL_TEST_COMMAND_PROCESS=1", "DATABASE_URL=unreachable")
	out, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "manifest 검증 완료") {
		t.Fatalf("실제 CLI 검사: %v %s", err, out)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("검사 중 출력 파일 생성: %v %v", entries, err)
	}
}
