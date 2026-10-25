package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func longMemoryFixture() []longMemoryQuestion {
	return []longMemoryQuestion{{ID: "private-task", Type: "multi-session", Question: "HELDOUT_QUESTION", Answer: jsontext.Value(`"HELDOUT_ANSWER"`), Date: "2023/05/30 (Tue) 23:40", Dates: []string{"2023/05/20 (Sat) 02:21", "2023/05/21 (Sun) 02:21"}, SessionIDs: []string{"private-session-a", "private-session-b"}, Sessions: [][]longMemoryTurn{{{Role: "user", Content: "RAW_UTTERANCE", HasAnswer: new(true)}, {Role: "assistant", Content: "RAW_REPLY"}}, {{Role: "user", Content: "OTHER_TURN", HasAnswer: new(true)}}}, AnswerSessions: []string{"private-session-a", "private-session-b"}}}
}

func TestPrepareLongMemorySeparation(t *testing.T) {
	fixture := longMemoryFixture()
	abstention := fixture[0]
	abstention.ID += "_abs"
	fixture = append(fixture, abstention)
	data, err := prepareLongMemory(fixture, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = data.finish(); err != nil {
		t.Fatal(err)
	}
	if data.Report.Independent != 1 || data.Report.Graphs != 2 || data.Report.Included != 2 || data.Report.Contexts != 6 {
		t.Fatalf("수/독립 단위 오류: %+v", data.Report)
	}
	a, b := data.Evaluation.Queries[0], data.Evaluation.Queries[1]
	if a.Group == b.Group || a.Unit != b.Unit || a.EvidenceStatus != "annotated" || len(a.Evidence) != 2 || b.EvidenceStatus != "not_applicable" || b.AnswerStatus != "unanswerable" || len(b.Evidence) > 0 || len(b.Answer) > 0 {
		t.Fatalf("분리/근거 오류: %+v %+v", a, b)
	}
	raw, err := json.Marshal(data.Memory)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"HELDOUT_QUESTION", "HELDOUT_ANSWER", "has_answer", "private-task", "private-session"} {
		if strings.Contains(string(raw), s) {
			t.Fatalf("기억으로 주석 유출: %s", s)
		}
	}
	for _, s := range []string{"RAW_UTTERANCE", "RAW_REPLY", "OTHER_TURN", "external_content", "2023-05-20T02:21:00Z"} {
		if !strings.Contains(string(raw), s) {
			t.Fatalf("원천 손실: %s", s)
		}
	}
	report, _ := json.Marshal(data.Report)
	for _, s := range []string{"RAW_UTTERANCE", "HELDOUT_QUESTION", "HELDOUT_ANSWER", "private-task", "private-session"} {
		if strings.Contains(string(report), s) {
			t.Fatalf("보고서 원문 유출: %s", s)
		}
	}
	if data.Mappings.Items[1].Speaker != "user" || data.Mappings.Items[1].HasAnswer == nil || !*data.Mappings.Items[1].HasAnswer {
		t.Fatal("원본 역할/주석 누락")
	}
	if data.Mappings.Items[0].Timestamp != fixture[0].Dates[0] {
		t.Fatal("원본 세션 시각 누락")
	}
}

func TestPrepareLongMemoryAnnotationAndEmptyPolicy(t *testing.T) {
	for _, test := range []struct {
		name           string
		mutate         func(*longMemoryQuestion)
		status, reason string
		contexts       int
	}{
		{"partial", func(q *longMemoryQuestion) { q.Sessions[1][0].HasAnswer = nil }, "unmeasured", "partial_annotation", 3},
		{"none", func(q *longMemoryQuestion) { q.Sessions[0][0].HasAnswer = nil; q.Sessions[1][0].HasAnswer = nil }, "unmeasured", "missing_annotation", 3},
		{"empty-and-repeat", func(q *longMemoryQuestion) {
			q.Dates = append(q.Dates, q.Dates[0], q.Dates[0])
			q.SessionIDs = append(q.SessionIDs, "empty-session", q.SessionIDs[0])
			q.Sessions = append(q.Sessions, []longMemoryTurn{}, q.Sessions[0])
			q.Sessions[0] = append(q.Sessions[0], longMemoryTurn{Role: "user", Content: " "})
		}, "annotated", "", 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := longMemoryFixture()
			test.mutate(&q[0])
			d, err := prepareLongMemory(q, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.finish(); err != nil {
				t.Fatal(err)
			}
			got := d.Evaluation.Queries[0]
			if got.EvidenceStatus != test.status || got.EvidenceReason != test.reason || d.Report.Contexts != test.contexts {
				t.Fatalf("상태 오류: %+v", got)
			}
			if test.name == "empty-and-repeat" && (d.Report.SourceCounts["empty_sessions"] != 1 || d.Report.SourceCounts["repeated_session_ids"] != 1 || d.Report.SourceCounts["empty_turns"] != 1) {
				t.Fatal("빈 입력 계측 누락")
			}
		})
	}
}

