package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestIndexAndNonGraphSearchIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	value := testSourceContext(t, graphID, actorID, "https://example.test/index-"+newTestID(t).String())
	value.Body = "색인 검색 기준선 고유어"
	stored, err := database.CreateContext(t.Context(), graphID, value, nil)
	if err != nil {
		t.Fatalf("색인 대상 생성: %v", err)
	}
	// 자기 작업만 앞으로 당기면 다른 테스트가 남긴 행이 더 앞선 시각일 때 그쪽이 먼저
	// 잡힌다. readyIndexTasks가 나머지를 함께 미뤄 확보 순서를 고정한다.
	readyIndexTasks(t, database, stored.ID)
	vector := make([]float64, 1024)
	vector[0] = 1
	processed, err := database.ProcessNextIndexTask(t.Context(), func(_ context.Context, task IndexTask) IndexTaskResult {
		if task.ContextID != stored.ID || task.Body != stored.Body {
			t.Fatalf("색인 작업 = %+v", task)
		}
		return IndexTaskResult{Embedding: vector, ModelID: "test:vector:1024"}
	})
	if err != nil {
		t.Fatalf("색인 작업 처리: %v", err)
	}
	if !processed.Found || !processed.Succeeded || indexTaskCount(t, database, stored.ID) != 0 {
		t.Fatalf("색인 작업 결과 = %+v", processed)
	}
	semantic, err := database.SemanticCandidates(t.Context(), graphID, "test:vector:1024", vector, time.Now().UTC(), 10)
	if err != nil || len(semantic) != 1 || semantic[0].Context.ID != stored.ID {
		t.Fatalf("의미 유사도 후보 = %#v, err=%v", semantic, err)
	}
	keyword, err := database.KeywordCandidates(t.Context(), graphID, "고유어", time.Now().UTC(), 10)
	if err != nil || len(keyword) != 1 || keyword[0].Context.ID != stored.ID {
		t.Fatalf("키워드 후보 = %#v, err=%v", keyword, err)
	}
	timed, err := database.TimeCandidates(t.Context(), graphID, time.Now().UTC(), 10)
	if err != nil || len(timed) != 1 || timed[0].Context.ID != stored.ID {
		t.Fatalf("시간 후보 = %#v, err=%v", timed, err)
	}
	origins, err := database.ContextOriginKinds(t.Context(), graphID, []model.ID{stored.ID})
	if err != nil || len(origins[stored.ID]) != 1 || origins[stored.ID][0] != model.OriginKindExternalContent {
		t.Fatalf("출처 구분 = %v, err=%v", origins, err)
	}
}

