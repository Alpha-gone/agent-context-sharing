package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newAgentContractDatabase(t *testing.T) (*Store, *Store, *pgxpool.Pool) {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_URL이 필요합니다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 등록 DB 시험을 건너뜁니다")
	}
	isolated, err := url.Parse(address)
	if err != nil || (isolated.Scheme != "postgres" && isolated.Scheme != "postgresql") {
		t.Fatal("PostgreSQL 시험 URL이 필요합니다")
	}
	admin, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal("시험 관리 연결 실패")
	}
	t.Cleanup(admin.Close)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "agent_contract_" + strings.ReplaceAll(id.String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal("시험 전용 DB 생성 실패")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Error("시험 전용 DB 정리 실패")
		}
	})
	isolated.Path, isolated.RawPath = "/"+name, ""
	pool, err := migrate.NewPool(t.Context(), isolated.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var actual string
	if err := pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&actual); err != nil || actual != name {
		t.Fatal("시험 DB 격리 대조 실패")
	}
	loaded, err := migrate.Load(migrations.FS, ".", migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if err := migrate.Apply(t.Context(), pool, migration); err != nil {
			t.Fatal(err)
		}
	}
	query := isolated.Query()
	query.Set("pool_max_conns", "4")
	isolated.RawQuery = query.Encode()
	makeStore := func() *Store {
		backend, err := New(t.Context(), isolated.String(), "agent_context", nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(backend.Close)
		return backend
	}
	return makeStore(), makeStore(), pool
}

func requireAgentField(t *testing.T, err error, field string) {
	t.Helper()
	actual, ok := errors.AsType[model.FieldError](err)
	if !ok || actual.Field != field {
		t.Fatal("등록·행위자 오류 필드 불일치")
	}
}

