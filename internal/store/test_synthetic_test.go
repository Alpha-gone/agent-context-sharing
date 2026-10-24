package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type foundationClock struct {
	mu sync.Mutex
	at time.Time
}

func (clock *foundationClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.at
}

func (clock *foundationClock) Advance(delta time.Duration) time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.at = clock.at.Add(delta)
	return clock.at
}

type foundationScene struct {
	clock                  *foundationClock
	graph                  model.Graph
	actor, team, agent     model.ID
	source, derived, event model.Context
}

func newFoundationScene(t testing.TB) foundationScene {
	t.Helper()
	clock := &foundationClock{at: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}
	actor, team, agent, graph := newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	source := testSourceContext(t, graph, actor, "api://synthetic/"+newTestID(t).String())
	source.RecordedAt, source.Source.OccurredAt = clock.Now(), clock.Now()
	derived := testDerivedContext(t, graph, actor)
	derived.RecordedAt = clock.Advance(time.Second)
	derived.Derived.DerivedFrom = []model.ID{source.ID}
	event := testEventContext(t, graph, actor, source.ID)
	event.RecordedAt = clock.Advance(time.Second)
	event.Event.Start, event.Event.End = source.Source.OccurredAt, new(derived.RecordedAt)
	event.Event.MemberIDs = []model.ID{source.ID, derived.ID}
	for _, value := range []*model.Context{&source, &derived, &event} {
		value.CreatedByAgent = agent
	}
	return foundationScene{
		clock: clock, actor: actor, team: team, agent: agent,
		graph: model.Graph{ID: graph, Name: "synthetic foundation", CreatedBy: actor,
			CreatedAt: source.RecordedAt, LastActivityAt: event.RecordedAt, Version: 1},
		source: source, derived: derived, event: event,
	}
}

func TestFoundationSyntheticSamples(t *testing.T) {
	for _, sample := range []struct {
		name  string
		check func(foundationScene) error
		field string
	}{
		{"source", func(s foundationScene) error { return s.source.Validate() }, ""},
		{"derived references", func(s foundationScene) error {
			return model.ValidateDerivedSources(s.derived, []model.Context{s.source})
		}, ""},
		{"event interval", func(s foundationScene) error {
			return model.ValidateEventMembers(s.event, []model.Context{s.source, s.derived})
		}, ""},
		{"event boundary", func(s foundationScene) error {
			s.event.Event.End = new(s.event.Event.Start)
			s.event.Event.MemberIDs = []model.ID{s.source.ID}
			return model.ValidateEventMembers(s.event, []model.Context{s.source})
		}, ""},
		{"time outside event", func(s foundationScene) error {
			s.source.Source.OccurredAt = s.event.Event.Start.Add(-time.Nanosecond)
			return model.ValidateEventMembers(s.event, []model.Context{s.source, s.derived})
		}, "member_refs"},
		{"foreign reference", func(s foundationScene) error {
			s.source.GraphID = newTestID(t)
			return model.ValidateDerivedSources(s.derived, []model.Context{s.source})
		}, "derived_from"},
		{"missing references", func(s foundationScene) error {
			s.derived.Derived.DerivedFrom = nil
			return s.derived.Validate()
		}, "derived_from"},
		{"non UTC", func(s foundationScene) error {
			s.source.RecordedAt = s.clock.Now().In(time.FixedZone("KST", 9*3600))
			return s.source.Validate()
		}, "recorded_at"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			err := sample.check(newFoundationScene(t))
			if sample.field == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != sample.field {
				t.Fatalf("합성 표본 오류=%v, want %s", err, sample.field)
			}
		})
	}
	first, second := newFoundationScene(t), newFoundationScene(t)
	seen := make(map[model.ID]bool)
	for _, scene := range []foundationScene{first, second} {
		for _, id := range []model.ID{scene.graph.ID, scene.actor, scene.team, scene.agent, scene.source.ID, scene.derived.ID, scene.event.ID} {
			if !id.IsV7() || seen[id] {
				t.Fatal("합성 표본 식별자가 겹쳤다")
			}
			seen[id] = true
		}
	}
	first.event.Event.MemberIDs[0] = first.agent
	if second.event.Event.MemberIDs[0] != second.source.ID {
		t.Fatal("표본 사이에 가변 참조가 공유됐다")
	}
}

