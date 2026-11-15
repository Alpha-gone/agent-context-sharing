package relinfer

import (
	"context"
	"net/http"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type idleRuntimeStore struct{}

func TestRuntimePromptVersions(t *testing.T) {
	cfg := RuntimeConfig{Mode: "propose", Provider: "openai", Model: "gpt-6-luna", APIKey: "synthetic", BaseURL: "https://provider.test/v1", EmbeddingModel: "e", Timeout: time.Second, IndexWait: time.Minute, AdjacencyWindow: time.Hour, CandidateLimit: 4, TargetChars: 20, CandidateChars: 20, InputChars: 1000}
	_, first, err := NewRuntime(idleRuntimeStore{}, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PromptVersion = "2"
	_, second, err := NewRuntime(idleRuntimeStore{}, cfg, nil, nil)
	if err != nil || first == second {
		t.Fatal("프롬프트 판의 조립과 지문 구분 실패")
	}
	cfg.PromptVersion = "3"
	if _, _, err := NewRuntime(idleRuntimeStore{}, cfg, nil, nil); err == nil {
		t.Fatal("지원하지 않는 판 허용")
	}
}

func (idleRuntimeStore) ClaimRelationInferenceTask(context.Context, time.Duration) (model.RelationInferenceTask, bool, error) {
	return model.RelationInferenceTask{}, false, nil
}
func (idleRuntimeStore) FailRelationInferenceTask(context.Context, model.RelationInferenceTask, string, bool, int) (bool, error) {
	return false, nil
}
func (idleRuntimeStore) DeferRelationInferenceTask(context.Context, model.RelationInferenceTask, time.Time) (bool, error) {
	return false, nil
}
func (idleRuntimeStore) InferenceCandidates(context.Context, model.InferenceCandidateRequest) (model.InferenceSnapshot, error) {
	return model.InferenceSnapshot{}, nil
}
func (idleRuntimeStore) ApplyInferenceResult(context.Context, model.InferenceApplication) (bool, error) {
	return false, nil
}
func TestRuntimePlacementLifecycle(t *testing.T) {
	if w, profile, err := NewRuntime(nil, RuntimeConfig{Mode: "off"}, nil, nil); err != nil || w != nil || profile != "" {
		t.Fatalf("off 생성: %v %q %v", w, profile, err)
	}
	cfg := RuntimeConfig{Mode: "propose", Provider: "openai", Model: "gpt-6-luna", APIKey: "synthetic", BaseURL: "https://provider.test/v1", EmbeddingModel: "e", Timeout: time.Second, IndexWait: time.Minute, AdjacencyWindow: time.Hour, CandidateLimit: 4, TargetChars: 20, CandidateChars: 20, InputChars: 1000}
	worker, profile, err := NewRuntime(idleRuntimeStore{}, cfg, &http.Client{}, nil)
	if err != nil || worker == nil || len(profile) != 64 {
		t.Fatalf("작업자 조립: %v %q %v", worker, profile, err)
	}
	worker.Start(t.Context())
	worker.Start(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := worker.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.Close(ctx); err != nil {
		t.Fatal(err)
	}
	unopened, _, err := NewRuntime(idleRuntimeStore{}, cfg, &http.Client{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := unopened.Close(ctx); err != nil {
		t.Fatal(err)
	}
	unopened.Start(t.Context())
	cfg.Provider = "unknown"
	if _, _, err := NewRuntime(idleRuntimeStore{}, cfg, &http.Client{}, nil); err == nil {
		t.Fatal("지원하지 않는 제공자를 허용했다")
	}
}
