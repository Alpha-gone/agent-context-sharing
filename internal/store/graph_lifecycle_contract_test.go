package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func lifeNewGraph(t *testing.T, creator model.ID) model.Graph {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	return model.Graph{ID: newTestID(t), Name: "같은 이름 ' 😀", Description: "설명\n$graph$", CreatedBy: creator, CreatedAt: now, LastActivityAt: now, Version: 1}
}

func TestLifeGraphCreationValidation(t *testing.T) {
	base := lifeNewGraph(t, newTestID(t))
	for _, sample := range []struct {
		name, field string
		change      func(*model.Graph)
	}{
		{"version", "version", func(g *model.Graph) { g.Version = 2 }},
		{"stored_chars", "stored_chars", func(g *model.Graph) { g.StoredChars = 1 }},
		{"deleted", "graph_id", func(g *model.Graph) { g.DeletedAt = new(g.CreatedAt) }},
		{"grace", "graph_id", func(g *model.Graph) { g.GraceStartedAt = new(g.CreatedAt) }},
		{"empty", "name", func(g *model.Graph) { g.Name = "" }},
		{"whitespace", "name", func(g *model.Graph) { g.Name = " \t\n　" }},
		{"invalid_text", "description", func(g *model.Graph) { g.Description = "\x00" }},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := base
			sample.change(&graph)
			database := &Store{} // DB 연결 전에 입력을 거부해야 한다.
			_, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{})
			if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != sample.field {
				t.Fatalf("입력 오류=%v, want %s", err, sample.field)
			}
		})
	}
}

func TestLifeGraphCreationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	creator, other := newTestID(t), newTestID(t)
	createTestAccount(t, database, creator)
	createTestAccount(t, database, other)
	for _, account := range []model.ID{creator, creator, other} {
		graph := lifeNewGraph(t, account)
		stored, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{GraphsPerAccount: 2})
		if err != nil || !reflect.DeepEqual(stored, graph) {
			t.Fatalf("동명 그래프 생성=%+v/%v", stored, err)
		}
		assertGrade(t, database, graph.ID, account, model.GraphGradeOwner)
		var grants int
		if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.graph_grant WHERE graph_id=$1`, graph.ID.String()).Scan(&grants); err != nil || grants != 1 {
			t.Fatalf("초기 소유자 부여=%d/%v", grants, err)
		}
	}
	// 생성 계정은 이력이며, 현재 소유자와 노드 기여 계정은 각각 별개다.
	graph := lifeNewGraph(t, other)
	if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := database.GrantGraph(t.Context(), graph.ID, other, creator, GrantSubjectAccount, model.GraphGradeOwner); err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeGraphGrantWithAudit(t.Context(), graph.ID, other, other, GrantSubjectAccount); err != nil {
		t.Fatal(err)
	}
	node := testSourceContext(t, graph.ID, creator, "api://life01/"+graph.ID.String())
	if _, err := database.CreateContext(t.Context(), graph.ID, node, nil); err != nil {
		t.Fatal(err)
	}
	before, err := database.Graph(t.Context(), graph.ID)
	if err != nil {
		t.Fatal(err)
	}
	systemBefore := lifeGraphSystemState(t, database, graph.ID)
	updated, err := database.UpdateGraph(t.Context(), graph.ID, 1, "갱신 ' 😀", "")
	if err != nil || updated.Name != "갱신 ' 😀" || updated.Description != "" || updated.Version != 2 {
		t.Fatalf("그래프 정보 갱신=%+v/%v", updated, err)
	}
	if err := model.ValidateGraphUpdate(before, updated); err != nil || updated.CreatedBy != other || node.CreatedBy != creator {
		t.Fatalf("생성 이력·기여·시스템 속성 보존=%v", err)
	}
	if systemBefore != lifeGraphSystemState(t, database, graph.ID) {
		t.Fatal("메타데이터 갱신이 내용 판·유예 만료 등 시스템 열을 바꿨다")
	}
	assertGrade(t, database, graph.ID, creator, model.GraphGradeOwner)
	if _, found, err := database.EffectiveGrade(t.Context(), graph.ID, other); err != nil || found {
		t.Fatalf("생성 이력이 접근 권한으로 사용됐다: %t/%v", found, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeGraphCreationFailureIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	creator := newTestID(t)
	createTestAccount(t, database, creator)
	graph := lifeNewGraph(t, creator)
	if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	duplicate := graph
	duplicate.Name = "중복 ID로 덮어쓰기"
	if _, err := database.CreateGraphWithOwner(t.Context(), duplicate, WriteLimits{}); err == nil {
		t.Fatal("중복 graph_id를 허용했다")
	}
	lifeAssertGraph(t, database, graph)
	assertGrade(t, database, graph.ID, creator, model.GraphGradeOwner)
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_creator/limited=%t", limited), func(t *testing.T) {
			missing := lifeNewGraph(t, newTestID(t))
			limits := WriteLimits{}
			if limited {
				limits.GraphsPerAccount = 1
			}
			if _, err := database.CreateGraphWithOwner(t.Context(), missing, limits); !errors.Is(err, ErrNotFound) {
				t.Fatalf("없는 생성 계정의 생성 결과=%v", err)
			}
			lifeAssertAbsent(t, database, missing.ID)
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeGraphUpdateValidationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	creator := newTestID(t)
	createTestAccount(t, database, creator)
	graph := lifeNewGraph(t, creator)
	if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct{ name, description, field string }{
		{"", "", "name"}, {" \t\n　", "", "name"}, {"a\x00", "", "name"},
		{string([]byte{0xff}), "", "name"}, {"valid", "a\x00", "description"},
		{"valid", string([]byte{0xff}), "description"},
	} {
		t.Run(fmt.Sprintf("%q/%q", sample.name, sample.description), func(t *testing.T) {
			_, err := database.UpdateGraph(t.Context(), graph.ID, 1, sample.name, sample.description)
			if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != sample.field {
				t.Fatalf("메타데이터 입력 오류=%v", err)
			}
			lifeAssertGraph(t, database, graph)
		})
	}
	for _, version := range []int64{0, -1} {
		_, err := database.UpdateGraph(t.Context(), graph.ID, version, "valid", "")
		if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != "expected_version" {
			t.Fatalf("기대 판 입력 오류=%v", err)
		}
	}
	if _, err := database.UpdateGraph(t.Context(), newTestID(t), 1, "없는 그래프", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("부재 그래프=%v", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeGraphWriteRollbackIntegration(t *testing.T) {
	for _, action := range []string{"create", "update"} {
		for _, deferred := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deferred=%t", action, deferred), func(t *testing.T) {
				database := newSmallPoolStore(t)
				creator := newTestID(t)
				createTestAccount(t, database, creator)
				graph := lifeNewGraph(t, creator)
				table, event := "graph_grant", "INSERT"
				if action == "update" {
					if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
						t.Fatal(err)
					}
					table, event = "context_graph", "UPDATE"
				}
				remove := lifeFailGraphWrite(t, database, graph.ID, table, event, deferred)
				var err error
				if action == "create" {
					_, err = database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{GraphsPerAccount: 1})
				} else {
					_, err = database.UpdateGraph(t.Context(), graph.ID, 1, "실패한 변경", "실패한 설명")
				}
				if _, ok := errors.AsType[*StorageError](err); !ok {
					t.Fatalf("장애 주입 결과=%v", err)
				}
				if action == "create" {
					lifeAssertAbsent(t, database, graph.ID)
				} else {
					lifeAssertGraph(t, database, graph)
				}
				remove()
				// 같은 ID·기대 판으로 재시도해 고아 행·소모된 판·한도가 없는지 확인한다.
				if action == "create" {
					_, err = database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{GraphsPerAccount: 1})
				} else {
					_, err = database.UpdateGraph(t.Context(), graph.ID, 1, "재시도", "")
				}
				if err != nil {
					t.Fatal(err)
				}
				assertGrade(t, database, graph.ID, creator, model.GraphGradeOwner)
				assertConnectionRecovery(t, database, nil, 0)
			})
		}
	}
}

func TestLifeGraphOuterRollbackIntegration(t *testing.T) {
	for _, create := range []bool{true, false} {
		t.Run(fmt.Sprintf("create=%t", create), func(t *testing.T) {
			database := newSmallPoolStore(t)
			creator := newTestID(t)
			createTestAccount(t, database, creator)
			graph := lifeNewGraph(t, creator)
			if !create {
				if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), tx)
			ctx := context.WithValue(t.Context(), writeTransactionContextKey{}, tx)
			if create {
				_, err = database.CreateGraphWithOwner(ctx, graph, WriteLimits{GraphsPerAccount: 1})
			} else {
				_, err = database.UpdateGraph(ctx, graph.ID, 1, "바깥 롤백", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if create {
				lifeAssertAbsent(t, database, graph.ID)
			} else {
				lifeAssertGraph(t, database, graph)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestLifeGraphConcurrentVersionIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	creator := newTestID(t)
	createTestAccount(t, database, creator)
	graph := lifeNewGraph(t, creator)
	if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const requests = 8
	results := make(chan error, requests)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range requests {
		group.Go(func() {
			<-start
			_, err := database.UpdateGraph(ctx, graph.ID, 1, "동시 갱신", "새 설명")
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	applied, conflicts := 0, 0
	for err := range results {
		if err == nil {
			applied++
		} else if conflict, ok := errors.AsType[VersionConflictError](err); ok && conflict.Current == 2 {
			conflicts++
		} else {
			t.Fatalf("동시 수정 결과=%v", err)
		}
	}
	if applied != 1 || conflicts != requests-1 {
		t.Fatalf("적용/충돌=%d/%d", applied, conflicts)
	}
	graph.Name, graph.Description, graph.Version = "동시 갱신", "새 설명", 2
	lifeAssertGraph(t, database, graph)
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeGraphLockRecheckIntegration(t *testing.T) {
	for _, action := range []string{"updated", "deleted", "rolled_back", "cancel_create", "cancel_update"} {
		t.Run(action, func(t *testing.T) {
			database := newSmallPoolStore(t)
			creator := newTestID(t)
			createTestAccount(t, database, creator)
			graph := lifeNewGraph(t, creator)
			if action != "cancel_create" {
				if _, err := database.CreateGraphWithOwner(t.Context(), graph, WriteLimits{}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(ctx, tx)
			if action == "cancel_create" {
				_, err = tx.Exec(ctx, `SELECT 1 FROM public.account WHERE account_id=$1 FOR UPDATE`, creator.String())
			} else if action == "deleted" {
				_, err = tx.Exec(ctx, `UPDATE public.context_graph SET deleted_at=$2 WHERE graph_id=$1`, graph.ID.String(), graph.CreatedAt)
			} else {
				_, err = tx.Exec(ctx, `UPDATE public.context_graph SET name='선행 변경', version=2 WHERE graph_id=$1`, graph.ID.String())
			}
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			request, stop := context.WithCancel(ctx)
			defer stop()
			done := make(chan error, 1)
			var group sync.WaitGroup
			group.Go(func() {
				var err error
				if action == "cancel_create" {
					_, err = database.CreateGraphWithOwner(request, graph, WriteLimits{GraphsPerAccount: 1})
				} else {
					_, err = database.UpdateGraph(request, graph.ID, 1, "대기 뒤 갱신", "")
				}
				done <- err
			})
			defer func() { stop(); rollbackTransaction(ctx, tx); group.Wait() }()
			waitGrantWaiters(t, ctx, database, pid, 1)
			switch action {
			case "cancel_create", "cancel_update":
				stop()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("잠금 대기 취소=%v", err)
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if action == "cancel_create" {
					lifeAssertAbsent(t, database, graph.ID)
				} else {
					lifeAssertGraph(t, database, graph)
				}
			case "rolled_back":
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				graph.Name, graph.Description, graph.Version = "대기 뒤 갱신", "", 2
				lifeAssertGraph(t, database, graph)
			default:
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				err := <-done
				if action == "deleted" {
					if !errors.Is(err, ErrNotFound) {
						t.Fatalf("삭제 뒤 재판정=%v", err)
					}
					graph.DeletedAt = new(graph.CreatedAt)
				} else {
					if conflict, ok := errors.AsType[VersionConflictError](err); !ok || conflict.Current != 2 {
						t.Fatalf("갱신 뒤 재판정=%v", err)
					}
					graph.Name, graph.Version = "선행 변경", 2
				}
				lifeAssertGraph(t, database, graph)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func lifeAssertGraph(t *testing.T, database *Store, want model.Graph) {
	t.Helper()
	got, err := database.Graph(t.Context(), want.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("그래프 상태=%+v/%v, want %+v", got, err, want)
	}
}

func lifeGraphSystemState(t *testing.T, database *Store, graph model.ID) string {
	t.Helper()
	var state string
	if err := database.pool.QueryRow(t.Context(), `SELECT (to_jsonb(g)-'name'-'description'-'version')::text
		FROM public.context_graph AS g WHERE graph_id=$1`, graph.String()).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func lifeAssertAbsent(t *testing.T, database *Store, graph model.ID) {
	t.Helper()
	var graphs, grants int
	if err := database.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM public.context_graph WHERE graph_id=$1),
		(SELECT count(*) FROM public.graph_grant WHERE graph_id=$1)`, graph.String()).Scan(&graphs, &grants); err != nil || graphs != 0 || grants != 0 {
		t.Fatalf("실패 뒤 그래프/등급=%d/%d/%v", graphs, grants, err)
	}
}

func lifeFailGraphWrite(t *testing.T, database *Store, graph model.ID, table, event string, deferred bool) func() {
	t.Helper()
	name := pgx.Identifier{"life01_" + graph.String()}.Sanitize()
	function := pgx.Identifier{"public", "life01_" + graph.String()}.Sanitize()
	target := pgx.Identifier{"public", table}.Sanitize()
	timing := "CREATE TRIGGER " + name + " BEFORE " + event
	if deferred {
		timing = "CREATE CONSTRAINT TRIGGER " + name + " AFTER " + event
	}
	timing += " ON " + target
	if deferred {
		timing += " DEFERRABLE INITIALLY DEFERRED"
	}
	remove := sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := database.pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON "+target+"; DROP FUNCTION IF EXISTS "+function+"()"); err != nil {
			t.Errorf("장애 주입 정리: %v", err)
		}
	})
	if _, err := database.pool.Exec(t.Context(), "CREATE FUNCTION "+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.graph_id='`+graph.String()+`'::uuid THEN RAISE EXCEPTION 'synthetic graph failure'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	if _, err := database.pool.Exec(t.Context(), timing+" FOR EACH ROW EXECUTE FUNCTION "+function+"()"); err != nil {
		t.Fatal(err)
	}
	return remove
}
