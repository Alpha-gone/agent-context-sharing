package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestEmbeddingComparisonCommandIntegration(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_URL required")
		}
		t.Skip("TEST_DATABASE_URL 미설정")
	}
	for _, axis := range []string{"dimension", "compression"} {
		t.Run(axis, func(t *testing.T) {
			dbEnv := "TEST_EMBEDDING_SMALL_URL"
			if axis == "compression" {
				dbEnv = "TEST_EMBEDDING_HALF_URL"
			}
			other := os.Getenv(dbEnv)
			if other == "" {
				if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
					t.Fatalf("%s required", dbEnv)
				}
				t.Skip(dbEnv + " 미설정")
			}
			var attempts atomic.Int64
			var rejectQueries atomic.Bool
			var failAt atomic.Int64
			var blockQueries atomic.Bool
			started := make(chan struct{}, 1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.Header.Get("x-goog-api-key") != "synthetic-secret" {
					w.WriteHeader(401)
					return
				}
				var payload struct {
					Content struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"content"`
					Config struct {
						Dimension int `json:"outputDimensionality"`
					} `json:"embedContentConfig"`
				}
				if err := json.UnmarshalRead(r.Body, &payload); err != nil {
					w.WriteHeader(400)
					return
				}
				text := payload.Content.Parts[0].Text
				if strings.HasPrefix(text, "task:") {
					if rejectQueries.Load() || (failAt.Load() > 0 && attempts.Load() >= failAt.Load()) {
						w.WriteHeader(400)
						return
					}
					if blockQueries.Load() {
						select {
						case started <- struct{}{}:
						default:
						}
						<-r.Context().Done()
						return
					}
				}
				vector := make([]float64, payload.Config.Dimension)
				vector[0] = 1
				switch {
				case strings.Contains(text, "private-positive"):
					vector[0] = 0.8
					if payload.Config.Dimension == 128 {
						vector[0] = 0.6
					}
					vector[1] = math.Sqrt(1 - vector[0]*vector[0])
				case strings.Contains(text, "private-negative"):
					vector[0] = 0.2
					if payload.Config.Dimension == 128 {
						vector[0] = 0.1
					}
					vector[1] = math.Sqrt(1 - vector[0]*vector[0])
				case strings.Contains(text, "needlemiss"):
					vector[0] = -1
				}
				raw, _ := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			trustEvalEmbeddingServer(t, server)
			for k, v := range map[string]string{"EVAL11_BASE_DB": os.Getenv("TEST_DATABASE_URL"), "EVAL11_OTHER_DB": other, "EVAL11_ENDPOINT": server.URL, "EVAL11_KEY": "synthetic-secret", "EVAL_TEST_COMMAND_PROCESS": "1"} {
				t.Setenv(k, v)
			}
			in := embeddingFixture(t)
			in.Axis = axis
			if axis == "compression" {
				in.Variants[1].Dimension = 768
				in.Variants[1].VectorType = "halfvec"
				in.Variants[1].RelationThresholds = []float64{0.3, 0.9}
			}
			in.Version = lifecycleInput(t).Version
			path := writeManifestFixture(t, in)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			invoke := func(out string, extra ...string) *exec.Cmd {
				args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-embedding-comparison=" + path, "-out=" + out}
				return exec.CommandContext(t.Context(), binary, append(args, extra...)...)
			}
			out := filepath.Join(t.TempDir(), "report.json")
			before := attempts.Load()
			logs, err := invoke(out, "-repeat=10").CombinedOutput()
			if err != nil {
				t.Fatalf("embedding CLI: %v\n%s", err, logs)
			}
			raw, err := os.ReadFile(out)
			if err != nil || bytes.Contains(raw, []byte("private-")) || bytes.Contains(raw, []byte("synthetic-secret")) || bytes.Contains(raw, []byte("postgres://")) || bytes.Contains(raw, []byte("needle")) {
				t.Fatalf("privacy/read: %v", err)
			}
			var report embeddingReport
			if err := json.Unmarshal(raw, &report, json.RejectUnknownMembers(true)); err != nil {
				t.Fatal(err)
			}
			if len(report.Runs) != 40 || len(report.Conditions) != 2 || report.StartedAt.IsZero() || report.Activated || report.Status != "deferred" {
				t.Fatalf("report boundary: %d %s", len(report.Runs), report.Status)
			}
			// 조정은 문서 5개+신호 2개, 최종은 문서 5개+신호 2개+검색 2개를 각각 20회 실행한다.
			if count := attempts.Load() - before; count != 320 {
				t.Fatalf("actual HTTP attempts %d != 320", count)
			}
			if report.Conditions[0].Selected.Relation != 0.3 || report.Conditions[1].Selected.Relation != in.Variants[1].RelationThresholds[0] {
				t.Fatal(report.Conditions)
			}
			baseBytes, candidateBytes := int64(0), int64(0)
			for i, run := range report.Runs {
				if run.Variant != in.Variants[(i+run.Repeat-1)%2].Code || run.Preparation.VectorRows != 5 || run.Preparation.EmbeddingCalls != 5 || run.Preparation.VectorBytes <= 0 || run.Preparation.DurationMS < 0 || run.DurationMS <= 0 || run.Cost != -1 {
					t.Fatalf("actual resources/order: %+v", run)
				}
				if run.Variant == "base" {
					baseBytes += run.Preparation.VectorBytes
				} else {
					candidateBytes += run.Preparation.VectorBytes
				}
				wantCalls := int64(2)
				if run.Split == "evaluation" {
					wantCalls = 4
					if len(run.Metrics.QuerySamples) != 2 {
						t.Fatal("retrieval omitted")
					}
				}
				if run.Calls != wantCalls {
					t.Fatalf("query attempts %d != %d", run.Calls, wantCalls)
				}
				// keyword가 없어도 음수 의미 점수를 0으로 바꿔 전역 판정을 뒤집지 않는다.
				if run.Signals[1].Keyword || run.Signals[1].Top >= 0 {
					t.Fatal(run.Signals)
				}
			}
			if candidateBytes >= baseBytes {
				t.Fatalf("vector footprint %d >= %d", candidateBytes, baseBytes)
			}
			for _, url := range []string{os.Getenv("TEST_DATABASE_URL"), other} {
				connection, err := pgx.Connect(t.Context(), url)
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range in.Samples {
					for _, v := range in.Variants {
						var active int
						if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM public.context_graph WHERE name=$1 AND deleted_at IS NULL`, "eval "+in.Version+"-"+s.Code+"-"+v.Code).Scan(&active); err != nil || active != 0 {
							t.Fatalf("cleanup %d %v", active, err)
						}
					}
				}
				_ = connection.Close(t.Context())
			}
			t.Run("invalid_second_schema_before_graph", func(t *testing.T) {
				t.Setenv("EVAL11_OTHER_DB", os.Getenv("TEST_DATABASE_URL"))
				before := attempts.Load()
				dest := filepath.Join(t.TempDir(), "failed.json")
				logs, err := invoke(dest).CombinedOutput()
				if err == nil {
					t.Fatalf("bad schema accepted: %s", logs)
				}
				if attempts.Load() != before {
					t.Fatal("schema preflight made HTTP calls")
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatal("failed report created")
				}
			})
			for _, extra := range []string{"-budget=1", "-adaptive", "-repeat=9"} {
				t.Run(extra, func(t *testing.T) {
					before := attempts.Load()
					dest := filepath.Join(t.TempDir(), "failed.json")
					if logs, err := invoke(dest, extra).CombinedOutput(); err == nil || strings.Contains(string(logs), "dial") {
						t.Fatalf("flag preflight: %v %s", err, logs)
					}
					if attempts.Load() != before {
						t.Fatal("preflight made calls")
					}
				})
			}
			t.Run("query_failure", func(t *testing.T) {
				rejectQueries.Store(true)
				defer rejectQueries.Store(false)
				dest := filepath.Join(t.TempDir(), "failed.json")
				if logs, err := invoke(dest).CombinedOutput(); err == nil {
					t.Fatalf("query failure accepted: %s", logs)
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatal("failure created report")
				}
			})
			t.Run("final_partial_channel_failure", func(t *testing.T) {
				failAt.Store(attempts.Load() + 148)
				defer failAt.Store(0)
				dest := filepath.Join(t.TempDir(), "partial.json")
				logs, err := invoke(dest).CombinedOutput()
				if err == nil || !strings.Contains(string(logs), "검색 채널 실패") {
					t.Fatalf("partial search failure: %v %s", err, logs)
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatal("partial failure created report")
				}
			})

			t.Run("signal_cleanup", func(t *testing.T) {
				blockQueries.Store(true)
				defer blockQueries.Store(false)
				dest := filepath.Join(t.TempDir(), "canceled.json")
				command := invoke(dest)
				var logs bytes.Buffer
				command.Stdout, command.Stderr = &logs, &logs
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-started:
				case <-time.After(10 * time.Second):
					_ = command.Process.Kill()
					_ = command.Wait()
					t.Fatal("no blocked query")
				}
				if err := command.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
				if err := command.Wait(); err == nil || !strings.Contains(logs.String(), "context canceled") {
					t.Fatalf("signal cancellation: %v %s", err, logs.String())
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatal("canceled report created")
				}
			})
			// 실패/취소에서도 두 DB에 활성 평가 그래프가 남지 않는다.
			for _, url := range []string{os.Getenv("TEST_DATABASE_URL"), other} {
				connection, err := pgx.Connect(t.Context(), url)
				if err != nil {
					t.Fatal(err)
				}
				var active int
				if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM public.context_graph WHERE name LIKE $1 AND deleted_at IS NULL`, "eval "+in.Version+"-%").Scan(&active); err != nil || active != 0 {
					t.Fatalf("failure cleanup: %d %v", active, err)
				}
				_ = connection.Close(context.WithoutCancel(t.Context()))
			}
		})
	}
}

func TestEmbeddingRelationScreenMatchesProposalIntegration(t *testing.T) {
	database, worker, _ := evalDatabase(t, nil)
	in := embeddingFixture(t)
	sample := in.Samples[0]
	sample.Contexts.Version = lifecycleInput(t).Version
	err := withEvaluationGraph(t.Context(), database, worker, sample.Contexts, 32, func(graph loadedGraph) error {
		raw, err := embeddingRelationScores(t.Context(), embeddingBackend{comparisonBackend{database: database, worker: worker}}, graph, sample)
		if err != nil {
			return err
		}
		proposal, err := store.New(t.Context(), os.Getenv("TEST_DATABASE_URL"), "agent_context", &store.RelationProposalConfig{AdjacencyWindow: time.Hour, SimilarityThreshold: 0.5, Limit: 1}, nil, store.IndexTargetsAllLayers)
		if err != nil {
			return err
		}
		defer proposal.Close()
		if err := proposal.ProposeSimilarEventRelations(t.Context(), graph.GraphID, graph.Keys[sample.Anchor], worker.ModelID()); err != nil {
			return err
		}
		relations, _, err := proposal.ListContextRelations(t.Context(), graph.GraphID, graph.Keys[sample.Anchor], []model.RelationState{model.RelationStateProposed}, []model.RelationType{model.RelationTypeRelatesTo}, "", 10)
		if err != nil {
			return err
		}
		if len(relations) != 1 || (relations[0].FromContextID != graph.Keys[raw[0].Target] && relations[0].ToContextID != graph.Keys[raw[0].Target]) {
			t.Fatalf("offline/production top-1: %+v %+v", raw, relations)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
