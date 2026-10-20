package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpWriteFixture struct {
	database                                               *store.Store
	pool                                                   *pgxpool.Pool
	principal                                              mcp.Principal
	agent, graph, source, derived, firstEvent, secondEvent model.ID
	handlers, other                                        map[string]mcp.ToolHandler
}

func newMCPWriteFixture(t *testing.T) *mcpWriteFixture {
	t.Helper()
	database, pool, address := newServerContractDatabase(t)
	second, err := store.New(t.Context(), address, "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	account, _ := model.NewID()
	if err := database.CreateAccount(t.Context(), store.Account{ID: account, LoginID: "mcp_writer", PasswordHash: "fixture_hash", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	f := &mcpWriteFixture{database: database, pool: pool, principal: mcp.Principal{AccountID: account, ClientID: "first"}}
	f.agent, err = database.RegisterAgent(t.Context(), account, "first", "registration_key_1")
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"writes_per_minute":100}}`, account))
	if err != nil {
		t.Fatal(err)
	}
	f.handlers, err = newMCPWriteHandlers(database, plans)
	if err != nil {
		t.Fatal(err)
	}
	f.other, err = newMCPWriteHandlers(second, plans)
	if err != nil {
		t.Fatal(err)
	}
	f.graph = f.id(t, "graph_create", map[string]any{"name": "그래프", "description": "설명"}, "graph", "graph_id")
	source := f.args("node_create")
	source["layer"], source["body"], source["origin_kind"], source["occurred_at"] = "source", "원천", "user_utterance", "2026-10-20T12:00:00Z"
	source["source_ref"] = map[string]any{"channel": "file", "locator": "file:///mcp06/first"}
	f.source = f.id(t, "node_create", source, "node", "context_id")
	derived := f.args("node_create")
	derived["layer"], derived["body"], derived["derived_from"], derived["derivation_kind"], derived["evidence_state"] = "derived", "파생", []string{f.source.String()}, "proposition", "observation"
	f.derived = f.id(t, "node_create", derived, "node", "context_id")
	event := f.args("node_create")
	event["layer"], event["body"], event["member_refs"], event["event_start"], event["event_end"] = "event", "사건", []string{f.source.String()}, "2026-10-20T11:00:00Z", "2026-10-20T13:00:00Z"
	f.firstEvent = f.id(t, "node_create", event, "node", "context_id")
	source["occurred_at"] = "2026-10-20T15:00:00Z"
	source["source_ref"] = map[string]any{"channel": "file", "locator": "file:///mcp06/second"}
	secondSource := f.id(t, "node_create", source, "node", "context_id")
	event["member_refs"], event["event_start"], event["event_end"] = []string{secondSource.String()}, "2026-10-20T14:00:00Z", "2026-10-20T16:00:00Z"
	f.secondEvent = f.id(t, "node_create", event, "node", "context_id")
	return f
}

func (f *mcpWriteFixture) args(tool string) map[string]any {
	args := map[string]any{"graph_id": f.graph.String()}
	if strings.HasPrefix(tool, "node_") || strings.HasPrefix(tool, "relation_") {
		args["agent_id"] = f.agent.String()
		args["judgment_input"] = "MCP-06 판단"
	}
	return args
}

func callMCPWrite(ctx context.Context, handlers map[string]mcp.ToolHandler, principal mcp.Principal, tool string, args map[string]any) (*sdk.CallToolResult, error) {
	data, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return handlers[tool](ctx, principal, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: tool, Arguments: data}})
}

func writeContent(t *testing.T, result *sdk.CallToolResult, err error, code string) map[string]any {
	t.Helper()
	if err != nil || result == nil {
		t.Fatal("쓰기 호출 실패", err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text, ok := result.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatal("text content가 필요하다")
	}
	if !bytes.Equal(data, []byte(text.Text)) { // jsonb의 공백은 의미 비교로 확인한다.
		var a, b any
		if json.Unmarshal(data, &a) != nil || json.Unmarshal([]byte(text.Text), &b) != nil {
			t.Fatal("외피 JSON")
		}
		x, _ := json.Marshal(a, json.Deterministic(true))
		y, _ := json.Marshal(b, json.Deterministic(true))
		if !bytes.Equal(x, y) {
			t.Fatal("text/structuredContent 불일치")
		}
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if code == "" {
		if result.IsError || value["code"] != nil {
			t.Fatalf("성공 예상: %s", data)
		}
	} else if !result.IsError || value["code"] != code {
		t.Fatalf("%s 예상: %s", code, data)
	}
	return value
}

func (f *mcpWriteFixture) call(t *testing.T, tool string, args map[string]any, code string) map[string]any {
	t.Helper()
	result, err := callMCPWrite(t.Context(), f.handlers, f.principal, tool, args)
	return writeContent(t, result, err, code)
}
func (f *mcpWriteFixture) id(t *testing.T, tool string, args map[string]any, object, field string) model.ID {
	t.Helper()
	value := f.call(t, tool, args, "")
	id, err := model.ParseID(value[object].(map[string]any)[field].(string))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mcpWriteFingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	parts := []string{}
	for _, table := range []string{"agent_context.\"Context\"", "agent_context._ag_label_edge", "public.context_graph", "public.graph_grant", "public.operation_log", "public.context_revision", "public.index_task", "public.request_rate", "public.idempotency_record"} {
		var hash string
		if err := pool.QueryRow(t.Context(), `SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY row_to_json(t)::text),'')) FROM `+table+` t`).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, hash)
	}
	return strings.Join(parts, ":")
}

func TestMCP06EightWriteAdaptersIntegration(t *testing.T) {
	for _, tool := range []string{"graph_create", "graph_update", "node_create", "node_update", "node_discard", "node_restore", "relation_confirm", "relation_discard"} {
		t.Run(tool, func(t *testing.T) {
			f := newMCPWriteFixture(t)
			args := f.args(tool)
			switch tool {
			case "graph_create":
				args = map[string]any{"name": "새 그래프"}
			case "graph_update":
				args["expected_version"], args["description"] = 1, ""
			case "node_create":
				args["layer"], args["body"], args["derived_from"], args["derivation_kind"], args["evidence_state"] = "derived", "새 파생", []string{f.source.String()}, "proposition", "observation"
			case "node_update":
				args["context_id"], args["expected_version"], args["body"] = f.derived.String(), 1, "수정 파생"
			case "node_discard", "node_restore":
				args["context_id"] = f.derived.String()
				if tool == "node_restore" {
					f.call(t, "node_discard", args, "")
				}
			case "relation_confirm", "relation_discard":
				args["relation_type"], args["from_context_id"], args["to_context_id"] = "precedes", f.firstEvent.String(), f.secondEvent.String()
				if tool == "relation_discard" {
					id := f.id(t, "relation_confirm", args, "relation", "relation_id")
					args = f.args(tool)
					args["relation_id"] = id.String()
				}
			}
			args["request_id"] = "request_key_00001"
			before := mcpWriteFingerprint(t, f.pool)
			first, err := callMCPWrite(t.Context(), f.handlers, f.principal, tool, args)
			writeContent(t, first, err, "")
			after := mcpWriteFingerprint(t, f.pool)
			if before == after {
				t.Fatal("업무 적용 누락")
			}
			replay, err := callMCPWrite(t.Context(), f.other, f.principal, tool, args)
			writeContent(t, replay, err, "")
			if first.Content[0].(*sdk.TextContent).Text != replay.Content[0].(*sdk.TextContent).Text || after != mcpWriteFingerprint(t, f.pool) {
				t.Fatal("재생 결과·업무·감사·색인·한도 중복")
			}
			args["request_id"] = "request_key_00002"
			code := ""
			switch tool {
			case "graph_update", "node_update":
				code = "version_conflict"
			case "node_discard", "node_restore", "relation_discard":
				code = "invalid_argument"
			}
			f.call(t, tool, args, code)
		})
	}
}

func TestMCP06DomainSemanticsIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	t.Run("source natural key and immutable", func(t *testing.T) {
		args := f.args("node_create")
		args["layer"], args["body"], args["origin_kind"], args["occurred_at"], args["source_ref"] = "source", "변경한 원문", "agent_output", "2026-10-20T12:00:00Z", map[string]any{"channel": "file", "locator": "file:///mcp06/first"}
		if got := f.id(t, "node_create", args, "node", "context_id"); got != f.source {
			t.Fatal("원천 자연 키")
		}
		args = f.args("node_update")
		args["context_id"], args["expected_version"], args["body"] = f.source.String(), 1, "변경"
		f.call(t, "node_update", args, "invalid_argument")
	})
	t.Run("keep and normalized judgment", func(t *testing.T) {
		args := f.args("node_update")
		delete(args, "judgment_input")
		args["context_id"], args["expected_version"], args["management_action"], args["request_id"] = f.derived.String(), 1, "keep", "request_key_keep"
		value := f.call(t, "node_update", args, "")
		if value["node"].(map[string]any)["version"] != float64(1) {
			t.Fatal("유지가 판 변경")
		}
		var judgment string
		if err := f.pool.QueryRow(t.Context(), `SELECT judgment_input FROM public.operation_log WHERE operation_kind='keep' ORDER BY applied_at DESC LIMIT 1`).Scan(&judgment); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(judgment, "request_id") || !strings.Contains(judgment, f.agent.String()) {
			t.Fatal("정규화 판단 입력")
		}
		args["body"] = "keep과 혼용"
		f.call(t, "node_update", args, "invalid_argument")
	})
	t.Run("supersede", func(t *testing.T) {
		args := f.args("node_create")
		args["layer"], args["body"], args["derived_from"], args["derivation_kind"], args["evidence_state"], args["supersedes_context_id"] = "derived", "대체 파생", []string{f.source.String()}, "proposition", "observation", f.derived.String()
		value := f.call(t, "node_create", args, "")
		if value["node"].(map[string]any)["supersedes_context_id"] != f.derived.String() {
			t.Fatal("대체 대상")
		}
	})
	t.Run("event null end and wrong layer fields", func(t *testing.T) {
		args := f.args("node_update")
		args["context_id"], args["expected_version"], args["event_end"] = f.secondEvent.String(), 1, nil
		value := f.call(t, "node_update", args, "")
		if value["node"].(map[string]any)["event_end"] != nil {
			t.Fatal("종료 null")
		}
		args["expected_version"], args["valid_to"] = 2, nil
		delete(args, "event_end")
		f.call(t, "node_update", args, "invalid_argument")
	})
	t.Run("restore web channel", func(t *testing.T) {
		if _, err := f.database.SetContextDeleted(t.Context(), f.graph, f.source, f.principal.AccountID, true, 0); err != nil {
			t.Fatal(err)
		}
		args := f.args("node_restore")
		args["context_id"] = f.source.String()
		value := f.call(t, "node_restore", args, "not_supported")
		if value["alternative_channel"] != "web" {
			t.Fatal("웹 대체 채널")
		}
	})
	t.Run("relation identity and cycle", func(t *testing.T) {
		args := f.args("relation_confirm")
		args["relation_type"], args["from_context_id"], args["to_context_id"] = "relates_to", f.secondEvent.String(), f.firstEvent.String()
		first := f.id(t, "relation_confirm", args, "relation", "relation_id")
		args["from_context_id"], args["to_context_id"] = f.firstEvent.String(), f.secondEvent.String()
		if f.id(t, "relation_confirm", args, "relation", "relation_id") != first {
			t.Fatal("무방향 정체성")
		}
		args["relation_type"], args["from_context_id"], args["to_context_id"] = "precedes", f.secondEvent.String(), f.firstEvent.String()
		f.call(t, "relation_confirm", args, "invalid_argument")
	})
}

func TestMCP06ConcurrentReplayAndCurrentGuardsIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	args := f.args("node_update")
	args["context_id"], args["expected_version"], args["body"], args["request_id"] = f.derived.String(), 1, "한 번만 적용", "request_key_00001"
	var wg sync.WaitGroup
	results := make([]*sdk.CallToolResult, 16)
	failures := make([]error, 16)
	for i := range 16 {
		wg.Go(func() {
			handlers := f.handlers
			if i%2 == 1 {
				handlers = f.other
			}
			results[i], failures[i] = callMCPWrite(t.Context(), handlers, f.principal, "node_update", args)
		})
	}
	wg.Wait()
	for i, result := range results {
		writeContent(t, result, failures[i], "")
		if result.Content[0].(*sdk.TextContent).Text != results[0].Content[0].(*sdk.TextContent).Text {
			t.Fatal("병행 재생 결과")
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE context_id=$1 AND operation_kind='update' AND result='applied'`, f.derived.String()).Scan(&count); err != nil || count != 1 {
		t.Fatal("중복 감사", count, err)
	}
	args["body"] = "같은 키 다른 인자"
	f.call(t, "node_update", args, "invalid_argument")
	args["body"] = "한 번만 적용"
	t.Run("foreign client", func(t *testing.T) {
		p := f.principal
		p.ClientID = "second"
		r, e := callMCPWrite(t.Context(), f.handlers, p, "node_update", args)
		writeContent(t, r, e, "invalid_argument")
	})
	t.Run("lost grade", func(t *testing.T) {
		other, _ := model.NewID()
		if err := f.database.CreateAccount(t.Context(), store.Account{ID: other, LoginID: "other_owner", PasswordHash: "fixture_hash", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := f.database.GrantGraph(t.Context(), f.graph, f.principal.AccountID, other, store.GrantSubjectAccount, model.GraphGradeOwner); err != nil {
			t.Fatal(err)
		}
		if err := f.database.GrantGraph(t.Context(), f.graph, other, f.principal.AccountID, store.GrantSubjectAccount, model.GraphGradeViewer); err != nil {
			t.Fatal(err)
		}
		value := f.call(t, "node_update", args, "permission_denied")
		if value["required_grade"] != "editor" {
			t.Fatal("필요 등급")
		}
		if err := f.database.RevokeGraphGrant(t.Context(), f.graph, store.GrantSubjectAccount, f.principal.AccountID); err != nil {
			t.Fatal(err)
		}
		f.call(t, "node_update", args, "not_found")
		if err := f.database.GrantGraph(t.Context(), f.graph, other, f.principal.AccountID, store.GrantSubjectAccount, model.GraphGradeEditor); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("revoked agent", func(t *testing.T) {
		if err := f.database.RevokeAgent(t.Context(), f.principal.AccountID, f.agent); err != nil {
			t.Fatal(err)
		}
		f.call(t, "node_update", args, "invalid_argument")
	})
}

func TestMCP06ResultFailureRollbackIntegration(t *testing.T) {
	for _, mode := range []string{"result", "commit", "rejected audit"} {
		t.Run(mode, func(t *testing.T) {
			f := newMCPWriteFixture(t)
			args := f.args("node_update")
			args["context_id"], args["expected_version"], args["body"], args["request_id"] = f.derived.String(), 1, "롤백 본문", "request_key_rollback"
			if mode == "rejected audit" {
				args["expected_version"] = 9
			}
			sql := `CREATE FUNCTION public.mcp06_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic private failure'; END $$; `
			switch mode {
			case "result":
				sql += `CREATE TRIGGER mcp06_fail BEFORE UPDATE ON public.idempotency_record FOR EACH ROW EXECUTE FUNCTION public.mcp06_fail()`
			case "commit":
				sql += `CREATE CONSTRAINT TRIGGER mcp06_fail AFTER UPDATE ON public.idempotency_record DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.mcp06_fail()`
			case "rejected audit":
				sql += `CREATE TRIGGER mcp06_fail BEFORE INSERT ON public.operation_log FOR EACH ROW EXECUTE FUNCTION public.mcp06_fail()`
			}
			if _, err := f.pool.Exec(t.Context(), sql); err != nil {
				t.Fatal(err)
			}
			before := mcpWriteFingerprint(t, f.pool)
			value := f.call(t, "node_update", args, "internal")
			data, _ := json.Marshal(value)
			if strings.Contains(string(data), "synthetic") {
				t.Fatal("원문 노출")
			}
			if before != mcpWriteFingerprint(t, f.pool) {
				t.Fatal("결과·업무·감사·누계·카운터 롤백 실패")
			}
			table := "public.idempotency_record"
			if mode == "rejected audit" {
				table = "public.operation_log"
			}
			if _, err := f.pool.Exec(t.Context(), fmt.Sprintf("DROP TRIGGER mcp06_fail ON %s; DROP FUNCTION public.mcp06_fail()", table)); err != nil {
				t.Fatal(err)
			}
			code := ""
			if mode == "rejected audit" {
				code = "version_conflict"
			}
			f.call(t, "node_update", args, code)
			if mode == "rejected audit" {
				after := mcpWriteFingerprint(t, f.pool)
				f.call(t, "node_update", args, code)
				if after != mcpWriteFingerprint(t, f.pool) {
					t.Fatal("거부 재생 중복")
				}
			}
		})
	}
}

func TestMCP06CurrentLimitsIntegration(t *testing.T) {
	for _, mode := range []string{"graph count", "stored characters", "write rate", "shrink above limit"} {
		t.Run(mode, func(t *testing.T) {
			f := newMCPWriteFixture(t)
			policy := `"graphs_per_account":1`
			tool := "graph_create"
			args := map[string]any{"name": "한도 초과", "request_id": "request_key_limit"}
			limit := "graphs_per_account"
			if mode != "graph count" {
				tool = "node_update"
				args = f.args(tool)
				args["context_id"], args["expected_version"], args["body"], args["request_id"] = f.derived.String(), 1, strings.Repeat("가", 100), "request_key_limit"
				policy = `"stored_characters_per_graph":1`
				limit = "stored_characters_per_graph"
				if mode == "write rate" {
					policy = `"writes_per_minute":1`
					limit = "writes_per_minute"
				}
				if mode == "shrink above limit" {
					args["body"] = "가"
				}
			}
			plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{%s}}`, f.principal.AccountID, policy))
			if err != nil {
				t.Fatal(err)
			}
			handlers, err := newMCPWriteHandlers(f.database, plans)
			if err != nil {
				t.Fatal(err)
			}
			r, e := callMCPWrite(t.Context(), handlers, f.principal, tool, args)
			code := "limit_exceeded"
			if mode == "shrink above limit" {
				code = ""
			}
			value := writeContent(t, r, e, code)
			if code != "" && (value["limit"] != limit || value["allowed"] != float64(1)) {
				t.Fatal("한도 부가 정보", value)
			}
			after := mcpWriteFingerprint(t, f.pool)
			// 같은 키는 더 느슨한 현재 플랜이어도 최초 완료 거부를 재생한다.
			r, e = callMCPWrite(t.Context(), f.handlers, f.principal, tool, args)
			writeContent(t, r, e, code)
			if after != mcpWriteFingerprint(t, f.pool) {
				t.Fatal("한도 재생 중 부수 효과")
			}
		})
	}
}

func TestMCP06GuardAfterAdvisoryWaitIntegration(t *testing.T) {
	for _, mode := range []string{"grade revoked", "agent revoked", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newMCPWriteFixture(t)
			args := f.args("node_update")
			args["context_id"], args["expected_version"], args["body"], args["request_id"] = f.derived.String(), 1, "최초 결과", "request_key_wait_1"
			f.call(t, "node_update", args, "")
			other, _ := model.NewID()
			if mode == "grade revoked" {
				if err := f.database.CreateAccount(t.Context(), store.Account{ID: other, LoginID: "wait_owner", PasswordHash: "fixture_hash", CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				if err := f.database.GrantGraph(t.Context(), f.graph, f.principal.AccountID, other, store.GrantSubjectAccount, model.GraphGradeOwner); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(context.WithoutCancel(t.Context()))
			account := f.principal.AccountID
			digest := sha256.Sum256(append(account[:], []byte("request_key_wait_1")...))
			key := int64(binary.BigEndian.Uint64(digest[:8]))
			if _, err := lock.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type answer struct {
				result *sdk.CallToolResult
				err    error
			}
			done := make(chan answer, 1)
			go func() { r, e := callMCPWrite(ctx, f.other, f.principal, "node_update", args); done <- answer{r, e} }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting bool
				if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%pg_advisory_xact_lock%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("실제 자문 잠금 대기 관찰 실패")
				}
				time.Sleep(5 * time.Millisecond)
			}
			switch mode {
			case "grade revoked":
				if err := f.database.RevokeGraphGrant(t.Context(), f.graph, store.GrantSubjectAccount, f.principal.AccountID); err != nil {
					t.Fatal(err)
				}
			case "agent revoked":
				if err := f.database.RevokeAgent(t.Context(), f.principal.AccountID, f.agent); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			if err := lock.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case answer := <-done:
				if mode == "cancelled" {
					if answer.result != nil || !errors.Is(answer.err, context.Canceled) {
						t.Fatal("대기 취소", answer.err)
					}
				} else {
					code := "not_found"
					if mode == "agent revoked" {
						code = "invalid_argument"
					}
					writeContent(t, answer.result, answer.err, code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("잠금 뒤 요청 종료 실패")
			}
			node, err := f.database.Context(t.Context(), f.graph, f.derived)
			if err != nil || node.Version != 2 {
				t.Fatal("재생 전 검사 중 업무 재적용", err)
			}
			var openTransactions int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state LIKE 'idle in transaction%'`).Scan(&openTransactions); err != nil || openTransactions != 0 {
				t.Fatal("종료 뒤 열린 트랜잭션", openTransactions, err)
			}
			recovery, stop := context.WithTimeout(t.Context(), 3*time.Second)
			defer stop()
			r, e := callMCPWrite(recovery, f.other, f.principal, "graph_create", map[string]any{"name": "잠금 뒤 새 쓰기", "request_id": "request_key_recover"})
			writeContent(t, r, e, "")
		})
	}
}

// 쓰기 모듈의 실제 OAuth/TLS 표본이다. 나머지 여섯 도구의 시험용 처리기는
// 호출되면 실패하며 전체 14종 조립 인수나 실제 호스트 지원을 뜻하지 않는다.
func TestMCP06ProtectedWriteTLSIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	config := authz.Config{Issuer: "https://issuer.test", BcryptCost: 4, AuthorizationCodes: f.database, AccessTokens: f.database, RefreshTokens: f.database, OAuth: &authz.OAuthConfig{Resource: "https://resource.test/mcp", Clients: map[string]authz.OAuthClient{"first": {RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "exact"}}}}
	service, err := authz.New(f.database, config)
	if err != nil {
		t.Fatal(err)
	}
	code, err := service.IssueAuthorizationCode(t.Context(), f.principal.AccountID, authz.AuthorizationRequest{ClientID: "first", RedirectURI: "http://127.0.0.1:49152/callback", Resource: config.OAuth.Resource, ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := service.ExchangeAuthorizationCode(t.Context(), code, "first", "http://127.0.0.1:49152/callback", config.OAuth.Resource, "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if err != nil {
		t.Fatal(err)
	}
	public, err := authz.NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	handlers := map[string]mcp.ToolHandler{}
	for name, handler := range f.handlers {
		handlers[name] = handler
	}
	for _, name := range []string{"agent_register", "graph_list", "graph_get", "node_get", "context_flow_get", "relation_list"} {
		handlers[name] = func(context.Context, mcp.Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			t.Error("쓰기 표본 밖의 도구 호출")
			return nil, fmt.Errorf("쓰기 인수 범위 밖")
		}
	}
	handler, err := newMCPHandler(public, []string{"https://allowed.test"}, handlers)
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(f.database, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true})
	app.mcp = handler
	server := httptest.NewTLSServer(app.handler())
	t.Cleanup(server.Close)
	server.Client().Timeout = 10 * time.Second
	exchange := func(tool string, args map[string]any, token string) (int, map[string]any) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "write-06", "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args, "_meta": map[string]any{sdk.MetaKeyProtocolVersion: "2026-07-28", sdk.MetaKeyClientCapabilities: map[string]any{}, sdk.MetaKeyClientInfo: map[string]any{"name": "forged-client", "version": "1"}}}})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "https://allowed.test")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", tool)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var value map[string]any
		data, err := io.ReadAll(response.Body)
		if err != nil || json.Unmarshal(data, &value) != nil {
			t.Fatal("HTTP JSON 응답", err)
		}
		if strings.Contains(string(data), tokens.AccessToken) || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("응답 자격/캐시 경계")
		}
		return response.StatusCode, value
	}
	for _, tool := range []string{"graph_create", "node_update"} {
		t.Run(tool, func(t *testing.T) {
			args := map[string]any{"name": "TLS 생성", "request_id": "request_key_tls_1"}
			if tool == "node_update" {
				args = f.args(tool)
				args["context_id"], args["expected_version"], args["body"], args["request_id"] = f.derived.String(), 1, "TLS 수정", "request_key_tls_2"
			}
			status, response := exchange(tool, args, tokens.AccessToken)
			if status != 200 {
				t.Fatal("보호 쓰기 HTTP", status)
			}
			result := response["result"].(map[string]any)
			if result["resultType"] != "complete" || result["isError"] == true || result["structuredContent"] == nil {
				t.Fatal("SDK 완료 외피", result)
			}
			after := mcpWriteFingerprint(t, f.pool)
			status, replay := exchange(tool, args, tokens.AccessToken)
			if status != 200 || after != mcpWriteFingerprint(t, f.pool) {
				t.Fatal("HTTP 재생 중복")
			}
			a, _ := json.Marshal(response, json.Deterministic(true))
			b, _ := json.Marshal(replay, json.Deterministic(true))
			if !bytes.Equal(a, b) {
				t.Fatal("HTTP 최초/재생 결과")
			}
			if tool == "node_update" {
				args["request_id"] = "request_key_tls_3"
				status, response = exchange(tool, args, tokens.AccessToken)
				result = response["result"].(map[string]any)
				domain := result["structuredContent"].(map[string]any)
				if status != 200 || result["isError"] != true || domain["code"] != "version_conflict" || domain["current_version"] != float64(2) {
					t.Fatal("HTTP 도메인 충돌", response)
				}
			}
		})
	}
	status, _ := exchange("graph_create", map[string]any{"name": "인증 거부"}, "invalid-token")
	if status != 401 {
		t.Fatal("Bearer 경계", status)
	}
}
