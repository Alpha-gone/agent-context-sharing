package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func nodeCreateSample(t *testing.T, graph, actor, seed model.ID, layer model.Layer) (model.Context, []model.ID) {
	t.Helper()
	switch layer {
	case model.LayerSource:
		return testSourceContext(t, graph, actor, "api://life02/"+newTestID(t).String()), nil
	case model.LayerDerived:
		return testDerivedContext(t, graph, actor), []model.ID{seed}
	default:
		return testEventContext(t, graph, actor, seed), nil
	}
}

func TestNodeCreationLayersIntegration(t *testing.T) {
	for _, targets := range []IndexTargets{IndexTargetsAllLayers, IndexTargetsWithoutSource} {
		for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
			t.Run(string(targets)+"/"+string(layer), func(t *testing.T) {
				database := newSmallPoolStore(t)
				database.indexTargets = targets
				graph, actor, seed := boundaryGraph(t, database)
				agent := newTestID(t)
				// MCP가 검증한 등록 식별자를 주입하는 저장 경계다. 인증·client 판정은 별도다.
				if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.agent_registration
					(agent_id,account_id,client_id,registration_key,created_at) VALUES ($1,$2,'life02-client','life02_registration_key',now())`, agent.String(), actor.String()); err != nil {
					t.Fatal(err)
				}
				value, refs := nodeCreateSample(t, graph, actor, seed.ID, layer)
				value.CreatedByAgent, value.Body = agent, "한글🙂본문"
				before, err := database.Graph(t.Context(), graph)
				if err != nil {
					t.Fatal(err)
				}
				var revision int64
				if err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id=$1`, graph.String()).Scan(&revision); err != nil {
					t.Fatal(err)
				}
				stored, err := database.CreateContextWithOperation(t.Context(), graph, value, refs, registrationOperation(value, OperationAdd), WriteLimits{})
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := database.Context(t.Context(), graph, value.ID)
				if err != nil || !reflect.DeepEqual(canonicalReferences(loaded), canonicalReferences(withReferences(value, refs, value))) || !reflect.DeepEqual(canonicalReferences(loaded), canonicalReferences(stored)) {
					t.Fatalf("계층·참조·주체 왕복=%+v/%v", loaded, err)
				}
				after, err := database.Graph(t.Context(), graph)
				if err != nil || after.StoredChars-before.StoredChars != 5 || after.LastActivityAt.Before(before.LastActivityAt) || after.Version != before.Version {
					t.Fatalf("저장량·활동 시각·그래프 판=%+v/%v", after, err)
				}
				var nextRevision int64
				var audits int
				if err := database.pool.QueryRow(t.Context(), `SELECT content_revision,
					(SELECT count(*) FROM public.operation_log WHERE graph_id=$1 AND context_id=$2 AND target_version=1
					 AND actor_account_id=$3 AND actor_agent_id=$4 AND operation_kind='add' AND result='applied')
					FROM public.context_graph WHERE graph_id=$1`, graph.String(), value.ID.String(), actor.String(), agent.String()).Scan(&nextRevision, &audits); err != nil || nextRevision != revision+1 || audits != 1 {
					t.Fatalf("내용 판·감사=%d/%d/%v", nextRevision, audits, err)
				}
				wantTasks := 1
				if layer == model.LayerSource && targets == IndexTargetsWithoutSource {
					wantTasks = 0
				}
				if got := indexTaskCount(t, database, value.ID); got != wantTasks {
					t.Fatalf("색인 등록=%d, want %d", got, wantTasks)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestNodeCreationInputAndActorIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, seed := boundaryGraph(t, database)
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		t.Run(string(layer), func(t *testing.T) {
			for _, mismatch := range []string{"version", "graph", "account", "agent", "target", "kind", "missing_account"} {
				t.Run(mismatch, func(t *testing.T) {
					value, refs := nodeCreateSample(t, graph, actor, seed.ID, layer)
					op := registrationOperation(value, OperationAdd)
					limits := WriteLimits{}
					switch mismatch {
					case "version":
						value.Version = 2
					case "graph":
						op.GraphID = newTestID(t)
					case "account":
						op.AccountID = newTestID(t)
					case "agent":
						op.AgentID = newTestID(t)
					case "target":
						op.ContextID = newTestID(t)
					case "kind":
						op.Kind = OperationUpdate
					case "missing_account":
						value.CreatedBy = newTestID(t)
						op.AccountID = value.CreatedBy
					}
					before := registrationStateFingerprint(t, database)
					_, err := database.CreateContextWithOperation(t.Context(), graph, value, refs, op, limits)
					if mismatch == "missing_account" {
						if !errors.Is(err, ErrNotFound) {
							t.Fatalf("부재 기여 계정=%v", err)
						}
					} else if _, ok := errors.AsType[model.FieldError](err); !ok {
						t.Fatalf("입력·주체 결합=%v", err)
					}
					if before != registrationStateFingerprint(t, database) {
						t.Fatal("거부된 생성의 부분 적용")
					}
				})
			}
		})
	}
	derived := testDerivedContext(t, graph, actor)
	derived.Derived.EvidenceInvalidated = true
	if _, err := database.CreateContext(t.Context(), graph, derived, []model.ID{seed.ID}); err == nil {
		t.Fatal("생성 시 시스템 근거 무효 표시 허용")
	}
	for _, sample := range []string{"derived_empty", "derived_duplicate", "event_empty", "event_duplicate"} {
		t.Run(sample, func(t *testing.T) {
			value, refs := nodeCreateSample(t, graph, actor, seed.ID, model.LayerDerived)
			field := "derived_from"
			switch sample {
			case "derived_empty":
				refs = nil
			case "derived_duplicate":
				refs = []model.ID{seed.ID, seed.ID}
			default:
				value, refs = nodeCreateSample(t, graph, actor, seed.ID, model.LayerEvent)
				field = "member_refs"
				value.Event.MemberIDs = nil
				if sample == "event_duplicate" {
					value.Event.MemberIDs = []model.ID{seed.ID, seed.ID}
				}
			}
			before := registrationStateFingerprint(t, database)
			_, err := database.CreateContext(t.Context(), graph, value, refs)
			if f, ok := errors.AsType[model.FieldError](err); !ok || f.Field != field {
				t.Fatalf("참조 기수성=%v", err)
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("참조 거부 뒤 부분 적용")
			}
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestNodeCreationSourceDuplicateIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, _ := boundaryGraph(t, database)
	first := testSourceContext(t, graph, actor, "api://duplicate/"+graph.String())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTransaction(ctx, tx)
	if _, err := database.CreateContextWithOperation(context.WithValue(ctx, writeTransactionContextKey{}, tx), graph, first, nil, registrationOperation(first, OperationAdd), WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	duplicate := testSourceContext(t, graph, actor, first.Source.Reference.Locator)
	duplicate.Body = "중복 본문은 반영하지 않는다"
	duplicate.Source.Reference.Channel = model.SourceChannelOther
	done := make(chan struct {
		value model.Context
		err   error
	}, 1)
	var group sync.WaitGroup
	group.Go(func() {
		value, err := database.CreateContextWithOperation(ctx, graph, duplicate, nil, registrationOperation(duplicate, OperationAdd), WriteLimits{})
		done <- struct {
			value model.Context
			err   error
		}{value, err}
	})
	defer func() { cancel(); rollbackTransaction(ctx, tx); group.Wait() }()
	waitGrantWaiters(t, ctx, database, int(tx.Conn().PgConn().PID()), 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || result.value.ID != first.ID || result.value.Body != first.Body {
		t.Fatalf("자연 키 경합=%+v/%v", result.value, result.err)
	}
	var audits, nodes int
	if err := database.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM public.operation_log WHERE graph_id=$1),
		(SELECT count(*) FROM `+database.contextTable()+` WHERE properties->>'graph_id'::text=$1::text AND properties->>'source_ref_locator'::text=$2::text)`, graph.String(), first.Source.Reference.Locator).Scan(&audits, &nodes); err != nil || audits != 1 || nodes != 1 || indexTaskCount(t, database, duplicate.ID) != 0 {
		t.Fatalf("중복 뒤 정점·감사=%d/%d/%v", nodes, audits, err)
	}
	before := registrationStateFingerprint(t, database)
	if result, err := database.CreateContextWithOperation(t.Context(), graph, duplicate, nil, registrationOperation(duplicate, OperationAdd), WriteLimits{ActorID: actor, WritesPerMinute: 1}); err != nil || result.ID != first.ID {
		t.Fatalf("중복 선조회=%+v/%v", result, err)
	}
	if before != registrationStateFingerprint(t, database) {
		t.Fatal("중복 반환이 활동·저장량·내용 판·큐·감사를 변경했다")
	}
	// 삭제된 그래프의 중복도 일반 생성과 같이 거부한다.
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at=now() WHERE graph_id=$1`, graph.String()); err != nil {
		t.Fatal(err)
	}
	before = registrationStateFingerprint(t, database)
	if _, err := database.CreateContext(t.Context(), graph, duplicate, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("삭제 그래프의 중복=%v", err)
	}
	if before != registrationStateFingerprint(t, database) {
		t.Fatal("삭제 그래프 중복의 부분 적용")
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestNodeCreationRollbackIntegration(t *testing.T) {
	for _, layer := range []model.Layer{model.LayerSource, model.LayerDerived, model.LayerEvent} {
		for _, stage := range []string{"vertex", "reference", "queue", "activity", "audit", "commit", "unrelated_unique"} {
			if layer == model.LayerSource && stage == "reference" {
				continue
			}
			t.Run(string(layer)+"/"+stage, func(t *testing.T) {
				database := newSmallPoolStore(t)
				graph, actor, seed := boundaryGraph(t, database)
				value, refs := nodeCreateSample(t, graph, actor, seed.ID, layer)
				request := t.Context()
				var outer pgx.Tx
				remove := func() {}
				if stage == "vertex" || stage == "reference" {
					var err error
					outer, err = database.pool.Begin(request)
					if err != nil {
						t.Fatal(err)
					}
					defer rollbackTransaction(request, outer)
					fragment := "CREATE (node:Context"
					if stage == "reference" {
						fragment = "CREATE (from)-[:"
					}
					request = context.WithValue(request, writeTransactionContextKey{}, pgx.Tx(nodeCreationFailureTx{Tx: outer, fragment: fragment}))
				} else {
					remove = nodeFailCreation(t, database, graph, stage)
				}
				before := registrationStateFingerprint(t, database)
				_, err := database.CreateContextWithOperation(request, graph, value, refs, registrationOperation(value, OperationAdd), WriteLimits{ActorID: actor, WritesPerMinute: 1})
				if _, ok := errors.AsType[*StorageError](err); !ok {
					t.Fatalf("장애 주입=%v", err)
				}
				// 저장점 롤백 뒤 바깥 커밋까지 성공해야 부분 적용 부재를 증명할 수 있다.
				if outer != nil {
					if err := outer.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "unrelated_unique" {
					if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.ConstraintName != "life02_other_unique" || errors.Is(err, ErrNotFound) {
						t.Fatalf("원래 오류 보존=%v", err)
					}
				}
				if before != registrationStateFingerprint(t, database) {
					t.Fatal("쓰기 실패가 정점·참조·큐·누계·감사에 부분 적용을 남겼다")
				}
				remove()
				if _, err := database.CreateContextWithOperation(t.Context(), graph, value, refs, registrationOperation(value, OperationAdd), WriteLimits{ActorID: actor, WritesPerMinute: 1}); err != nil {
					t.Fatalf("실패 뒤 같은 요청·빈도 한도 재시도=%v", err)
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

// AGE 쓰기 실패는 실제 트랜잭션의 질의 경계에서 주입한다.
type nodeCreationFailureTx struct {
	pgx.Tx
	fragment string
}

func (tx nodeCreationFailureTx) Begin(ctx context.Context) (pgx.Tx, error) {
	nested, err := tx.Tx.Begin(ctx)
	return nodeCreationFailureTx{Tx: nested, fragment: tx.fragment}, err
}

func (tx nodeCreationFailureTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, tx.fragment) {
		return nodeCreationErrorRow{cause: &pgconn.PgError{Code: "XX000", Message: "synthetic node query failure"}}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

type nodeCreationErrorRow struct{ cause error }

func (row nodeCreationErrorRow) Scan(...any) error { return row.cause }

func (tx nodeCreationFailureTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, tx.fragment) {
		return pgconn.CommandTag{}, &pgconn.PgError{Code: "XX000", Message: "synthetic reference query failure"}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func nodeFailCreation(t *testing.T, database *Store, graph model.ID, stage string) func() {
	t.Helper()
	table, event, condition := "operation_log", "INSERT", "NEW.graph_id"
	schema := "public"
	switch stage {
	case "queue":
		table = "index_task"
	case "history":
		table = "context_revision"
	case "activity":
		table, event = "context_graph", "UPDATE"
	}
	name := pgx.Identifier{"life02_" + graph.String()}.Sanitize()
	function := pgx.Identifier{"public", "life02_" + graph.String()}.Sanitize()
	target := pgx.Identifier{schema, table}.Sanitize()
	raise := "RAISE EXCEPTION 'synthetic node failure'"
	if stage == "unrelated_unique" {
		raise += " USING ERRCODE='23505', CONSTRAINT='life02_other_unique', SCHEMA='public'"
	}
	remove := sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := database.pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON "+target+"; DROP FUNCTION IF EXISTS "+function+"()"); err != nil {
			t.Errorf("장애 주입 정리=%v", err)
		}
	})
	if _, err := database.pool.Exec(t.Context(), "CREATE FUNCTION "+function+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF "+condition+"='"+graph.String()+"'::uuid THEN "+raise+"; END IF; RETURN NEW; END $$"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	timing := "CREATE TRIGGER " + name + " BEFORE " + event + " ON " + target
	if stage == "commit" {
		timing = "CREATE CONSTRAINT TRIGGER " + name + " AFTER " + event + " ON " + target + " DEFERRABLE INITIALLY DEFERRED"
	}
	if _, err := database.pool.Exec(t.Context(), timing+" FOR EACH ROW EXECUTE FUNCTION "+function+"()"); err != nil {
		t.Fatal(err)
	}
	return remove
}
