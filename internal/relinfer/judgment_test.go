package relinfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	json "encoding/json/v2"
)

func judgmentFixture(t *testing.T) (PreparedInput, model.Context, model.Context) {
	t.Helper()
	graph := inferenceTestID(t)
	target := inferenceTestEvent(t, graph, "원인 사건 발췌")
	candidate := inferenceTestEvent(t, graph, "결과 사건 발췌")
	value := Candidate{Event: candidate, Allowed: []model.RelationType{model.RelationTypeCauses}, CauseDirections: []string{"T_to_C"}}
	input, err := BuildInput(inferenceTestProfile(), target, []Channel{{Name: "keyword", Candidates: []Candidate{value}}})
	if err != nil {
		t.Fatal(err)
	}
	return input, target, candidate
}
func judgmentOutput(t *testing.T, judgments ...Judgment) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		SchemaVersion string     `json:"schema_version"`
		Decisions     []Judgment `json:"decisions"`
	}{"1", judgments})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestJudgmentValidationContract(t *testing.T) {
	input, _, candidate := judgmentFixture(t)
	valid := Judgment{Candidate: "C1", RelationType: model.RelationTypeCauses, Decision: "relation", Direction: "T_to_C", Evidence: Evidence{Target: "원인 사건", Candidate: "결과 사건"}}
	for _, tc := range []struct {
		name   string
		change func(*Judgment)
		reason string
	}{
		{"valid", func(*Judgment) {}, ""}, {"unknown_alias", func(v *Judgment) { v.Candidate = "C9" }, "output_invalid"}, {"unknown_type", func(v *Judgment) { v.RelationType = model.RelationTypePrecedes }, "output_invalid"}, {"direction", func(v *Judgment) { v.Direction = "C_to_T" }, "output_invalid"}, {"quotation", func(v *Judgment) { v.Evidence.Target = "없는 발췌" }, "evidence_unverified"}, {"decision", func(v *Judgment) { v.Decision = "yes" }, "output_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := valid
			tc.change(&v)
			got, err := ValidateOutput(input, judgmentOutput(t, v))
			if err != nil || len(got) < 1 || got[0].InvalidReason != tc.reason {
				t.Fatalf("판정=%+v %v", got, err)
			}
			if tc.name == "valid" && got[0].CandidateID != candidate.ID {
				t.Fatal("후보 대응 오류")
			}
		})
	}
	duplicate, err := ValidateOutput(input, judgmentOutput(t, valid, valid))
	if err != nil || len(duplicate) != 2 || duplicate[0].InvalidReason != "output_invalid" || duplicate[1].InvalidReason != "output_invalid" {
		t.Fatalf("중복=%+v %v", duplicate, err)
	}
	for _, raw := range []string{`{}`, `{"schema_version":"other","decisions":[]}`, `{"schema_version":"1","decisions":null}`, `{"schema_version":"1","decisions":[],"extra":1}`} {
		if _, err := ValidateOutput(input, []byte(raw)); !errors.Is(err, ErrInvalidOutput) {
			t.Fatalf("전체 위반 %s: %v", raw, err)
		}
	}
}

type resultSinkStub struct {
	result model.InferenceApplication
	calls  int
}

func (s *resultSinkStub) ApplyInferenceResult(_ context.Context, request model.InferenceApplication) (bool, error) {
	s.calls++
	s.result = request
	return false, nil
}
func TestDecisionProcessorUsesPreparedData(t *testing.T) {
	input, target, candidate := judgmentFixture(t)
	task := model.RelationInferenceTask{GraphID: target.GraphID, ContextID: target.ID, TargetVersion: target.Version}
	value := Judgment{Candidate: "C1", RelationType: model.RelationTypeRelatesTo, Decision: "relation", Evidence: Evidence{Target: "원인 사건", Candidate: "결과 사건"}}
	source := &candidateSourceStub{snapshot: model.InferenceSnapshot{Target: target, Channels: []Channel{{Name: "keyword", Candidates: []Candidate{{Event: candidate, Allowed: []model.RelationType{model.RelationTypeRelatesTo}}}}}}}
	sink := &resultSinkStub{}
	processor := &DecisionProcessor{Builder: InputBuilder{Source: source, Profile: inferenceTestProfile()}, Judge: StaticJudge{Result: JudgeResult{Output: judgmentOutput(t, value)}}, Sink: sink, Mode: "propose"}
	raw, failure, err := processor.Prepare(t.Context(), task)
	if err != nil || failure != nil {
		t.Fatalf("준비 %v %v", failure, err)
	}
	failure, err = processor.Process(t.Context(), task, raw, 1)
	if err != nil || failure != nil || sink.calls != 1 || len(sink.result.Decisions) != 1 || sink.result.Decisions[0].CandidateID != candidate.ID {
		t.Fatalf("처리 %+v %v %+v", failure, err, sink.result)
	}
	if len(input.Data) == 0 {
		t.Fatal("입력 없음")
	}
}

