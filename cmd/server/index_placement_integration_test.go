package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type indexProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	logs bytes.Buffer
}

func startIndexProcess(t *testing.T, binary string, values map[string]string, certificate string) *indexProcess {
	t.Helper()
	process := &indexProcess{cmd: exec.Command(binary), done: make(chan struct{})}
	process.cmd.Env = []string{"SSL_CERT_FILE=" + certificate, "SSL_CERT_DIR=" + t.TempDir(), "GORACE=atexit_sleep_ms=0"}
	for _, key := range config.AllowedKeys() {
		process.cmd.Env = append(process.cmd.Env, key+"="+values[key])
	}
	process.cmd.Stdout, process.cmd.Stderr = &process.logs, &process.logs
	if err := process.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { process.err = process.cmd.Wait(); close(process.done) }()
	t.Cleanup(func() {
		select {
		case <-process.done:
		default:
			process.cmd.Process.Kill()
			<-process.done
		}
		if t.Failed() {
			t.Log(process.logs.String())
		}
	})
	return process
}

func stopIndexProcess(t *testing.T, process *indexProcess) {
	t.Helper()
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.done:
		if process.err != nil || strings.Contains(process.logs.String(), "DATA RACE") {
			t.Fatalf("프로세스 종료: %v\n%s", process.err, process.logs.String())
		}
	case <-time.After(12 * time.Second):
		t.Fatal("프로세스가 작업자 종료 예산 안에 끝나지 않았다")
	}
}

func waitIndexCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("색인 처리 조건의 제한 시간을 초과했다")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// 시험마다 빈 DB에 정식 마이그레이션을 적용한다. 다른 시험의 큐를 복사하지 않는다.
func indexPlacementDatabase(t *testing.T, count int) (*pgx.Conn, string) {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("필수 시험 DB 주소가 없다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 배치 DB 시험을 건너뛴다")
	}
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	u.Path, u.RawPath = "/postgres", ""
	queryOptions := u.Query()
	queryOptions.Del("dbname")
	queryOptions.Del("database")
	u.RawQuery = queryOptions.Encode()
	admin, err := pgx.Connect(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := model.NewID()
	name := "idx06_" + strings.ReplaceAll(id.String(), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(t.Context())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Errorf("시험 DB 정리: %v", err)
		}
		admin.Close(ctx)
	})
	u.Path, u.RawPath = "/"+name, ""
	address = u.String()
	pool, err := migrate.NewPool(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var actual string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&actual); err != nil || actual != name {
		t.Fatal("실제 연결이 시험 전용 DB가 아니므로 마이그레이션을 중단한다")
	}
	cfg := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 128, IndexTargetLayers: "all_layers"}
	for pass := range 2 {
		want := 0
		appliedCount := 0
		err := migrate.WithLock(t.Context(), pool, func(ctx context.Context, conn *pgxpool.Conn) error {
			loaded, applied, err := migrate.Prepare(ctx, conn, migrations.FS, ".", cfg)
			if err != nil {
				return err
			}
			if pass == 0 {
				want = len(loaded)
			}
			pending, err := migrate.Pending(loaded, applied)
			if err != nil {
				return err
			}
			for _, item := range pending {
				if err := migrate.Apply(ctx, conn, item); err != nil {
					return err
				}
				appliedCount++
			}
			return nil
		})
		if err != nil || appliedCount != want {
			t.Fatalf("정식 적용=%d want=%d: %v", appliedCount, want, err)
		}
	}
	conn, err := pgx.Connect(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.WithoutCancel(t.Context())) })
	database, err := store.New(t.Context(), address, "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	actor, _ := model.NewID()
	graph, _ := model.NewID()
	now := time.Now().UTC().Truncate(time.Second)
	if err := database.CreateAccount(t.Context(), store.Account{ID: actor, LoginID: strings.ReplaceAll(actor.String(), "-", ""), PasswordHash: "synthetic-password-hash", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateGraph(t.Context(), model.Graph{ID: graph, Name: "consumer fixture", CreatedBy: actor, CreatedAt: now, LastActivityAt: now, Version: 1}); err != nil {
		t.Fatal(err)
	}
	for i := range count {
		id, _ := model.NewID()
		node := model.Context{ID: id, GraphID: graph, Layer: model.LayerSource, Body: fmt.Sprintf("synthetic consumer %d", i), RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1,
			Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelAPI, Locator: fmt.Sprintf("api://consumer/%d", i)}, OccurredAt: now, OriginKind: model.OriginKindExternalContent}}
		if _, err := database.CreateContext(t.Context(), graph, node, nil); err != nil {
			t.Fatal(err)
		}
	}
	return conn, address
}

