package store

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestQueryIndexCatalogIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	indexes := []struct {
		schema, table, name string
		unique              bool
		fragments           []string
	}{
		{"public", "account", "account_login_id_idx", true, []string{"(login_id)"}},
		{"public", "context_graph", "context_graph_active_activity_idx", false, []string{"(last_activity_at DESC, graph_id DESC)", "deleted_at IS NULL"}},
		{"public", "graph_grant", "graph_grant_subject_idx", false, []string{"(subject_id)"}},
		{"public", "team_member", "team_member_account_idx", false, []string{"(account_id)"}},
		{"public", "index_task", "index_task_context_idx", true, []string{"(context_id)"}},
		{"public", "index_task", "index_task_state_next_attempt_idx", false, []string{"(state, next_attempt_at)"}},
		{"public", "operation_log", "operation_log_context_applied_idx", false, []string{"(context_id, applied_at)"}},
		{"public", "operation_log", "operation_log_relation_idx", false, []string{"(relation_id, applied_at)", "relation_id IS NOT NULL"}},
		{"public", "operation_log", "operation_log_graph_applied_idx", false, []string{"(graph_id, applied_at)"}},
		{"public", "idempotency_record", "idempotency_record_expires_idx", false, []string{"(expires_at)"}},
		{"public", "web_audit_log", "web_audit_log_graph_occurred_idx", false, []string{"(graph_id, occurred_at DESC)"}},
		{"public", "web_audit_log", "web_audit_log_team_actor_idx", false, []string{"(actor_account_id, occurred_at)", "graph_id IS NULL"}},
		{"public", "request_rate", "request_rate_window_idx", false, []string{"(window_started_at)"}},
		{"public", "signing_key", "signing_key_active_idx", true, []string{"(state)", "state = 'active'"}},
		{"public", "revoked_token", "revoked_token_expires_idx", false, []string{"(expires_at)"}},
		{"public", "oauth_grant", "oauth_grant_account_client_idx", false, []string{"(account_id, client_id, authorized_at DESC, grant_id DESC)"}},
		{"public", "oauth_grant", "oauth_grant_retain_until_idx", false, []string{"(retain_until)"}},
		{"public", "oauth_authorization_code", "oauth_authorization_code_grant_idx", false, []string{"(grant_id)"}},
		{"public", "oauth_authorization_code", "oauth_authorization_code_unconsumed_expires_idx", false, []string{"(expires_at)", "consumed_at IS NULL"}},
		{"public", "oauth_refresh_token", "oauth_refresh_token_grant_idx", false, []string{"(grant_id)"}},
		{"public", "agent_registration", "agent_registration_account_client_idx", false, []string{"(account_id, client_id, created_at, agent_id)"}},
		{"public", "agent_registration", "agent_registration_active_account_idx", false, []string{"(account_id)", "revoked_at IS NULL"}},
		{database.graphName, "Context", "context_graph_id_text_idx", false, []string{"properties ->> 'graph_id'::text"}},
		{database.graphName, "Context", "context_context_id_agtype_idx", false, []string{"agtype_access_operator", `"context_id"`}},
		{database.graphName, "Context", "context_active_graph_context_idx", false, []string{"properties ->> 'graph_id'::text", `COLLATE "C"`, "properties ->> 'context_id'::text", "deleted_at'::text) IS NULL"}},
		{database.graphName, "Context", "context_graph_context_lookup_idx", false, []string{"properties ->> 'graph_id'::text", `COLLATE "C"`, "properties ->> 'context_id'::text"}},
		{database.graphName, "Context", "context_active_event_proposal_idx", false, []string{"properties ->> 'graph_id'::text", `COLLATE "C"`, "DESC", "deleted_at'::text) IS NULL", "'event'"}},
		{database.graphName, "Context", "context_body_fts_idx", false, []string{"USING gin", "to_tsvector('simple'::regconfig", "body'::text"}},
		{database.graphName, "Context", "context_source_ref_idx", true, []string{"properties ->> 'graph_id'::text", "source_ref_locator'::text", "layer'::text) = 'source'"}},
	}
	for _, label := range []string{"DERIVED_FROM", "SUPERSEDES", "HAS_MEMBER", "PRECEDES", "CAUSES", "PART_OF", "RELATES_TO"} {
		indexes = append(indexes, struct {
			schema, table, name string
			unique              bool
			fragments           []string
		}{database.graphName, label, strings.ToLower(label) + "_hop_endpoints_idx", false, []string{"(start_id, end_id)"}})
	}
	for _, label := range []string{"PRECEDES", "CAUSES", "PART_OF"} {
		indexes = append(indexes, struct {
			schema, table, name string
			unique              bool
			fragments           []string
		}{database.graphName, label, strings.ToLower(label) + "_graph_id_agtype_idx", false, []string{"agtype_access_operator", `"graph_id"`}})
	}
	for label := range relationLabels {
		for suffix, fields := range map[string][]string{
			"_graph_relation_idx": {`"graph_id"`, `"relation_id"`},
			"_graph_identity_idx": {`"graph_id"`, `"from_context_id"`, `"to_context_id"`},
			"_graph_target_idx":   {`"graph_id"`, `"to_context_id"`},
		} {
			indexes = append(indexes, struct {
				schema, table, name string
				unique              bool
				fragments           []string
			}{database.graphName, label, strings.ToLower(label) + suffix, false, append([]string{"agtype_access_operator"}, fields...)})
		}
	}
	for _, index := range indexes {
		t.Run(index.name, func(t *testing.T) {
			var definition, table string
			var unique, valid bool
			err := database.pool.QueryRow(t.Context(), `SELECT pg_get_indexdef(i.indexrelid), tab.relname,
				i.indisunique, i.indisvalid AND i.indisready
				FROM pg_index i JOIN pg_class idx ON idx.oid=i.indexrelid
				JOIN pg_namespace ns ON ns.oid=idx.relnamespace JOIN pg_class tab ON tab.oid=i.indrelid
				WHERE ns.nspname=$1 AND idx.relname=$2`, index.schema, index.name).Scan(&definition, &table, &unique, &valid)
			if err != nil || table != index.table || unique != index.unique || !valid {
				t.Fatalf("인덱스 대상·유일·준비=%s/%t/%t: %v", table, unique, valid, err)
			}
			for _, fragment := range index.fragments {
				if !strings.Contains(definition, fragment) {
					t.Errorf("인덱스 정의에 %q 없음: %s", fragment, definition)
				}
			}
		})
	}
}

