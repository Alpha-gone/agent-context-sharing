package relinfer

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
)

func inferenceTestID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func inferenceTestEvent(t *testing.T, graph model.ID, body string) model.Context {
	t.Helper()
	return model.Context{ID: inferenceTestID(t), GraphID: graph, Layer: model.LayerEvent, Body: body, Version: 1, RecordedAt: time.Now().UTC(), Event: &model.EventAttributes{Start: time.Now().UTC()}}
}
func inferenceTestProfile() Profile {
	return Profile{Provider: "test", Model: "test", PromptVersion: "1", SchemaVersion: "1", EmbeddingModel: "e", IndexWait: time.Minute, AdjacencyWindow: time.Hour, Channels: []string{"keyword", "member"}, CandidateLimit: 4, TargetChars: 20, CandidateChars: 10, InputChars: 3000, Parameters: map[string]string{"z": "2", "a": "1"}}
}

func TestBuildInputRankingPrivacyAndFingerprint(t *testing.T) {
	graph := inferenceTestID(t)
	target := inferenceTestEvent(t, graph, "대상 사건 본문")
	a := inferenceTestEvent(t, graph, "후보 사건 본문 긴 내용")
	b := inferenceTestEvent(t, graph, "다른 사건")
	ca := Candidate{Event: a, Allowed: []model.RelationType{model.RelationTypeRelatesTo}}
	cb := Candidate{Event: b, Allowed: []model.RelationType{model.RelationTypeRelatesTo}}
	p := inferenceTestProfile()
	channels := []Channel{{Name: "keyword", Candidates: []Candidate{cb, ca}}, {Name: "member", Candidates: []Candidate{ca}}}
	result, err := BuildInput(p, target, channels)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 2 || result.Candidates[0].ID != a.ID || result.Candidates[0].Alias != "C1" || len(result.Candidates[0].Contributions) != 2 {
		t.Fatalf("결합 순위: %+v", result.Candidates)
	}
	for _, forbidden := range []string{graph.String(), target.ID.String(), a.ID.String(), b.ID.String()} {
		if bytes.Contains(result.Data, []byte(forbidden)) {
			t.Fatalf("식별자 노출: %s", forbidden)
		}
	}
	if !bytes.Contains(result.Data, []byte(`"alias":"C1"`)) || !bytes.Contains(result.Data, []byte("후보 사건")) {
		t.Fatalf("후보 본문 누락: %s", result.Data)
	}
	reversed := p
	reversed.Channels = []string{"member", "keyword"}
	reversed.Parameters = map[string]string{"a": "1", "z": "2"}
	fingerprint, err := reversed.Fingerprint()
	if err != nil || fingerprint != result.ProfileFingerprint {
		t.Fatalf("지문 불일치 %q %v", fingerprint, err)
	}
	reversed.CandidateLimit++
	changed, _ := reversed.Fingerprint()
	if changed == fingerprint {
		t.Fatal("설정 변경을 지문에 반영하지 않았다")
	}
	p.InputChars = utf8.RuneCount(result.Data) - 1
	limited, err := BuildInput(p, target, channels)
	if err != nil || utf8.RuneCount(limited.Data) > p.InputChars || len(limited.Candidates) >= len(result.Candidates) {
		t.Fatalf("입력 상한: %s %v", limited.Data, err)
	}
	other := ca
	other.Event.GraphID = inferenceTestID(t)
	if _, err := BuildInput(p, target, []Channel{{Name: "keyword", Candidates: []Candidate{other}}}); err == nil {
		t.Fatal("다른 그래프 사건 허용")
	}
	if strings.Contains(string(result.Data), "source body") {
		t.Fatal("원천 본문 노출")
	}
}

type candidateSourceStub struct {
	snapshot model.InferenceSnapshot
	request  model.InferenceCandidateRequest
}

func (s *candidateSourceStub) InferenceCandidates(_ context.Context, r model.InferenceCandidateRequest) (model.InferenceSnapshot, error) {
	s.request = r
	return s.snapshot, nil
}
func TestInputBuilderWaitAndSnapshot(t *testing.T) {
	graph := inferenceTestID(t)
	target := inferenceTestEvent(t, graph, "target")
	task := model.RelationInferenceTask{GraphID: graph, ContextID: target.ID, TargetVersion: 1}
	until := time.Now().Add(time.Minute)
	source := &candidateSourceStub{snapshot: model.InferenceSnapshot{IndexWaitUntil: &until}}
	b := InputBuilder{Source: source, Profile: inferenceTestProfile()}
	input, wait, err := b.Prepare(t.Context(), task)
	if err != nil || wait == nil || len(input.Data) != 0 || len(source.request.Channels) != 2 {
		t.Fatalf("색인 대기: %+v %v %v", input, wait, err)
	}
	source.snapshot = model.InferenceSnapshot{Target: target, SemanticUnavailable: true}
	input, wait, err = b.Prepare(t.Context(), task)
	if err != nil || wait != nil || !input.SemanticUnavailable {
		t.Fatalf("색인 생략: %+v %v %v", input, wait, err)
	}
	source.snapshot.Target.Version++
	if _, _, err = b.Prepare(t.Context(), task); err == nil {
		t.Fatal("다른 대상 판 허용")
	}
}
