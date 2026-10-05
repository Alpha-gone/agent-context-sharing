package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"github.com/jackc/pgx/v5"
)

// TestStoredCharacterLimitHoldsUnderConcurrencyIntegration은 저장량 한도가 저장
// 트랜잭션 안에서 강제되는지 확인한다. 밖에서 읽은 값으로만 판정하면 한도 직전 그래프에
// 생성이 동시에 와도 둘 다 통과해 한도를 넘는다.
func TestStoredCharacterLimitHoldsUnderConcurrencyIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)

	// 본문 길이만큼만 남기고 한도를 채운다. 한 건만 들어갈 수 있는 상태다.
	body := testSourceContext(t, graphID, actorID, "https://example.test/limit-probe").Body
	limit := int64(len([]rune(body)))
	limits := WriteLimits{StoredCharsPerGraph: limit}

	const requests = 6
	var waitGroup sync.WaitGroup
	results := make(chan error, requests)
	start := make(chan struct{})
	for index := range requests {
		waitGroup.Go(func() {
			value := testSourceContext(t, graphID, actorID, "https://example.test/limit-"+graphID.String()+"-"+string(rune('a'+index)))
			<-start
			_, err := database.CreateContextWithOperation(t.Context(), graphID, value, nil, nil, limits)
			results <- err
		})
	}
	close(start)
	waitGroup.Wait()
	close(results)

	succeeded, exceeded := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.As(err, new(plan.LimitError)):
			exceeded++
		default:
			t.Fatalf("동시 생성이 한도 초과가 아닌 오류로 끝났다: %v", err)
		}
	}
	if succeeded != 1 || exceeded != requests-1 {
		t.Fatalf("동시 생성 = 성공 %d, 한도 초과 %d; want 1과 %d", succeeded, exceeded, requests-1)
	}
	var stored int64
	if err := database.pool.QueryRow(t.Context(), `SELECT stored_chars FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&stored); err != nil {
		t.Fatalf("저장량 조회: %v", err)
	}
	if stored > limit {
		t.Fatalf("저장량 = %d, 한도 %d를 넘었다", stored, limit)
	}
}

// TestGraphCountLimitHoldsUnderConcurrencyIntegration은 계정당 그래프 수 한도가 생성
// 트랜잭션 안에서 강제되는지 확인한다.
func TestGraphCountLimitHoldsUnderConcurrencyIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)

	limits := WriteLimits{GraphsPerAccount: 2}
	initial := model.Graph{ID: newTestID(t), Name: "기존 그래프", CreatedBy: actorID, CreatedAt: nowUTC(), LastActivityAt: nowUTC(), Version: 1}
	if _, err := database.CreateGraphWithOwner(t.Context(), initial, limits); err != nil {
		t.Fatalf("한도 직전 그래프 준비: %v", err)
	}
	const requests = 5
	var waitGroup sync.WaitGroup
	results := make(chan error, requests)
	start := make(chan struct{})
	for range requests {
		waitGroup.Go(func() {
			graph := model.Graph{ID: newTestID(t), Name: "한도 그래프", CreatedBy: actorID, CreatedAt: nowUTC(), LastActivityAt: nowUTC(), Version: 1}
			<-start
			_, err := database.CreateGraphWithOwner(t.Context(), graph, limits)
			results <- err
		})
	}
	close(start)
	waitGroup.Wait()
	close(results)

	succeeded, exceeded := 0, 0
	for err := range results {
		_, limitExceeded := errors.AsType[plan.LimitError](err)
		switch {
		case err == nil:
			succeeded++
		case limitExceeded:
			exceeded++
		default:
			t.Fatalf("동시 그래프 생성이 한도 초과가 아닌 오류로 끝났다: %v", err)
		}
	}
	if succeeded != 1 || exceeded != requests-1 {
		t.Fatalf("동시 그래프 생성 = 성공 %d, 한도 초과 %d; want 1과 %d", succeeded, exceeded, requests-1)
	}
	count, err := database.OwnedGraphCount(t.Context(), actorID)
	if err != nil || int64(count) != limits.GraphsPerAccount {
		t.Fatalf("동시 생성 후 그래프 수 = %d, 오류 %v; want %d", count, err, limits.GraphsPerAccount)
	}
}

// TestGraphCountLimitWithDistinctIdempotencyKeysIntegration은 저장점 해제 뒤에도
// 계정 잠금이 바깥 커밋까지 유지되어 다른 키의 생성이 미커밋 그래프를 놓치지 않는지 확인한다.
func TestGraphCountLimitWithDistinctIdempotencyKeysIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	limits := WriteLimits{GraphsPerAccount: 2}
	graph := func() model.Graph {
		return model.Graph{ID: newTestID(t), Name: "멱등성 한도 그래프", CreatedBy: actorID, CreatedAt: nowUTC(), LastActivityAt: nowUTC(), Version: 1}
	}
	if _, err := database.CreateGraphWithOwner(t.Context(), graph(), limits); err != nil {
		t.Fatalf("한도 직전 그래프 준비: %v", err)
	}
	firstGraph, secondGraph := graph(), graph()
	firstRequest := IdempotencyRequest{AccountID: actorID, Key: newTestID(t), ToolName: "graph_create"}
	secondRequest := IdempotencyRequest{AccountID: actorID, Key: newTestID(t), ToolName: "graph_create"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var waitGroup sync.WaitGroup
	defer waitGroup.Wait()
	defer cancel()
	commitFirst := make(chan struct{})
	releaseFirst := sync.OnceFunc(func() { close(commitFirst) })
	defer releaseFirst()
	firstCreated, secondEntered := make(chan uint32, 1), make(chan uint32, 1)
	firstResult, secondResult := make(chan error, 1), make(chan error, 1)
	waitGroup.Go(func() {
		_, err := database.ReplayIdempotent(ctx, firstRequest, func(ctx context.Context) ([]byte, error) {
			if _, err := database.CreateGraphWithOwner(ctx, firstGraph, limits); err != nil {
				return nil, err
			}
			firstCreated <- ctx.Value(writeTransactionContextKey{}).(pgx.Tx).Conn().PgConn().PID()
			select {
			case <-commitFirst:
				return []byte(`{"created":true}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		firstResult <- err
	})
	var firstPID uint32
	select {
	case firstPID = <-firstCreated:
	case err := <-firstResult:
		t.Fatalf("첫 생성의 커밋 전 준비 실패: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitGroup.Go(func() {
		_, err := database.ReplayIdempotent(ctx, secondRequest, func(ctx context.Context) ([]byte, error) {
			secondEntered <- ctx.Value(writeTransactionContextKey{}).(pgx.Tx).Conn().PgConn().PID()
			_, err := database.CreateGraphWithOwner(ctx, secondGraph, limits)
			return []byte(`{"created":true}`), err
		})
		secondResult <- err
	})
	var secondPID uint32
	select {
	case secondPID = <-secondEntered:
	case err := <-secondResult:
		t.Fatalf("다른 키의 생성 진입 실패: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// 경과 시간으로 겹침을 추측하지 않고 실제 DB 잠금 대기를 확인한 뒤 커밋한다.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for blocked := false; !blocked; {
		select {
		case err := <-secondResult:
			t.Fatalf("첫 커밋 전에 다른 생성이 끝났다: %v", err)
		case <-ctx.Done():
			t.Fatalf("계정 잠금 대기를 확인하지 못했다: %v", ctx.Err())
		case <-ticker.C:
			if err := database.pool.QueryRow(ctx, `SELECT $1::integer = ANY(pg_blocking_pids($2::integer))`, firstPID, secondPID).Scan(&blocked); err != nil {
				t.Fatalf("생성 잠금 대기 조회: %v", err)
			}
		}
	}
	releaseFirst()
	if err := <-firstResult; err != nil {
		t.Fatalf("첫 생성 커밋: %v", err)
	}
	if err := <-secondResult; err == nil {
		t.Fatal("다른 키의 생성이 그래프 수 한도를 넘겼다")
	} else if limitError, ok := errors.AsType[plan.LimitError](err); !ok || limitError.Name != "graphs_per_account" {
		t.Fatalf("다른 키의 생성 오류 = %v; want 그래프 수 한도 초과", err)
	}
	count, err := database.OwnedGraphCount(t.Context(), actorID)
	if err != nil || int64(count) != limits.GraphsPerAccount {
		t.Fatalf("멱등성 동시 생성 후 그래프 수 = %d, 오류 %v; want %d", count, err, limits.GraphsPerAccount)
	}
}

