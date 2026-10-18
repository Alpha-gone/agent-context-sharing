package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func lifeAuditCreate(t *testing.T, database *Store, value model.Context, refs []model.ID) model.Context {
	t.Helper()
	operation := registrationOperation(value, OperationAdd)
	operation.JudgmentInput = "근거: " + value.Body
	stored, err := database.CreateContextWithOperation(t.Context(), value.GraphID, value, refs, operation, WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupIndexTasks(t, database, stored.ID)
	return stored
}

func TestLifeAuditRecordValidation(t *testing.T) {
	id := newTestID(t)
	base := OperationRecord{Kind: OperationAdd, GraphID: id, AccountID: id, AgentID: id, JudgmentInput: "reason"}
	for _, sample := range []struct {
		name string
		edit func(*OperationRecord)
	}{
		{"bad_context", func(o *OperationRecord) { o.ContextID = model.ID{1} }},
		{"bad_relation", func(o *OperationRecord) { o.RelationID = model.ID{1} }},
		{"both_targets", func(o *OperationRecord) { o.ContextID, o.RelationID, o.TargetVersion = id, id, 1 }},
		{"relation_version", func(o *OperationRecord) { o.RelationID, o.TargetVersion = id, 1 }},
		{"nul_input", func(o *OperationRecord) { o.JudgmentInput = "input\x00" }},
	} {
		t.Run(sample.name, func(t *testing.T) {
			operation := base
			sample.edit(&operation)
			if _, ok := errors.AsType[model.FieldError](operation.valid("rejected")); !ok {
				t.Fatal("잘못된 감사 입력이 논리 검증을 통과했다")
			}
		})
	}
}

func TestLifeAuditHistoryAndRollbackIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	value := testDerivedContext(t, graph, actor)
	value.CreatedByAgent = newTestID(t)
	created := lifeAuditCreate(t, database, value, []model.ID{source.ID})
	updated := created
	updated.Body += " updated"
	updated.Version++
	updated, err := database.UpdateContextWithOperation(t.Context(), graph, created.Version, updated, registrationOperation(created, OperationUpdate), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	// 낙관적 충돌 뒤 별도 경계에 거부를 기록하고 변경은 남기지 않는다.
	stale := created
	stale.Version++
	if _, err := database.UpdateContextWithOperation(t.Context(), graph, created.Version, stale, registrationOperation(created, OperationUpdate), WriteLimits{}); err != nil {
		if conflict, ok := errors.AsType[VersionConflictError](err); !ok || conflict.Current != updated.Version {
			t.Fatal(err)
		}
	} else {
		t.Fatal("오래된 판 갱신이 적용됐다")
	}
	rejected := *registrationOperation(created, OperationUpdate)
	rejected.TargetVersion = created.Version
	if err := database.RecordRejectedOperation(t.Context(), rejected, "version_conflict"); err != nil {
		t.Fatal(err)
	}
	// 바깥 멱등성 트랜잭션의 저장점만 되돌린 뒤 거부와 완료 결과를 함께 확정한다.
	request := IdempotencyRequest{AccountID: actor, Key: newTestID(t), ToolName: "node_update"}
	runs := 0
	for range 2 {
		_, err := database.ReplayIdempotent(t.Context(), request, func(ctx context.Context) ([]byte, error) {
			runs++
			if _, err := database.UpdateContextWithOperation(ctx, graph, created.Version, stale, registrationOperation(created, OperationUpdate), WriteLimits{}); err != nil {
				if _, ok := errors.AsType[VersionConflictError](err); !ok {
					return nil, err
				}
			} else {
				return nil, errors.New("판 충돌 누락")
			}
			if err := database.RecordRejectedOperation(ctx, rejected, "idempotent_conflict"); err != nil {
				return nil, err
			}
			return []byte(`{"error":"version_conflict"}`), nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if runs != 1 {
		t.Fatal("거부가 재생 시 중복 기록됐다")
	}
	request = IdempotencyRequest{AccountID: actor, Key: newTestID(t), ToolName: "node_update"}
	sentinel := errors.New("outer rollback")
	if _, err := database.ReplayIdempotent(t.Context(), request, func(ctx context.Context) ([]byte, error) {
		if _, err := database.KeepContext(ctx, graph, updated.ID, updated.Version, registrationOperation(updated, OperationKeep)); err != nil {
			return nil, err
		}
		return nil, sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	deleted, err := database.DiscardContext(t.Context(), graph, updated.ID, registrationOperation(updated, OperationDiscard), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	history, err := database.OperationHistory(t.Context(), graph, actor, OperationTarget{ContextID: created.ID})
	if err != nil || len(history) != 5 {
		t.Fatalf("이력=%+v/%v", history, err)
	}
	wantKinds := []OperationKind{OperationAdd, OperationUpdate, OperationUpdate, OperationUpdate, OperationDiscard}
	wantResults := []model.OperationResult{model.OperationApplied, model.OperationApplied, model.OperationRejected, model.OperationRejected, model.OperationApplied}
	wantVersions := []int64{1, 2, 1, 1, deleted.Version}
	for i, entry := range history {
		if !entry.ID.IsV7() || entry.GraphID != graph || entry.ContextID != created.ID || !entry.RelationID.IsZero() ||
			entry.AccountID != actor || entry.AgentID != value.CreatedByAgent || entry.Kind != wantKinds[i] || entry.Result != wantResults[i] ||
			entry.TargetVersion != wantVersions[i] || entry.JudgmentInput == "" || entry.AppliedAt.Location() != time.UTC {
			t.Fatalf("이력 주체·판·결과=%+v", entry)
		}
	}
	if history[2].RejectionReason != "version_conflict" || history[3].RejectionReason != "idempotent_conflict" {
		t.Fatal(history)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeAuditRelationHistoryIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, first, second := relationTestEvents(t, database)
	grantAccount(t, database, graph, first.CreatedBy, model.GraphGradeOwner)
	relation := confirmable(t, graph, model.RelationTypePrecedes, first.ID, second.ID)
	operation := &OperationRecord{Kind: OperationAdd, GraphID: graph, RelationID: relation.ID, JudgmentInput: "relation judgment", AccountID: relation.ConfirmedBy, AgentID: relation.ConfirmedByAgent}
	stored, err := database.ConfirmRelation(t.Context(), graph, relation, operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.Kind, operation.RelationID = OperationDiscard, stored.ID
	if _, err := database.DiscardRelation(t.Context(), graph, stored.ID, operation); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordRejectedOperation(t.Context(), *operation, "invalid_state"); err != nil {
		t.Fatal(err)
	}
	history, err := database.OperationHistory(t.Context(), graph, first.CreatedBy, OperationTarget{RelationID: stored.ID})
	if err != nil || len(history) != 3 {
		t.Fatalf("관계 이력=%+v/%v", history, err)
	}
	for _, entry := range history {
		if !entry.ContextID.IsZero() || entry.TargetVersion != 0 || entry.RelationID != stored.ID || entry.AccountID != operation.AccountID || entry.AgentID != operation.AgentID {
			t.Fatal(entry)
		}
	}
}

func TestLifeRejectedAuditFailureIntegration(t *testing.T) {
	for _, outer := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "idempotent"}[outer], func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			op := registrationOperation(source, OperationKeep)
			op.TargetVersion = source.Version
			before := registrationStateFingerprint(t, database)
			remove := lifeFailGraphWrite(t, database, graph, "operation_log", "INSERT", false)
			run := func() error {
				if !outer {
					return database.RecordRejectedOperation(t.Context(), *op, "reason")
				}
				_, err := database.ReplayIdempotent(t.Context(), IdempotencyRequest{AccountID: actor, Key: newTestID(t), ToolName: "node_update"}, func(ctx context.Context) ([]byte, error) {
					if err := database.RecordRejectedOperation(ctx, *op, "reason"); err != nil {
						return nil, err
					}
					return []byte(`{"error":"rejected"}`), nil
				})
				return err
			}
			if _, ok := errors.AsType[*StorageError](run()); !ok {
				t.Fatal("감사 저장 장애가 도메인 거부로 전달됐다")
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("거부 감사 실패의 부분 변경")
			}
			remove()
			if err := run(); err != nil {
				t.Fatal(err)
			}
			history, err := database.OperationHistory(t.Context(), graph, actor, OperationTarget{ContextID: source.ID})
			if err != nil || len(history) != 1 || history[0].Result != model.OperationRejected {
				t.Fatal(history, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestLifeOriginKindsIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	var sources []model.Context
	for _, kind := range []model.OriginKind{model.OriginKindUserUtterance, model.OriginKindAgentOutput, model.OriginKindExternalContent} {
		source := testSourceContext(t, graph, actor, "api://origin/"+string(kind)+graph.String())
		source.Source.OriginKind = kind
		sources = append(sources, lifeAuditCreate(t, database, source, nil))
	}
	first := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{sources[0].ID, sources[1].ID})
	second := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{first.ID, sources[2].ID})
	event := testEventContext(t, graph, actor, second.ID)
	event.Event.MemberIDs = []model.ID{second.ID, sources[0].ID}
	stored := lifeAuditCreate(t, database, event, nil)
	if _, err := database.DiscardContext(t.Context(), graph, sources[2].ID, registrationOperation(sources[2], OperationDiscard), WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	before := registrationStateFingerprint(t, database)
	origins, err := database.ContextOriginKinds(t.Context(), graph, []model.ID{stored.ID, first.ID, stored.ID, sources[2].ID})
	want := []model.OriginKind{model.OriginKindAgentOutput, model.OriginKindExternalContent, model.OriginKindUserUtterance}
	if err != nil || !slices.Equal(origins[stored.ID], want) || !slices.Equal(origins[first.ID], []model.OriginKind{model.OriginKindAgentOutput, model.OriginKindUserUtterance}) ||
		!slices.Equal(origins[sources[2].ID], []model.OriginKind{model.OriginKindExternalContent}) {
		t.Fatalf("다중 출처=%+v/%v", origins, err)
	}
	if before != registrationStateFingerprint(t, database) {
		t.Fatal("출처 조회가 property·순위·색인·감사를 변경했다")
	}
	other := createTestGraph(t, database, actor)
	if got, err := database.ContextOriginKinds(t.Context(), other, []model.ID{stored.ID}); err == nil || got != nil {
		t.Fatal("다른 그래프의 출처가 반환됐다")
	}
	if got, err := database.ContextOriginKinds(t.Context(), graph, []model.ID{newTestID(t)}); err == nil || got != nil {
		t.Fatal("누락된 출처를 정상 빈 결과로 반환했다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeContextImpactIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	otherSource := lifeAuditCreate(t, database, testSourceContext(t, graph, actor, "api://sibling/"+graph.String()), nil)
	chain := []model.Context{source}
	for range 12 {
		node := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{chain[len(chain)-1].ID, otherSource.ID})
		chain = append(chain, node)
	}
	first := lifeAuditCreate(t, database, testEventContext(t, graph, actor, chain[len(chain)-1].ID), nil)
	second := lifeAuditCreate(t, database, testEventContext(t, graph, actor, otherSource.ID), nil)
	third := lifeAuditCreate(t, database, testEventContext(t, graph, actor, otherSource.ID), nil)
	excluded := lifeAuditCreate(t, database, testEventContext(t, graph, actor, otherSource.ID), nil)
	for _, pair := range [][2]model.ID{{first.ID, second.ID}, {second.ID, third.ID}, {third.ID, first.ID}} {
		if _, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypeRelatesTo, pair[0], pair[1]), nil); err != nil {
			t.Fatal(err)
		}
	}
	discarded, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypeRelatesTo, first.ID, excluded.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DiscardRelation(t.Context(), graph, discarded.ID, nil); err != nil {
		t.Fatal(err)
	}
	// 폐기된 시작점의 근거 무효 표시는 이미 저장되어 있으며 조회가 더 전파하지 않는다.
	if _, err := database.DiscardContext(t.Context(), graph, source.ID, registrationOperation(source, OperationDiscard), WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	before := registrationStateFingerprint(t, database)
	impact, err := database.ContextImpact(t.Context(), graph, source.ID, actor)
	if err != nil || len(impact.Contexts) != 16 || len(impact.Edges) != 16 || impact.Truncated || impact.Distances[chain[12].ID] != 12 {
		t.Fatalf("영향=%d정점/%d간선/%+v, err=%v", len(impact.Contexts), len(impact.Edges), impact.Distances, err)
	}
	if _, found := impact.Distances[otherSource.ID]; found {
		t.Fatal("역방향 영향이 다른 근거까지 퍼졌다")
	}
	if _, found := impact.Distances[excluded.ID]; found {
		t.Fatal("폐기 관계가 영향에 포함됐다")
	}
	if before != registrationStateFingerprint(t, database) {
		t.Fatal("영향 조회가 상태·색인·기록을 변경했다")
	}
	if _, err := database.ContextImpact(t.Context(), graph, newTestID(t), actor); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeAuditOwnerBoundaryIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, owner, source := boundaryGraph(t, database)
	for _, grade := range []model.GraphGrade{model.GraphGradeViewer, model.GraphGradeEditor, model.GraphGradeOwner} {
		t.Run(string(grade), func(t *testing.T) {
			actor := newTestID(t)
			createTestAccount(t, database, actor)
			grantAccount(t, database, graph, actor, grade)
			_, historyErr := database.OperationHistory(t.Context(), graph, actor, OperationTarget{ContextID: source.ID})
			_, impactErr := database.ContextImpact(t.Context(), graph, source.ID, actor)
			if grade == model.GraphGradeOwner {
				if historyErr != nil || impactErr != nil {
					t.Fatal(historyErr, impactErr)
				}
			} else if !errors.Is(historyErr, ErrNotFound) || !errors.Is(impactErr, ErrNotFound) {
				t.Fatal(historyErr, impactErr)
			}
		})
	}
	foreign, foreignOwner, _ := boundaryGraph(t, database)
	if _, err := database.ContextImpact(t.Context(), graph, source.ID, foreignOwner); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := database.ContextImpact(t.Context(), foreign, source.ID, foreignOwner); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := database.SetGraphDeleted(t.Context(), graph, owner, true); err != nil {
		t.Fatal(err)
	}
	if _, err := database.OperationHistory(t.Context(), graph, owner, OperationTarget{ContextID: source.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ContextImpact(t.Context(), graph, source.ID, owner); err != nil {
		t.Fatal(err)
	}
}

func TestLifeAuditTeamOwnerIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, owner, source := boundaryGraph(t, database)
	member := newTestID(t)
	createTestAccount(t, database, member)
	team, err := database.CreateTeam(t.Context(), owner, "audit owners")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddTeamMember(t.Context(), team.ID, owner, member); err != nil {
		t.Fatal(err)
	}
	grantTeam(t, database, graph, team.ID, model.GraphGradeOwner)
	for i, deleted := range []bool{false, true, false} {
		if i > 0 {
			if err := database.SetTeamDeleted(t.Context(), team.ID, owner, deleted); err != nil {
				t.Fatal(err)
			}
		}
		_, err := database.OperationHistory(t.Context(), graph, member, OperationTarget{ContextID: source.ID})
		if deleted && !errors.Is(err, ErrNotFound) || !deleted && err != nil {
			t.Fatal(err)
		}
		_, err = database.ContextImpact(t.Context(), graph, source.ID, member)
		if deleted && !errors.Is(err, ErrNotFound) || !deleted && err != nil {
			t.Fatal(err)
		}
	}
}

func TestLifeAuditStoredRecordValidationIntegration(t *testing.T) {
	for _, field := range []string{"agent", "judgment", "reason"} {
		t.Run(field, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, owner, source := boundaryGraph(t, database)
			op := registrationOperation(source, OperationKeep)
			op.TargetVersion = 1
			if err := database.RecordRejectedOperation(t.Context(), *op, "reason"); err != nil {
				t.Fatal(err)
			}
			assign := map[string]string{"agent": "actor_agent_id='00000000-0000-4000-8000-000000000000'", "judgment": "judgment_input=''", "reason": "reject_reason=NULL"}[field]
			claimExec(t, database, "UPDATE public.operation_log SET "+assign+" WHERE graph_id=$1", graph.String())
			got, err := database.OperationHistory(t.Context(), graph, owner, OperationTarget{ContextID: source.ID})
			if _, ok := errors.AsType[*StorageError](err); !ok || got != nil {
				t.Fatalf("손상 감사=%+v/%v", got, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestLifeAuditInvalidBoundaries(t *testing.T) {
	id := newTestID(t)
	database := &Store{}
	for _, target := range []OperationTarget{{}, {ContextID: id, RelationID: id}, {ContextID: model.ID{1}}, {RelationID: model.ID{1}}} {
		if got, err := database.OperationHistory(t.Context(), id, id, target); err == nil || got != nil {
			t.Fatal("잘못된 감사 대상이 DB 경계에 도달했다")
		}
	}
	if got, err := database.ContextImpact(t.Context(), id, id, model.ID{}); err == nil || len(got.Contexts) != 0 {
		t.Fatal("실행 계정 없는 영향 조회")
	}
	if got, err := database.ContextOriginKinds(t.Context(), model.ID{}, nil); err == nil || got != nil {
		t.Fatal("그래프 없는 출처 조회")
	}
	if got, err := database.ContextOriginKinds(t.Context(), id, []model.ID{{1}}); err == nil || got != nil {
		t.Fatal("잘못된 출처 대상")
	}
	op := OperationRecord{Kind: OperationAdd, GraphID: id, AccountID: id, AgentID: id, JudgmentInput: "rejected create args"}
	for _, reason := range []string{"", "invalid\x00reason"} {
		if err := database.RecordRejectedOperation(t.Context(), op, reason); err == nil {
			t.Fatal("잘못된 거부 사유가 DB 경계에 도달했다")
		}
	}
}

func TestLifeContextImpactFiltersIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	first := lifeAuditCreate(t, database, testEventContext(t, graph, actor, source.ID), nil)
	sibling := lifeAuditCreate(t, database, testSourceContext(t, graph, actor, "api://isolated/"+graph.String()), nil)
	second := lifeAuditCreate(t, database, testEventContext(t, graph, actor, sibling.ID), nil)
	proposal := proposedRelation(graph, model.RelationTypeRelatesTo, first.ID, second.ID)
	proposal.ID = newTestID(t)
	if _, err := database.CreateRelation(t.Context(), graph, proposal); err != nil {
		t.Fatal(err)
	}
	derived := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	child := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{derived.ID})
	// 대체 연결 자체는 영향 참조가 아니다. 새 파생의 근거는 별도 원천이다.
	replacement, err := database.CreateSupersedingContextWithOperation(t.Context(), graph, testDerivedContext(t, graph, actor), []model.ID{sibling.ID}, derived.ID, nil, WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetContextDeleted(t.Context(), graph, derived.ID, actor, true, 0); err != nil {
		t.Fatal(err)
	}
	before := registrationStateFingerprint(t, database)
	impact, err := database.ContextImpact(t.Context(), graph, source.ID, actor)
	if err != nil || len(impact.Contexts) != 2 || len(impact.Edges) != 1 {
		t.Fatal(impact, err)
	}
	for _, id := range []model.ID{second.ID, derived.ID, child.ID, replacement.ID, sibling.ID} {
		if _, found := impact.Distances[id]; found {
			t.Fatal("미확정·삭제·대체 또는 역방향 밖의 참조를 탐색했다")
		}
	}
	if before != registrationStateFingerprint(t, database) {
		t.Fatal("필터 조회에서 부분 변경")
	}
}

// 읽기 취소는 실제 PostgreSQL 잠금 대기에서 재현한다.
func TestLifeAuditCancelIntegration(t *testing.T) {
	for _, mode := range []string{"history", "impact", "origins"} {
		t.Run(mode, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			blocker, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(t.Context())
			table := "public.operation_log"
			if mode != "history" {
				table = database.contextTable()
			}
			if _, err := blocker.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := blocker.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := errors.New("audit canceled")
			done := make(chan error, 1)
			go func() {
				var err error
				switch mode {
				case "history":
					_, err = database.OperationHistory(ctx, graph, actor, OperationTarget{ContextID: source.ID})
				case "impact":
					_, err = database.ContextImpact(ctx, graph, source.ID, actor)
				default:
					_, err = database.ContextOriginKinds(ctx, graph, []model.ID{source.ID})
				}
				done <- err
			}()
			waitGrantWaiters(t, t.Context(), database, pid, 1)
			cancel(cause)
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("감사 취소가 반환되지 않았다")
			}
			if err := blocker.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

type lifeOriginSnapshotTracer struct {
	once   sync.Once
	hit    chan struct{}
	resume chan struct{}
}

func (tracer *lifeOriginSnapshotTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "DERIVED_FROM") {
		tracer.once.Do(func() {
			close(tracer.hit)
			select {
			case <-tracer.resume:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}
func (*lifeOriginSnapshotTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestLifeOriginStandaloneSnapshotIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, source := boundaryGraph(t, database)
	first := lifeAuditCreate(t, database, testDerivedContext(t, graph, actor), []model.ID{source.ID})
	secondSource := testSourceContext(t, graph, actor, "api://new-origin/"+graph.String())
	secondSource.Source.OriginKind = model.OriginKindAgentOutput
	second := lifeAuditCreate(t, database, secondSource, nil)
	tracer := &lifeOriginSnapshotTracer{hit: make(chan struct{}), resume: make(chan struct{})}
	config := database.pool.Config()
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reader := *database
	reader.pool = pool
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		origins, err := reader.ContextOriginKinds(ctx, graph, []model.ID{first.ID})
		if err == nil && !reflect.DeepEqual(origins[first.ID], []model.OriginKind{source.Source.OriginKind}) {
			err = errors.New("출처가 서로 다른 커밋의 정점과 근거를 섞었다")
		}
		done <- err
	}()
	select {
	case <-tracer.hit:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// 첫 정점 읽기 뒤 참조를 바꾼다. 기존 단독 출처 조회는 다음 질의에서 이 변경을 읽었다.
	write, err := database.pool.Begin(t.Context())
	if err != nil {
		close(tracer.resume)
		t.Fatal(err)
	}
	defer write.Rollback(t.Context())
	query := "MATCH (a:Context)-[e:DERIVED_FROM]->(b:Context) WHERE a.context_id = " + cypherString(first.ID.String()) + " DELETE e"
	if _, err := write.Exec(t.Context(), database.cypherSQL(query, "value agtype"), pgx.QueryExecModeExec); err != nil {
		close(tracer.resume)
		t.Fatal(err)
	}
	if err := database.createReferenceEdges(t.Context(), write, graph, first, []model.ID{second.ID}); err != nil {
		close(tracer.resume)
		t.Fatal(err)
	}
	if err := write.Commit(t.Context()); err != nil {
		close(tracer.resume)
		t.Fatal(err)
	}
	close(tracer.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	origins, err := database.ContextOriginKinds(t.Context(), graph, []model.ID{first.ID})
	if err != nil || !slices.Equal(origins[first.ID], []model.OriginKind{model.OriginKindAgentOutput}) {
		t.Fatal(origins, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}
