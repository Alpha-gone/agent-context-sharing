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

// TestEvidenceInvalidationConflictRollsBackIntegration은 그래프 잠금을 우회하는 시험용
// AGE 갱신을 주입해, 방어적 0행 검사가 폐기·대체 전체를 롤백하는지 확인한다.
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
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := database.pool.Begin(ctx)
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
				properties, encodeErr := encodeProperties(contextProperties(next))
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				// 공개 갱신은 그래프 잠금에서 대기하므로 직접 AGE 쓰기로만 손상을 주입한다.
				query := "MATCH (node:Context) WHERE node.graph_id = " + cypherString(graphID.String()) +
					" AND node.context_id = " + cypherString(derived.ID.String()) + " SET node = " + properties + " RETURN node"
				updated, err = database.contextFromCypher(ctx, database.pool, graphID, query)
				if err != nil {
					t.Fatalf("전파 대상 조회 뒤 동시 갱신 커밋: %v", err)
				}
			}}
			err = apply(context.WithValue(ctx, writeTransactionContextKey{}, pgx.Tx(wrapped)))
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

// TestEvidenceInvalidationWriteSerializationIntegration은 미커밋 파생 갱신 뒤 시작한
// 전파가 그래프 잠금에서 기다린 뒤 새 본문·판을 읽어 한 단계만 표시하는지 확인한다.
func TestEvidenceInvalidationWriteSerializationIntegration(t *testing.T) {
	for _, action := range []string{"discard_source", "discard_derived", "supersede"} {
		t.Run(action, func(t *testing.T) {
			database := newIsolatedIntegrationStore(t)
			actorID := newTestID(t)
			createTestAccount(t, database, actorID)
			graphID := createTestGraph(t, database, actorID)
			source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/pending-evidence-"+graphID.String()), nil)
			if err != nil {
				t.Fatalf("원천 준비: %v", err)
			}
			evidence := source
			if action != "discard_source" {
				evidence, err = database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
				if err != nil {
					t.Fatal(err)
				}
			}
			derived, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{evidence.ID})
			if err != nil {
				t.Fatalf("전파 대상 준비: %v", err)
			}
			sibling, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{evidence.ID})
			if err != nil {
				t.Fatal(err)
			}
			indirect, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{derived.ID})
			if err != nil {
				t.Fatal(err)
			}
			replacement := testDerivedContext(t, graphID, actorID)
			apply := func(ctx context.Context) error {
				if action == "supersede" {
					_, err := database.CreateSupersedingContextWithOperation(ctx, graphID, replacement, []model.ID{source.ID}, evidence.ID, registrationOperation(replacement, OperationSupersede), WriteLimits{})
					return err
				}
				_, err := database.DiscardContext(ctx, graphID, evidence.ID, nil, WriteLimits{})
				return err
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
				result <- apply(context.WithValue(ctx, writeTransactionContextKey{}, second))
			})
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for blocked := false; !blocked; {
				select {
				case err := <-result:
					t.Fatalf("파생 갱신 커밋 전에 전파가 끝났다: %v", err)
				case <-ctx.Done():
					t.Fatalf("전파의 그래프 잠금 대기를 확인하지 못했다: %v", ctx.Err())
				case <-ticker.C:
					if err := database.pool.QueryRow(ctx, `SELECT $1::integer = ANY(pg_blocking_pids($2::integer))`, firstPID, secondPID).Scan(&blocked); err != nil {
						t.Fatalf("전파의 그래프 잠금 대기 조회: %v", err)
					}
				}
			}
			if err := first.Commit(ctx); err != nil {
				t.Fatalf("파생 갱신 커밋: %v", err)
			}
			if err := <-result; err != nil {
				t.Fatalf("갱신 커밋 뒤 근거 폐기: %v", err)
			}
			if err := second.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if action == "supersede" {
				current, err := database.Context(t.Context(), graphID, replacement.ID)
				if err != nil || current.Derived == nil || current.Derived.Supersedes == nil || *current.Derived.Supersedes != evidence.ID {
					t.Fatalf("직렬화한 대체: %#v, 오류 %v", current, err)
				}
			} else {
				current, err := database.Context(t.Context(), graphID, evidence.ID)
				if err != nil || current.DeletedAt == nil {
					t.Fatalf("직렬화한 근거 폐기: %#v, 오류 %v", current, err)
				}
			}
			invalidated, err := database.Context(t.Context(), graphID, derived.ID)
			if err != nil || invalidated.Derived == nil || !invalidated.Derived.EvidenceInvalidated || invalidated.Body != updated.Body || invalidated.Version != updated.Version+1 {
				t.Fatalf("직렬화한 전파 뒤 파생 = %#v, 오류 %v", invalidated, err)
			}
			marked, err := database.Context(t.Context(), graphID, sibling.ID)
			if err != nil || marked.Derived == nil || !marked.Derived.EvidenceInvalidated || marked.Version != sibling.Version+1 || marked.Body != sibling.Body {
				t.Fatalf("다른 직접 파생의 전파: %#v, 오류 %v", marked, err)
			}
			unchanged, err := database.Context(t.Context(), graphID, indirect.ID)
			if err != nil || unchanged.Derived == nil || unchanged.Derived.EvidenceInvalidated || unchanged.Version != indirect.Version || unchanged.Body != indirect.Body {
				t.Fatalf("간접 파생으로 전파됐다: %#v, 오류 %v", unchanged, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

// afterEvidenceReadTx는 실제 AGE 결과를 끝까지 읽은 직후 다른 트랜잭션을 실행한다.
// 프로덕션 코드에 시험용 분기 없이 두 Read Committed 트랜잭션의 순서를 고정한다.
type afterEvidenceReadTx struct {
	pgx.Tx
	afterRead           func()
	evidenceTargetsRead bool
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
	if err != nil {
		return rows, err
	}
	if strings.Contains(sql, "RETURN derived.context_id") {
		tx.evidenceTargetsRead = true
	}
	// 대상 ID 조회 뒤 공통 조립 경계가 본문과 판을 다시 읽는다. ID만 읽은
	// 시점에 커밋하면 새 판을 정상 사용하므로, 실제 정점 판을 읽은 뒤에 겹친다.
	if !tx.evidenceTargetsRead || !strings.Contains(sql, "RETURN node ORDER BY node.context_id") || tx.afterRead == nil {
		return rows, nil
	}
	afterRead := tx.afterRead
	tx.afterRead = nil
	return &afterEvidenceReadRows{Rows: rows, afterRead: afterRead}, nil
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