// TestGlobalSummaryCandidatesIntegration은 전역 검색 시작점이 전역 요약 파생만
// 유효 기간과 기록 시각 순서에 따라 반환하는지 실제 AGE에서 확인한다.
func TestGlobalSummaryCandidatesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	now := time.Now().UTC()

	source := testSourceContext(t, graphID, actorID, "https://example.test/global-summary-"+newTestID(t).String())
	storedSource, err := database.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		t.Fatalf("전역 요약 근거 생성: %v", err)
	}
	createSummary := func(body string, scope model.SummaryScope, recordedAt time.Time, validTo *time.Time) model.Context {
		return model.Context{
			ID:             newTestID(t),
			GraphID:        graphID,
			Layer:          model.LayerDerived,
			Body:           body,
			RecordedAt:     recordedAt,
			CreatedBy:      actorID,
			CreatedByAgent: actorID,
			Version:        1,
			Derived: &model.DerivedAttributes{
				Kind:          model.DerivationKindSummary,
				SummaryScope:  scope,
				EvidenceState: model.EvidenceStateExperience,
				ValidTo:       validTo,
			},
		}
	}
	older, err := database.CreateContext(t.Context(), graphID, createSummary("이전 전역 요약", model.SummaryScopeGlobal, now.Add(-2*time.Minute), nil), []model.ID{storedSource.ID})
	if err != nil {
		t.Fatalf("이전 전역 요약 생성: %v", err)
	}
	newer, err := database.CreateContext(t.Context(), graphID, createSummary("최신 전역 요약", model.SummaryScopeGlobal, now.Add(-time.Minute), nil), []model.ID{storedSource.ID})
	if err != nil {
		t.Fatalf("최신 전역 요약 생성: %v", err)
	}
	if _, err := database.CreateContext(t.Context(), graphID, createSummary("국소 요약", model.SummaryScopeLocal, now, nil), []model.ID{storedSource.ID}); err != nil {
		t.Fatalf("국소 요약 생성: %v", err)
	}
	expiredAt := new(time.Time)
	*expiredAt = now.Add(-time.Second)
	if _, err := database.CreateContext(t.Context(), graphID, createSummary("만료 전역 요약", model.SummaryScopeGlobal, now, expiredAt), []model.ID{storedSource.ID}); err != nil {
		t.Fatalf("만료 전역 요약 생성: %v", err)
	}

	candidates, err := database.GlobalSummaryCandidates(t.Context(), graphID, now, 10)
	if err != nil {
		t.Fatalf("전역 요약 후보 조회: %v", err)
	}
	if len(candidates) != 2 || candidates[0].Context.ID != newer.ID || candidates[1].Context.ID != older.ID {
		t.Fatalf("전역 요약 후보 = %#v, want 최신·이전 전역 요약", candidates)
	}
	limited, err := database.GlobalSummaryCandidates(t.Context(), graphID, now, 1)
	if err != nil || len(limited) != 1 || limited[0].Context.ID != newer.ID {
		t.Fatalf("전역 요약 후보 상한 = %#v, err=%v", limited, err)
	}
}

func TestIndexTaskClaimConcurrency(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	targets := make([]model.ID, 0, 2)
	for range 2 {
		value := testSourceContext(t, graphID, actorID, "https://example.test/index-claim-"+newTestID(t).String())
		stored, err := database.CreateContext(t.Context(), graphID, value, nil)
		if err != nil {
			t.Fatalf("동시 확보 대상 생성: %v", err)
		}
		targets = append(targets, stored.ID)
	}
	readyIndexTasks(t, database, targets...)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started := make(chan model.ID, 2)
	release := make(chan struct{})
	results := make(chan IndexProcessResult, 2)
	failures := make(chan error, 2)
	var group sync.WaitGroup
	claim := func() {
		result, err := database.ProcessNextIndexTask(ctx, func(_ context.Context, task IndexTask) IndexTaskResult {
			started <- task.ContextID
			<-release
			return IndexTaskResult{Failure: "동시 확보 확인", Retryable: true}
		})
		if err != nil {
			failures <- err
			return
		}
		results <- result
	}
	group.Go(claim)
	group.Go(claim)
	<-started
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		group.Wait()
		t.Fatal("두 작업자가 서로 다른 색인 작업을 확보하지 못했다")
	}
	close(release)
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatalf("동시 색인 작업 처리: %v", err)
	}
	claimed := make(map[model.ID]struct{}, 2)
	for result := range results {
		if !result.Found || result.Succeeded {
			t.Fatalf("동시 확보 결과 = %+v", result)
		}
		claimed[result.Task.ContextID] = struct{}{}
	}
	for _, target := range targets {
		if _, found := claimed[target]; !found {
			t.Fatalf("작업 %s를 확보하지 못했다: %v", target, claimed)
		}
	}
}