func TestPrepareLongMemoryRejectsInvalid(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*longMemoryQuestion)
	}{
		{"role", func(q *longMemoryQuestion) { q.Sessions[0][0].Role = "system" }},
		{"date", func(q *longMemoryQuestion) { q.Dates[0] = "invalid" }},
		{"alignment", func(q *longMemoryQuestion) { q.SessionIDs = nil }},
		{"missing-answer-session", func(q *longMemoryQuestion) { q.AnswerSessions = []string{"missing"} }},
		{"empty-annotated", func(q *longMemoryQuestion) { q.Sessions[0][0].Content = "" }},
		{"type", func(q *longMemoryQuestion) { q.Type = "unknown" }},
		{"answer", func(q *longMemoryQuestion) { q.Answer = jsontext.Value(`null`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := longMemoryFixture()
			test.mutate(&q[0])
			if _, err := prepareLongMemory(q, nil, 1); err == nil {
				t.Fatal("잘못된 원본 수용")
			}
		})
	}
	q := longMemoryFixture()
	if _, err := prepareLongMemory(q, nil, 500); err == nil {
		t.Fatal("원본 수 불일치 수용")
	}
	if _, err := prepareLongMemory(append(q, q[0]), nil, 2); err == nil {
		t.Fatal("중복 문항 수용")
	}
}

func locomoFixture() []locomoConversation {
	return []locomoConversation{{ID: "PRIVATE_SAMPLE", Conversation: map[string]jsontext.Value{
		"speaker_a": jsontext.Value(`"PRIVATE_PERSON_A"`), "speaker_b": jsontext.Value(`"PRIVATE_PERSON_B"`),
		"session_1_date_time": jsontext.Value(`"1:56 pm on 8 May, 2023"`),
		"session_1":           jsontext.Value(`[{"dia_id":"D1:1","speaker":"PRIVATE_PERSON_A","text":"RAW_TEXT"},{"dia_id":"D1:2","speaker":"PRIVATE_PERSON_B","text":"PHOTO_TEXT","img_url":["PRIVATE_IMAGE_URL"],"blip_caption":"IMAGE_CAPTION","query":"IMAGE_SEARCH"}]`),
		"session_2_date_time": jsontext.Value(`"1:56 pm on 9 May, 2023"`),
	}, EventSummary: jsontext.Value(`"HELDOUT_SUMMARY"`), Questions: []locomoQuestion{
		{Question: "HELDOUT_Q1", Answer: jsontext.Value(`"HELDOUT_A1"`), Category: 1, Evidence: []string{"D1:1"}},
		{Question: "HELDOUT_Q2", Answer: jsontext.Value(`42`), Category: 4, Evidence: []string{"D1:2"}},
		{Question: "HELDOUT_Q3", Answer: jsontext.Value(`"A3"`), Category: 3},
		{Question: "HELDOUT_Q4", Answer: jsontext.Value(`"A4"`), Category: 2, Evidence: []string{"D1:1 D1:2"}},
		{Question: "HELDOUT_Q5", Category: 5, Evidence: []string{"D1:1"}, AdversarialAnswer: jsontext.Value(`"WRONG_ANSWER"`)},
	}}}
}