func TestIndexWorkerPlacementsIntegration(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("필수 시험 DB 주소가 없다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 색인 배치 시험을 건너뛴다")
	}
	binaries := t.TempDir()
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-race", "-o", binaries+string(os.PathSeparator), "./cmd/server", "./cmd/indexer")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("시험 바이너리 빌드: %v\n%s", err, output)
	}
	var published string
	for _, mode := range []string{"embedded", "external", "two_workers", "interrupted", "schema_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			count := 3
			if mode == "interrupted" {
				count = 1
			}
			conn, address := indexPlacementDatabase(t, count)
			var calls atomic.Int32
			var mu sync.Mutex
			inputs := map[string]int{}
			canceled := make(chan struct{}, 8)
			release := make(chan struct{})
			var releaseOnce sync.Once
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				inputs[string(body)]++
				mu.Unlock()
				n := calls.Add(1)
				if mode == "two_workers" || (mode == "interrupted" && n == 1) {
					select {
					case <-release:
					case <-r.Context().Done():
						canceled <- struct{}{}
						return
					}
				}
				vector := make([]float64, 128)
				vector[0] = 1
				encoded, err := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
				if err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(encoded)
			}))
			t.Cleanup(provider.Close)
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			certificate := filepath.Join(t.TempDir(), "provider.pem")
			if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			values := serverConfiguration(t)
			values["DATABASE_URL"], values["AGE_GRAPH_NAME"] = address, "agent_context"
			values["EMBEDDING_BASE_URL"], values["EMBEDDING_DIMENSION"] = provider.URL, "128"
			values["TLS_TERMINATION"], values["TLS_CERT_FILE"], values["TLS_KEY_FILE"] = "proxy", "", ""
			values["TRUSTED_PROXY_CIDRS"], values["INDEX_WORKER_PLACEMENT"] = "127.0.0.1/32", "external"
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			values["HTTP_ADDR"] = listener.Addr().String()
			listener.Close()
			var processes []*indexProcess
			queryCount := func(sql string) int {
				var n int
				if err := conn.QueryRow(t.Context(), sql).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			start := func(name string) *indexProcess {
				// 각 프로세스의 기록자 잠금과 파일을 분리하고 시험이 만든 경로만 정리한다.
				directory, err := os.MkdirTemp(".", ".index-placement-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.RemoveAll(directory); err != nil {
						t.Error(err)
					}
				})
				absolute, err := filepath.Abs(directory)
				if err != nil {
					t.Fatal(err)
				}
				values["DIAGNOSTIC_LOG_DIR"] = filepath.Join(absolute, "logs")
				return startIndexProcess(t, filepath.Join(binaries, name), values, certificate)
			}
			if mode == "embedded" || mode == "external" {
				values["INDEX_WORKER_PLACEMENT"] = mode
				processes = append(processes, start("server"))
				client := &http.Client{Timeout: time.Second}
				defer client.CloseIdleConnections()
				waitIndexCondition(t, func() bool {
					response, err := client.Get("http://" + values["HTTP_ADDR"] + "/readyz")
					if err != nil {
						return false
					}
					defer response.Body.Close()
					return response.StatusCode == http.StatusOK
				})
				if mode == "external" {
					time.Sleep(200 * time.Millisecond)
					if calls.Load() != 0 || queryCount(`SELECT count(*) FROM public.index_task`) != count || queryCount(`SELECT count(*) FROM public.context_embedding`) != 0 {
						t.Fatal("external 서버가 색인 큐를 소비했다")
					}
					processes = append(processes, start("indexer"))
				}
			} else {
				if mode == "schema_mismatch" {
					values["EMBEDDING_DIMENSION"] = "256"
				}
				processes = append(processes, start("indexer"))
				if mode == "schema_mismatch" {
					select {
					case <-processes[0].done:
						if processes[0].err == nil || calls.Load() != 0 || queryCount(`SELECT count(*) FROM public.index_task`) != count {
							t.Fatal("DB 형상 불일치가 외부 호출 전에 차단되지 않았다")
						}
					case <-time.After(10 * time.Second):
						t.Fatal("DB 형상 불일치 실행기가 종료되지 않았다")
					}
					return
				}
			}
			if mode == "two_workers" {
				processes = append(processes, start("indexer"))
				waitIndexCondition(t, func() bool { return calls.Load() == 2 })
				if queryCount(`SELECT count(*) FROM public.index_task WHERE next_attempt_at>statement_timestamp()`) != 2 || queryCount(`SELECT count(*) FROM public.context_embedding`) != 0 {
					t.Fatal("두 작업자의 확보·리스·공개 경계가 다르다")
				}
				time.Sleep(200 * time.Millisecond)
				if calls.Load() != 2 {
					t.Fatal("유효 리스를 중복 처리했다")
				}
				releaseOnce.Do(func() { close(release) })
			}
			if mode == "interrupted" {
				waitIndexCondition(t, func() bool { return calls.Load() == 1 })
				stopIndexProcess(t, processes[0])
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("종료가 제공자 HTTP 요청을 취소하지 않았다")
				}
				if queryCount(`SELECT count(*) FROM public.index_task WHERE attempts=0 AND state='pending' AND next_attempt_at>statement_timestamp()`) != 1 {
					t.Fatal("중단한 작업의 미완료 리스가 보존되지 않았다")
				}
				processes = []*indexProcess{start("indexer")}
				time.Sleep(300 * time.Millisecond)
				if calls.Load() != 1 {
					t.Fatal("중단된 작업의 유효 리스를 재처리했다")
				}
				// 2분 실시간 대기 대신 이 시험 DB의 유일한 리스를 만료시킨다.
				if _, err := conn.Exec(t.Context(), `UPDATE public.index_task SET next_attempt_at=statement_timestamp()-interval '1 second'`); err != nil {
					t.Fatal(err)
				}
			}
			waitIndexCondition(t, func() bool { return queryCount(`SELECT count(*) FROM public.index_task`) == 0 })
			if queryCount(`SELECT count(*) FROM public.context_embedding WHERE model_id='gemini:gemini-embedding-2:retrieval-v1:vector:128'`) != count {
				t.Fatal("색인 결과의 모델·행 수가 다르다")
			}
			for _, process := range processes {
				stopIndexProcess(t, process)
			}
			if mode == "embedded" || mode == "external" {
				var result string
				if err := conn.QueryRow(t.Context(), `SELECT string_agg(model_id||':'||embedding::text,',' ORDER BY model_id,embedding::text) FROM public.context_embedding`).Scan(&result); err != nil {
					t.Fatal(err)
				}
				if mode == "embedded" {
					published = result
				} else if result != published {
					t.Fatal("embedded·external 배치의 공개 결과가 다르다")
				}
			}
			if mode != "interrupted" {
				mu.Lock()
				defer mu.Unlock()
				if len(inputs) != count {
					t.Fatalf("제공자 입력 수=%d want=%d", len(inputs), count)
				}
				for _, n := range inputs {
					if n != 1 {
						t.Fatal("한 본문을 여러 작업자가 중복 호출했다")
					}
				}
			}
		})
	}
}