func TestReindexExcludesAndReplacesOutdatedModel(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	value := testSourceContext(t, graphID, actorID, "https://example.test/index-reindex-"+newTestID(t).String())
	stored, err := database.CreateContext(t.Context(), graphID, value, nil)
	if err != nil {
		t.Fatalf("재색인 대상 생성: %v", err)
	}
	readyIndexTasks(t, database, stored.ID)
	vector := make([]float64, 1024)
	vector[0] = 1
	const currentModel = "current:vector:1024"
	if _, err := database.ProcessNextIndexTask(t.Context(), func(context.Context, IndexTask) IndexTaskResult {
		return IndexTaskResult{Embedding: vector, ModelID: currentModel}
	}); err != nil {
		t.Fatalf("현재 모델 색인: %v", err)
	}
	before, err := database.OutdatedEmbeddingCount(t.Context(), currentModel)
	if err != nil {
		t.Fatalf("재색인 전 이전 모델 행 수 조회: %v", err)
	}
	query := "UPDATE public.context_embedding SET model_id = 'old:vector:1024' WHERE context_id = $1"
	if _, err := database.pool.Exec(t.Context(), query, stored.ID.String()); err != nil {
		t.Fatalf("이전 모델 표시: %v", err)
	}
	if remaining, err := database.OutdatedEmbeddingCount(t.Context(), currentModel); err != nil || remaining != before+1 {
		t.Fatalf("재색인 전 이전 모델 행 수 = %d, want %d, err=%v", remaining, before+1, err)
	}
	candidates, err := database.SemanticCandidates(t.Context(), graphID, currentModel, vector, time.Now().UTC(), 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("이전 모델 제외 = %#v, err=%v", candidates, err)
	}
	if err := database.ReindexOutdatedEmbeddings(t.Context(), currentModel); err != nil {
		t.Fatalf("이전 모델 재색인 등록: %v", err)
	}
	if count := indexTaskCount(t, database, stored.ID); count != 1 {
		t.Fatalf("재색인 작업 수 = %d, want 1", count)
	}
	readyIndexTasks(t, database, stored.ID)
	if _, err := database.ProcessNextIndexTask(t.Context(), func(context.Context, IndexTask) IndexTaskResult {
		return IndexTaskResult{Embedding: vector, ModelID: currentModel}
	}); err != nil {
		t.Fatalf("재색인 처리: %v", err)
	}
	candidates, err = database.SemanticCandidates(t.Context(), graphID, currentModel, vector, time.Now().UTC(), 10)
	if err != nil || len(candidates) != 1 || candidates[0].Context.ID != stored.ID {
		t.Fatalf("재색인 결과 = %#v, err=%v", candidates, err)
	}
	if remaining, err := database.OutdatedEmbeddingCount(t.Context(), currentModel); err != nil || remaining != before {
		t.Fatalf("재색인 뒤 이전 모델 행 수 = %d, want %d, err=%v", remaining, before, err)
	}
}

func TestIndexRetryAndImmediateFailureIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	value := testSourceContext(t, graphID, actorID, "https://example.test/index-retry-"+newTestID(t).String())
	stored, err := database.CreateContext(t.Context(), graphID, value, nil)
	if err != nil {
		t.Fatalf("재시도 대상 생성: %v", err)
	}
	readyIndexTasks(t, database, stored.ID)
	before := time.Now().UTC()
	processed, err := database.ProcessNextIndexTask(t.Context(), func(context.Context, IndexTask) IndexTaskResult {
		return IndexTaskResult{Failure: "임베딩 제공자 호출 실패", Retryable: true}
	})
	if err != nil {
		t.Fatalf("재시도 가능 실패 처리: %v", err)
	}
	if !processed.Found || processed.Succeeded {
		t.Fatalf("재시도 가능 실패 결과 = %+v", processed)
	}
	state := indexTaskState(t, database, stored.ID)
	if state.attempts != 1 || state.state != "pending" || !state.nextAttempt.After(before.Add(30*time.Second)) {
		t.Fatalf("첫 실패 상태 = %+v, 기준 시각 %s", state, before)
	}

	query := "UPDATE public.index_task SET attempts = 4, next_attempt_at = to_timestamp(-3000000000) WHERE context_id = $1"
	if _, err := database.pool.Exec(t.Context(), query, stored.ID.String()); err != nil {
		t.Fatalf("다섯 번째 시도 준비: %v", err)
	}
	processed, err = database.ProcessNextIndexTask(t.Context(), func(context.Context, IndexTask) IndexTaskResult {
		return IndexTaskResult{Failure: "임베딩 제공자 호출 실패", Retryable: true}
	})
	if err != nil {
		t.Fatalf("재시도 한계 실패 처리: %v", err)
	}
	if !processed.Found || processed.Succeeded {
		t.Fatalf("재시도 한계 결과 = %+v", processed)
	}
	state = indexTaskState(t, database, stored.ID)
	if state.attempts != 5 || state.state != "failed" {
		t.Fatalf("한계 초과 상태 = %+v", state)
	}

	query = "UPDATE public.index_task SET attempts = 0, state = 'pending', next_attempt_at = to_timestamp(-3000000000) WHERE context_id = $1"
	if _, err := database.pool.Exec(t.Context(), query, stored.ID.String()); err != nil {
		t.Fatalf("차원 실패 시도 준비: %v", err)
	}
	processed, err = database.ProcessNextIndexTask(t.Context(), func(context.Context, IndexTask) IndexTaskResult {
		return IndexTaskResult{Failure: "임베딩 차원이 다르다", Retryable: false}
	})
	if err != nil {
		t.Fatalf("차원 불일치 처리: %v", err)
	}
	if !processed.Found || processed.Succeeded {
		t.Fatalf("차원 불일치 결과 = %+v", processed)
	}
	state = indexTaskState(t, database, stored.ID)
	if state.attempts != 1 || state.state != "failed" || state.lastError != "임베딩 차원이 다르다" {
		t.Fatalf("차원 불일치 상태 = %+v", state)
	}
}

type indexTaskStateValue struct {
	attempts    int
	state       string
	lastError   string
	nextAttempt time.Time
}

func indexTaskState(t *testing.T, store *Store, contextID model.ID) indexTaskStateValue {
	t.Helper()
	var value indexTaskStateValue
	query := "SELECT attempts, state, COALESCE(last_error, ''), next_attempt_at FROM public.index_task WHERE context_id = $1"
	err := store.pool.QueryRow(t.Context(), query, contextID.String()).Scan(
		&value.attempts, &value.state, &value.lastError, &value.nextAttempt,
	)
	if err != nil {
		t.Fatalf("색인 작업 상태 조회: %v", err)
	}
	return value
}

// readyIndexTasks는 지정한 작업만 확보 가능하게 만든다.
//
// 작업자는 그래프를 가리지 않고 가장 오래된 대기 작업을 가져가므로, 다른 테스트나 이전
// 실행이 남긴 대기 작업이 있으면 이 테스트가 남의 작업을 확보한다. 대상이 아닌 대기 작업을
// 뒤로 밀어 확보 대상을 하나로 고정한다.
func readyIndexTasks(t *testing.T, store *Store, contextIDs ...model.ID) {
	t.Helper()
	targets := make([]string, 0, len(contextIDs))
	for _, contextID := range contextIDs {
		targets = append(targets, contextID.String())
	}
	postpone := "UPDATE public.index_task SET next_attempt_at = now() + interval '1 hour' WHERE state = 'pending' AND NOT (context_id = ANY($1))"
	if _, err := store.pool.Exec(t.Context(), postpone, targets); err != nil {
		t.Fatalf("다른 색인 작업 미루기: %v", err)
	}
	ready := "UPDATE public.index_task SET enqueued_at = to_timestamp(-3000000000), next_attempt_at = to_timestamp(-3000000000) WHERE context_id = ANY($1)"
	if _, err := store.pool.Exec(t.Context(), ready, targets); err != nil {
		t.Fatalf("색인 작업 우선순위 설정: %v", err)
	}
}