func TestPrepareLocomoTextAndPrivateMapping(t *testing.T) {
	fixture := locomoFixture()
	d, err := prepareLocomo(fixture, nil, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.finish(); err != nil {
		t.Fatal(err)
	}
	if d.Report.Included != 4 || d.Report.Contexts != 2 || d.Report.Independent != 1 || d.Report.SourceCounts["timestamp_only_sessions"] != 1 || d.Report.Evidence["unmeasured"] != 2 || len(d.Report.Exclusions.Items) != 1 || d.Report.Exclusions.Items[0].Reason != "image_evidence" {
		t.Fatalf("텍스트 범위/분모 오류: %+v", d.Report)
	}
	queries := d.Evaluation.Queries
	if queries[0].EvidenceStatus != "annotated" || len(queries[0].Evidence) != 1 || queries[1].EvidenceReason != "missing_annotation" || queries[2].EvidenceReason != "invalid_annotation" || queries[3].AnswerStatus != "unanswerable" || len(queries[3].Answer) > 0 {
		t.Fatalf("근거/유보 오류: %+v", queries)
	}
	raw, _ := json.Marshal(d.Memory)
	for _, s := range []string{"PRIVATE_PERSON", "PRIVATE_SAMPLE", "PRIVATE_IMAGE_URL", "IMAGE_CAPTION", "IMAGE_SEARCH", "HELDOUT_", "WRONG_ANSWER"} {
		if strings.Contains(string(raw), s) {
			t.Fatalf("기억 유출: %s", s)
		}
	}
	if !strings.Contains(string(raw), "PHOTO_TEXT") || !strings.Contains(string(raw), "RAW_TEXT") {
		t.Fatal("텍스트 원천 손실")
	}
	report, _ := json.Marshal(d.Report)
	for _, s := range []string{"PRIVATE_", "RAW_TEXT", "HELDOUT_", "WRONG_ANSWER"} {
		if strings.Contains(string(report), s) {
			t.Fatalf("공개 보고서 유출: %s", s)
		}
	}
	a, err := prepareLocomo(fixture, nil, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Mappings, d.Mappings) {
		t.Fatal("매핑 순서 비결정적")
	}
	if d.Mappings.Items[0].Timestamp != "1:56 pm on 9 May, 2023" {
		t.Fatal("시각만 있는 세션의 원본 누락")
	}
}

func TestPrepareLocomoRejectsInvalid(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*locomoConversation)
	}{
		{"unknown-field", func(c *locomoConversation) { c.Conversation["private"] = jsontext.Value(`"value"`) }},
		{"missing-speaker", func(c *locomoConversation) { delete(c.Conversation, "speaker_a") }},
		{"duplicate-speaker", func(c *locomoConversation) { c.Conversation["speaker_a"] = c.Conversation["speaker_b"] }},
		{"missing-date", func(c *locomoConversation) { delete(c.Conversation, "session_1_date_time") }},
		{"invalid-date", func(c *locomoConversation) { c.Conversation["session_1_date_time"] = jsontext.Value(`"invalid"`) }},
		{"bad-turn", func(c *locomoConversation) {
			c.Conversation["session_1"] = jsontext.Value(`[{"dia_id":"D1:1","speaker":"A","text":"hello","unknown":"secret"}]`)
		}},
		{"missing-answer", func(c *locomoConversation) { c.Questions[0].Answer = nil }},
		{"bad-category", func(c *locomoConversation) { c.Questions[0].Category = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			x := locomoFixture()
			test.mutate(&x[0])
			if _, err := prepareLocomo(x, nil, 1, 5); err == nil {
				t.Fatal("잘못된 원본 수용")
			}
		})
	}
	if _, err := prepareLocomo(locomoFixture(), nil, 10, 5); err == nil {
		t.Fatal("대화 수 불일치 수용")
	}
	if _, err := prepareLocomo(locomoFixture(), nil, 1, 6); err == nil {
		t.Fatal("문항 수 불일치 수용")
	}
}

func TestPreparePinnedReader(t *testing.T) {
	for _, test := range []struct {
		name, raw        string
		badHash, invalid bool
	}{
		{name: "valid", raw: `{"value":"text"}`},
		{name: "checksum", raw: `{"value":"text"}`, badHash: true, invalid: true},
		{name: "unknown", raw: `{"value":"text","PRIVATE_UNKNOWN":"SECRET"}`, invalid: true},
		{name: "duplicate", raw: `{"value":"text","value":"SECRET"}`, invalid: true},
		{name: "trailing", raw: `{"value":"text"} {}`, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte(test.raw), 0600); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(test.raw))
			pin := publicDataPin{SHA256: hex.EncodeToString(hash[:])}
			if test.badHash {
				pin.SHA256 = strings.Repeat("0", 64)
			}
			var out struct {
				Value string `json:"value"`
			}
			err := readPinnedPublic(path, pin, &out)
			if (err != nil) != test.invalid {
				t.Fatalf("검사 결과 오류: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "PRIVATE_UNKNOWN")) {
				t.Fatalf("원문 오류 유출: %v", err)
			}
		})
	}
}