func TestFoundationGradeAndPlanSamples(t *testing.T) {
	for _, grade := range []model.GraphGrade{model.GraphGradeViewer, model.GraphGradeEditor, model.GraphGradeOwner} {
		if !grade.Valid() {
			t.Fatalf("정상 등급 %q이 거부됐다", grade)
		}
	}
	if model.GraphGrade("unknown").Valid() || model.GraphGrade("").Valid() {
		t.Fatal("위반 등급이 허용됐다")
	}
	for _, sample := range []struct {
		name       string
		current    int64
		delta      int64
		limit      int64
		wantExceed bool
	}{
		{"normal", 4, 2, 10, false}, {"boundary", 9, 1, 10, false},
		{"exceed", 10, 1, 10, true}, {"unlimited", 10, 1, 0, false},
		{"shrink after downgrade", 12, -1, 10, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			err := plan.CheckIncrease("stored_characters_per_graph", sample.current, sample.delta, sample.limit)
			if (err != nil) != sample.wantExceed {
				t.Fatalf("합성 누적 표본: %v", err)
			}
		})
	}
	scene := newFoundationScene(t)
	for _, sample := range []struct{ override, field string }{{`{"max_hops":0}`, ""}, {`{"max_hops":1}`, ""}, {`{"max_hops":-1}`, "max_hops"}} {
		_, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":%s}`, scene.actor, sample.override))
		if sample.field == "" && err != nil {
			t.Fatal(err)
		} else if sample.field != "" {
			if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != sample.field {
				t.Fatalf("합성 플랜 표본: %v", err)
			}
		}
	}
}

// 두 연결이 쓰기를 마친 뒤 커밋 전에 멈춘다. 실패·취소 때도 대기자를 풀어 준다.
type foundationCommitGate struct {
	arrived chan struct{}
	release chan struct{}
	open    func()
}

func newFoundationCommitGate(t testing.TB, count int) *foundationCommitGate {
	t.Helper()
	gate := &foundationCommitGate{arrived: make(chan struct{}, count), release: make(chan struct{})}
	gate.open = sync.OnceFunc(func() { close(gate.release) })
	t.Cleanup(gate.open)
	return gate
}