func TestAgentStoreInvalidInputs(t *testing.T) {
	backend := &Store{}
	account, agent := newTestID(t), newTestID(t)
	for _, test := range []struct {
		name  string
		call  func() error
		field string
	}{
		{"register account", func() error {
			_, err := backend.RegisterAgent(t.Context(), model.ID{}, "client", "registration_key_1")
			return err
		}, "registration_key"},
		{"register client", func() error {
			_, err := backend.RegisterAgent(t.Context(), account, "", "registration_key_1")
			return err
		}, "registration_key"},
		{"register key", func() error { _, err := backend.RegisterAgent(t.Context(), account, "client", "short"); return err }, "registration_key"},
		{"validate account", func() error { return backend.ValidateAgent(t.Context(), model.ID{}, "client", agent) }, "agent_id"},
		{"validate client", func() error { return backend.ValidateAgent(t.Context(), account, "", agent) }, "agent_id"},
		{"validate id", func() error { return backend.ValidateAgent(t.Context(), account, "client", model.ID{}) }, "agent_id"},
		{"revoke account", func() error { return backend.RevokeAgent(t.Context(), model.ID{}, agent) }, "agent_id"},
		{"revoke id", func() error { return backend.RevokeAgent(t.Context(), account, model.ID{}) }, "agent_id"},
	} {
		t.Run(test.name, func(t *testing.T) { requireAgentField(t, test.call(), test.field) })
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := backend.RegisterAgent(ctx, account, "client", "registration_key_1"); !errors.Is(err, context.Canceled) {
		t.Fatal("취소 등록이 DB에 진입")
	}
	if err := backend.ValidateAgent(ctx, account, "client", agent); !errors.Is(err, context.Canceled) {
		t.Fatal("취소 검증이 DB에 진입")
	}
	if err := backend.RevokeAgent(ctx, account, agent); !errors.Is(err, context.Canceled) {
		t.Fatal("취소 폐기가 DB에 진입")
	}
}

func TestAgentRegistrationDatabase(t *testing.T) {
	first, second, pool := newAgentContractDatabase(t)
	account := func(t *testing.T) model.ID {
		t.Helper()
		id := newTestID(t)
		createTestAccount(t, first, id)
		return id
	}
	register := func(t *testing.T, source *Store, account model.ID, client, key string) model.ID {
		t.Helper()
		id, err := source.RegisterAgent(t.Context(), account, client, key)
		if err != nil || !id.IsV7() {
			t.Fatal("등록 준비 실패")
		}
		return id
	}
	count := func(t *testing.T, account model.ID) int {
		t.Helper()
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.agent_registration WHERE account_id=$1 AND revoked_at IS NULL`, account.String()).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	waitAccount := func(t *testing.T, ctx context.Context) {
		t.Helper()
		for {
			var waiting bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%FROM public.account%FOR UPDATE%')`).Scan(&waiting); err != nil {
				t.Fatal("등록 잠금 대기 확인 실패")
			}
			if waiting {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("등록이 잠금 대기에 도달하지 않음")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	t.Run("stable id and case sensitive bindings", func(t *testing.T) {
		owner := account(t)
		id := register(t, first, owner, "client", "registration_key_1")
		var created time.Time
		if err := pool.QueryRow(t.Context(), `SELECT created_at FROM public.agent_registration WHERE agent_id=$1`, id.String()).Scan(&created); err != nil {
			t.Fatal(err)
		}
		if got := register(t, second, owner, "client", "registration_key_1"); got != id {
			t.Fatal("등록 재조회가 ID 변경")
		}
		var unchanged bool
		if err := pool.QueryRow(t.Context(), `SELECT created_at=$2 FROM public.agent_registration WHERE agent_id=$1`, id.String(), created).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("재조회가 생성 시각 변경")
		}
		for _, input := range []struct {
			owner       model.ID
			client, key string
		}{{owner, "other-client", "registration_key_1"}, {account(t), "client", "registration_key_1"}, {owner, "client", "Registration_key_1"}} {
			if register(t, second, input.owner, input.client, input.key) == id {
				t.Fatal("계정·client·키 결합 공유")
			}
		}
		for _, input := range []struct {
			owner, agent model.ID
			client       string
		}{{owner, id, "other-client"}, {account(t), id, "client"}, {owner, newTestID(t), "client"}} {
			err := second.ValidateAgent(t.Context(), input.owner, input.client, input.agent)
			requireAgentField(t, err, "agent_id")
			if err.Error() != "등록된 행위자를 확인해 주십시오." {
				t.Fatal("타 계정/client 존재 노출")
			}
		}
		_, err := first.RegisterAgent(t.Context(), newTestID(t), "client", "registration_key_1")
		requireAgentField(t, err, "registration_key")
	})
	t.Run("actor boundary blocks revoked registration", func(t *testing.T) {
		owner := account(t)
		principal := mcp.Principal{AccountID: owner, ClientID: "client"}
		id, err := mcp.RegisterAgent(t.Context(), first, principal, "registration_key_1")
		if err != nil {
			t.Fatal(err)
		}
		actor, err := mcp.BindActor(t.Context(), second, principal, id)
		if err != nil || actor.AccountID != owner || actor.AgentID != id {
			t.Fatal("행위자 바인딩 실패")
		}
		if err := first.RevokeAgent(t.Context(), owner, id); err != nil {
			t.Fatal(err)
		}
		actor, err = mcp.BindActor(t.Context(), second, principal, id)
		requireAgentField(t, err, "agent_id")
		if actor != (mcp.Actor{}) {
			t.Fatal("폐기 행위자 전달")
		}
	})
	t.Run("same key concurrent registration", func(t *testing.T) {
		owner := account(t)
		type result struct {
			id  model.ID
			err error
		}
		results := make(chan result, 16)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for i := range 16 {
			workers.Go(func() {
				<-start
				id, err := []*Store{first, second}[i%2].RegisterAgent(t.Context(), owner, "client", "registration_key_1")
				results <- result{id, err}
			})
		}
		close(start)
		workers.Wait()
		close(results)
		var expected model.ID
		for result := range results {
			if result.err != nil || !result.id.IsV7() {
				t.Fatal("동시 등록 실패")
			}
			if expected.IsZero() {
				expected = result.id
			} else if result.id != expected {
				t.Fatal("동일 키가 여러 ID 반환")
			}
		}
		if count(t, owner) != 1 {
			t.Fatal("동일 등록 중복 저장")
		}
	})
	t.Run("128 cap spans clients and revoked slot is reusable", func(t *testing.T) {
		owner := account(t)
		var firstID model.ID
		for i := range 127 {
			id := register(t, first, owner, fmt.Sprintf("client-%d", i%2), fmt.Sprintf("registration_key_%03d", i))
			if i == 0 {
				firstID = id
			}
		}
		start := make(chan struct{})
		failures := make(chan error, 16)
		var workers sync.WaitGroup
		var succeeded int
		var mu sync.Mutex
		for i := range 16 {
			workers.Go(func() {
				<-start
				_, err := []*Store{first, second}[i%2].RegisterAgent(t.Context(), owner, "third-client", fmt.Sprintf("registration_new_%03d", i))
				if err == nil {
					mu.Lock()
					succeeded++
					mu.Unlock()
				} else {
					failures <- err
				}
			})
		}
		close(start)
		workers.Wait()
		close(failures)
		for err := range failures {
			limit, ok := errors.AsType[plan.LimitError](err)
			if !ok || limit.Name != "agent_registrations_per_account" || limit.Current != 128 || limit.Allowed != 128 {
				t.Fatal("활성 상한 오류 불일치")
			}
		}
		if succeeded != 1 || count(t, owner) != 128 {
			t.Fatal("동시 등록이 128개 상한 초과")
		}
		if register(t, second, owner, "client-0", "registration_key_000") != firstID {
			t.Fatal("상한에서 활성 재조회 거부")
		}
		if err := second.RevokeAgent(t.Context(), owner, firstID); err != nil {
			t.Fatal(err)
		}
		var revokedAt time.Time
		if err := pool.QueryRow(t.Context(), `SELECT revoked_at FROM public.agent_registration WHERE agent_id=$1`, firstID.String()).Scan(&revokedAt); err != nil {
			t.Fatal(err)
		}
		if err := first.RevokeAgent(t.Context(), owner, firstID); err != nil {
			t.Fatal("반복 폐기 거부")
		}
		var stable bool
		if err := pool.QueryRow(t.Context(), `SELECT revoked_at=$2 FROM public.agent_registration WHERE agent_id=$1`, firstID.String(), revokedAt).Scan(&stable); err != nil || !stable {
			t.Fatal("반복 폐기가 최초 시각 변경")
		}
		_, err := first.RegisterAgent(t.Context(), owner, "client-0", "registration_key_000")
		requireAgentField(t, err, "registration_key")
		register(t, first, owner, "client", "registration_replacement")
		if count(t, owner) != 128 {
			t.Fatal("폐기 후 활성 자리 미회복")
		}
		register(t, second, account(t), "client", "registration_key_000")
	})
	t.Run("owner revoke preserves contributions and audit", func(t *testing.T) {
		graph, owner, _ := boundaryGraph(t, first)
		agent := register(t, first, owner, "client", "registration_key_1")
		value := testSourceContext(t, graph, owner, "api://agent-history/"+graph.String())
		value.CreatedByAgent = agent
		stored, err := first.CreateContextWithOperation(t.Context(), graph, value, nil, registrationOperation(value, OperationAdd), WriteLimits{})
		if err != nil {
			t.Fatal(err)
		}
		other := account(t)
		requireAgentField(t, second.RevokeAgent(t.Context(), other, agent), "agent_id")
		requireAgentField(t, second.RevokeAgent(t.Context(), other, newTestID(t)), "agent_id")
		if err := first.ValidateAgent(t.Context(), owner, "client", agent); err != nil {
			t.Fatal("타 계정 폐기 시도가 소유 등록 변경")
		}
		if err := first.RevokeAgent(t.Context(), owner, agent); err != nil {
			t.Fatal(err)
		}
		loaded, err := second.Context(t.Context(), graph, stored.ID)
		if err != nil || loaded.CreatedByAgent != agent || loaded.CreatedBy != owner {
			t.Fatal("폐기가 과거 기여 재작성")
		}
		var audits int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE graph_id=$1 AND context_id=$2 AND actor_account_id=$3 AND actor_agent_id=$4`, graph.String(), stored.ID.String(), owner.String(), agent.String()).Scan(&audits); err != nil || audits != 1 {
			t.Fatal("폐기가 과거 감사 변경")
		}
		requireAgentField(t, second.ValidateAgent(t.Context(), owner, "client", agent), "agent_id")
	})
	t.Run("lock wait cancellation and another account progress", func(t *testing.T) {
		owner, other := account(t), account(t)
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.WithoutCancel(t.Context()))
		if _, err := tx.Exec(t.Context(), `SELECT account_id FROM public.account WHERE account_id=$1 FOR UPDATE`, owner.String()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		done := make(chan error, 1)
		go func() { _, err := first.RegisterAgent(ctx, owner, "client", "registration_key_1"); done <- err }()
		waitAccount(t, waitCtx)
		if _, err := second.RegisterAgent(waitCtx, other, "client", "registration_key_1"); err != nil {
			t.Fatal("계정 하나의 잠금이 다른 계정 차단")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("잠금 취소 원인 누락")
			}
		case <-waitCtx.Done():
			t.Fatal("잠금 취소 미반환")
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if count(t, owner) != 0 {
			t.Fatal("취소된 등록 저장")
		}
		register(t, first, owner, "client", "registration_key_1")
		assertConnectionRecovery(t, first, nil, 0)
	})
	t.Run("revocation while registration waits", func(t *testing.T) {
		owner := account(t)
		agent := register(t, first, owner, "client", "registration_key_1")
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.WithoutCancel(t.Context()))
		if err := lockAgentAccount(t.Context(), tx, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `UPDATE public.agent_registration SET revoked_at=now() WHERE agent_id=$1`, agent.String()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := second.RegisterAgent(ctx, owner, "client", "registration_key_1"); done <- err }()
		waitAccount(t, ctx)
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			requireAgentField(t, err, "registration_key")
		case <-ctx.Done():
			t.Fatal("폐기 커밋 뒤 재조회 미반환")
		}
		if count(t, owner) != 0 {
			t.Fatal("대기 중 폐기한 등록을 복구")
		}
	})
	for _, mode := range []string{"insert rollback", "register commit rollback", "revoke rollback", "revoke commit rollback"} {
		t.Run(mode, func(t *testing.T) {
			owner := account(t)
			agent := register(t, first, owner, "client", "registration_existing")
			event := "BEFORE INSERT"
			if strings.HasPrefix(mode, "revoke") {
				event = "BEFORE UPDATE"
			}
			trigger := "CREATE TRIGGER agent_fault " + event + " ON public.agent_registration FOR EACH ROW EXECUTE FUNCTION public.agent_fault()"
			if strings.Contains(mode, "commit") {
				event = "INSERT"
				if strings.HasPrefix(mode, "revoke") {
					event = "UPDATE"
				}
				trigger = "CREATE CONSTRAINT TRIGGER agent_fault AFTER " + event + " ON public.agent_registration DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.agent_fault()"
			}
			if _, err := pool.Exec(t.Context(), `CREATE FUNCTION public.agent_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic agent secret'; END $$; `+trigger); err != nil {
				t.Fatal(err)
			}
			cleaned := false
			cleanup := func() {
				if !cleaned {
					if _, err := pool.Exec(context.WithoutCancel(t.Context()), `DROP TRIGGER agent_fault ON public.agent_registration; DROP FUNCTION public.agent_fault()`); err != nil {
						t.Error("장애 주입 정리 실패")
					}
					cleaned = true
				}
			}
			defer cleanup()
			var err error
			if strings.HasPrefix(mode, "revoke") {
				err = first.RevokeAgent(t.Context(), owner, agent)
			} else {
				var id model.ID
				id, err = first.RegisterAgent(t.Context(), owner, "client", "registration_new_1")
				if !id.IsZero() {
					t.Fatal("실패 등록의 ID 반환")
				}
			}
			if _, ok := errors.AsType[*StorageError](err); !ok || strings.Contains(err.Error(), "synthetic agent secret") || count(t, owner) != 1 {
				t.Fatal("DB 실패의 롤백·오류 비노출 실패")
			}
			cleanup()
			if err := second.ValidateAgent(t.Context(), owner, "client", agent); err != nil {
				t.Fatal("실패 폐기가 등록 변경")
			}
			register(t, second, owner, "client", "registration_new_1")
		})
	}
	t.Run("closed database is internal failure", func(t *testing.T) {
		owner := account(t)
		agent := register(t, first, owner, "client", "registration_key_1")
		first.Close()
		_, registerErr := first.RegisterAgent(t.Context(), owner, "client", "registration_other")
		for _, err := range []error{registerErr, first.ValidateAgent(t.Context(), owner, "client", agent), first.RevokeAgent(t.Context(), owner, agent)} {
			if _, ok := errors.AsType[*StorageError](err); !ok {
				t.Fatal("저장 장애를 자격·등록 오류로 처리")
			}
		}
	})
}
