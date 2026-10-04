package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// TestEvidenceInvalidationConflictRollsBackIntegration은 전파 대상을 읽은 뒤 다른
// 트랜잭션의 파생 갱신을 커밋해, 0행 전파가 폐기·대체와 함께 롤백되는지 확인한다.
func TestEvidenceInvalidationConflictRollsBackIntegration(t *testing.T) {
	for _, action := range []string{"discard_source", "discard_derived", "supersede"} {
		t.Run(action, func(t *testing.T) {
			database := newIntegrationStore(t)
			actorID := newTestID(t)
			createTestAccount(t, database, actorID)
			graphID := createTestGraph(t, database, actorID)
			source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/evidence-"+graphID.String()), nil)
			if err != nil {
				t.Fatalf("원천 준비: %v", err)
			}
			evidence := source
			if action != "discard_source" {
				evidence, err = database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
				if err != nil {
					t.Fatalf("파생 근거 준비: %v", err)
				}
			}
			derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{evidence.ID})
			if err != nil {
				t.Fatalf("전파 대상 준비: %v", err)
			}
			indirect, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{derived.ID})
			if err != nil {
				t.Fatalf("간접 파생 준비: %v", err)
			}
			replacement := testDerivedContext(t, graphID, actorID)
			operation := OperationRecord{Kind: OperationDiscard, GraphID: graphID, ContextID: evidence.ID,
				TargetVersion: evidence.Version, JudgmentInput: "evidence-conflict-" + graphID.String(), AccountID: actorID, AgentID: actorID}
			if action == "supersede" {
				operation.Kind, operation.ContextID = OperationSupersede, replacement.ID
			}
			apply := func(ctx context.Context) error {
				if action == "supersede" {
					_, err := database.CreateSupersedingContextWithOperation(ctx, graphID, replacement, []model.ID{source.ID}, evidence.ID, &operation, WriteLimits{})
					return err
				}
				_, err := database.DiscardContext(ctx, graphID, evidence.ID, &operation, WriteLimits{})
				return err
			}
			tx, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("경합 트랜잭션 준비: %v", err)
			}
			defer tx.Rollback(t.Context())
			var updated model.Context
			interleaved := false
			wrapped := &afterEvidenceReadTx{Tx: tx, afterRead: func() {
				interleaved = true
				next := derived
				next.Body, next.Version = "동시에 커밋한 파생 본문", derived.Version+1
				updated, err = database.UpdateContext(t.Context(), graphID, derived.Version, next)
				if err != nil {
					t.Fatalf("전파 대상 조회 뒤 동시 갱신 커밋: %v", err)
				}
			}}
			err = apply(context.WithValue(t.Context(), writeTransactionContextKey{}, pgx.Tx(wrapped)))
			conflict, ok := errors.AsType[VersionConflictError](err)
			if !interleaved || !ok || conflict.Current != updated.Version {
				t.Fatalf("근거 무효 전파 결과 = %v, 겹침 %t; want 현재 판 %d의 충돌", err, interleaved, updated.Version)
			}
			current, err := database.Context(t.Context(), graphID, evidence.ID)
			if err != nil || current.DeletedAt != nil || current.Version != evidence.Version {
				t.Fatalf("실패한 폐기가 남았다: %#v, 오류 %v", current, err)
			}
			if action == "supersede" {
				if _, err := database.Context(t.Context(), graphID, replacement.ID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("실패한 대체의 새 파생이 남았다: %v", err)
				}
				if count := indexTaskCount(t, database, replacement.ID); count != 0 {
					t.Fatalf("실패한 대체의 색인 작업 = %d, want 0", count)
				}
			}
			if applied, err := database.HasOperationJudgment(t.Context(), graphID, operation.JudgmentInput); err != nil || applied {
				t.Fatalf("실패한 적용 기록이 남았다: %t, 오류 %v", applied, err)
			}
			// 재시도는 새 판의 본문을 보존하고 무효 표시만 켜야 한다.
			if err := apply(t.Context()); err != nil {
				t.Fatalf("폐기·대체 재시도: %v", err)
			}
			invalidated, err := database.Context(t.Context(), graphID, derived.ID)
			if err != nil || invalidated.Derived == nil || !invalidated.Derived.EvidenceInvalidated || invalidated.Body != updated.Body || invalidated.Version != updated.Version+1 || invalidated.DeletedAt != nil {
				t.Fatalf("재시도 뒤 근거 무효 표시·본문·판 = %#v, 오류 %v", invalidated, err)
			}
			unchanged, err := database.Context(t.Context(), graphID, indirect.ID)
			if err != nil || unchanged.Derived == nil || unchanged.Derived.EvidenceInvalidated || unchanged.Version != indirect.Version {
				t.Fatalf("근거 무효 표시가 두 단계로 전파됐다: %#v, 오류 %v", unchanged, err)
			}
			// 반대 순서의 옛 판 갱신도 표시를 지우지 않고 충돌해야 한다.
			stale := updated
			stale.Body, stale.Version = "무효 표시 뒤 옛 판 갱신", updated.Version+1
			if _, err := database.UpdateContext(t.Context(), graphID, updated.Version, stale); err == nil {
				t.Fatal("무효 표시 뒤 옛 판 갱신이 통과했다")
			} else if conflict, ok := errors.AsType[VersionConflictError](err); !ok || conflict.Current != invalidated.Version {
				t.Fatalf("무효 표시 뒤 옛 판 갱신 오류 = %v; want 판 충돌", err)
			}
		})
	}
}