// 실제 쓰기 검사의 질의를 그대로 관찰해 EXPLAIN한다. 운영 코드에 시험용 선택기를 두지 않는다.
type indexPlanQueryer struct {
	cypherQueryer
	queries   []string
	arguments [][]any
}

func (q *indexPlanQueryer) Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error) {
	q.queries = append(q.queries, sql)
	q.arguments = append(q.arguments, arguments)
	return q.cypherQueryer.Query(ctx, sql, arguments...)
}

func planUsesIndex(plan hopPlanNode, name string) bool {
	if plan.Index == name && plan.Loops > 0 {
		return true
	}
	for _, child := range plan.Plans {
		if planUsesIndex(child, name) {
			return true
		}
	}
	return false
}

func TestQueryIndexPlansIntegration(t *testing.T) {
	for _, size := range []int{1000, 10000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			// 실행 계획이 다른 시험의 누적 데이터·통계에 좌우되지 않게 시험 DB를 격리한다.
			database := newIsolatedIntegrationStore(t)
			graph, hub := createHopHub(t, database, size)
			createHopHub(t, database, size) // 다른 그래프도 같은 label 테이블을 공유한다.
			for _, label := range []string{"Context", "DERIVED_FROM"} {
				if _, err := database.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{database.graphName, label}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}
			leaves, err := database.hopCandidateIDs(t.Context(), graph, []model.ID{hub.ID}, []model.ID{hub.ID}, traversalLabel{label: "DERIVED_FROM"}, "in", 1)
			if err != nil || len(leaves) != 2 {
				t.Fatalf("주변부 표본=%v: %v", leaves, err)
			}
			recorder := &indexPlanQueryer{cypherQueryer: database.pool}
			snapshot, err := database.loadAuditSnapshot(t.Context(), recorder, graph, leaves[:1], EmbeddingExpectation{})
			if err != nil || len(snapshot.edges) != 1 || len(recorder.queries) != 5 {
				t.Fatalf("실제 주변부 조회=%d/%d: %v", len(snapshot.edges), len(recorder.queries), err)
			}
			for index, name := range []string{"outgoing", "incoming", "contexts"} {
				plan := explainHop(t, database, recorder.queries[index])
				logQueryIndexPlan(t, name, plan)
				if !planUsesIndex(plan.Root, "context_context_id_agtype_idx") {
					t.Errorf("%s: 실제 주변부 질의가 context_id 인덱스를 사용하지 않음", name)
				}
				// 작은 테이블의 순차 스캔은 비용 선택으로 허용하고 실제 스캔량을 기록한다.
				// 큰 표본에서도 전체 간선을 훑는 회귀는 별도로 거부한다.
				if plan.Root.scanned("Context") >= float64(size) || (size >= 10000 && plan.Root.scanned("DERIVED_FROM") >= float64(size)) {
					t.Errorf("%s: 주변부 밖 전체 스캔", name)
				}
			}
			dangling := explainHop(t, database, recorder.queries[4], recorder.arguments[4]...)
			logQueryIndexPlan(t, "dangling", dangling)
			if size >= 10000 && dangling.Root.scanned("DERIVED_FROM") >= float64(size) {
				t.Error("고아 간선 보충 조회가 주변부 밖 전체 간선을 스캔함")
			}
			query, err := database.hopCandidateSQL(graph, []model.ID{hub.ID}, []model.ID{hub.ID}, traversalLabel{label: "DERIVED_FROM"}, "in", 9)
			if err != nil {
				t.Fatal(err)
			}
			plan := explainHop(t, database, query)
			logQueryIndexPlan(t, "ordered_hop", plan)
			if plan.Root.Rows != 10 || !planUsesIndex(plan.Root, "context_active_graph_context_idx") || !planUsesIndex(plan.Root, "derived_from_hop_endpoints_idx") {
				t.Error("정렬된 후보의 결과·복합 인덱스 사용 불일치")
			}
			if plan.Root.scanned("DERIVED_FROM") >= float64(size) {
				t.Error("LIMIT 아래에서도 모든 간선을 스캔함")
			}
			var stats int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stats
				WHERE schemaname=$1 AND tablename='context_context_id_agtype_idx'`, database.graphName).Scan(&stats); err != nil || stats != 1 {
				t.Fatalf("agtype 표현식 통계=%d: %v", stats, err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func logQueryIndexPlan(t *testing.T, name string, plan hopPlan) {
	t.Helper()
	t.Logf("%s: time=%.3fms rows=%g ContextScans=%g EdgeScans=%g buffers=%d+%d temp=%d/%d", name,
		plan.Execution, plan.Root.Rows, plan.Root.scanned("Context"), plan.Root.scanned("DERIVED_FROM"),
		plan.Root.Hit, plan.Root.Read, plan.Root.TempRead, plan.Root.TempWritten)
	logHopPlan(t, name, plan.Root)
}

func TestQueryRelationalIndexPlansIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	for _, size := range []int{1000, 10000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			tx, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(t.Context()))
			// EXPLAIN ANALYZE가 실행하는 삭제도 이 시험 트랜잭션과 함께 되돌린다.
			seed := fmt.Sprintf(`CREATE TEMP TABLE data05_index_seed ON COMMIT DROP AS
				SELECT n, ('019b0000-0000-7000-8000-' || lpad(n::text,12,'0'))::uuid AS id,
				('019b0000-0000-7000-8000-' || lpad((n%%100+1)::text,12,'0'))::uuid AS graph,
				'2030-01-01'::timestamptz + n * interval '1 second' AS at
				FROM generate_series(1,%d) n;
			INSERT INTO public.account SELECT id,'data05_'||n,'synthetic',at FROM data05_index_seed;
			INSERT INTO public.context_graph (graph_id,name,created_by,created_at,last_activity_at)
				SELECT id,'synthetic',id,at,at FROM data05_index_seed;
			INSERT INTO public.graph_grant SELECT graph,'account',id,'viewer' FROM data05_index_seed;
			INSERT INTO public.team_member SELECT graph,id FROM data05_index_seed;
			INSERT INTO public.index_task (task_id,context_id,graph_id,state,enqueued_at,next_attempt_at)
				SELECT id,id,graph,'pending',at,at FROM data05_index_seed;
			INSERT INTO public.operation_log (operation_id,graph_id,operation_kind,context_id,relation_id,
				target_version,judgment_input,actor_account_id,actor_agent_id,applied_at,result)
				SELECT id,graph,'discard',CASE WHEN n%%2=0 THEN id END,CASE WHEN n%%2=1 THEN id END,
				CASE WHEN n%%2=0 THEN 1 END,'synthetic',id,id,at,'applied' FROM data05_index_seed;
			INSERT INTO public.web_audit_log (audit_id,target_kind,action,actor_account_id,occurred_at,graph_id)
				SELECT id,'team','add',id,at,CASE WHEN n%%2=1 THEN graph END FROM data05_index_seed;
			INSERT INTO public.revoked_token SELECT 'data05_'||n,at FROM data05_index_seed;`, size)
			if _, err := tx.Exec(t.Context(), seed, pgx.QueryExecModeSimpleProtocol); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"account", "context_graph", "graph_grant", "team_member", "index_task", "operation_log", "web_audit_log", "revoked_token"} {
				if _, err := tx.Exec(t.Context(), "ANALYZE "+pgx.Identifier{"public", table}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}
			for _, sample := range []struct{ name, sql, index string }{
				{"login", `SELECT account_id FROM public.account WHERE login_id='data05_2'`, "account_login_id_idx"},
				{"graphs", `SELECT graph_id FROM public.context_graph WHERE deleted_at IS NULL ORDER BY last_activity_at DESC,graph_id DESC LIMIT 10`, "context_graph_active_activity_idx"},
				{"grant", `SELECT graph_id FROM public.graph_grant WHERE subject_id='019b0000-0000-7000-8000-000000000002'`, "graph_grant_subject_idx"},
				{"membership", `SELECT team_id FROM public.team_member WHERE account_id='019b0000-0000-7000-8000-000000000002'`, "team_member_account_idx"},
				{"pending", `SELECT task_id FROM public.index_task WHERE state='pending' AND next_attempt_at<='2030-01-01T00:00:10Z' ORDER BY next_attempt_at LIMIT 10`, "index_task_state_next_attempt_idx"},
				{"context_log", `SELECT operation_id FROM public.operation_log WHERE context_id='019b0000-0000-7000-8000-000000000002' ORDER BY applied_at DESC`, "operation_log_context_applied_idx"},
				{"relation_log", `SELECT operation_id FROM public.operation_log WHERE relation_id='019b0000-0000-7000-8000-000000000001' ORDER BY applied_at DESC`, "operation_log_relation_idx"},
				{"graph_log", `SELECT operation_id FROM public.operation_log WHERE graph_id='019b0000-0000-7000-8000-000000000002' ORDER BY applied_at DESC`, "operation_log_graph_applied_idx"},
				{"web_log", `SELECT audit_id FROM public.web_audit_log WHERE graph_id='019b0000-0000-7000-8000-000000000002' ORDER BY occurred_at DESC`, "web_audit_log_graph_occurred_idx"},
				{"team_log", `SELECT audit_id FROM public.web_audit_log WHERE graph_id IS NULL AND actor_account_id='019b0000-0000-7000-8000-000000000002' ORDER BY occurred_at`, "web_audit_log_team_actor_idx"},
				{"expired_tokens", `DELETE FROM public.revoked_token WHERE expires_at<='2030-01-01T00:00:10Z'`, "revoked_token_expires_idx"},
			} {
				t.Run(sample.name, func(t *testing.T) {
					var raw []byte
					if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+sample.sql).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var plans []hopPlan
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatalf("실행 계획 해석: %v", err)
					}
					logQueryIndexPlan(t, sample.name, plans[0])
					if !planUsesIndex(plans[0].Root, sample.index) {
						t.Errorf("선택적 조회에 %s가 사용되지 않음", sample.index)
					}
				})
			}
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestQueryUniqueIndexRacesIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	for _, name := range []string{"login", "source", "active_key", "index_task"} {
		t.Run(name, func(t *testing.T) {
			id, other, graph := newTestID(t), newTestID(t), newTestID(t)
			var firstSQL, secondSQL, constraint string
			switch name {
			case "login":
				login := "data05_" + strings.ReplaceAll(id.String()[14:], "-", "")
				firstSQL = fmt.Sprintf("INSERT INTO public.account VALUES ('%s','%s','synthetic',now())", id, login)
				secondSQL = fmt.Sprintf("INSERT INTO public.account VALUES ('%s','%s','synthetic',now())", other, login)
				constraint = "account_login_id_idx"
			case "source":
				actor := newTestID(t)
				makeSQL := func(contextID model.ID) string {
					value := testSourceContext(t, graph, actor, "api://unique/"+id.String())
					value.ID = contextID
					properties, err := encodeProperties(contextProperties(value))
					if err != nil {
						t.Fatal(err)
					}
					return database.cypherSQL("CREATE (n:Context "+properties+") RETURN n.context_id", "id agtype")
				}
				firstSQL, secondSQL = makeSQL(id), makeSQL(other)
				constraint = "context_source_ref_idx"
			case "active_key":
				firstSQL = fmt.Sprintf("INSERT INTO public.signing_key VALUES ('%s','synthetic','public','private','active',now())", id)
				secondSQL = fmt.Sprintf("INSERT INTO public.signing_key VALUES ('%s','synthetic','public','private','active',now())", other)
				constraint = "signing_key_active_idx"
				t.Cleanup(func() {
					if _, err := database.pool.Exec(context.WithoutCancel(t.Context()), "DELETE FROM public.signing_key WHERE key_id=$1", id.String()); err != nil {
						t.Error(err)
					}
				})
			case "index_task":
				contextID := newTestID(t)
				firstSQL = fmt.Sprintf("INSERT INTO public.index_task (task_id,context_id,graph_id,state,enqueued_at,next_attempt_at) VALUES ('%s','%s','%s','pending',now(),now())", id, contextID, graph)
				secondSQL = fmt.Sprintf("INSERT INTO public.index_task (task_id,context_id,graph_id,state,enqueued_at,next_attempt_at) VALUES ('%s','%s','%s','pending',now(),now())", other, contextID, graph)
				constraint = "index_task_context_idx"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(ctx))
			if _, err := tx.Exec(ctx, firstSQL, pgx.QueryExecModeExec); err != nil {
				t.Fatal(err)
			}
			holderPID := tx.Conn().PgConn().PID()
			result := make(chan error, 1)
			var worker sync.WaitGroup
			worker.Go(func() {
				_, err := database.pool.Exec(ctx, secondSQL, pgx.QueryExecModeExec)
				result <- err
			})
			defer func() { tx.Rollback(context.WithoutCancel(ctx)); cancel(); worker.Wait() }()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err := database.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE datname=current_database() AND $1::integer=ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("실제 DB 경합 전에 두 번째 쓰기 종료: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-result
			if failure, ok := errors.AsType[*pgconn.PgError](err); !ok || failure.Code != "23505" || failure.ConstraintName != constraint {
				t.Fatalf("중복 쓰기=%v, want unique_violation/%s", err, constraint)
			}
			worker.Wait()
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func (q *indexPlanQueryer) QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row {
	q.queries = append(q.queries, sql)
	q.arguments = append(q.arguments, arguments)
	return q.cypherQueryer.QueryRow(ctx, sql, arguments...)
}

func TestQuerySourceReferenceIndexPlanIntegration(t *testing.T) {
	for _, size := range []int{1000, 10000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			database := newIsolatedIntegrationStore(t)
			actor := newTestID(t)
			createTestAccount(t, database, actor)
			graph := createTestGraph(t, database, actor)
			other := createTestGraph(t, database, actor)
			locator := "api://source-index/O'Brien?tag=$store0$"
			source, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, locator), nil)
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := database.CreateContext(t.Context(), other, testSourceContext(t, other, actor, locator), nil)
			if err != nil || foreign.ID == source.ID {
				t.Fatalf("foreign=%+v,%v", foreign, err)
			}
			// 대상 그래프의 1,000개 원천은 고정하고 다른 그래프의 원천만 늘린다.
			for _, seed := range []struct {
				graph         model.ID
				count, offset int
			}{{graph, 1000, 0}, {other, size, 100000}} {
				properties, err := json.Marshal(contextProperties(testSourceContext(t, seed.graph, actor, "api://source-index/noise")))
				if err != nil {
					t.Fatal(err)
				}
				_, err = database.pool.Exec(t.Context(), `INSERT INTO `+database.contextTable()+` (properties)
					SELECT ($1::jsonb || jsonb_build_object(
					  'context_id','019b0189-0000-7000-8000-' || lpad((n+$3::int)::text,12,'0'),
					  'source_ref_locator','api://source-index/noise/' || n))::text::ag_catalog.agtype
					FROM generate_series(1,$2::int) n`, string(properties), seed.count, seed.offset)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.pool.Exec(t.Context(), "ANALYZE "+database.contextTable()); err != nil {
				t.Fatal(err)
			}
			for _, scenario := range []string{"active", "missing", "discarded"} {
				t.Run(scenario, func(t *testing.T) {
					lookup := locator
					if scenario == "missing" {
						lookup += "-missing"
					}
					if scenario == "discarded" {
						if _, err := database.DiscardContext(t.Context(), graph, source.ID, nil, WriteLimits{}); err != nil {
							t.Fatal(err)
						}
					}
					recorder := &indexPlanQueryer{cypherQueryer: database.pool}
					got, err := database.findSourceWith(t.Context(), recorder, graph, lookup)
					if scenario == "missing" {
						if !errors.Is(err, ErrNotFound) {
							t.Fatalf("missing=%v", err)
						}
					} else if err != nil || got.ID != source.ID || got.Source.Reference.Locator != locator || got.Body != source.Body || (got.DeletedAt != nil) != (scenario == "discarded") {
						t.Fatalf("source=%+v,%v", got, err)
					}
					if len(recorder.queries) != 1 {
						t.Fatalf("queries=%d", len(recorder.queries))
					}
					plan := explainHop(t, database, recorder.queries[0], recorder.arguments[0]...)
					logQueryIndexPlan(t, "source_ref_"+scenario, plan)
					if !planUsesIndex(plan.Root, "context_source_ref_idx") || plan.Root.scanned("Context") > 1 {
						t.Error("source_ref index missing or unrelated contexts scanned")
					}
				})
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
