package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func lifeRelationRequest(t *testing.T, from, to model.Context, kind model.RelationType) (model.Relation, *OperationRecord) {
	t.Helper()
	request := confirmable(t, from.GraphID, kind, from.ID, to.ID)
	request.ConfirmedBy, request.ConfirmedByAgent = from.CreatedBy, from.CreatedByAgent
	return request, &OperationRecord{Kind: OperationAdd, GraphID: from.GraphID, RelationID: request.ID,
		JudgmentInput: "life04 판단", AccountID: request.ConfirmedBy, AgentID: request.ConfirmedByAgent}
}

func lifeContentRevision(t *testing.T, database *Store, graph model.ID) int64 {
	t.Helper()
	var revision int64
	if err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestLifeRelationTransitionsIntegration(t *testing.T) {
	for _, kind := range []model.RelationType{model.RelationTypePrecedes, model.RelationTypeCauses, model.RelationTypePartOf, model.RelationTypeRelatesTo} {
		for _, initial := range []string{"new", "proposed", "discarded", "confirmed"} {
			t.Run(string(kind)+"/"+initial, func(t *testing.T) {
				database := newSmallPoolStore(t)
				graph, from, to := relationTestEvents(t, database)
				request, operation := lifeRelationRequest(t, from, to, kind)
				var previous model.Relation
				if initial == "proposed" {
					candidate := proposedRelation(graph, kind, from.ID, to.ID)
					candidate.ID = newTestID(t)
					var err error
					previous, err = database.CreateRelation(t.Context(), graph, candidate)
					if err != nil {
						t.Fatal(err)
					}
				} else if initial != "new" {
					var err error
					previous, err = database.ConfirmRelation(t.Context(), graph, request, nil)
					if err != nil {
						t.Fatal(err)
					}
					if initial == "discarded" {
						previous, err = database.DiscardRelation(t.Context(), graph, previous.ID, nil)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				request.ID = newTestID(t) // 후보·기존 관계의 실제 ID를 감사에 사용해야 한다.
				operation.RelationID = request.ID
				request.ProposedBy = model.ProposalSourceSystem // 후보를 넘겨도 신규 직접 확정 출처는 agent다.
				if kind == model.RelationTypeRelatesTo {
					request.FromContextID, request.ToContextID = to.ID, from.ID
				}
				before, err := database.Graph(t.Context(), graph)
				if err != nil {
					t.Fatal(err)
				}
				revision := lifeContentRevision(t, database, graph)
				task := registrationTaskFor(t, database, from.ID)
				stored, err := database.ConfirmRelation(t.Context(), graph, request, operation)
				if err != nil || stored.State != model.RelationStateConfirmed || stored.DeletedAt != nil {
					t.Fatalf("확정=%+v/%v", stored, err)
				}
				if !previous.ID.IsZero() && (stored.ID != previous.ID || stored.ProposedBy != previous.ProposedBy || !stored.ProposedAt.Equal(previous.ProposedAt)) {
					t.Fatal("재확정이 관계 정체성·제안 이력을 변경했다")
				}
				if initial == "new" && stored.ProposedBy != model.ProposalSourceAgent {
					t.Fatal("직접 확정이 시스템 제안으로 저장됐다")
				}
				wantRevision := revision + 1
				if initial == "confirmed" {
					wantRevision = revision
				}
				if lifeContentRevision(t, database, graph) != wantRevision {
					t.Fatal("관계 확정 내용 판")
				}
				if initial == "confirmed" && !reflect.DeepEqual(stored, previous) {
					t.Fatal("멱등 확정이 현재 관계를 변경했다")
				}
				var audits int
				if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE graph_id=$1 AND relation_id=$2
				 AND operation_kind='add' AND context_id IS NULL AND target_version IS NULL AND actor_account_id=$3 AND actor_agent_id=$4`, graph.String(), stored.ID.String(), operation.AccountID.String(), operation.AgentID.String()).Scan(&audits); err != nil || audits != 1 {
					t.Fatalf("실제 대상·주체 감사=%d/%v", audits, err)
				}
				after, err := database.Graph(t.Context(), graph)
				if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(task, registrationTaskFor(t, database, from.ID)) {
					t.Fatal("관계가 노드 저장량·활동·색인을 변경했다")
				}
				discard := *operation
				discard.Kind, discard.RelationID = OperationDiscard, stored.ID
				stored, err = database.DiscardRelation(t.Context(), graph, stored.ID, &discard)
				if err != nil || stored.State != model.RelationStateDiscarded || stored.DeletedAt == nil || stored.ConfirmedAt != nil || !stored.ConfirmedBy.IsZero() || !stored.ConfirmedByAgent.IsZero() {
					t.Fatalf("폐기=%+v/%v", stored, err)
				}
				if lifeContentRevision(t, database, graph) != wantRevision+1 {
					t.Fatal("폐기 내용 판")
				}
				fingerprint := registrationStateFingerprint(t, database)
				if _, err := database.DiscardRelation(t.Context(), graph, stored.ID, &discard); !errors.Is(err, ErrInvalidState) {
					t.Fatalf("재폐기=%v", err)
				}
				if fingerprint != registrationStateFingerprint(t, database) {
					t.Fatal("거부된 재폐기의 부분 적용")
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeRelationInputIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, from, to := relationTestEvents(t, database)
	request, operation := lifeRelationRequest(t, from, to, model.RelationTypePrecedes)
	for _, mutation := range []string{"state", "source", "account", "agent", "graph", "kind", "context", "target", "version", "self", "non_event", "time"} {
		t.Run(mutation, func(t *testing.T) {
			value, op := request, *operation
			switch mutation {
			case "state":
				value.State, value.ConfirmedAt, value.ConfirmedBy, value.ConfirmedByAgent = model.RelationStateProposed, nil, model.ID{}, model.ID{}
			case "source":
				value.ProposedBy = "invalid"
			case "account":
				op.AccountID = newTestID(t)
			case "agent":
				op.AgentID = newTestID(t)
			case "graph":
				op.GraphID = newTestID(t)
			case "kind":
				op.Kind = OperationDiscard
			case "context":
				op.ContextID = from.ID
			case "target":
				op.RelationID = newTestID(t)
			case "version":
				op.TargetVersion = 1
			case "self":
				value.ToContextID = value.FromContextID
			case "non_event":
				value.FromContextID = from.Event.MemberIDs[0]
			case "time":
				value.Type, value.FromContextID, value.ToContextID = model.RelationTypePartOf, to.ID, from.ID
			}
			before := registrationStateFingerprint(t, database)
			_, err := database.ConfirmRelation(t.Context(), graph, value, &op)
			if _, ok := errors.AsType[model.FieldError](err); !ok {
				t.Fatalf("입력 거부=%v", err)
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("거부된 확정의 부분 적용")
			}
		})
	}
	stored, err := database.ConfirmRelation(t.Context(), graph, request, operation)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"graph", "kind", "context", "target", "version"} {
		t.Run("discard/"+mutation, func(t *testing.T) {
			op := *operation
			op.Kind, op.RelationID = OperationDiscard, stored.ID
			switch mutation {
			case "graph":
				op.GraphID = newTestID(t)
			case "kind":
				op.Kind = OperationAdd
			case "context":
				op.ContextID = from.ID
			case "target":
				op.RelationID = newTestID(t)
			case "version":
				op.TargetVersion = 2
			}
			before := registrationStateFingerprint(t, database)
			if _, err := database.DiscardRelation(t.Context(), graph, stored.ID, &op); err == nil {
				t.Fatal("잘못된 폐기 기록을 허용했다")
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("거부된 폐기의 부분 적용")
			}
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeEventReconstructionIntegration(t *testing.T) {
	for _, mode := range []string{"merge", "split"} {
		t.Run(mode, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			derived := lifeDerived(t, database, graph, actor, source.ID)
			originalValue := testEventContext(t, graph, actor, source.ID)
			if mode == "split" {
				originalValue.Event.MemberIDs = []model.ID{source.ID, derived.ID}
			}
			original, err := database.CreateContext(t.Context(), graph, originalValue, nil)
			if err != nil {
				t.Fatal(err)
			}
			otherValue := testEventContext(t, graph, actor, derived.ID)
			var other model.Context
			if mode == "merge" {
				other, err = database.CreateContext(t.Context(), graph, otherValue, nil)
				if err != nil {
					t.Fatal(err)
				}
				request, operation := lifeRelationRequest(t, original, other, model.RelationTypeRelatesTo)
				if _, err := database.ConfirmRelation(t.Context(), graph, request, operation); err != nil {
					t.Fatal(err)
				}
			} else {
				op := registrationOperation(otherValue, OperationAdd)
				op.JudgmentInput = mode
				other, err = database.CreateContextWithOperation(t.Context(), graph, otherValue, nil, op, WriteLimits{})
				if err != nil {
					t.Fatal(err)
				}
			}
			next := attachReferences(original, nil, slices.Clone(original.Event.MemberIDs))
			next.Version++
			next.Body += " 재구성"
			if mode == "split" {
				next.Event.MemberIDs = []model.ID{source.ID}
			} else {
				next.Event.MemberIDs = []model.ID{source.ID, derived.ID}
			}
			op := registrationOperation(next, OperationUpdate)
			op.JudgmentInput = mode
			updated, err := database.UpdateContextWithOperation(t.Context(), graph, original.Version, next, op, WriteLimits{})
			if err != nil || !reflect.DeepEqual(updated.Event, next.Event) {
				t.Fatalf("재구성 갱신=%+v/%v", updated, err)
			}
			if mode == "merge" {
				discard := registrationOperation(other, OperationDiscard)
				discard.JudgmentInput = mode
				if _, err := database.DiscardContext(t.Context(), graph, other.ID, discard, WriteLimits{}); err != nil {
					t.Fatal(err)
				}
				relations, _, err := database.ListRelations(t.Context(), graph, "", 50)
				if err != nil || len(relations) != 1 || relations[0].State != model.RelationStateDiscarded {
					t.Fatalf("흡수 사건의 확정 관계=%+v/%v", relations, err)
				}
			}
			var count int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE graph_id=$1 AND judgment_input=$2 AND result='applied' AND actor_account_id=$3`, graph.String(), mode, actor.String()).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 2
			if mode == "merge" {
				want = 3
			} // 사건 갱신·흡수 사건 폐기·확정 관계 폐기.
			if count != want {
				t.Fatalf("같은 판단 입력 기록=%d, want %d", count, want)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestLifeEventRelationRollbackIntegration(t *testing.T) {
	for _, action := range []string{"update_event", "confirm", "discard_relation", "discard_event"} {
		for _, stage := range []string{"age", "graph", "audit", "commit"} {
			t.Run(action+"/"+stage, func(t *testing.T) {
				database := newSmallPoolStore(t)
				graph, from, to := relationTestEvents(t, database)
				request, operation := lifeRelationRequest(t, from, to, model.RelationTypePrecedes)
				var stored model.Relation
				var err error
				if action == "discard_relation" || action == "discard_event" {
					stored, err = database.ConfirmRelation(t.Context(), graph, request, operation)
					if err != nil {
						t.Fatal(err)
					}
				}
				run := func(ctx context.Context) error {
					switch action {
					case "confirm":
						_, err := database.ConfirmRelation(ctx, graph, request, operation)
						return err
					case "discard_relation":
						op := *operation
						op.Kind, op.RelationID = OperationDiscard, stored.ID
						_, err := database.DiscardRelation(ctx, graph, stored.ID, &op)
						return err
					case "discard_event":
						_, err := database.DiscardContext(ctx, graph, from.ID, registrationOperation(from, OperationDiscard), WriteLimits{})
						return err
					default:
						next := attachReferences(from, nil, from.Event.MemberIDs)
						next.Body += " 변경"
						next.Version++
						_, err := database.UpdateContextWithOperation(ctx, graph, from.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
						return err
					}
				}
				ctx := t.Context()
				var outer pgx.Tx
				remove := func() {}
				if stage == "age" {
					outer, err = database.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer rollbackTransaction(t.Context(), outer)
					fragment := "SET edge ="
					if action == "confirm" {
						fragment = "CREATE (from)-[edge:"
					}
					if action == "update_event" {
						fragment = "DELETE edge"
					}
					ctx = context.WithValue(ctx, writeTransactionContextKey{}, pgx.Tx(nodeCreationFailureTx{Tx: outer, fragment: fragment}))
				} else {
					table, event := "operation_log", "INSERT"
					if stage == "graph" {
						table, event = "context_graph", "UPDATE"
					}
					remove = lifeFailGraphWrite(t, database, graph, table, event, stage == "commit")
				}
				before := registrationStateFingerprint(t, database)
				if err := run(ctx); err == nil {
					t.Fatal("장애 주입이 적용되지 않았다")
				}
				if outer != nil {
					if err := outer.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if before != registrationStateFingerprint(t, database) {
					t.Fatal("실패 후 부분 적용")
				}
				remove()
				if err := run(t.Context()); err != nil {
					t.Fatal(err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeEventRelationTimeRaceIntegration(t *testing.T) {
	for _, kind := range []model.RelationType{model.RelationTypePrecedes, model.RelationTypeCauses, model.RelationTypePartOf} {
		for _, updateFirst := range []bool{false, true} {
			name := string(kind) + "/confirm_first"
			if updateFirst {
				name = string(kind) + "/update_first"
			}
			t.Run(name, func(t *testing.T) {
				database := newSmallPoolStore(t)
				graph, from, to := relationTestEvents(t, database)
				request, op := lifeRelationRequest(t, from, to, kind)
				next := attachReferences(from, nil, from.Event.MemberIDs)
				next.Version++
				if kind == model.RelationTypePartOf {
					next.Event.End = new(to.Event.End.Add(time.Microsecond))
				} else {
					next.Event.Start = to.Event.Start.Add(time.Microsecond)
				}
				update := func(ctx context.Context) error {
					_, err := database.UpdateContextWithOperation(ctx, graph, from.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
					return err
				}
				confirm := func(ctx context.Context) error {
					_, err := database.ConfirmRelation(ctx, graph, request, op)
					return err
				}
				firstAction, secondAction := confirm, update
				if updateFirst {
					firstAction, secondAction = update, confirm
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				outer, err := database.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var group sync.WaitGroup
				defer func() { cancel(); rollbackTransaction(t.Context(), outer); group.Wait() }()
				if err := firstAction(context.WithValue(ctx, writeTransactionContextKey{}, outer)); err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				group.Go(func() { result <- secondAction(ctx) })
				waitGrantWaiters(t, ctx, database, int(outer.Conn().PgConn().PID()), 1)
				if err := outer.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-result; !errors.Is(err, ErrInvalidRelation) {
					t.Fatalf("잠금 뒤 시간 제약=%v", err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeEventMembersAndIndexIntegration(t *testing.T) {
	for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
		t.Run(string(targets), func(t *testing.T) {
			database := newSmallPoolStore(t)
			database.indexTargets = targets
			graph, actor, source := boundaryGraph(t, database)
			derived := lifeDerived(t, database, graph, actor, source.ID)
			other, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://life04/"+graph.String()), nil)
			if err != nil {
				t.Fatal(err)
			}
			event, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.index_task SET state='failed',attempts=2,last_error='synthetic' WHERE context_id=$1`, event.ID.String()); err != nil {
				t.Fatal(err)
			}
			task := registrationTaskFor(t, database, event.ID)
			revision := lifeContentRevision(t, database, graph)
			next := attachReferences(event, nil, []model.ID{other.ID, derived.ID})
			next.Version++
			next.Event.Start = next.Event.Start.Add(-time.Second)
			next.Event.End = new(next.Event.End.Add(time.Second))
			stored, err := database.UpdateContextWithOperation(t.Context(), graph, event.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := database.Context(t.Context(), graph, event.ID)
			if err != nil || !reflect.DeepEqual(canonicalReferences(stored), canonicalReferences(loaded)) || !reflect.DeepEqual(canonicalReferences(stored), canonicalReferences(next)) {
				t.Fatalf("사건 구성원 교체=%+v/%v", loaded, err)
			}
			if !reflect.DeepEqual(task, registrationTaskFor(t, database, event.ID)) || lifeContentRevision(t, database, graph) != revision+1 {
				t.Fatal("본문 불변 갱신의 큐·내용 판")
			}
			next = attachReferences(stored, nil, stored.Event.MemberIDs)
			next.Version++
			next.Body += " 새 본문🙂"
			if _, err := database.UpdateContextWithOperation(t.Context(), graph, stored.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			updatedTask := registrationTaskFor(t, database, event.ID)
			if updatedTask.id != task.id || updatedTask.state != "pending" || updatedTask.attempts != 0 || updatedTask.lastError != nil || !updatedTask.enqueued.After(task.enqueued) {
				t.Fatal("사건 본문 변경의 색인 재등록")
			}
			foreignGraph := createTestGraph(t, database, actor)
			foreign, err := database.CreateContext(t.Context(), foreignGraph, testSourceContext(t, foreignGraph, actor, "api://life04/foreign"), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, mutation := range []string{"empty", "duplicate", "event_member", "foreign", "source_time", "version", "immutable"} {
				t.Run(mutation, func(t *testing.T) {
					value := attachReferences(next, nil, next.Event.MemberIDs)
					expected := next.Version
					value.Version++
					switch mutation {
					case "empty":
						value.Event.MemberIDs = nil
					case "duplicate":
						value.Event.MemberIDs = []model.ID{other.ID, other.ID}
					case "event_member":
						value.Event.MemberIDs = []model.ID{event.ID}
					case "foreign":
						value.Event.MemberIDs = []model.ID{foreign.ID}
					case "source_time":
						value.Event.Start = other.Source.OccurredAt.Add(time.Second)
					case "version":
						expected--
					case "immutable":
						value.CreatedBy = newTestID(t)
					}
					before := registrationStateFingerprint(t, database)
					_, err := database.UpdateContext(t.Context(), graph, expected, value)
					if err == nil {
						t.Fatal("잘못된 사건 갱신 허용")
					}
					if mutation == "version" {
						if conflict, ok := errors.AsType[VersionConflictError](err); !ok || conflict.Current != next.Version {
							t.Fatalf("현재 판 충돌=%v", err)
						}
					}
					if before != registrationStateFingerprint(t, database) {
						t.Fatal("거부된 갱신의 부분 적용")
					}
				})
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestLifeRelationCycleStatesIntegration(t *testing.T) {
	for _, kind := range []model.RelationType{model.RelationTypePrecedes, model.RelationTypeCauses, model.RelationTypePartOf, model.RelationTypeRelatesTo} {
		for _, state := range []model.RelationState{model.RelationStateProposed, model.RelationStateDiscarded} {
			t.Run(string(kind)+"/"+string(state), func(t *testing.T) {
				database := newSmallPoolStore(t)
				graph, first, second := relationTestEvents(t, database)
				// 동일 범위로 두어 세 방향 모두 시간 제약을 통과하고 순환 검사만 남긴다.
				next := attachReferences(second, nil, second.Event.MemberIDs)
				next.Version++
				next.Event.End = first.Event.End
				var err error
				second, err = database.UpdateContext(t.Context(), graph, second.Version, next)
				if err != nil {
					t.Fatal(err)
				}
				thirdValue := testEventContext(t, graph, first.CreatedBy, first.Event.MemberIDs[0])
				thirdValue.Event.Start, thirdValue.Event.End = first.Event.Start, first.Event.End
				third, err := database.CreateContext(t.Context(), graph, thirdValue, nil)
				if err != nil {
					t.Fatal(err)
				}
				cycle, operation := lifeRelationRequest(t, third, first, kind)
				candidate := proposedRelation(graph, kind, cycle.FromContextID, cycle.ToContextID)
				candidate.ID = newTestID(t)
				candidate = normalizeRelation(candidate)
				stored, err := database.CreateRelation(t.Context(), graph, candidate)
				if err != nil {
					t.Fatal(err)
				}
				if state == model.RelationStateDiscarded {
					if _, err := database.DiscardRelation(t.Context(), graph, stored.ID, nil); err != nil {
						t.Fatal(err)
					}
				}
				for _, pair := range [][2]model.Context{{first, second}, {second, third}} {
					request, op := lifeRelationRequest(t, pair[0], pair[1], kind)
					if _, err := database.ConfirmRelation(t.Context(), graph, request, op); err != nil {
						t.Fatal(err)
					}
				}
				before := registrationStateFingerprint(t, database)
				_, err = database.ConfirmRelation(t.Context(), graph, cycle, operation)
				if kind == model.RelationTypeRelatesTo {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if !errors.Is(err, ErrInvalidRelation) {
						t.Fatalf("세 간선 순환=%v", err)
					}
					if before != registrationStateFingerprint(t, database) {
						t.Fatal("순환 거부의 부분 적용")
					}
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeEventRelationCancelIntegration(t *testing.T) {
	for _, action := range []string{"update", "confirm", "discard"} {
		t.Run(action, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, from, to := relationTestEvents(t, database)
			request, operation := lifeRelationRequest(t, from, to, model.RelationTypePrecedes)
			if action == "discard" {
				if _, err := database.ConfirmRelation(t.Context(), graph, request, operation); err != nil {
					t.Fatal(err)
				}
			}
			ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
			defer stop()
			outer, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var group sync.WaitGroup
			defer func() { stop(); rollbackTransaction(t.Context(), outer); group.Wait() }()
			if err := requireActiveGraph(ctx, outer, graph); err != nil {
				t.Fatal(err)
			}
			before := registrationStateFingerprint(t, database)
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			result := make(chan error, 1)
			group.Go(func() {
				var err error
				switch action {
				case "confirm":
					_, err = database.ConfirmRelation(requestCtx, graph, request, operation)
				case "discard":
					op := *operation
					op.Kind = OperationDiscard
					_, err = database.DiscardRelation(requestCtx, graph, request.ID, &op)
				default:
					next := attachReferences(from, nil, from.Event.MemberIDs)
					next.Version++
					_, err = database.UpdateContext(requestCtx, graph, from.Version, next)
				}
				result <- err
			})
			waitGrantWaiters(t, ctx, database, int(outer.Conn().PgConn().PID()), 1)
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("대기 취소=%v", err)
			}
			if err := outer.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("취소의 부분 적용")
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
