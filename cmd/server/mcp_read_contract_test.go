package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

type mcp07Embedder struct{}

func (mcp07Embedder) Embed(context.Context, string) ([]float64, error) {
	return nil, errors.New("private upstream response")
}
func (mcp07Embedder) ModelID() string { return "test:vector:768" }
func newMCP07Reads(t *testing.T, f *mcpWriteFixture, limits plan.AccountPlans) map[string]mcp.ToolHandler {
	t.Helper()
	flow := newMCP07Search(t, f)
	handlers, err := newMCPReadHandlers(f.database, limits, flow)
	if err != nil {
		t.Fatal(err)
	}
	return handlers
}

func newMCP07Search(t *testing.T, f *mcpWriteFixture) *search.Service {
	t.Helper()
	flow, err := search.New(f.database, mcp07Embedder{}, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 20, SemanticThreshold: 0.6, FoldThreshold: 0.95, GraphStage: search.GraphStageRelations, Consistency: search.ConsistencySnapshot}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return flow
}
func readMCP07(t *testing.T, h map[string]mcp.ToolHandler, p mcp.Principal, tool string, args map[string]any, code string) map[string]any {
	t.Helper()
	result, err := callMCPWrite(t.Context(), h, p, tool, args)
	return writeContent(t, result, err, code)
}