// TestWebContextDeletionAuditSharesTransactionIntegration은 컨텍스트 웹 삭제의 상태
// 변경과 감사 기록이 같은 트랜잭션에 남는지 확인한다. FR-AGENT_CONTEXT-109가 웹 관리
// 행위의 기록을 요구하므로 변경만 남고 기록이 사라지면 안 된다.
func TestWebContextDeletionAuditSharesTransactionIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/web-audit-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}

	if _, err := database.SetContextDeleted(t.Context(), graphID, source.ID, actorID, true, 0); err != nil {
		t.Fatalf("웹 컨텍스트 삭제: %v", err)
	}
	var audits int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log WHERE target_context_id = $1 AND action = 'delete'`, source.ID.String()).Scan(&audits); err != nil {
		t.Fatalf("감사 기록 조회: %v", err)
	}
	if audits != 1 {
		t.Fatalf("웹 삭제 감사 기록 = %d줄, want 1줄", audits)
	}

	// 상태 전이가 거부되면 감사 기록도 남지 않아야 한다.
	if _, err := database.SetContextDeleted(t.Context(), graphID, source.ID, actorID, true, 0); err == nil {
		t.Fatal("이미 삭제된 컨텍스트의 재삭제가 허용됐다")
	}
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.web_audit_log WHERE target_context_id = $1 AND action = 'delete'`, source.ID.String()).Scan(&audits); err != nil {
		t.Fatalf("감사 기록 재조회: %v", err)
	}
	if audits != 1 {
		t.Fatalf("거부 뒤 감사 기록 = %d줄, want 1줄", audits)
	}
}

