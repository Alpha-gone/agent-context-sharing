package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPQueryEmbeddingDeadlineReturnsPartialFlow(t *testing.T) {
	f := newMCPWriteFixture(t)
	var calls atomic.Int64
	canceled, release := make(chan struct{}, 1), make(chan struct{})
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			canceled <- struct{}{}
		case <-release:
		}
	}))
	defer provider.Close()
	defer close(release)
	address, err := url.Parse(provider.URL)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	embedder, err := index.New(f.database, index.Config{Provider: "gemini", APIKey: "synthetic-query-key", BaseURL: address, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 768}, provider.Client(), logger)
	if err != nil {
		t.Fatal(err)
	}
	flow, err := search.New(f.database, embedder, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 20, SemanticThreshold: .6, FoldThreshold: .95, GraphStage: search.GraphStageRelations, Consistency: search.ConsistencySnapshot}, logger)
	if err != nil {
		t.Fatal(err)
	}
	reads, err := newMCPReadHandlers(f.database, plan.AccountPlans{}, flow)
	if err != nil {
		t.Fatal(err)
	}
	handlers := mcp08Handlers(func(context.Context, mcp.Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		t.Error("검색 이외의 도구 호출")
		return nil, nil
	})
	for name, handler := range reads {
		handlers[name] = handler
	}
	authorization := &mcp08Authorization{principal: authz.BearerPrincipal{AccountID: f.principal.AccountID, ClientID: f.principal.ClientID}}
	handler, err := newMCPHandler(authorization, []string{"https://allowed.test"}, handlers)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	args := fmt.Sprintf(`{"graph_id":%q,"work_context":"원천 사건","scope":"local","as_of":"2026-10-20T17:00:00Z"}`, f.graph.String())
	response, err := server.Client().Do(mcp08Request(t, ctx, server.URL, "context_flow_get", args))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Error  any `json:"error"`
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Contexts []any `json:"contexts"`
				Channels map[string]struct {
					Failure string `json:"failure"`
				} `json:"channels"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("MCP 응답 해석: %v, %s", err, body)
	}
	value := result.Result.StructuredContent
	if response.StatusCode != http.StatusOK || result.Error != nil || result.Result.IsError || len(value.Contexts) == 0 || ctx.Err() != nil {
		t.Fatalf("제공자 지연 뒤 정상 부분 응답: %d %s, 요청=%v", response.StatusCode, body, ctx.Err())
	}
	if value.Channels["semantic"].Failure != "embedding_unavailable" || value.Channels["keyword"].Failure != "" || value.Channels["time"].Failure != "" || calls.Load() != 1 {
		t.Fatalf("채널 상태/제공자 호출: %+v/%d", value.Channels, calls.Load())
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("임베딩 전용 마감이 제공자 HTTP 요청을 취소하지 않았다")
	}
}
