package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"github.com/jackc/pgx/v5"
)

func TestEntryChannelArgumentsBeforeIO(t *testing.T) {
	database := &Store{}
	graph := newTestID(t)
	for _, at := range []time.Time{time.Time{}, time.Now().In(time.FixedZone("KST", 9*3600)), time.Now().In(time.FixedZone("zero", 0))} {
		for name, read := range map[string]func() ([]SearchCandidate, error){
			"semantic": func() ([]SearchCandidate, error) {
				return database.SemanticCandidates(t.Context(), graph, "model", []float64{1}, at, 1)
			},
			"keyword": func() ([]SearchCandidate, error) {
				return database.KeywordCandidates(t.Context(), graph, "query", at, 1)
			},
			"time":   func() ([]SearchCandidate, error) { return database.TimeCandidates(t.Context(), graph, at, 1) },
			"global": func() ([]SearchCandidate, error) { return database.GlobalSummaryCandidates(t.Context(), graph, at, 1) },
		} {
			if result, err := read(); err == nil || result != nil {
				t.Fatalf("%s가 잘못된 시각을 허용했다: %v", name, err)
			}
		}
	}
}

type entryQueryTracer struct {
	mu   sync.Mutex
	sql  string
	args []any
}

func (tracer *entryQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT candidate.context_id::text") {
		tracer.mu.Lock()
		tracer.sql, tracer.args = data.SQL, slices.Clone(data.Args)
		tracer.mu.Unlock()
	}
	return ctx
}
func (*entryQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type recordingEntryStore struct {
	*Store
	mu    sync.Mutex
	lists map[string][]model.ID
}

func (database *recordingEntryStore) record(name string, values []SearchCandidate) {
	database.mu.Lock()
	defer database.mu.Unlock()
	for _, value := range values {
		database.lists[name] = append(database.lists[name], value.Context.ID)
	}
}
func (database *recordingEntryStore) SemanticCandidates(ctx context.Context, graph model.ID, modelID string, vector []float64, at time.Time, limit int) ([]SearchCandidate, error) {
	values, err := database.Store.SemanticCandidates(ctx, graph, modelID, vector, at, limit)
	database.record("semantic", values)
	return values, err
}
func (database *recordingEntryStore) KeywordCandidates(ctx context.Context, graph model.ID, text string, at time.Time, limit int) ([]SearchCandidate, error) {
	values, err := database.Store.KeywordCandidates(ctx, graph, text, at, limit)
	database.record("keyword", values)
	return values, err
}
func (database *recordingEntryStore) TimeCandidates(ctx context.Context, graph model.ID, at time.Time, limit int) ([]SearchCandidate, error) {
	values, err := database.Store.TimeCandidates(ctx, graph, at, limit)
	database.record("time", values)
	return values, err
}

type entryEmbedder struct{ vector []float64 }

func (provider entryEmbedder) Embed(context.Context, string) ([]float64, error) {
	return slices.Clone(provider.vector), nil
}
func (entryEmbedder) ModelID() string { return "search02" }

func TestEntryChannelCandidatesIntegration(t *testing.T) {
	for _, vectorType := range []string{"vector", "halfvec"} {
		t.Run(vectorType, func(t *testing.T) {
			database := newEmbeddingContractStore(t, vectorType, 8)
			tracer := &entryQueryTracer{}
			configureTierPool(t, database, tracer)
			actor := newTestID(t)
			createTestAccount(t, database, actor)
			graph := createTestGraph(t, database, actor)
			grantAccount(t, database, graph, actor, model.GraphGradeOwner)
			at := time.Now().UTC().Truncate(time.Microsecond)
			nodes := map[string]model.Context{}
			vectors := map[model.ID][]float64{}
			sequence := 0
			storeNode := func(name string, node model.Context, refs []model.ID, x, y float64) model.Context {
				t.Helper()
				sequence++
				var err error
				node.ID, err = model.ParseID(fmt.Sprintf("019a0000-0000-7000-8000-%012d", sequence))
				if err != nil {
					t.Fatal(err)
				}
				node.RecordedAt = at
				if name == "gamma" {
					node.RecordedAt = at.Add(time.Minute)
				}
				value, err := database.CreateContext(t.Context(), node.GraphID, node, refs)
				if err != nil {
					t.Fatal(err)
				}
				nodes[name] = value
				vectors[value.ID] = []float64{x, y, 0, 0, 0, 0, 0, 0}
				return value
			}
			source := func(name, body string, x, y float64) model.Context {
				value := testSourceContext(t, graph, actor, "api://search02/"+name+"/"+graph.String())
				value.Body = body
				return storeNode(name, value, nil, x, y)
			}
			first := source("phrase", "alpha beta", 1, 0)
			source("gapped", "alpha gap beta", 0.8, 0.6)
			source("gamma", "gamma", 0, 1)
			event := testEventContext(t, graph, actor, first.ID)
			event.Body = "alpha beta"
			storeNode("event", event, nil, 0.6, 0.8)
			for _, name := range []string{"expired", "future", "boundary", "global"} {
				value := testDerivedContext(t, graph, actor)
				value.Body = "alpha beta"
				switch name {
				case "expired":
					value.Derived.ValidFrom, value.Derived.ValidTo = new(at.Add(-2*time.Hour)), new(at.Add(-time.Minute))
				case "future":
					value.Derived.ValidFrom, value.Derived.ValidTo = new(at.Add(time.Hour)), new(at.Add(2*time.Hour))
				case "boundary":
					value.Derived.ValidTo = new(at)
				case "global":
					value.Derived.Kind, value.Derived.SummaryScope = model.DerivationKindSummary, model.SummaryScopeGlobal
				}
				storeNode(name, value, []model.ID{first.ID}, 0, 1)
			}
			source("old_model", "alpha beta", 1, 0)
			deleted := source("deleted", "alpha beta", 1, 0)
			foreign := testSourceContext(t, createTestGraph(t, database, actor), actor, "api://search02/foreign/"+graph.String())
			foreign.Body = "alpha beta"
			storeNode("foreign", foreign, nil, 1, 0)
			for _, graphID := range []model.ID{graph, foreign.GraphID} {
				for {
					result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, func(_ context.Context, task IndexTask) IndexTaskResult {
						modelID := "search02"
						if task.ContextID == nodes["old_model"].ID {
							modelID = "old-model"
						}
						return IndexTaskResult{Embedding: vectors[task.ContextID], ModelID: modelID}
					})
					if err != nil {
						t.Fatal(err)
					}
					if !result.Found {
						break
					}
					if !result.Succeeded {
						t.Fatalf("색인 실패: %+v", result)
					}
				}
			}
			if _, err := database.SetContextDeleted(t.Context(), graph, deleted.ID, actor, true, 0); err != nil {
				t.Fatal(err)
			}
			ids := func(names ...string) []model.ID {
				result := make([]model.ID, 0, len(names))
				for _, name := range names {
					result = append(result, nodes[name].ID)
				}
				return result
			}
			check := func(t *testing.T, values []SearchCandidate, err error, want []model.ID) {
				t.Helper()
				got := make([]model.ID, 0, len(values))
				for _, value := range values {
					if value.Context.GraphID != graph || value.Context.DeletedAt != nil {
						t.Fatal("그래프·활성 경계 위반")
					}
					got = append(got, value.Context.ID)
				}
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("후보 순서: got=%v want=%v err=%v", got, want, err)
				}
			}
			t.Run("scope_order_limits", func(t *testing.T) {
				for _, limit := range []int{1, 3, 20} {
					want := ids("phrase", "gapped", "event", "gamma", "future", "boundary")
					values, err := database.SemanticCandidates(t.Context(), graph, "search02", vectors[first.ID], at, limit)
					check(t, values, err, want[:min(limit, len(want))])
					want = ids("phrase", "gapped", "event", "future", "boundary", "old_model")
					values, err = database.KeywordCandidates(t.Context(), graph, "alpha missing", at, limit)
					check(t, values, err, want[:min(limit, len(want))])
					want = ids("gamma", "phrase", "gapped", "event", "boundary", "old_model")
					values, err = database.TimeCandidates(t.Context(), graph, at, limit)
					check(t, values, err, want[:min(limit, len(want))])
				}
				values, err := database.TimeCandidates(t.Context(), graph, at.Add(-90*time.Second), 20)
				if err != nil || !slices.ContainsFunc(values, func(value SearchCandidate) bool { return value.Context.ID == nodes["expired"].ID }) {
					t.Fatalf("과거 유효 시각: %v", err)
				}
			})
			t.Run("keyword_syntax", func(t *testing.T) {
				for text, want := range map[string][]model.ID{
					`"alpha beta" missing`: ids("phrase", "event", "future", "boundary", "old_model"),
					`-"alpha beta"`:        ids("gapped", "gamma"),
					" ":                    {},
					"doesnotexist":         {},
				} {
					values, err := database.KeywordCandidates(t.Context(), graph, text, at, 20)
					check(t, values, err, want)
				}
			})
			t.Run("execution_equivalence", func(t *testing.T) {
				var baseline model.ContextFlow
				var baselineLists map[string][]model.ID
				for _, execution := range []search.Execution{search.ExecutionSequential, search.ExecutionParallel} {
					recording := &recordingEntryStore{Store: database, lists: map[string][]model.ID{}}
					service, err := search.New(recording, entryEmbedder{vectors[first.ID]}, search.Config{Execution: execution, CandidateLimit: 3, FoldThreshold: 0.9, Consistency: search.ConsistencySnapshot}, nil)
					if err != nil {
						t.Fatal(err)
					}
					flow, err := service.Flow(t.Context(), model.FlowInput{GraphID: graph, WorkContext: "alpha", Scope: "local", AsOf: at})
					if err != nil {
						t.Fatal(err)
					}
					flow.Read = model.ReadStats{}
					for channel, metric := range flow.Channels {
						metric.Latency = 0
						flow.Channels[channel] = metric
					}
					if execution == search.ExecutionSequential {
						baseline, baselineLists = flow, recording.lists
					} else if !reflect.DeepEqual(baseline, flow) || !reflect.DeepEqual(baselineLists, recording.lists) {
						t.Fatal("병렬·순차 채널 목록·결합·부분 상태가 다르다")
					}
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
			t.Run("hnsw_scope_and_failure", func(t *testing.T) {
				before := func() string {
					t.Helper()
					var value string
					if err := database.pool.QueryRow(t.Context(), `SELECT current_setting('hnsw.iterative_scan')`).Scan(&value); err != nil {
						t.Fatal(err)
					}
					return value
				}()
				_, err := database.WithReadSnapshot(t.Context(), func(ctx context.Context) error {
					read, finish, err := database.enterReadScope(ctx)
					if err != nil {
						return err
					}
					defer finish()
					_, err = database.SemanticCandidates(read, graph, "search02", vectors[first.ID], at, 3)
					if err != nil {
						return err
					}
					var setting string
					if err = database.reader(read).QueryRow(read, `SELECT current_setting('hnsw.iterative_scan')`).Scan(&setting); err != nil {
						return err
					}
					if setting != "strict_order" {
						t.Errorf("반복 탐색=%s", setting)
					}
					tracer.mu.Lock()
					query, args := tracer.sql, slices.Clone(tracer.args)
					tracer.mu.Unlock()
					if _, err := read.Value(readTxKey{}).(pgx.Tx).Exec(read, `SET LOCAL enable_seqscan=off; SET LOCAL enable_sort=off`); err != nil {
						return err
					}
					rows, err := database.reader(read).Query(read, "EXPLAIN "+query, args...)
					if err != nil {
						return err
					}
					var plan strings.Builder
					for rows.Next() {
						var line string
						if err := rows.Scan(&line); err != nil {
							rows.Close()
							return err
						}
						plan.WriteString(line)
					}
					err = rows.Err()
					rows.Close()
					if err != nil {
						return err
					}
					indexNames := []string{"context_embedding_cold_embedding_idx", "context_embedding_hot_embedding_idx"}
					var hnswIndexes int
					if err := database.reader(read).QueryRow(read, `SELECT count(*) FROM pg_class i JOIN pg_am a ON a.oid=i.relam WHERE i.relnamespace='public'::regnamespace AND i.relname=ANY($1::text[]) AND a.amname='hnsw'`, indexNames).Scan(&hnswIndexes); err != nil {
						return err
					}
					if hnswIndexes != len(indexNames) {
						t.Fatalf("HNSW 카탈로그 불일치: %d", hnswIndexes)
					}
					for _, name := range indexNames {
						if !strings.Contains(plan.String(), name) {
							t.Errorf("HNSW 이용 가능 계획이 없다: %s", plan.String())
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, vector := range [][]float64{vectors[first.ID], {1}} {
					values, err := database.SemanticCandidates(t.Context(), graph, "search02", vector, at, 3)
					if len(vector) == 1 && (err == nil || values != nil) || len(vector) > 1 && err != nil {
						t.Fatalf("의미 질의 경계: %v", err)
					}
				}
				connections := database.pool.AcquireAllIdle(t.Context())
				if len(connections) == 0 {
					t.Fatal("설정을 확인할 풀 연결이 없다")
				}
				for _, conn := range connections {
					defer conn.Release()
				}
				for _, conn := range connections {
					var setting string
					err = conn.QueryRow(t.Context(), `SELECT current_setting('hnsw.iterative_scan')`).Scan(&setting)
					conn.Release()
					if err != nil || setting != before {
						t.Fatalf("풀 설정 잔류=%q want=%q: %v", setting, before, err)
					}
				}
				assertConnectionRecovery(t, database, nil, 0)
			})
			for _, scoped := range []bool{false, true} {
				t.Run(fmt.Sprintf("semantic_cancel/snapshot=%t", scoped), func(t *testing.T) {
					ctx, end := context.WithTimeout(t.Context(), 10*time.Second)
					defer end()
					blocker, err := database.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer rollbackTransaction(t.Context(), blocker)
					if _, err := blocker.Exec(ctx, `LOCK TABLE public.context_embedding IN ACCESS EXCLUSIVE MODE`); err != nil {
						t.Fatal(err)
					}
					readContext, cancel := context.WithCancelCause(ctx)
					defer cancel(nil)
					finished := make(chan error, 1)
					go func() {
						read := func(ctx context.Context) error {
							values, err := database.SemanticCandidates(ctx, graph, "search02", vectors[first.ID], at, 3)
							if values != nil {
								return fmt.Errorf("취소된 읽기에 후보가 남았다: %v", values)
							}
							return err
						}
						var err error
						if scoped {
							_, err = database.WithReadSnapshot(readContext, read)
						} else {
							err = read(readContext)
						}
						finished <- err
					}()
					waitGrantWaiters(t, ctx, database, int(blocker.Conn().PgConn().PID()), 1)
					cause := errors.New("의미 채널 잠금 대기 취소")
					cancel(cause)
					select {
					case err := <-finished:
						if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
							t.Fatalf("취소 원인: %v", err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if err := rollbackTransaction(t.Context(), blocker); err != nil {
						t.Fatal(err)
					}
					assertConnectionRecovery(t, database, nil, 0)
				})
			}
		})
	}
}