func TestPrepareSupplementAndPrivateFiles(t *testing.T) {
	data, err := prepareSupplement()
	if err != nil {
		t.Fatal(err)
	}
	if err = data.finish(); err != nil {
		t.Fatal(err)
	}
	if data.Report.Independent != 6 || data.Report.Graphs != 7 || data.Report.Included != 6 || len(data.Report.Types) != 6 {
		t.Fatal("보완 세트 분모 오류")
	}
	for _, q := range data.Evaluation.Queries {
		if len(q.Steps) == 0 || len(q.Actors) == 0 {
			t.Fatal("시나리오 준비 누락")
		}
	}
	expected := []string{"event", "evidence-state", "isolation", "recovery", "contamination", "handover"}
	for _, kind := range expected {
		if data.Report.Types[kind] != 1 {
			t.Fatalf("시나리오 누락: %s", kind)
		}
	}
	output := filepath.Join(t.TempDir(), "prepared")
	if err = writePreparedData(output, data); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatal("디렉터리 권한 오류")
	}
	var report preparationReport
	if err = readJSON(filepath.Join(output, "preparation.json"), &report); err != nil {
		t.Fatal(err)
	}
	for name, checksum := range report.Files {
		file := filepath.Join(output, name)
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != checksum {
			t.Fatal("산출물 checksum 오류")
		}
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("비공개 파일 권한 오류")
		}
	}
	before, err := os.ReadFile(filepath.Join(output, "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = writePreparedData(output, data); err == nil {
		t.Fatal("기존 출력 덮어쓰기")
	}
	after, _ := os.ReadFile(filepath.Join(output, "memory.json"))
	if !slices.Equal(before, after) {
		t.Fatal("기존 결과 변경")
	}
	for _, g := range data.Memory.Groups {
		path := filepath.Join(t.TempDir(), "contexts.json")
		if err := writeDataset(path, g.Contexts); err != nil {
			t.Fatal(err)
		}
		if _, err := loadContextSet(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareOptionsFailBeforeOutput(t *testing.T) {
	for _, args := range [][]string{
		{"unknown", "missing"}, {"longmemeval", ""}, {"locomo", "missing"}, {"own-supplement", "unexpected"},
	} {
		t.Run(args[0]+args[1], func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "absent")
			if err := runPrepareData(args[0], args[1], out); err == nil {
				t.Fatal("잘못된 준비 실행")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("실패한 준비가 파일 생성")
			}
		})
	}
}

func TestPrepareCommandPreflightAndNoDatabase(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		args    []string
		success bool
	}{
		{"supplement", []string{"-prepare-data=own-supplement"}, true},
		{"conflicting-mode", []string{"-prepare-data=own-supplement", "-continual"}, false},
		{"explicit-default", []string{"-prepare-data=own-supplement", "-repeat=10"}, false},
		{"manifest", []string{"-prepare-data=own-supplement", "-check-manifest=missing"}, false},
		{"orphan-option", []string{"-prepare-source=missing"}, false},
		{"public-missing", []string{"-prepare-data=longmemeval", "-prepare-source=missing"}, false},
		{"unknown", []string{"-prepare-data=unknown"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "prepared")
			args := append([]string{"-test.run=^TestEvalCommandProcess$", "--", "-prepare-out=" + output}, test.args...)
			command := exec.CommandContext(t.Context(), binary, args...)
			command.Env = append(os.Environ(), "EVAL_TEST_COMMAND_PROCESS=1", "DATABASE_URL=unreachable")
			raw, err := command.CombinedOutput()
			if (err == nil) != test.success {
				t.Fatalf("실제 CLI 결과: %v %s", err, raw)
			}
			_, statErr := os.Stat(output)
			if test.success && statErr != nil || !test.success && !os.IsNotExist(statErr) {
				t.Fatalf("출력 경계 오류: %v", statErr)
			}
		})
	}
}
