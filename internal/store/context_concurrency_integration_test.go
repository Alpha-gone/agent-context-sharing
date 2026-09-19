package store

import (
	"errors"
	"sync"
	"testing"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
)

// TestConcurrentDiscardAppliesOnceIntegration은 같은 컨텍스트에 동시에 온 폐기가 한 번만
// 적용되는지 확인한다. 쓰기 조건이 없으면 둘 다 성공해 저장량이 두 번 차감되고 적용
// 기록도 두 줄 남는다.
func TestConcurrentDiscardAppliesOnceIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)

	const rounds, requests = 5, 8
	for round := range rounds {
		graphID := createTestGraph(t, database, actorID)
		target, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/discard-"+graphID.String()), nil)
		if err != nil {
			t.Fatalf("%d회차 폐기 대상 생성: %v", round, err)
		}
		// 폐기하지 않을 컨텍스트를 함께 둔다. 저장량이 대상 본문의 두 배보다 커야 차감이
		// 두 번 일어나도 음수가 되지 않아, 이중 적용이 갱신 실패에 가려지지 않는다.
		keep, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/keep-"+graphID.String()), nil)
		if err != nil {
			t.Fatalf("%d회차 유지 컨텍스트 생성: %v", round, err)
		}
		want := int64(utf8.RuneCountInString(keep.Body))
		source := target

		var waitGroup sync.WaitGroup
		results := make(chan error, requests)
		start := make(chan struct{})
		for range requests {
			waitGroup.Go(func() {
				<-start
				_, err := database.DiscardContext(t.Context(), graphID, source.ID, nil, WriteLimits{})
				results <- err
			})
		}
		close(start)
		waitGroup.Wait()
		close(results)

		succeeded := 0
		for err := range results {
			if err == nil {
				succeeded++
			}
		}
		if succeeded != 1 {
			t.Fatalf("%d회차 동시 폐기 성공 = %d, want 1", round, succeeded)
		}
		var after int64
		if err := database.pool.QueryRow(t.Context(), `SELECT stored_chars FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&after); err != nil {
			t.Fatalf("%d회차 저장량 재조회: %v", round, err)
		}
		if after != want {
			t.Fatalf("%d회차 폐기 뒤 저장량 = %d, want %d; 차감이 여러 번 적용됐다", round, after, want)
		}
		var operations int
		if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE context_id = $1 AND operation_kind = 'discard' AND result = 'applied'`, source.ID.String()).Scan(&operations); err != nil {
			t.Fatalf("%d회차 적용 기록 조회: %v", round, err)
		}
		if operations > 1 {
			t.Fatalf("%d회차 폐기 적용 기록 = %d줄, want 1줄 이하", round, operations)
		}
	}
}

// TestConcurrentUpdateReportsVersionConflictIntegration은 같은 판 번호로 동시에 온 수정이
// internal이 아니라 판 번호 충돌로 끝나는지 확인한다. AGE의 동시 갱신 오류를 그대로
// 흘리면 SRS의 충돌 응답과 SDD의 재시도 계약이 성립하지 않는다.
func TestConcurrentUpdateReportsVersionConflictIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)

	const rounds, requests = 5, 4
	for round := range rounds {
		graphID := createTestGraph(t, database, actorID)
		source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/update-"+graphID.String()), nil)
		if err != nil {
			t.Fatalf("%d회차 원천 생성: %v", round, err)
		}
		derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
		if err != nil {
			t.Fatalf("%d회차 파생 생성: %v", round, err)
		}

		var waitGroup sync.WaitGroup
		results := make(chan error, requests)
		start := make(chan struct{})
		for index := range requests {
			waitGroup.Go(func() {
				next := derived
				next.Body = "동시 수정 본문 " + string(rune('A'+index))
				next.Version = derived.Version + 1
				<-start
				_, err := database.UpdateContext(t.Context(), graphID, derived.Version, next)
				results <- err
			})
		}
		close(start)
		waitGroup.Wait()
		close(results)

		succeeded, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				succeeded++
			case errors.As(err, new(VersionConflictError)):
				conflicts++
			default:
				t.Fatalf("%d회차 동시 수정이 충돌이 아닌 오류로 끝났다: %v", round, err)
			}
		}
		if succeeded != 1 || conflicts != requests-1 {
			t.Fatalf("%d회차 동시 수정 = 성공 %d, 충돌 %d; want 1과 %d", round, succeeded, conflicts, requests-1)
		}
	}
}
