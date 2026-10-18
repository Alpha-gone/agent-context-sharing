package store

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func TestContextRevisionCreationDuringInvalidationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	lifeDerived(t, database, graph, actor, source.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	outer, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(t.Context(), outer)
	var group sync.WaitGroup
	defer func() { cancel(); rollbackTransaction(t.Context(), outer); group.Wait() }()
	result := make(chan error, 1)
	started := false
	wrapped := &afterEvidenceReadTx{Tx: outer, afterRead: func() {
		started = true
		group.Go(func() {
			_, err := database.CreateContext(ctx, graph, testDerivedContext(t, graph, actor), []model.ID{source.ID})
			result <- err
		})
		// 재검토에서 누락을 재현했던 바로 그 읽기 경계에서 새 생성의 실제 대기를 확인한다.
		waitGrantWaiters(t, ctx, database, int(outer.Conn().PgConn().PID()), 1)
	}}
	if _, err := database.DiscardContext(context.WithValue(ctx, writeTransactionContextKey{}, pgx.Tx(wrapped)), graph, source.ID, nil, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("전파 대상 읽기 경계에 도달하지 않았다")
	}
	if err := outer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if field, ok := errors.AsType[model.FieldError](<-result); !ok || field.Field != "derived_from" {
		t.Fatal("전파 대상 조회 뒤 시작한 새 생성이 폐기된 근거를 사용했다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestContextRevisionReadValidationIntegration(t *testing.T) {
	for _, field := range []string{"graph", "context", "version"} {
		t.Run(field, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			node := lifeDerived(t, database, graph, actor, source.ID)
			corrupt := node
			switch field {
			case "graph":
				corrupt.GraphID = newTestID(t)
			case "context":
				corrupt.ID = newTestID(t)
			case "version":
				corrupt.Version++
			}
			raw, err := json.Marshal(corrupt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_revision SET snapshot=$1 WHERE graph_id=$2 AND context_id=$3`, raw, graph.String(), node.ID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := database.ContextRevision(t.Context(), graph, node.ID, node.Version); err == nil {
				t.Fatal("이력의 요청 식별자·판 재검사가 없다")
			} else if _, ok := errors.AsType[*StorageError](err); !ok {
				t.Fatalf("저장 손상 오류=%v", err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestContextRevisionHistoryIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	first := lifeDerived(t, database, graph, actor, source.ID)
	second := attachReferences(first, first.Derived.DerivedFrom, nil)
	second.Body, second.Version = "두 번째 본문🙂", 2
	second, err := database.UpdateContext(t.Context(), graph, 1, second)
	if err != nil {
		t.Fatal(err)
	}
	third := attachReferences(second, second.Derived.DerivedFrom, nil)
	third.Derived.ConfidenceState, third.Version = model.ConfidenceStateDisputed, 3
	third, err = database.UpdateContext(t.Context(), graph, 2, third)
	if err != nil {
		t.Fatal(err)
	}
	candidate := testDerivedContext(t, graph, actor)
	replacement, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, candidate, []model.ID{source.ID}, third.ID, registrationOperation(candidate, OperationSupersede), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for index, expected := range []model.Context{first, second, third} {
		revision, err := database.ContextRevision(t.Context(), graph, first.ID, int64(index+1))
		if err != nil || !reflect.DeepEqual(revision.Context, expected) {
			t.Fatalf("과거 판 %d=%+v/%v", index+1, revision, err)
		}
		if index == 0 && revision.PreviousVersion != nil || index > 0 && (revision.PreviousVersion == nil || *revision.PreviousVersion != int64(index)) {
			t.Fatalf("이전 판 연결=%+v", revision)
		}
	}
	fourth := third
	fourth.Body, fourth.Version = "대체 뒤에 이전 컨텍스트도 변경", 4
	if _, err := database.UpdateContext(t.Context(), graph, 3, fourth); err != nil {
		t.Fatal(err)
	}
	newer := attachReferences(replacement, replacement.Derived.DerivedFrom, nil)
	newer.Body, newer.Version = "대체 결과 갱신", 2
	if _, err := database.UpdateContext(t.Context(), graph, 1, newer); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{1, 2} {
		revision, err := database.ContextRevision(t.Context(), graph, replacement.ID, version)
		if err != nil || revision.SupersedesVersion == nil || *revision.SupersedesVersion != 3 {
			t.Fatalf("대체 당시 판 고정=%+v/%v", revision, err)
		}
	}
	archived, err := database.ContextRevision(t.Context(), graph, first.ID, 3)
	if err != nil || archived.Context.Body != third.Body {
		t.Fatalf("대체 당시 본문 보존=%+v/%v", archived, err)
	}
	if _, err := database.DiscardContext(t.Context(), graph, source.ID, registrationOperation(source, OperationDiscard), WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	flagged, err := database.ContextRevision(t.Context(), graph, replacement.ID, 3)
	if err != nil || !flagged.Context.Derived.EvidenceInvalidated || flagged.PreviousVersion == nil || *flagged.PreviousVersion != 2 {
		t.Fatalf("시스템 전이 이력=%+v/%v", flagged, err)
	}
	deleted, err := database.DiscardContext(t.Context(), graph, replacement.ID, registrationOperation(flagged.Context, OperationDiscard), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := database.RestoreContext(t.Context(), graph, replacement.ID, registrationOperation(deleted, OperationUpdate), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{4, 5} {
		revision, err := database.ContextRevision(t.Context(), graph, replacement.ID, version)
		if err != nil || (revision.Context.DeletedAt != nil) != (version == 4) {
			t.Fatalf("폐기·복구 이력=%+v/%v", revision, err)
		}
	}
	var before, after int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.context_revision WHERE graph_id=$1`, graph.String()).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := database.KeepContext(t.Context(), graph, replacement.ID, restored.Version, registrationOperation(restored, OperationKeep)); err != nil {
		t.Fatal(err)
	}
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.context_revision WHERE graph_id=$1`, graph.String()).Scan(&after); err != nil || before != after {
		t.Fatalf("유지의 이력 증가=%d/%d/%v", before, after, err)
	}
	if _, err := database.ContextRevision(t.Context(), newTestID(t), first.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("이력 그래프 격리=%v", err)
	}
	if _, err := database.ContextRevision(t.Context(), graph, first.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("없는 판=%v", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestContextRevisionLegacyBaselineIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	node := lifeDerived(t, database, graph, actor, source.ID)
	// 013 도입 이전에 이미 판 7이 존재하던 경우를 재현한다.
	if _, err := database.pool.Exec(t.Context(), `DELETE FROM public.context_revision WHERE graph_id=$1`, graph.String()); err != nil {
		t.Fatal(err)
	}
	isolationExec(t, database, "MATCH (n:Context) WHERE n.context_id = "+cypherString(node.ID.String())+" SET n.version = 7")
	node.Version = 7
	next := node
	next.Version = 8
	next.Body = "도입 이후 첫 수정"
	if _, err := database.UpdateContext(t.Context(), graph, 7, next); err != nil {
		t.Fatal(err)
	}
	baseline, err := database.ContextRevision(t.Context(), graph, node.ID, 7)
	if err != nil || baseline.PreviousVersion != nil || !reflect.DeepEqual(baseline.Context, node) {
		t.Fatalf("관찰 기준 판=%+v/%v", baseline, err)
	}
	if _, err := database.ContextRevision(t.Context(), graph, node.ID, 6); !errors.Is(err, ErrNotFound) {
		t.Fatalf("알 수 없는 과거 판 생성=%v", err)
	}
	current, err := database.ContextRevision(t.Context(), graph, node.ID, 8)
	if err != nil || current.PreviousVersion == nil || *current.PreviousVersion != 7 {
		t.Fatalf("기준 판 이후 연결=%+v/%v", current, err)
	}
}

func TestContextRevisionRejectedSupersedeEvidenceIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	previous := lifeDerived(t, database, graph, actor, source.ID)
	next := testDerivedContext(t, graph, actor)
	before := registrationStateFingerprint(t, database)
	_, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, next, []model.ID{previous.ID}, previous.ID, registrationOperation(next, OperationSupersede), WriteLimits{})
	if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != "derived_from" {
		t.Fatalf("자기 대체 근거 거부=%v", err)
	}
	if before != registrationStateFingerprint(t, database) {
		t.Fatal("거부된 대체의 부분 적용")
	}
}

func TestContextRevisionReferenceSerializationIntegration(t *testing.T) {
	for _, action := range []string{"discard_first", "create_first", "supersede_first", "cancel", "other_graph"} {
		t.Run(action, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			evidence := source
			if action == "supersede_first" {
				evidence = lifeDerived(t, database, graph, actor, source.ID)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			outer, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), outer)
			if err := lockReferenceChanges(ctx, outer, graph); err != nil {
				t.Fatal(err)
			}
			next := testDerivedContext(t, graph, actor)
			writeCtx := context.WithValue(ctx, writeTransactionContextKey{}, outer)
			if action == "create_first" {
				if _, err := database.CreateContext(writeCtx, graph, next, []model.ID{source.ID}); err != nil {
					t.Fatal(err)
				}
			}
			request, stop := context.WithCancel(ctx)
			var group sync.WaitGroup
			defer func() { stop(); rollbackTransaction(t.Context(), outer); group.Wait() }()
			result := make(chan error, 1)
			if action == "other_graph" {
				otherGraph, otherActor, otherSource := boundaryGraph(t, database)
				if _, err := database.CreateContext(request, otherGraph, testDerivedContext(t, otherGraph, otherActor), []model.ID{otherSource.ID}); err != nil {
					t.Fatal(err)
				}
				if err := outer.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
			group.Go(func() {
				if action == "create_first" {
					_, err := database.DiscardContext(request, graph, source.ID, nil, WriteLimits{})
					result <- err
				} else {
					_, err := database.CreateContext(request, graph, next, []model.ID{evidence.ID})
					result <- err
				}
			})
			waitGrantWaiters(t, ctx, database, int(outer.Conn().PgConn().PID()), 1)
			if action == "cancel" {
				stop()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("참조 잠금 취소=%v", err)
				}
				if err := outer.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if action == "discard_first" {
					if _, err := database.DiscardContext(writeCtx, graph, source.ID, nil, WriteLimits{}); err != nil {
						t.Fatal(err)
					}
				} else if action == "supersede_first" {
					if _, err := database.CreateSupersedingContextWithOperation(writeCtx, graph, testDerivedContext(t, graph, actor), []model.ID{source.ID}, evidence.ID, nil, WriteLimits{}); err != nil {
						t.Fatal(err)
					}
				}
				if err := outer.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				err := <-result
				if action == "discard_first" || action == "supersede_first" {
					if _, ok := errors.AsType[model.FieldError](err); !ok {
						t.Fatalf("폐기 후 생성 거부=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if action == "create_first" {
					loaded, err := database.Context(ctx, graph, next.ID)
					if err != nil || !loaded.Derived.EvidenceInvalidated {
						t.Fatalf("새 직접 파생 표시=%+v/%v", loaded, err)
					}
				}
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestContextRevisionFailureIntegration(t *testing.T) {
	for _, action := range []string{"create", "supersede", "update", "invalidation"} {
		t.Run(action, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			node := lifeDerived(t, database, graph, actor, source.ID)
			next := testDerivedContext(t, graph, actor)
			remove := nodeFailCreation(t, database, graph, "history")
			before := registrationStateFingerprint(t, database)
			outer, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), outer)
			request := context.WithValue(t.Context(), writeTransactionContextKey{}, pgx.Tx(outer))
			switch action {
			case "create":
				_, err = database.CreateContext(request, graph, next, []model.ID{source.ID})
			case "supersede":
				_, err = database.CreateSupersedingContextWithOperation(request, graph, next, []model.ID{source.ID}, node.ID, nil, WriteLimits{})
			case "update":
				next = node
				next.Version++
				next.Body = "변경"
				_, err = database.UpdateContext(request, graph, node.Version, next)
			case "invalidation":
				_, err = database.DiscardContext(request, graph, source.ID, nil, WriteLimits{})
			}
			if err == nil {
				t.Fatal("이력 실패를 무시했다")
			}
			if err := outer.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("이력 실패 뒤 부분 적용")
			}
			remove()
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