type judgmentRoundTrip func(*http.Request) (*http.Response, error)

func (f judgmentRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenAIJudgePromptVersion(t *testing.T) {
	client := &http.Client{Transport: judgmentRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(body), "For none or abstain, both evidence strings must be empty.") {
			t.Error("판 2 지시가 요청에 없다")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`))}, nil
	})}
	judge, err := NewOpenAIJudgeWithPrompt(client, "https://provider.test/v1", "key", "model", "2")
	if err != nil {
		t.Fatal(err)
	}
	profile := inferenceTestProfile()
	profile.Provider, profile.Model, profile.PromptVersion = "openai", "model", "2"
	profile.Parameters = nil
	if err := judge.ValidateProfile(profile); err != nil {
		t.Fatal(err)
	}
	profile.PromptVersion = "1"
	if err := judge.ValidateProfile(profile); err == nil {
		t.Fatal("다른 프롬프트 판 허용")
	}
	if _, err := judge.Judge(t.Context(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOpenAIJudgeWithPrompt(client, "https://provider.test/v1", "key", "model", "3"); err == nil {
		t.Fatal("미지원 판 허용")
	}
}

func TestOpenAIJudgeStructuredOutputAndFailure(t *testing.T) {
	output := `{"schema_version":"1","decisions":[]}`
	status := http.StatusOK
	client := &http.Client{Transport: judgmentRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/responses" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Errorf("요청 경계: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"strict":true`) || !strings.Contains(string(body), `"store":false`) {
			t.Errorf("요청 구성")
		}
		header := http.Header{}
		header.Set("Retry-After", "7")
		raw, _ := json.Marshal(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": output}}}}, "usage": map[string]any{"input_tokens": 12, "output_tokens": 3}})
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	judge, err := NewOpenAIJudge(client, "https://provider.test/v1", "synthetic-key", "gpt-6-luna")
	if err != nil {
		t.Fatal(err)
	}
	profile := inferenceTestProfile()
	profile.Provider, profile.Model, profile.Parameters = "openai", "gpt-6-luna", nil
	if err := judge.ValidateProfile(profile); err != nil {
		t.Fatal(err)
	}
	profile.Parameters = map[string]string{"temperature": "0"}
	if err := judge.ValidateProfile(profile); err == nil {
		t.Fatal("지원하지 않는 생성 매개변수를 허용했다")
	}
	result, err := judge.Judge(t.Context(), []byte(`{"data":{}}`))
	if err != nil || string(result.Output) != output || result.InputTokens == nil || *result.InputTokens != 12 {
		t.Fatalf("응답 %+v %v", result, err)
	}
	status = http.StatusTooManyRequests
	_, err = judge.Judge(t.Context(), []byte(`{}`))
	provider, ok := errors.AsType[ProviderError](err)
	if !ok || provider.Status != 429 || provider.RetryAfter != 7*time.Second {
		t.Fatalf("실패 %v", err)
	}
}

func TestJudgmentPartOfDirectionAndMissingType(t *testing.T) {
	graph := inferenceTestID(t)
	target := inferenceTestEvent(t, graph, "target")
	candidate := inferenceTestEvent(t, graph, "candidate")
	input, err := BuildInput(inferenceTestProfile(), target, []Channel{{Name: "keyword", Candidates: []Candidate{{Event: candidate, Allowed: []model.RelationType{model.RelationTypePartOf}, PartOfDirection: "T_to_C"}}}})
	if err != nil {
		t.Fatal(err)
	}
	j := Judgment{Candidate: "C1", RelationType: model.RelationTypePartOf, Decision: "relation", Direction: "C_to_T", Evidence: Evidence{Target: "target", Candidate: "candidate"}}
	checked, err := ValidateOutput(input, judgmentOutput(t, j))
	if err != nil || len(checked) != 1 || checked[0].InvalidReason != "output_invalid" {
		t.Fatalf("뒤집힌 방향 %+v %v", checked, err)
	}
	checked, err = ValidateOutput(input, judgmentOutput(t))
	if err != nil || len(checked) != 1 || checked[0].InvalidReason != "output_invalid" || checked[0].CandidateID != candidate.ID {
		t.Fatalf("누락 판정 %+v %v", checked, err)
	}
}

type failingJudge struct{ err error }

func (j failingJudge) Judge(context.Context, []byte) (JudgeResult, error) {
	return JudgeResult{}, j.err
}
func TestDecisionProcessorClassifiesProviderFailure(t *testing.T) {
	_, target, candidate := judgmentFixture(t)
	task := model.RelationInferenceTask{GraphID: target.GraphID, ContextID: target.ID, TargetVersion: target.Version}
	source := &candidateSourceStub{snapshot: model.InferenceSnapshot{Target: target, Channels: []Channel{{Name: "keyword", Candidates: []Candidate{{Event: candidate, Allowed: []model.RelationType{model.RelationTypeRelatesTo}}}}}}}
	q := &queueStub{task: task}
	processor := &DecisionProcessor{Builder: InputBuilder{Source: source, Profile: inferenceTestProfile()}, Judge: failingJudge{err: ProviderError{Status: 429, RetryAfter: time.Minute}}, Sink: &resultSinkStub{}, Mode: "propose"}
	worker, err := New(q, processor, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if q.failures != 1 || q.reason != "provider_unavailable" || !q.retryable || worker.retryAt.IsZero() {
		t.Fatalf("제공자 오류 분류 %+v", q)
	}
}

func TestOpenAIJudgeOutputLimitProfileContract(t *testing.T) {
	var request map[string]any
	client := &http.Client{Transport: judgmentRoundTrip(func(r *http.Request) (*http.Response, error) {
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(raw, &request); e != nil {
			t.Fatal(e)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"schema_version\":\"1\",\"decisions\":[]}"}]}]}`)), Header: http.Header{}}, nil
	})}
	judge, e := NewOpenAIJudgeWithOptions(client, "https://example.test/v1", "unused", "gpt-6-luna", "2", 1536, "default")
	if e != nil {
		t.Fatal(e)
	}
	profile := inferenceTestProfile()
	profile.Provider = "openai"
	profile.Model = "gpt-6-luna"
	profile.PromptVersion = "2"
	profile.Parameters = map[string]string{"max_output_tokens": "1536", "service_tier": "default"}
	if e = judge.ValidateProfile(profile); e != nil {
		t.Fatal(e)
	}
	if _, e = judge.Judge(t.Context(), []byte(`{"schema_version":"1"}`)); e != nil {
		t.Fatal(e)
	}
	if fmt.Sprint(request["max_output_tokens"]) != "1536" || request["service_tier"] != "default" {
		t.Fatalf("bounded request mismatch: %v", request)
	}
	profile.Parameters["max_output_tokens"] = "1024"
	if judge.ValidateProfile(profile) == nil {
		t.Fatal("mismatched output cap accepted")
	}
	profile.Parameters = map[string]string{"max_output_tokens": "1536", "service_tier": "default", "temperature": "0"}
	if judge.ValidateProfile(profile) == nil {
		t.Fatal("unsupported generation parameter accepted")
	}
	if _, e = NewOpenAIJudgeWithOptions(client, "https://example.test/v1", "unused", "gpt-6-luna", "2", -1, "default"); e == nil {
		t.Fatal("negative output cap accepted")
	}
}