func TestMCP07GraphPagesFiltersAndIsolation(t *testing.T) {
	f := newMCPWriteFixture(t)
	h := newMCP07Reads(t, f, plan.AccountPlans{})
	graphs := []model.ID{f.graph}
	for i := range 3 {
		graphs = append(graphs, f.id(t, "graph_create", map[string]any{"name": fmt.Sprintf("공통_%d%%", i)}, "graph", "graph_id"))
	}
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if _, err := f.pool.Exec(t.Context(), "UPDATE public.context_graph SET last_activity_at=$1", at); err != nil {
		t.Fatal(err)
	}
	var got []string
	cursor := ""
	for range 4 {
		page := readMCP07(t, h, f.principal, "graph_list", map[string]any{"page_size": 1, "cursor": cursor}, "")
		items := page["graphs"].([]any)
		if len(items) != 1 {
			t.Fatal("같은 활동 시각의 페이지 크기")
		}
		item := items[0].(map[string]any)
		got = append(got, item["graph_id"].(string))
		if item["grade"] != "owner" {
			t.Fatal("유효 등급")
		}
		cursor = page["next_cursor"].(string)
	}
	if cursor != "" || len(got) != 4 {
		t.Fatal("목록 마지막 커서")
	}
	unlimited, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"graph_page":{"maximum":0}}}`, f.principal.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	large := readMCP07(t, newMCP07Reads(t, f, unlimited), f.principal, "graph_list", map[string]any{"page_size": int64(1<<63 - 1)}, "")
	if len(large["graphs"].([]any)) != 4 || large["next_cursor"] != "" {
		t.Fatal("무제한 플랜의 최대 정수 페이지")
	}
	for i := range got {
		if got[i] != graphs[3-i].String() {
			t.Fatal("동률 UUID 내림차순/중복/누락", got)
		}
	}
	for _, filter := range []map[string]any{{"name_filter": "_1%"}, {"grade_filter": []string{"owner"}, "name_filter": "_1%"}} {
		value := readMCP07(t, h, f.principal, "graph_list", filter, "")
		if len(value["graphs"].([]any)) != 1 {
			t.Fatal("이름 리터럴 부분 일치")
		}
	}
	other, _ := model.NewID()
	if err := f.database.CreateAccount(t.Context(), store.Account{ID: other, LoginID: "mcp07_viewer", PasswordHash: "fixture", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	p := mcp.Principal{AccountID: other, ClientID: "first"}
	if len(readMCP07(t, h, p, "graph_list", nil, "")["graphs"].([]any)) != 0 {
		t.Fatal("다른 계정 목록 노출")
	}
	readMCP07(t, h, p, "graph_get", map[string]any{"graph_id": f.graph.String()}, "not_found")
	if _, err := f.pool.Exec(t.Context(), "INSERT INTO public.graph_grant(graph_id,subject_type,subject_id,grade) VALUES($1,'account',$2,'viewer')", f.graph.String(), other.String()); err != nil {
		t.Fatal(err)
	}
	page := readMCP07(t, h, p, "graph_list", map[string]any{"grade_filter": []string{"viewer"}}, "")
	if len(page["graphs"].([]any)) != 1 || page["graphs"].([]any)[0].(map[string]any)["grade"] != "viewer" {
		t.Fatal("계정 유효 등급 필터")
	}
	readMCP07(t, h, p, "graph_get", map[string]any{"graph_id": f.graph.String()}, "")
	if _, err := f.pool.Exec(t.Context(), "DELETE FROM public.graph_grant WHERE graph_id=$1 AND subject_id=$2", f.graph.String(), other.String()); err != nil {
		t.Fatal(err)
	}
	readMCP07(t, h, p, "graph_get", map[string]any{"graph_id": f.graph.String()}, "not_found")
	for _, bad := range []string{"%%%", base64.RawURLEncoding.EncodeToString([]byte("2026-10-20T00:00:00Z|invalid"))} {
		value := readMCP07(t, h, f.principal, "graph_list", map[string]any{"cursor": bad}, "invalid_argument")
		if value["field"] != "cursor" {
			t.Fatal("커서 필드")
		}
	}
}

func TestMCP07HopRelationsAndDeletedGraph(t *testing.T) {
	f := newMCPWriteFixture(t)
	h := newMCP07Reads(t, f, plan.AccountPlans{})
	args := map[string]any{"graph_id": f.graph.String(), "context_id": f.source.String(), "hops": 0}
	value := readMCP07(t, h, f.principal, "node_get", args, "")
	if len(value["contexts"].([]any)) != 1 || len(value["references"].([]any)) != 0 {
		t.Fatal("실제 0홉")
	}
	args["hops"], args["direction"], args["traversal_filter"] = 1, "in", []string{"derived_from"}
	value = readMCP07(t, h, f.principal, "node_get", args, "")
	if len(value["contexts"].([]any)) != 2 || len(value["references"].([]any)) != 1 {
		t.Fatal("실제 필터/역방향 참조", value)
	}
	for _, direction := range []string{"out", "both"} {
		args["direction"] = direction
		result := readMCP07(t, h, f.principal, "node_get", args, "")
		expected := 1
		if direction == "both" {
			expected = 2
		}
		if len(result["contexts"].([]any)) != expected {
			t.Fatal("탐색 방향", direction)
		}
	}
	members := readMCP07(t, h, f.principal, "node_get", map[string]any{"graph_id": f.graph.String(), "context_id": f.firstEvent.String(), "hops": 1, "direction": "out", "traversal_filter": []string{"member_refs"}}, "")
	if len(members["contexts"].([]any)) != 2 || members["references"].([]any)[0].(map[string]any)["reference_type"] != "member_refs" {
		t.Fatal("구성원 참조의 공개/저장 이름 사상", members)
	}
	limits, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"max_hop_nodes":1}}`, f.principal.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	bounded := newMCP07Reads(t, f, limits)
	value = readMCP07(t, bounded, f.principal, "node_get", args, "")
	if len(value["contexts"].([]any)) != 1 || value["truncated"].(map[string]any)["result_truncated"] != true {
		t.Fatal("정상 홉 절단", value)
	}
	for _, kind := range []string{"precedes", "causes", "relates_to"} {
		a := f.args("relation_confirm")
		a["relation_type"], a["from_context_id"], a["to_context_id"] = kind, f.firstEvent.String(), f.secondEvent.String()
		f.call(t, "relation_confirm", a, "")
	}
	a := map[string]any{"graph_id": f.graph.String(), "context_id": f.firstEvent.String(), "page_size": 1, "state_filter": []string{"confirmed"}}
	cursor := ""
	var ids []string
	for range 3 {
		a["cursor"] = cursor
		v := readMCP07(t, h, f.principal, "relation_list", a, "")
		rows := v["relations"].([]any)
		if len(rows) != 1 {
			t.Fatal("관계 페이지")
		}
		ids = append(ids, rows[0].(map[string]any)["relation_id"].(string))
		cursor = v["next_cursor"].(string)
	}
	if cursor != "" || !(ids[0] > ids[1] && ids[1] > ids[2]) {
		t.Fatal("관계 커서 정렬", ids)
	}
	unlimited, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"relation_page":{"maximum":0}}}`, f.principal.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	large := readMCP07(t, newMCP07Reads(t, f, unlimited), f.principal, "relation_list", map[string]any{"graph_id": f.graph.String(), "context_id": f.firstEvent.String(), "page_size": int64(1<<63 - 1)}, "")
	if len(large["relations"].([]any)) != 3 {
		t.Fatal("무제한 플랜의 최대 정수 관계 페이지")
	}
	a["cursor"], a["type_filter"] = "", []string{"causes"}
	v := readMCP07(t, h, f.principal, "relation_list", a, "")
	if v["relations"].([]any)[0].(map[string]any)["relation_type"] != "causes" {
		t.Fatal("관계 유형 필터")
	}
	a["cursor"] = base64.RawURLEncoding.EncodeToString([]byte("not-a-uuid"))
	readMCP07(t, h, f.principal, "relation_list", a, "invalid_argument")
	a["cursor"], a["context_id"] = "", f.source.String()
	readMCP07(t, h, f.principal, "relation_list", a, "invalid_argument")
	other := f.id(t, "graph_create", map[string]any{"name": "다른 그래프"}, "graph", "graph_id")
	readMCP07(t, h, f.principal, "node_get", map[string]any{"graph_id": other.String(), "context_id": f.source.String()}, "not_found")
	f.call(t, "node_discard", map[string]any{"graph_id": f.graph.String(), "context_id": f.derived.String(), "agent_id": f.agent.String()}, "")
	readMCP07(t, h, f.principal, "node_get", map[string]any{"graph_id": f.graph.String(), "context_id": f.derived.String()}, "not_found")
	if _, err := f.pool.Exec(t.Context(), "UPDATE public.context_graph SET deleted_at=now() WHERE graph_id=$1", f.graph.String()); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"graph_get", "node_get", "relation_list", "context_flow_get"} {
		a := map[string]any{"graph_id": f.graph.String()}
		if tool == "context_flow_get" {
			a["work_context"] = "작업"
		} else if tool != "graph_get" {
			a["context_id"] = f.firstEvent.String()
		}
		readMCP07(t, h, f.principal, tool, a, "not_found")
	}
}

func TestMCP07RealFlowPartialBudgetAndIsolation(t *testing.T) {
	f := newMCPWriteFixture(t)
	h := newMCP07Reads(t, f, plan.AccountPlans{})
	args := map[string]any{"graph_id": f.graph.String(), "work_context": "원천 사건", "scope": "local", "as_of": "2026-10-20T17:00:00Z"}
	value := readMCP07(t, h, f.principal, "context_flow_get", args, "")
	rows := value["contexts"].([]any)
	if len(rows) == 0 {
		t.Fatal("실제 검색 결과 없음", value)
	}
	if value["channels"].(map[string]any)["semantic"].(map[string]any)["failure"] != "embedding_unavailable" {
		t.Fatal("일부 채널 장애가 정상 결과에서 누락")
	}
	for _, raw := range rows {
		node := raw.(map[string]any)
		if node["graph_id"] != f.graph.String() || node["rank"] == nil || node["origin_kinds"] == nil || node["entry_distance"] == nil {
			t.Fatal("평면 검색 메타데이터", node)
		}
	}
	args["budget"] = 1
	value = readMCP07(t, h, f.principal, "context_flow_get", args, "")
	if len(value["contexts"].([]any)) != 0 || value["truncated"].(map[string]any)["result_truncated"] != true || value["truncated"].(map[string]any)["excluded"].(float64) <= 0 {
		t.Fatal("최상위 본문 미만 예산의 정상 빈 절단", value)
	}
	other := f.id(t, "graph_create", map[string]any{"name": "빈 그래프"}, "graph", "graph_id")
	args["graph_id"] = other.String()
	delete(args, "budget")
	value = readMCP07(t, h, f.principal, "context_flow_get", args, "")
	if len(value["contexts"].([]any)) != 0 || value["truncated"].(map[string]any)["result_truncated"] != false {
		t.Fatal("다른 그래프 검색이 섞이거나 빈 결과를 오류 처리")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := callMCPWrite(ctx, h, f.principal, "context_flow_get", args)
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("실제 경계의 취소")
	}
	var active int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='idle in transaction'").Scan(&active); err != nil || active != 0 {
		t.Fatal("읽기 트랜잭션 잔류", active, err)
	}
}

func TestMCP07ReadAdapterErrors(t *testing.T) {
	for _, sample := range []struct {
		err   error
		field bool
	}{{fmt.Errorf("decode: %w", store.ErrInvalidCursor), true}, {model.FieldError{Field: "private"}, false}, {&store.StorageError{}, false}} {
		err := mcpReadError(sample.err)
		_, isField := err.(model.FieldError)
		if isField != sample.field {
			t.Fatal("내부 장애/커서 구분", err)
		}
		if !sample.field && !errors.Is(err, sample.err) {
			t.Fatal("오류 원인 손실")
		}
	}
	if !reflect.DeepEqual(mcpReadError(model.ErrNotFound), model.ErrNotFound) {
		t.Fatal("부재 원인 손실")
	}
}
