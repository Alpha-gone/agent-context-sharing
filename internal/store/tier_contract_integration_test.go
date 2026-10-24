package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newTierFixture(t *testing.T, count int) (*Store, model.ID, []model.Context) {
	t.Helper()
	database := newEmbeddingContractStore(t, "vector", 8)
	configureTierPool(t, database, nil)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	nodes := make([]model.Context, 0, count)
	for range count {
		node, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://data08/"+newTestID(t).String()), nil)
		if err != nil {
			t.Fatal(err)
		}
		readyIndexTasks(t, database, graph, node.ID)
		result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
			return IndexTaskResult{Embedding: []float64{1, 0, 0, 0, 0, 0, 0, 0}, ModelID: "data08"}
		})
		if err != nil || !result.Succeeded {
			t.Fatalf("계층 시험 색인=%+v: %v", result, err)
		}
		nodes = append(nodes, node)
	}
	return database, graph, nodes
}

func configureTierPool(t *testing.T, database *Store, tracer pgx.QueryTracer) {
	t.Helper()
	cfg := database.pool.Config()
	database.pool.Close()
	cfg.MaxConns = 4
	cfg.ConnConfig.Tracer = tracer
	cfg.ConnConfig.RuntimeParams["application_name"] = "data08-access"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	database.pool = pool
	database.reservations, err = newReservations(4)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTierPolicyBoundaryIntegration(t *testing.T) {
	database, graph, nodes := newTierFixture(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.AddDate(0, 0, -30)
	for i, at := range []time.Time{cutoff.Add(-time.Microsecond), cutoff, cutoff.Add(time.Microsecond)} {
		if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET last_accessed_at=$2 WHERE context_id=$1`, nodes[i].ID.String(), at); err != nil {
			t.Fatal(err)
		}
	}
	// 정상 조회는 접근 시각을 바꾸므로 AGE 원문과 그래프 행을 직접 지문화한다.
	fingerprint := func() string {
		var value string
		if err := database.pool.QueryRow(t.Context(), `SELECT md5(row_to_json(g)::text ||
			(SELECT string_agg(properties::text,'' ORDER BY id) FROM `+database.contextTable()+`))
			FROM public.context_graph g WHERE graph_id=$1`, graph.String()).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := fingerprint()
	moved, err := database.MoveColdEmbeddings(t.Context(), now, func(model.ID) int { return 30 })
	if err != nil || moved != 1 {
		t.Fatalf("기간 초과 경계 이동=%d: %v", moved, err)
	}
	for i, node := range nodes {
		want := embeddingTierHot
		if i == 0 {
			want = embeddingTierCold
		}
		if got := embeddingTier(t, database, node.ID); got != want {
			t.Fatalf("경계 %d 계층=%s, want %s", i, got, want)
		}
	}
	if fingerprint() != before {
		t.Fatal("계층 이동이 AGE 정점 또는 그래프 행을 변경했다")
	}
	// cold 벡터는 승격하기 전 부모 테이블을 통한 의미 검색에 포함된다.
	candidates, err := database.SemanticCandidates(t.Context(), graph, "data08", []float64{1, 0, 0, 0, 0, 0, 0, 0}, now, 10)
	if err != nil || len(candidates) != 3 || embeddingTier(t, database, nodes[0].ID) != embeddingTierHot {
		t.Fatalf("cold 직접 검색·되읽기=%d: %v", len(candidates), err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestTierDeferredAccessSkipsDiscardedIntegration(t *testing.T) {
	for _, graphDiscard := range []bool{false, true} {
		t.Run(fmt.Sprintf("graph=%t", graphDiscard), func(t *testing.T) {
			database, graph, nodes := newTierFixture(t, 1)
			node := nodes[0]
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET storage_tier='cold' WHERE graph_id=$1`, graph.String()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := database.BeginReadSnapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = snapshot.Close(t.Context()) })
			readCtx, release, err := database.enterReadScope(snapshot.Context(t.Context()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = release() })
			if _, err := database.Context(readCtx, graph, node.ID); err != nil {
				t.Fatal(err)
			}
			if graphDiscard {
				_, err = database.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at=now() WHERE graph_id=$1`, graph.String())
			} else {
				// 다른 서버 인스턴스의 폐기처럼 읽기 요청의 예약과 독립된 저장소로 커밋한다.
				writer, openErr := New(t.Context(), database.pool.Config().ConnString(), "agent_context", nil, nil, "")
				if openErr != nil {
					t.Fatal(openErr)
				}
				t.Cleanup(writer.Close)
				_, err = writer.DiscardContext(t.Context(), graph, node.ID, nil, WriteLimits{})
			}
			if err != nil {
				t.Fatal(err)
			}
			before := embeddingStateFingerprint(t, database, graph)
			if err := release(); err != nil {
				t.Fatal(err)
			}
			if embeddingStateFingerprint(t, database, graph) != before {
				t.Fatal("읽기 뒤 폐기된 대상의 임베딩을 승격하거나 접근 시각을 바꿨다")
			}
			if !graphDiscard {
				if _, err := database.Context(t.Context(), graph, node.ID); err != nil || embeddingTier(t, database, node.ID) != embeddingTierCold {
					t.Fatalf("폐기 복구 조회=%v", err)
				}
			}
			if _, err := snapshot.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertConnectionRecovery(t, database, snapshot.scope, 0)
		})
	}
}

func TestTierLockedRowsSkipIntegration(t *testing.T) {
	for _, kind := range []string{"inactive", "context", "graph", "touch"} {
		t.Run(kind, func(t *testing.T) {
			database, graph, nodes := newTierFixture(t, 2)
			now := time.Now().UTC()
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET last_accessed_at=$2 WHERE graph_id=$1`, graph.String(), now.AddDate(0, 0, -31)); err != nil {
				t.Fatal(err)
			}
			if kind == "context" {
				for _, node := range nodes {
					if _, err := database.DiscardContext(t.Context(), graph, node.ID, nil, WriteLimits{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "graph" {
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at=now() WHERE graph_id=$1`, graph.String()); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "touch" {
				if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET storage_tier='cold' WHERE graph_id=$1`, graph.String()); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := database.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), tx)
			if _, err := tx.Exec(t.Context(), `SELECT context_id FROM public.context_embedding WHERE context_id=$1 FOR UPDATE`, nodes[0].ID.String()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if kind == "touch" {
				err = database.touchEmbeddings(ctx, graph, []model.ID{nodes[1].ID, nodes[0].ID}, now)
			} else {
				var moved int
				moved, err = database.MoveColdEmbeddings(ctx, now, func(model.ID) int { return 30 })
				if moved != 1 {
					t.Errorf("잠긴 행을 제외한 이동=%d", moved)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			want0, want1 := embeddingTierHot, embeddingTierCold
			if kind == "touch" {
				want0, want1 = want1, want0
			}
			if embeddingTier(t, database, nodes[0].ID) != want0 || embeddingTier(t, database, nodes[1].ID) != want1 {
				t.Fatal("잠긴 행 건너뜀 또는 나머지 행 이동이 올바르지 않다")
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if kind == "touch" {
				err = database.touchEmbeddings(ctx, graph, []model.ID{nodes[0].ID}, now)
			} else {
				_, err = database.MoveColdEmbeddings(ctx, now, func(model.ID) int { return 30 })
			}
			if err != nil || embeddingTier(t, database, nodes[0].ID) != want1 {
				t.Fatalf("잠금 해제 뒤 재처리=%v", err)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

type tierAccessTracer struct {
	mu   sync.Mutex
	args [][]any
	errs []error
}

func (tracer *tierAccessTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	access := strings.Contains(data.SQL, "SET last_accessed_at = $3")
	if access {
		tracer.mu.Lock()
		tracer.args = append(tracer.args, slices.Clone(data.Args))
		tracer.mu.Unlock()
	}
	return context.WithValue(ctx, tierTraceKey{}, access)
}

type tierTraceKey struct{}

func (tracer *tierAccessTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if access, _ := ctx.Value(tierTraceKey{}).(bool); access {
		tracer.mu.Lock()
		tracer.errs = append(tracer.errs, data.Err)
		tracer.mu.Unlock()
	}
}

// 문장 스냅숏을 얻은 접근 기록을 서버의 BEFORE STATEMENT 트리거에서 멈추고,
// 다른 연결에서 파티션 이동을 커밋한 뒤 재개해 실제 PostgreSQL 40001을 만든다.
func TestTierConcurrentPartitionRetryIntegration(t *testing.T) {
	database, graph, nodes := newTierFixture(t, 1)
	tracer := new(tierAccessTracer)
	configureTierPool(t, database, tracer)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cfg := database.pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "data08-mover"
	other, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.WithoutCancel(ctx))
	if _, err := other.Exec(ctx, `CREATE FUNCTION public.tier_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF current_setting('application_name') = 'data08-access' THEN
		    PERFORM pg_advisory_xact_lock(8008);
		  END IF;
		  RETURN NULL;
		END $$;
		CREATE TRIGGER tier_barrier BEFORE UPDATE ON public.context_embedding FOR EACH STATEMENT EXECUTE FUNCTION public.tier_barrier();
		SELECT pg_advisory_lock(8008)`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	done := make(chan error, 1)
	go func() { done <- database.touchEmbeddings(ctx, graph, []model.ID{nodes[0].ID}, at) }()
	for {
		var waiting bool
		if err := other.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name='data08-access' AND wait_event='advisory')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("서버 경합 장벽 전에 접근 기록이 끝났다: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err := other.Exec(ctx, `UPDATE public.context_embedding SET storage_tier='cold' WHERE context_id=$1`, nodes[0].ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock(8008)`); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	if len(tracer.args) != 2 || len(tracer.errs) != 2 || !reflect.DeepEqual(tracer.args[0], tracer.args[1]) {
		t.Fatalf("실제 이동 재시도 인자/횟수=%d/%d", len(tracer.args), len(tracer.errs))
	}
	failure, ok := errors.AsType[*pgconn.PgError](tracer.errs[0])
	if !ok || failure.Code != "40001" || tracer.errs[1] != nil {
		t.Fatalf("실제 이동 오류=%v", tracer.errs)
	}
	var accessed time.Time
	var count int
	if err := database.pool.QueryRow(ctx, `SELECT count(*), max(last_accessed_at) FROM public.context_embedding WHERE context_id=$1 AND storage_tier='hot'`, nodes[0].ID.String()).Scan(&count, &accessed); err != nil || count != 1 || !accessed.Equal(at) {
		t.Fatalf("재시도 뒤 유일성·시각=%d/%v: %v", count, accessed, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestTierAccessFailureContractIntegration(t *testing.T) {
	for _, test := range []struct {
		name, first, second, want string
		attempts                  int
	}{
		{"second_conflict", "40001", "40001", "40001", 2},
		{"second_io_error", "40001", "58030", "58030", 2},
		{"deadlock", "40P01", "40P01", "40P01", 1},
		{"io_error", "58030", "58030", "58030", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, graph, nodes := newTierFixture(t, 1)
			if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET storage_tier='cold' WHERE graph_id=$1`, graph.String()); err != nil {
				t.Fatal(err)
			}
			// sequence는 실패 문장이 롤백되어도 횟수를 남긴다. IO 오류는 합성 주입이다.
			if _, err := database.pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE SEQUENCE public.tier_attempt;
				CREATE FUNCTION public.tier_failure() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
				  IF nextval('public.tier_attempt') = 1 THEN
				    RAISE EXCEPTION 'synthetic tier failure' USING ERRCODE = '%s';
				  END IF;
				  RAISE EXCEPTION 'synthetic tier failure' USING ERRCODE = '%s';
				END $$;
				CREATE TRIGGER tier_failure BEFORE UPDATE ON public.context_embedding
				FOR EACH ROW EXECUTE FUNCTION public.tier_failure();`, test.first, test.second)); err != nil {
				t.Fatal(err)
			}
			before := embeddingStateFingerprint(t, database, graph)
			result, err := database.Context(t.Context(), graph, nodes[0].ID)
			failure, ok := errors.AsType[*pgconn.PgError](err)
			var attempts int
			if err := database.pool.QueryRow(t.Context(), `SELECT last_value FROM public.tier_attempt`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if !ok || failure.Code != test.want || attempts != test.attempts || result.ID != (model.ID{}) {
				t.Fatalf("실패 결과=%s/%v, 시도=%d", result.ID, err, attempts)
			}
			if embeddingStateFingerprint(t, database, graph) != before {
				t.Fatal("접근 기록 실패가 임베딩·내용 판을 변경했다")
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestTierConcurrentMovementSmallPoolIntegration(t *testing.T) {
	database, graph, nodes := newTierFixture(t, 12)
	ids := make([]model.ID, len(nodes))
	for i, node := range nodes {
		ids[i] = node.ID
	}
	now := time.Now().UTC()
	// 접근 기록과 이동의 대상이 계속 겹치도록 두 시각을 기준보다 과거로 고정한다.
	old := now.AddDate(0, 0, -31)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET last_accessed_at=$2 WHERE graph_id=$1`, graph.String(), old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var group sync.WaitGroup
	var moveConflicts, accessConflicts atomic.Int64
	errs := make(chan error, 8)
	for worker := range 8 {
		group.Go(func() {
			for range 25 {
				var err error
				if worker%2 == 0 {
					_, err = database.MoveColdEmbeddings(ctx, now, func(model.ID) int { return 30 })
					// 주기 이동의 충돌은 호출자에게 반환한다. 접근 기록만 한 번 재시도하며,
					// 다음 주기 회차에서도 동일 데이터가 정상적으로 처리되는지 계속 확인한다.
					if failure, ok := errors.AsType[*pgconn.PgError](err); ok && failure.Code == "40001" {
						moveConflicts.Add(1)
						continue
					}
				} else {
					err = database.touchEmbeddings(ctx, graph, ids, old)
					// 한 번 재시도한 뒤에도 이동이 경합하면 정본대로 40001을 반환한다.
					if failure, ok := errors.AsType[*pgconn.PgError](err); ok && failure.Code == "40001" {
						accessConflicts.Add(1)
						continue
					}
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("온라인 이동·접근 기록 경합: %v", err)
	}
	t.Logf("40001 반환: 주기 이동=%d, 접근 기록의 한 번 재시도 소진=%d", moveConflicts.Load(), accessConflicts.Load())
	var count, unique int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*), count(DISTINCT context_id) FROM public.context_embedding WHERE graph_id=$1`, graph.String()).Scan(&count, &unique); err != nil || count != len(nodes) || unique != len(nodes) {
		t.Fatalf("경합 뒤 유실·중복=%d/%d: %v", count, unique, err)
	}
	if database.pool.Stat().MaxConns() != 4 || database.pool.Stat().TotalConns() > 4 {
		t.Fatal("최소 풀의 실제 연결 상한을 넘었다")
	}
	if err := database.touchEmbeddings(t.Context(), graph, ids, now); err != nil {
		t.Fatalf("경합 종료 뒤 접근 기록 회복: %v", err)
	}
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.context_embedding WHERE graph_id=$1 AND storage_tier='hot' AND last_accessed_at=$2`, graph.String(), now).Scan(&count); err != nil || count != len(nodes) {
		t.Fatalf("경합 종료 뒤 전체 승격·접근 시각=%d: %v", count, err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestTierColdReadFailureIntegration(t *testing.T) {
	database, graph, nodes := newTierFixture(t, 1)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_embedding SET storage_tier='cold' WHERE graph_id=$1`, graph.String()); err != nil {
		t.Fatal(err)
	}
	// 실제 디스크를 훼손하지 않고 cold 행을 읽는 DB 질의에서 IO 오류를 주입한다.
	if _, err := database.pool.Exec(t.Context(), `
		CREATE FUNCTION public.tier_read_failure() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
		BEGIN RAISE EXCEPTION 'synthetic cold IO failure' USING ERRCODE='58030'; END $$;
		ALTER TABLE public.context_embedding RENAME TO context_embedding_saved;
		CREATE VIEW public.context_embedding AS
		SELECT * FROM public.context_embedding_saved WHERE storage_tier <> 'cold' OR public.tier_read_failure()`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.BeginReadSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = snapshot.Close(t.Context()) })
	candidates, err := database.SemanticCandidates(snapshot.Context(t.Context()), graph, "data08", []float64{1, 0, 0, 0, 0, 0, 0, 0}, time.Now().UTC(), 10)
	failure, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || failure.Code != "58030" || candidates != nil {
		t.Fatalf("cold 읽기 장애 결과=%v: %v", candidates, err)
	}
	if _, err := snapshot.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.context_embedding_saved WHERE context_id=$1 AND storage_tier='cold'`, nodes[0].ID.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("읽기 장애 뒤 cold 행 보존=%d: %v", count, err)
	}
	assertConnectionRecovery(t, database, snapshot.scope, 0)
}