// TestWriteResultsCarryReferencesWithoutRereadIntegration은 생성·갱신·상태 전이의 응답이
// 커밋 뒤 재조회 없이도 참조 목록을 담는지 확인한다. 재조회를 두면 그 실패가 이미 적용된
// 연산을 실패로 응답해 적용 기록과 거부 기록이 모순된다.
func TestWriteResultsCarryReferencesWithoutRereadIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/reread-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}

	derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}
	if got := derived.Derived.DerivedFrom; len(got) != 1 || got[0] != source.ID {
		t.Fatalf("생성 응답의 근거 = %v, want [%s]", got, source.ID)
	}

	next := derived
	next.Body = "갱신한 파생 본문"
	next.Version = derived.Version + 1
	updated, err := database.UpdateContext(t.Context(), graphID, derived.Version, next)
	if err != nil {
		t.Fatalf("파생 갱신: %v", err)
	}
	if got := updated.Derived.DerivedFrom; len(got) != 1 || got[0] != source.ID {
		t.Fatalf("갱신 응답의 근거 = %v, want [%s]", got, source.ID)
	}

	discarded, err := database.DiscardContext(t.Context(), graphID, derived.ID, nil, WriteLimits{})
	if err != nil {
		t.Fatalf("파생 폐기: %v", err)
	}
	if got := discarded.Derived.DerivedFrom; len(got) != 1 || got[0] != source.ID {
		t.Fatalf("폐기 응답의 근거 = %v, want [%s]", got, source.ID)
	}

	event, err := database.CreateContext(t.Context(), graphID, testEventContext(t, graphID, actorID, source.ID), nil)
	if err != nil {
		t.Fatalf("사건 생성: %v", err)
	}
	if got := event.Event.MemberIDs; len(got) != 1 || got[0] != source.ID {
		t.Fatalf("사건 생성 응답의 구성원 = %v, want [%s]", got, source.ID)
	}
}

// TestRejectedWriteDoesNotConsumeRateIntegration은 거부된 쓰기가 요청 빈도 한도를
// 소비하지 않는지 확인한다. 「계정 플랜 값」이 카운터 갱신을 이미 열려 있는 저장
// 트랜잭션 안에서 하기로 확정했으므로, 되돌아간 요청은 세지 않아야 한다.
func TestRejectedWriteDoesNotConsumeRateIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/rate-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}

	limits := WriteLimits{WritesPerMinute: 10, ActorID: actorID}
	// 판 번호를 틀려 거부되는 수정을 여러 번 보낸다.
	for range 5 {
		next := derived
		next.Body = "충돌하는 본문"
		next.Version = derived.Version + 1
		if _, err := database.UpdateContextWithOperation(t.Context(), graphID, derived.Version+7, next, nil, limits); err == nil {
			t.Fatal("판 번호가 틀린 수정이 통과했다")
		}
	}
	if count := currentRateCount(t, database, actorID); count != 0 {
		t.Fatalf("거부된 요청 뒤 카운터 = %d, want 0", count)
	}

	// 성공한 쓰기만 카운터를 올린다.
	next := derived
	next.Body = "적용되는 본문"
	next.Version = derived.Version + 1
	if _, err := database.UpdateContextWithOperation(t.Context(), graphID, derived.Version, next, nil, limits); err != nil {
		t.Fatalf("정상 수정: %v", err)
	}
	if count := currentRateCount(t, database, actorID); count != 1 {
		t.Fatalf("성공한 요청 뒤 카운터 = %d, want 1", count)
	}
}

// TestWriteRateLimitRejectsOverLimitIntegration은 한도를 넘는 쓰기가 한도 초과로
// 거부되는지 확인한다.
func TestWriteRateLimitRejectsOverLimitIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)

	limits := WriteLimits{WritesPerMinute: 2, ActorID: actorID}
	succeeded := 0
	var limitErr error
	for index := range 4 {
		value := testSourceContext(t, graphID, actorID, "https://example.test/rate-limit-"+graphID.String()+"-"+string(rune('a'+index)))
		_, err := database.CreateContextWithOperation(t.Context(), graphID, value, nil, nil, limits)
		switch {
		case err == nil:
			succeeded++
		case errors.As(err, new(plan.LimitError)):
			limitErr = err
		default:
			t.Fatalf("예상 밖 오류: %v", err)
		}
	}
	if succeeded != 2 {
		t.Fatalf("성공한 쓰기 = %d, want 2", succeeded)
	}
	limit, _ := errors.AsType[plan.LimitError](limitErr)
	if limit.Name != "writes_per_minute" {
		t.Fatalf("한도 이름 = %q, want writes_per_minute", limit.Name)
	}
}

// currentRateCount는 현재 창의 요청 빈도 카운터를 읽는다. 행이 없으면 0이다.
func currentRateCount(t *testing.T, database *Store, accountID model.ID) int {
	t.Helper()
	var count int
	err := database.pool.QueryRow(t.Context(), `
		SELECT COALESCE(sum(count), 0) FROM public.request_rate WHERE account_id = $1`, accountID.String()).Scan(&count)
	if err != nil {
		t.Fatalf("요청 빈도 조회: %v", err)
	}
	return count
}