func (gate *foundationCommitGate) Wait(ctx context.Context) error {
	select {
	case gate.arrived <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-gate.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestFoundationCommitGateCancellation(t *testing.T) {
	gate := newFoundationCommitGate(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := gate.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("동기화 대기 취소=%v", err)
	}
}

func TestFoundationSceneRoundTripIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	scene := newFoundationScene(t)
	createTestAccount(t, database, scene.actor)
	createTestAccount(t, database, scene.agent)
	if _, err := database.CreateGraphWithOwner(t.Context(), scene.graph, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	createTestTeam(t, database, scene.team, scene.actor, false)
	addTestTeamMember(t, database, scene.team, scene.agent)
	grantTeam(t, database, scene.graph.ID, scene.team, model.GraphGradeViewer)
	for _, value := range []model.Context{scene.source, scene.derived, scene.event} {
		var references []model.ID
		if value.Derived != nil {
			references = value.Derived.DerivedFrom
		}
		if _, err := database.CreateContext(t.Context(), scene.graph.ID, value, references); err != nil {
			t.Fatal(err)
		}
		stored, err := database.Context(t.Context(), scene.graph.ID, value.ID)
		if err != nil || stored.Body != value.Body || stored.Layer != value.Layer || !stored.RecordedAt.Equal(value.RecordedAt) {
			t.Fatalf("합성 계층 왕복=%s: %v", value.Layer, err)
		}
	}
	assertGrade(t, database, scene.graph.ID, scene.actor, model.GraphGradeOwner)
	assertGrade(t, database, scene.graph.ID, scene.agent, model.GraphGradeViewer)
	if violations, err := database.AuditGraph(t.Context(), scene.graph.ID, EmbeddingExpectation{}); err != nil || len(violations) != 0 {
		t.Fatalf("합성 그래프 감사=%v: %v", violations, err)
	}
}

func foundationAccountFault(t *testing.T, database *Store, actor model.ID) func() {
	t.Helper()
	name := "foundation_" + strings.ReplaceAll(newTestID(t).String(), "-", "")
	function := pgx.Identifier{"public", name}.Sanitize()
	trigger := pgx.Identifier{name}.Sanitize()
	remove := sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if _, err := database.pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+trigger+" ON public.account; DROP FUNCTION IF EXISTS "+function+"()"); err != nil {
			t.Error("합성 장애 정리 실패")
		}
	})
	t.Cleanup(remove)
	if _, err := database.pool.Exec(t.Context(), "CREATE FUNCTION "+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.account_id='`+actor.String()+`'::uuid THEN RAISE EXCEPTION 'synthetic foundation fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER `+trigger+` BEFORE UPDATE ON public.account FOR EACH ROW EXECUTE FUNCTION `+function+`() `); err != nil {
		t.Fatal("합성 장애 준비 실패")
	}
	return remove
}

func TestFoundationCommitAndFaultHelpersIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	scene := newFoundationScene(t)
	createTestAccount(t, database, scene.actor)
	createTestAccount(t, database, scene.agent)
	gate := newFoundationCommitGate(t, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); gate.open(); workers.Wait() })
	update := func(actor model.ID) (err error) {
		tx, err := database.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, rollbackTransaction(ctx, tx)) }()
		if _, err := tx.Exec(ctx, "UPDATE public.account SET password_hash='synthetic-next' WHERE account_id=$1", actor.String()); err != nil {
			return err
		}
		if err := gate.Wait(ctx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	for _, actor := range []model.ID{scene.actor, scene.agent} {
		workers.Go(func() {
			results <- update(actor)
		})
	}
	for range 2 {
		select {
		case <-gate.arrived:
		case <-ctx.Done():
			t.Fatal("두 쓰기가 커밋 동기화 지점에 도달하지 않았다")
		}
	}
	countUpdated := func() int {
		var count int
		if err := database.pool.QueryRow(ctx, "SELECT count(*) FROM public.account WHERE password_hash='synthetic-next'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got := countUpdated(); got != 0 {
		t.Fatalf("커밋 전 변경 노출=%d", got)
	}
	gate.open()
	workers.Wait()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := countUpdated(); got != 2 {
		t.Fatalf("커밋 뒤 변경=%d, want 2", got)
	}
	remove := foundationAccountFault(t, database, scene.agent)
	tx, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			t.Error(err)
		}
	}()
	if _, err := tx.Exec(ctx, "UPDATE public.account SET password_hash='synthetic-rollback' WHERE account_id=$1", scene.actor.String()); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, "UPDATE public.account SET password_hash='synthetic-rollback' WHERE account_id=$1", scene.agent.String())
	if failure, ok := errors.AsType[*pgconn.PgError](err); !ok || failure.Code != "P0001" {
		t.Fatalf("주입한 DB 장애가 아니다: %v", err)
	}
	if err := rollbackTransaction(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if countUpdated() != 2 {
		t.Fatal("실패한 트랜잭션의 앞선 변경이 남았다")
	}
	remove()
	if _, err := database.pool.Exec(ctx, "UPDATE public.account SET password_hash='synthetic-recovered' WHERE account_id=$1", scene.agent.String()); err != nil {
		t.Fatal("장애 제거 뒤 다시 쓰지 못했다")
	}
}
