package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcp08HTTPEmbedder struct {
	address string
	client  *http.Client
}

func (e mcp08HTTPEmbedder) Embed(ctx context.Context, _ string) ([]float64, error) {
	request, err := http.NewRequestWithContext(ctx, "POST", e.address, nil)
	if err != nil {
		return nil, err
	}
	response, err := e.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return nil, errors.New("private provider result")
}
func (mcp08HTTPEmbedder) ModelID() string { return "test:vector:768" }

func TestMCP08RequestBudgetCancelsProviderIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	entered, stopped := make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(stopped) }))
	t.Cleanup(provider.Close)
	flow, err := search.New(f.database, mcp08HTTPEmbedder{address: provider.URL, client: provider.Client()}, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 20, SemanticThreshold: 0.6, FoldThreshold: 0.95, GraphStage: search.GraphStageRelations, Consistency: search.ConsistencySnapshot}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reads, err := newMCPReadHandlers(f.database, plan.AccountPlans{}, flow)
	if err != nil {
		t.Fatal(err)
	}
	handlers := mcp08Handlers(func(context.Context, mcp.Principal, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		t.Error("범위 밖 도구 호출")
		return nil, nil
	})
	for name, h := range reads {
		handlers[name] = h
	}
	authorization := &mcp08Authorization{principal: authz.BearerPrincipal{AccountID: f.principal.AccountID, ClientID: f.principal.ClientID}}
	handler, err := newMCPHandler(authorization, []string{"https://allowed.test"}, handlers)
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(f.database, slog.New(slog.NewTextHandler(io.Discard, nil)), transportSecurity{directTLS: true})
	app.mcp = withMCPRequestBudget(handler, 200*time.Millisecond)
	server := httptest.NewTLSServer(app.handler())
	t.Cleanup(server.Close)
	server.Client().Timeout = 3 * time.Second
	request := mcp08Request(t, t.Context(), server.URL, "context_flow_get", `{"graph_id":"`+f.graph.String()+`","work_context":"작업"}`)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	if response.Header.Get("WWW-Authenticate") != "" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("제공자 시간 초과가 재인가/캐시 신호로 변환")
	}
	for _, stage := range []struct {
		name string
		done <-chan struct{}
	}{{"provider entered", entered}, {"provider stopped", stopped}} {
		select {
		case <-stage.done:
		case <-time.After(3 * time.Second):
			t.Fatal(stage.name)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.database.Ping(ctx); err != nil {
		t.Fatal("제공자 취소 뒤 저장소", err)
	}
}
