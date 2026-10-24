package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"github.com/jackc/pgx/v5"
)

func lifeDerived(t *testing.T, database *Store, graph, actor model.ID, refs ...model.ID) model.Context {
	t.Helper()
	value := testDerivedContext(t, graph, actor)
	value.Derived.EvidenceState = model.EvidenceStateOpinion
	value.Derived.ConfidenceState = model.ConfidenceStateUncertain
	stored, err := database.CreateContext(t.Context(), graph, value, refs)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestDerivedLifecycleUpdateIntegration(t *testing.T) {
	for _, change := range []string{"body", "confidence", "validity"} {
		t.Run(change, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			old := lifeDerived(t, database, graph, actor, source.ID)
			replacement := testDerivedContext(t, graph, actor)
			replacement.Derived.EvidenceState = model.EvidenceStateOpinion
			replacement.Derived.ConfidenceState = model.ConfidenceStateUncertain
			value, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, replacement, []model.ID{source.ID}, old.ID, registrationOperation(replacement, OperationSupersede), WriteLimits{})
			if err != nil {
				t.Fatal(err)
			}
			// 편집 권한은 요청 경계의 책임이다. 원 작성자와 다른 편집자의 감사 주체를 주입한다.
			editor := newTestID(t)
			createTestAccount(t, database, editor)
			if err := database.GrantGraph(t.Context(), graph, actor, editor, GrantSubjectAccount, model.GraphGradeEditor); err != nil {
				t.Fatal(err)
			}
			if err := perm.Require(t.Context(), database, graph, editor, model.GraphGradeEditor); err != nil {
				t.Fatal(err)
			}
			viewer := newTestID(t)
			createTestAccount(t, database, viewer)
			if err := database.GrantGraph(t.Context(), graph, actor, viewer, GrantSubjectAccount, model.GraphGradeViewer); err != nil {
				t.Fatal(err)
			}
			if _, ok := errors.AsType[perm.DeniedError](perm.Require(t.Context(), database, graph, viewer, model.GraphGradeEditor)); !ok {
				t.Fatal("조회자에게 갱신을 허용했다")
			}
			before, _ := database.Graph(t.Context(), graph)
			task := registrationTaskFor(t, database, value.ID)
			next := attachReferences(value, value.Derived.DerivedFrom, nil)
			next.Version++
			switch change {
			case "body":
				next.Body = "새🙂 파생 본문"
			case "confidence":
				next.Derived.ConfidenceState = model.ConfidenceStateDisputed
			case "validity":
				next.Derived.ValidFrom = new(time.Now().UTC())
				next.Derived.ValidTo = new(next.Derived.ValidFrom.Add(time.Hour))
			}
			op := registrationOperation(next, OperationUpdate)
			op.AccountID, op.AgentID = editor, editor
			stored, err := database.UpdateContextWithOperation(t.Context(), graph, value.Version, next, op, WriteLimits{})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := database.Context(t.Context(), graph, value.ID)
			if err != nil || !reflect.DeepEqual(stored, loaded) || !reflect.DeepEqual(next, loaded) {
				t.Fatalf("갱신·참조 왕복=%+v/%v", loaded, err)
			}
			oldAfter, err := database.Context(t.Context(), graph, old.ID)
			if err != nil || !reflect.DeepEqual(old, oldAfter) {
				t.Fatalf("대체된 이전 파생 보존=%+v/%v", oldAfter, err)
			}
			after, err := database.Graph(t.Context(), graph)
			if err != nil || after.StoredChars-before.StoredChars != int64(utf8.RuneCountInString(next.Body)-utf8.RuneCountInString(value.Body)) || after.Version != before.Version {
				t.Fatalf("저장량·그래프 판=%+v/%v", after, err)
			}
			nextTask := registrationTaskFor(t, database, value.ID)
			if change == "body" && !nextTask.enqueued.After(task.enqueued) || change != "body" && !reflect.DeepEqual(task, nextTask) {
				t.Fatalf("본문 변경만 색인 등록=%+v/%+v", task, nextTask)
			}
			var audits int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE context_id=$1 AND operation_kind='update' AND target_version=$2 AND actor_account_id=$3`, value.ID.String(), next.Version, editor.String()).Scan(&audits); err != nil || audits != 1 {
				t.Fatalf("편집자·결과 판 감사=%d/%v", audits, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestDerivedLifecycleRejectIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	value := lifeDerived(t, database, graph, actor, source.ID)
	for _, field := range []string{"source", "kind", "scope", "references", "evidence_state", "invalidated", "supersedes", "recorded_at", "creator", "agent", "version", "deleted", "audit_graph", "audit_target", "audit_kind", "audit_relation"} {
		t.Run(field, func(t *testing.T) {
			next := attachReferences(value, value.Derived.DerivedFrom, nil)
			next.Version++
			op := registrationOperation(value, OperationUpdate)
			switch field {
			case "source":
				next = source
				op = registrationOperation(source, OperationUpdate)
			case "kind":
				next.Derived.Kind = model.DerivationKindReflection
			case "scope":
				next.Derived.SummaryScope = model.SummaryScopeGlobal
			case "references":
				next.Derived.DerivedFrom = []model.ID{newTestID(t)}
			case "evidence_state":
				next.Derived.EvidenceState, next.Derived.ConfidenceState = model.EvidenceStateObservation, ""
			case "invalidated":
				next.Derived.EvidenceInvalidated = true
			case "supersedes":
				next.Derived.Supersedes = new(source.ID)
			case "recorded_at":
				next.RecordedAt = next.RecordedAt.Add(time.Second)
			case "creator":
				next.CreatedBy = newTestID(t)
			case "agent":
				next.CreatedByAgent = newTestID(t)
			case "version":
				next.Version++
			case "deleted":
				next.DeletedAt = new(time.Now().UTC())
			case "audit_graph":
				op.GraphID = newTestID(t)
			case "audit_target":
				op.ContextID = source.ID
			case "audit_kind":
				op.Kind = OperationKeep
			case "audit_relation":
				op.RelationID = newTestID(t)
			}
			before := registrationStateFingerprint(t, database)
			if _, err := database.UpdateContextWithOperation(t.Context(), graph, 1, next, op, WriteLimits{}); err == nil {
				t.Fatal("거부해야 할 갱신이 적용됐다")
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("거부된 갱신의 부분 적용")
			}
		})
	}
	for _, invalid := range []string{"missing", "graph", "target", "kind", "relation"} {
		t.Run("keep_"+invalid, func(t *testing.T) {
			op := registrationOperation(value, OperationKeep)
			switch invalid {
			case "missing":
				op = nil
			case "graph":
				op.GraphID = newTestID(t)
			case "target":
				op.ContextID = source.ID
			case "kind":
				op.Kind = OperationUpdate
			case "relation":
				op.RelationID = newTestID(t)
			}
			before := registrationStateFingerprint(t, database)
			if _, err := database.KeepContext(t.Context(), graph, value.ID, value.Version, op); err == nil {
				t.Fatal("잘못된 유지 기록 허용")
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("거부된 유지의 부분 적용")
			}
		})
	}
}

func TestDerivedLifecycleEvidenceAndKeepIntegration(t *testing.T) {
	for _, action := range []string{"discard_source", "discard_derived", "supersede"} {
		t.Run(action, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			evidence := source
			if action != "discard_source" {
				evidence = lifeDerived(t, database, graph, actor, source.ID)
			}
			direct := lifeDerived(t, database, graph, actor, evidence.ID)
			indirect := lifeDerived(t, database, graph, actor, direct.ID)
			if action == "supersede" {
				next := testDerivedContext(t, graph, actor)
				if _, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, next, []model.ID{source.ID}, evidence.ID, registrationOperation(next, OperationSupersede), WriteLimits{}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := database.DiscardContext(t.Context(), graph, evidence.ID, registrationOperation(evidence, OperationDiscard), WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			flagged, err := database.Context(t.Context(), graph, direct.ID)
			if err != nil || !flagged.Derived.EvidenceInvalidated || flagged.Version != direct.Version+1 || flagged.DeletedAt != nil || flagged.Body != direct.Body || !reflect.DeepEqual(flagged.Derived.DerivedFrom, direct.Derived.DerivedFrom) {
				t.Fatalf("한 단계 근거 표시=%+v/%v", flagged, err)
			}
			unchanged, err := database.Context(t.Context(), graph, indirect.ID)
			if err != nil || !reflect.DeepEqual(indirect, unchanged) {
				t.Fatalf("간접 파생으로 전파=%+v/%v", unchanged, err)
			}
			before, _ := database.Graph(t.Context(), graph)
			task := registrationTaskFor(t, database, direct.ID)
			var revision, afterRevision int64
			if err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			kept, err := database.KeepContext(t.Context(), graph, direct.ID, flagged.Version, registrationOperation(flagged, OperationKeep))
			if err != nil || !reflect.DeepEqual(flagged, kept) {
				t.Fatalf("무효 표시 유지=%+v/%v", kept, err)
			}
			after, _ := database.Graph(t.Context(), graph)
			var audits int
			if err := database.pool.QueryRow(t.Context(), `SELECT content_revision,(SELECT count(*) FROM public.operation_log WHERE context_id=$2 AND operation_kind='keep' AND target_version=$3) FROM public.context_graph WHERE graph_id=$1`, graph.String(), direct.ID.String(), flagged.Version).Scan(&afterRevision, &audits); err != nil || audits != 1 || revision != afterRevision || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(task, registrationTaskFor(t, database, direct.ID)) {
				t.Fatalf("유지는 상태 없이 기록만 적용=%d/%d/%d/%v", revision, afterRevision, audits, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestDerivedLifecycleRollbackIntegration(t *testing.T) {
	for _, action := range []string{"update", "supersede", "keep"} {
		stages := []string{"vertex", "queue", "activity", "history", "audit", "commit"}
		if action == "supersede" {
			stages = append(stages, "supersedes", "invalidation")
		} else if action == "keep" {
			stages = []string{"audit", "commit"}
		}
		for _, stage := range stages {
			t.Run(action+"/"+stage, func(t *testing.T) {
				database := newSmallPoolStore(t)
				graph, actor, source := boundaryGraph(t, database)
				value := lifeDerived(t, database, graph, actor, source.ID)
				lifeDerived(t, database, graph, actor, value.ID)
				next := attachReferences(value, value.Derived.DerivedFrom, nil)
				next.Body, next.Version = "새🙂 본문", value.Version+1
				if action == "supersede" {
					next = testDerivedContext(t, graph, actor)
				}
				apply := func(ctx context.Context) error {
					switch action {
					case "keep":
						_, err := database.KeepContext(ctx, graph, value.ID, value.Version, registrationOperation(value, OperationKeep))
						return err
					case "supersede":
						_, err := database.CreateSupersedingContextWithOperation(ctx, graph, next, []model.ID{source.ID}, value.ID, registrationOperation(next, OperationSupersede), WriteLimits{ActorID: actor, WritesPerMinute: 1})
						return err
					default:
						_, err := database.UpdateContextWithOperation(ctx, graph, value.Version, next, registrationOperation(value, OperationUpdate), WriteLimits{ActorID: actor, WritesPerMinute: 1})
						return err
					}
				}
				request := t.Context()
				var outer pgx.Tx
				remove := func() {}
				if stage == "vertex" || stage == "supersedes" || stage == "invalidation" {
					var err error
					outer, err = database.pool.Begin(request)
					if err != nil {
						t.Fatal(err)
					}
					defer rollbackTransaction(request, outer)
					fragment := " SET node = "
					if action == "supersede" && stage == "vertex" {
						fragment = "CREATE (node:Context"
					} else if stage == "supersedes" {
						fragment = "CREATE (next)-[:SUPERSEDES"
					} else if stage == "invalidation" {
						fragment = "AND node.evidence_invalidated <> true"
					}
					request = context.WithValue(request, writeTransactionContextKey{}, pgx.Tx(nodeCreationFailureTx{Tx: outer, fragment: fragment}))
				} else {
					remove = nodeFailCreation(t, database, graph, stage)
				}
				before := registrationStateFingerprint(t, database)
				if err := apply(request); err == nil {
					t.Fatal("주입 장애가 적용되지 않았다")
				}
				if outer != nil {
					if err := outer.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if before != registrationStateFingerprint(t, database) {
					t.Fatal("실패한 수명주기 전이의 부분 적용")
				}
				remove()
				if err := apply(t.Context()); err != nil {
					t.Fatalf("실패 후 같은 요청 재시도=%v", err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestDerivedLifecycleKeepWaitIntegration(t *testing.T) {
	for _, change := range []string{"update", "discard", "cancel"} {
		t.Run(change, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			value := lifeDerived(t, database, graph, actor, source.ID)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			first, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), first)
			if err := requireActiveGraph(ctx, first, graph); err != nil {
				t.Fatal(err)
			}
			if err := database.lockContext(ctx, first, graph, value.ID); err != nil {
				t.Fatal(err)
			}
			request, stop := context.WithCancel(ctx)
			var group sync.WaitGroup
			defer func() { stop(); group.Wait() }()
			result := make(chan error, 1)
			group.Go(func() {
				_, err := database.KeepContext(request, graph, value.ID, value.Version, registrationOperation(value, OperationKeep))
				result <- err
			})
			waitGrantWaiters(t, ctx, database, int(first.Conn().PgConn().PID()), 1)
			if change == "cancel" {
				stop()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("유지 잠금 취소=%v", err)
				}
				if err := first.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				writeCtx := context.WithValue(ctx, writeTransactionContextKey{}, first)
				if change == "update" {
					next := value
					next.Body, next.Version = "잠금 뒤 새 판", value.Version+1
					_, err = database.UpdateContext(writeCtx, graph, value.Version, next)
				} else {
					_, err = database.DiscardContext(writeCtx, graph, value.ID, nil, WriteLimits{})
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := first.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				err = <-result
				if change == "discard" && !errors.Is(err, ErrNotFound) {
					t.Fatalf("폐기 뒤 유지=%v", err)
				}
				if change == "update" {
					if conflict, ok := errors.AsType[VersionConflictError](err); !ok || conflict.Current != value.Version+1 {
						t.Fatalf("새 판 뒤 유지=%v", err)
					}
				}
			}
			var count int
			if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM public.operation_log WHERE context_id=$1 AND operation_kind='keep'`, value.ID.String()).Scan(&count); err != nil || count != 0 {
				t.Fatalf("거부된 유지 감사=%d/%v", count, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestDerivedLifecycleKeepBeforeDiscardIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		t.Run(string(layer), func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			value := source
			if layer != model.LayerSource {
				candidate, refs := nodeCreateSample(t, graph, actor, source.ID, layer)
				var err error
				value, err = database.CreateContext(t.Context(), graph, candidate, refs)
				if err != nil {
					t.Fatal(err)
				}
			}
			before, err := database.Graph(t.Context(), graph)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			outer, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), outer)
			kept, err := database.KeepContext(context.WithValue(ctx, writeTransactionContextKey{}, outer), graph, value.ID, value.Version, registrationOperation(value, OperationKeep))
			if err != nil || !reflect.DeepEqual(value, kept) {
				t.Fatalf("활성 노드 유지=%+v/%v", kept, err)
			}
			result := make(chan error, 1)
			var group sync.WaitGroup
			defer func() { cancel(); group.Wait() }()
			group.Go(func() {
				_, err := database.DiscardContext(ctx, graph, value.ID, registrationOperation(value, OperationDiscard), WriteLimits{})
				result <- err
			})
			waitGrantWaiters(t, ctx, database, int(outer.Conn().PgConn().PID()), 1)
			if err := outer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatalf("유지 커밋 뒤 폐기=%v", err)
			}
			var keepVersion int64
			if err := database.pool.QueryRow(ctx, `SELECT target_version FROM public.operation_log WHERE context_id=$1 AND operation_kind='keep'`, value.ID.String()).Scan(&keepVersion); err != nil || keepVersion != value.Version {
				t.Fatalf("유지 대상 판=%d/%v", keepVersion, err)
			}
			after, err := database.Graph(ctx, graph)
			if err != nil || after.StoredChars != before.StoredChars-int64(utf8.RuneCountInString(value.Body)) {
				t.Fatalf("폐기만 저장량 변경=%+v/%v", after, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestDerivedLifecycleSupersedeWaitIntegration(t *testing.T) {
	for _, action := range []string{"discard", "cancel"} {
		t.Run(action, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			previous := lifeDerived(t, database, graph, actor, source.ID)
			next := testDerivedContext(t, graph, actor)
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
			if err := database.lockContext(ctx, outer, graph, previous.ID); err != nil {
				t.Fatal(err)
			}
			request, stop := context.WithCancel(ctx)
			result := make(chan error, 1)
			var group sync.WaitGroup
			defer func() { stop(); group.Wait() }()
			group.Go(func() {
				_, err := database.CreateSupersedingContextWithOperation(request, graph, next, []model.ID{source.ID}, previous.ID, registrationOperation(next, OperationSupersede), WriteLimits{})
				result <- err
			})
			waitGrantWaiters(t, ctx, database, int(outer.Conn().PgConn().PID()), 1)
			if action == "cancel" {
				stop()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("대체 잠금 취소=%v", err)
				}
				if err := outer.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := database.DiscardContext(context.WithValue(ctx, writeTransactionContextKey{}, outer), graph, previous.ID, nil, WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				if err := outer.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if _, ok := errors.AsType[model.FieldError](<-result); !ok {
					t.Fatal("폐기된 이전 파생을 대체했다")
				}
			}
			if _, err := database.Context(ctx, graph, next.ID); !errors.Is(err, ErrNotFound) || indexTaskCount(t, database, next.ID) != 0 {
				t.Fatalf("거부된 대체 생성의 부분 적용=%v", err)
			}
			if applied, err := database.HasOperationJudgment(ctx, graph, registrationOperation(next, OperationSupersede).JudgmentInput); err != nil || applied {
				t.Fatalf("거부된 대체 감사=%t/%v", applied, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