// TestEvidenceInvalidationAGEConflictIntegration은 파생 갱신이 아직 미커밋일 때
// 시작한 전파가 AGE 동시 갱신 오류를 internal이 아닌 판 충돌로 반환하는지 확인한다.
func TestEvidenceInvalidationAGEConflictIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/pending-evidence-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 준비: %v", err)
	}
	derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("전파 대상 준비: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var waitGroup sync.WaitGroup
	defer waitGroup.Wait()
	defer cancel()
	first, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("파생 갱신 트랜잭션 준비: %v", err)
	}
	defer first.Rollback(t.Context())
	next := derived
	next.Body, next.Version = "커밋 대기 중인 파생 본문", derived.Version+1
	updated, err := database.UpdateContext(context.WithValue(ctx, writeTransactionContextKey{}, first), graphID, derived.Version, next)
	if err != nil {
		t.Fatalf("미커밋 파생 갱신: %v", err)
	}
	second, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("근거 폐기 트랜잭션 준비: %v", err)
	}
	// 두 번째 연결의 정리는 goroutine이 끝난 뒤 수행한다.
	defer func() { cancel(); waitGroup.Wait(); second.Rollback(t.Context()) }()
	firstPID, secondPID := first.Conn().PgConn().PID(), second.Conn().PgConn().PID()
	result := make(chan error, 1)
	waitGroup.Go(func() {
		_, err := database.DiscardContext(context.WithValue(ctx, writeTransactionContextKey{}, second), graphID, source.ID, nil, WriteLimits{})
		result <- err
	})
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for blocked := false; !blocked; {
		select {
		case err := <-result:
			t.Fatalf("파생 갱신 커밋 전에 전파가 끝났다: %v", err)
		case <-ctx.Done():
			t.Fatalf("전파의 AGE 잠금 대기를 확인하지 못했다: %v", ctx.Err())
		case <-ticker.C:
			if err := database.pool.QueryRow(ctx, `SELECT $1::integer = ANY(pg_blocking_pids($2::integer))`, firstPID, secondPID).Scan(&blocked); err != nil {
				t.Fatalf("전파의 AGE 잠금 대기 조회: %v", err)
			}
		}
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("파생 갱신 커밋: %v", err)
	}
	if err := <-result; err == nil {
		t.Fatal("동시 파생 갱신과 겹친 전파가 성공했다")
	} else if conflict, ok := errors.AsType[VersionConflictError](err); !ok || conflict.Current != updated.Version {
		t.Fatalf("AGE 전파 충돌 = %v; want 현재 판 %d의 충돌", err, updated.Version)
	}
	current, err := database.Context(t.Context(), graphID, source.ID)
	if err != nil || current.DeletedAt != nil {
		t.Fatalf("실패한 근거 폐기가 남았다: %#v, 오류 %v", current, err)
	}
	if _, err := database.DiscardContext(t.Context(), graphID, source.ID, nil, WriteLimits{}); err != nil {
		t.Fatalf("AGE 충돌 뒤 폐기 재시도: %v", err)
	}
	invalidated, err := database.Context(t.Context(), graphID, derived.ID)
	if err != nil || invalidated.Derived == nil || !invalidated.Derived.EvidenceInvalidated || invalidated.Body != updated.Body || invalidated.Version != updated.Version+1 {
		t.Fatalf("AGE 충돌 재시도 뒤 파생 = %#v, 오류 %v", invalidated, err)
	}
}

// afterEvidenceReadTx는 실제 AGE 결과를 끝까지 읽은 직후 다른 트랜잭션을 실행한다.
// 프로덕션 코드에 시험용 분기 없이 두 Read Committed 트랜잭션의 순서를 고정한다.
type afterEvidenceReadTx struct {
	pgx.Tx
	afterRead func()
}

func (tx *afterEvidenceReadTx) Begin(ctx context.Context) (pgx.Tx, error) {
	child, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &afterEvidenceReadTx{Tx: child, afterRead: tx.afterRead}, nil
}

func (tx *afterEvidenceReadTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil || !strings.Contains(sql, "RETURN derived") {
		return rows, err
	}
	return &afterEvidenceReadRows{Rows: rows, afterRead: tx.afterRead}, nil
}

type afterEvidenceReadRows struct {
	pgx.Rows
	afterRead func()
}

func (rows *afterEvidenceReadRows) Next() bool {
	if rows.Rows.Next() {
		return true
	}
	if rows.Rows.Err() == nil && rows.afterRead != nil {
		afterRead := rows.afterRead
		rows.afterRead = nil
		afterRead()
	}
	return false
}
