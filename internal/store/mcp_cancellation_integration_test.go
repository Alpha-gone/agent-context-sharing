package store

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/mcp"
	"github.com/jackc/pgx/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcp08Completion struct {
	err   error
	scope *readScope
}

// HTTP/SDK에서 받은 context를 실제 저장 경계에 전달하는 자원 시험용 처리기다.
// 도구별 업무 변환은 cmd/server의 실제 어댑터 인수가 별도로 담당한다.
func TestMCP08HTTPDatabaseCancellationIntegration(t *testing.T) {
	for _, stage := range []string{"reservation", "snapshot", "before commit", "after commit"} {
		for _, interruption := range []string{"disconnect", "deadline"} {
			t.Run(stage+"/"+interruption, func(t *testing.T) {
				database := newSmallPoolStore(t)
				write := lifeAcceptanceFixture(t, database, "graph_create")
				entered := make(chan struct{})
				completed := make(chan mcp08Completion, 1)
				key := "mcp08_request_key_1"
				input := []byte(`{"name":"same"}`)
				releaseHold := func() {}
				if stage == "reservation" {
					var err error
					releaseHold, err = database.reserveConnections(t.Context(), connectionBudget(4))
					if err != nil {
						t.Fatal(err)
					}
					defer releaseHold()
				}
				handlers := map[string]mcp.ToolHandler{}
				for _, name := range []string{"agent_register", "graph_list", "graph_create", "graph_get", "graph_update", "node_create", "node_get", "node_update", "node_discard", "node_restore", "context_flow_get", "relation_list", "relation_confirm", "relation_discard"} {
					handlers[name] = func(ctx context.Context, _ mcp.Principal, _ *sdk.CallToolRequest) (result *sdk.CallToolResult, resultErr error) {
						outcome := mcp08Completion{}
						defer func() { outcome.err = resultErr; completed <- outcome }()
						if stage == "snapshot" {
							snapshot, err := database.BeginReadSnapshot(ctx)
							if err != nil {
								return nil, err
							}
							outcome.scope = snapshot.scope
							defer func() { _, err := snapshot.Close(ctx); resultErr = errors.Join(resultErr, err) }()
							read, release, err := database.enterReadScope(snapshot.Context(ctx))
							if err != nil {
								return nil, err
							}
							defer func() { resultErr = errors.Join(resultErr, release()) }()
							close(entered)
							_, err = read.Value(readTxKey{}).(pgx.Tx).Exec(read, "SELECT pg_sleep(5)")
							return nil, err
						}
						if stage == "reservation" {
							close(entered)
						}
						data, err := database.ExecuteWrite(ctx, write.actor, key, "graph_create", input, writeReplayCheck, func(txCtx context.Context) ([]byte, error) {
							if err := write.run(txCtx); err != nil {
								return nil, err
							}
							if stage == "before commit" {
								close(entered)
								if _, err := txCtx.Value(writeTransactionContextKey{}).(pgx.Tx).Exec(txCtx, "SELECT pg_sleep(5)"); err != nil {
									return nil, err
								}
							}
							return []byte(`{"ok":true}`), nil
						})
						if err != nil {
							return nil, err
						}
						if stage == "after commit" {
							close(entered)
							<-ctx.Done()
							return nil, ctx.Err()
						}
						return &sdk.CallToolResult{StructuredContent: jsontext.Value(data), Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}}, nil
					}
				}
				handler, err := mcp.NewHandler(mcp.HTTPConfig{AllowedOrigins: []string{"https://allowed.test"}, Handlers: handlers, Protect: func(next mcp.AuthenticatedHandler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						next(w, r, mcp.Principal{AccountID: write.actor, ClientID: "first"})
					})
				}})
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
					defer cancel()
					handler.ServeHTTP(w, r.WithContext(ctx))
				}))
				t.Cleanup(server.Close)
				clientCtx, cancelClient := context.WithCancel(t.Context())
				defer cancelClient()
				body := `{"jsonrpc":"2.0","id":"resource","method":"tools/call","params":{"name":"graph_create","arguments":{"name":"same"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
				request, err := http.NewRequestWithContext(clientCtx, "POST", server.URL, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Accept", "application/json, text/event-stream")
				request.Header.Set("MCP-Protocol-Version", "2026-07-28")
				request.Header.Set("Mcp-Method", "tools/call")
				request.Header.Set("Mcp-Name", "graph_create")
				clientDone := make(chan error, 1)
				go func() {
					response, err := server.Client().Do(request)
					if err == nil {
						_, err = io.Copy(io.Discard, response.Body)
						response.Body.Close()
					}
					clientDone <- err
				}()
				select {
				case <-entered:
				case err := <-completed:
					t.Fatal("중단 단계 전에 종료", err.err)
				case <-time.After(3 * time.Second):
					t.Fatal("중단 단계 도달 실패")
				}
				if stage == "reservation" && database.pool.Stat().AcquiredConns() != 0 {
					t.Fatal("예약 대기 중 연결 획득")
				}
				if interruption == "disconnect" {
					cancelClient()
				}
				var outcome mcp08Completion
				select {
				case outcome = <-completed:
				case <-time.After(5 * time.Second):
					t.Fatal("HTTP 취소/서버 마감이 저장 경계에 도달하지 않음")
				}
				if !errors.Is(outcome.err, context.Canceled) && !errors.Is(outcome.err, context.DeadlineExceeded) {
					t.Fatal("취소 원인 손실", outcome.err)
				}
				select {
				case <-clientDone:
				case <-time.After(3 * time.Second):
					t.Fatal("HTTP 종료 실패")
				}
				releaseHold()
				assertConnectionRecovery(t, database, outcome.scope, 0)
				if stage == "before commit" || stage == "after commit" {
					var graphs, records, grants int
					if err := database.pool.QueryRow(t.Context(), "SELECT count(*) FROM public.context_graph WHERE graph_id=$1", write.graph.String()).Scan(&graphs); err != nil {
						t.Fatal(err)
					}
					if err := database.pool.QueryRow(t.Context(), "SELECT count(*) FROM public.idempotency_record WHERE actor_account_id=$1 AND idempotency_key=$2", write.actor.String(), key).Scan(&records); err != nil {
						t.Fatal(err)
					}
					if err := database.pool.QueryRow(t.Context(), "SELECT count(*) FROM public.graph_grant WHERE graph_id=$1", write.graph.String()).Scan(&grants); err != nil {
						t.Fatal(err)
					}
					want := 0
					if stage == "after commit" {
						want = 1
					}
					if graphs != want || records != want || grants != want {
						t.Fatal("커밋 전 롤백/커밋 후 결과 보존", graphs, records, grants)
					}
					if stage == "after commit" {
						second := newSmallPoolStore(t)
						value, err := second.ExecuteWrite(t.Context(), write.actor, key, "graph_create", input, writeReplayCheck, func(context.Context) ([]byte, error) { return nil, errors.New("재생에서 업무 재적용") })
						if err != nil || !strings.Contains(string(value), `"ok": true`) && !strings.Contains(string(value), `"ok":true`) {
							t.Fatal("다른 인스턴스의 최초 결과 회수", string(value), err)
						}
						assertConnectionRecovery(t, second, nil, 0)
					}
				}
				var open int
				if err := database.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state LIKE 'idle in transaction%'").Scan(&open); err != nil || open != 0 {
					t.Fatal("취소 뒤 열린 트랜잭션", open, err)
				}
				if err := database.Ping(t.Context()); err != nil {
					t.Fatal(fmt.Errorf("취소 뒤 후속 읽기: %w", err))
				}
			})
		}
	}
}
