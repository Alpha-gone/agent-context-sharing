package store

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"sync"
	"testing"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

type obs05Sink struct {
	mu     sync.Mutex
	events []diagnostic.Event
}

func (s *obs05Sink) Emit(_ context.Context, e diagnostic.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return false
}
func (s *obs05Sink) snapshot(t *testing.T) []diagnostic.Event {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]diagnostic.Event(nil), s.events...)
	for _, e := range result {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
func obs05Reporter(t *testing.T, kind diagnostic.Kind, s *obs05Sink) *diagnostic.Reporter {
	t.Helper()
	source, err := diagnostic.NewSource("obs05")
	if err != nil {
		t.Fatal(err)
	}
	r, err := diagnostic.NewReporter(kind, s, source)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func obs05Context(t *testing.T) context.Context {
	t.Helper()
	trace := diagnostic.NewRequestTrace().WithTestRun(diagnostic.NewTestRun())
	id, err := model.ParseID(trace.CorrelationID())
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := model.WithCorrelationID(diagnostic.WithTrace(t.Context(), trace), id)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestOBS05IndexGenerationAndCorrelationDatabase(t *testing.T) {
	database, graph, nodes := newIndexClaimFixture(t, 1)
	sink := &obs05Sink{}
	database.SetOperationDiagnostics(nil, obs05Reporter(t, diagnostic.Index, sink))
	oldCtx, newCtx := obs05Context(t), obs05Context(t)
	enqueue := func(ctx context.Context) {
		t.Helper()
		tx, err := database.writeTransaction(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := database.enqueueIndexTask(ctx, tx, graph, nodes[0].ID, nodes[0].Layer); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(oldCtx)
	old := registrationTaskFor(t, database, nodes[0].ID)
	result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(ctx context.Context, task IndexTask) IndexTaskResult {
		if task.CorrelationID != model.CorrelationID(oldCtx).String() || !task.Generation.Equal(old.enqueued) {
			t.Fatal("확보 세대·상관 ID 불일치")
		}
		if diagnostic.TraceFromContext(ctx).CorrelationID() != task.CorrelationID || diagnostic.TraceFromContext(ctx).JobID() == "" {
			t.Fatal("제공자 context 흐름 누락")
		}
		enqueue(newCtx)
		return IndexTaskResult{Embedding: make([]float64, 8), ModelID: "obs05"}
	})
	if err != nil || result.Succeeded {
		t.Fatalf("오래된 확보 결과 공개: %+v %v", result, err)
	}
	current := registrationTaskFor(t, database, nodes[0].ID)
	if current.correlation == nil || *current.correlation != model.CorrelationID(newCtx).String() {
		t.Fatal("오래된 확보가 새 상관 ID 변경")
	}
	result, err = database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
		return IndexTaskResult{Embedding: []float64{1, 0, 0, 0, 0, 0, 0, 0}, ModelID: "obs05"}
	})
	if err != nil || !result.Succeeded {
		t.Fatalf("새 세대 적용: %v", err)
	}
	stale, committed := false, false
	for _, e := range sink.snapshot(t) {
		if e.Stage == "apply" && e.ReasonCode == "stale_generation" {
			stale = true
			if e.CorrelationID != model.CorrelationID(oldCtx).String() || e.Fields.Index.Generation != uint64(old.enqueued.UnixMicro()) {
				t.Fatal("무효 작업의 세대 연결 변경")
			}
		}
		if e.Stage == "apply" && e.Result == "committed" {
			committed = true
			if e.CorrelationID != model.CorrelationID(newCtx).String() {
				t.Fatal("새 세대 연결 누락")
			}
		}
	}
	if !stale || !committed {
		t.Fatalf("결과 사건 누락: stale=%v committed=%v", stale, committed)
	}
}

func TestOBS05RelationStagesAndOuterOutcomeDatabase(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	graph, first, second := relationTestEvents(t, database)
	sink := &obs05Sink{}
	database.SetOperationDiagnostics(obs05Reporter(t, diagnostic.Management, sink), nil)
	ctx := obs05Context(t)
	relation := confirmable(t, graph, model.RelationTypePrecedes, first.ID, second.ID)
	_, err := database.ConfirmRelation(ctx, graph, relation, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := sink.snapshot(t)
	e := events[len(events)-1]
	if e.Fields.Management.CommitState != "committed" || e.Stage != "commit" {
		t.Fatalf("실제 커밋 미관측: %+v", e)
	}
	reverse := confirmable(t, graph, model.RelationTypePrecedes, second.ID, first.ID)
	_, err = database.ConfirmRelation(ctx, graph, reverse, nil)
	if err == nil {
		t.Fatal("순환 허용")
	}
	events = sink.snapshot(t)
	e = events[len(events)-1]
	if e.Stage != "cycle_check" || e.ReasonCode != "cycle_detected" || e.Fields.Management.CommitState != "rolled_back" {
		t.Fatalf("순환 실패 분류: %+v", e)
	}
	for _, rollback := range []bool{true, false} {
		before := len(sink.snapshot(t))
		relation = confirmable(t, graph, model.RelationTypeRelatesTo, first.ID, second.ID)
		_, err = database.ExecuteWrite(ctx, first.CreatedBy, newTestID(t).String(), "relation_confirm", []byte(`{"safe":true}`), func(context.Context) error { return nil }, func(writeCtx context.Context) ([]byte, error) {
			if _, err := database.ConfirmRelation(writeCtx, graph, relation, nil); err != nil {
				return nil, err
			}
			if len(sink.snapshot(t)) != before {
				t.Fatal("저장점 해제를 커밋으로 기록")
			}
			if rollback {
				return nil, errors.New("sensitive URI https://private.example/token")
			}
			return []byte(`{"relation":true}`), nil
		})
		if (err != nil) != rollback {
			t.Fatalf("바깥 처리 결과: %v", err)
		}
		events = sink.snapshot(t)
		e = events[len(events)-1]
		want := "committed"
		if rollback {
			want = "rolled_back"
		}
		if e.Fields.Management.CommitState != want {
			t.Fatalf("바깥 결과: %+v", e)
		}
	}
	raw, _ := json.Marshal(sink.snapshot(t))
	if strings.Contains(string(raw), "private.example") {
		t.Fatal("오류 원문 누출")
	}
}

type obs05UncertainTx struct{ pgx.Tx }

func (*obs05UncertainTx) Commit(context.Context) error {
	return errors.New("private connection failure")
}
func (*obs05UncertainTx) Rollback(context.Context) error { return pgx.ErrTxClosed }
func TestOBS05UncertainCommitIsNeverReportedAsSuccess(t *testing.T) {
	sink := &obs05Sink{}
	database := &Store{}
	database.SetOperationDiagnostics(obs05Reporter(t, diagnostic.Management, sink), nil)
	ctx, observation := beginManagement(obs05Context(t), "relation_confirm")
	tx := observeWriteTx(ctx, &obs05UncertainTx{}, false)
	err := tx.Commit(ctx)
	if err == nil {
		t.Fatal("주입 실패 미발생")
	}
	finishWrite(ctx, tx, "obs05", &err)
	database.finishManagement(ctx, observation, err)
	events := sink.snapshot(t)
	if len(events) != 1 || events[0].Fields.Management.CommitState != "unknown" || events[0].ReasonCode != "commit_unknown" || events[0].Result == "completed" {
		t.Fatalf("미확인 커밋 결과: %+v", events)
	}
}

func TestOBS05StorageFailureIsNotDomainRejection(t *testing.T) {
	sink := &obs05Sink{}
	database := &Store{}
	database.SetOperationDiagnostics(obs05Reporter(t, diagnostic.Management, sink), nil)
	ctx, observation := beginManagement(obs05Context(t), "relation_confirm")
	observation.stage = "cycle_check"
	observation.state = "rolled_back"
	database.finishManagement(ctx, observation, storageError("query", model.FieldError{Field: "body", Message: "private storage content"}))
	events := sink.snapshot(t)
	if len(events) != 1 || events[0].ReasonCode != "store_unavailable" || events[0].Fields.Management.ValidationField != "" {
		t.Fatalf("저장 장애가 도메인 거부로 바뀜: %+v", events)
	}
}
