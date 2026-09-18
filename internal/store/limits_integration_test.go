package store

import (
	"errors"
	"sync"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
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

	limits := WriteLimits{GraphsPerAccount: 1}
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
		switch {
		case err == nil:
			succeeded++
		case errors.As(err, new(plan.LimitError)):
			exceeded++
		default:
			t.Fatalf("동시 그래프 생성이 한도 초과가 아닌 오류로 끝났다: %v", err)
		}
	}
	if succeeded != 1 || exceeded != requests-1 {
		t.Fatalf("동시 그래프 생성 = 성공 %d, 한도 초과 %d; want 1과 %d", succeeded, exceeded, requests-1)
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

	if _, err := database.SetContextDeleted(t.Context(), graphID, source.ID, actorID, true); err != nil {
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
	if _, err := database.SetContextDeleted(t.Context(), graphID, source.ID, actorID, true); err == nil {
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

	discarded, err := database.DiscardContext(t.Context(), graphID, derived.ID, nil)
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
