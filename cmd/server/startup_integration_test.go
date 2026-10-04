package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/config"
)

func TestStartupFailureClosesWorkersIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_URL이 필요하다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 기동 실패 통합 테스트를 건너뛴다")
	}
	// 조립 실패 뒤 남은 색인 작업이 있어도 실제 임베딩 제공자로 보내지 않는다.
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer embeddings.Close()
	embeddingURL, err := url.Parse(embeddings.URL)
	if err != nil {
		t.Fatal(err)
	}
	dimension := 768
	if text := os.Getenv("EMBEDDING_DIMENSION"); text != "" {
		dimension, err = strconv.Atoi(text)
		if err != nil {
			t.Fatal(err)
		}
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	resource := &url.URL{Scheme: "https", Host: "service.test", Path: "/mcp"}
	issuer := &url.URL{Scheme: "https", Host: "service.test"}
	for _, placement := range []config.ComponentPlacement{config.PlacementEmbedded, config.PlacementExternal} {
		for _, failure := range []string{"검색 실행기 준비", "인가 서버 준비", "HTTP 서버 수신"} {
			t.Run(string(placement)+"/"+failure, func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				cfg := config.Config{
					DatabaseURL: databaseURL, GraphName: graphName, HTTPAddr: listener.Addr().String(),
					EmbeddingProvider: "ollama", EmbeddingBaseURL: embeddingURL, EmbeddingModel: "startup-test", EmbeddingVectorType: "vector", EmbeddingDimension: dimension,
					SearchExecution: config.SearchExecutionParallel, SearchCandidateLimit: 50, SearchSemanticThreshold: .5, SearchFoldThreshold: .9, SearchGraphStage: config.SearchGraphStageBaseline,
					RelationAdjacencyWindow: time.Hour, RelationSimilarityThreshold: .8, RelationProposalLimit: 10,
					BcryptCost: 4, ResourceServerURL: resource, AuthorizationServerURL: issuer, MCPAllowedOrigins: []*url.URL{issuer},
					OAuthClients:         map[string][]*url.URL{"test-client": {{Scheme: "http", Host: "127.0.0.1", Path: "/callback"}}},
					IndexWorkerPlacement: placement, AuthorizationServerPlacement: config.PlacementEmbedded, IndexTargetLayers: config.IndexTargetLayersAll, TLSMode: config.TLSModeProxy,
				}
				switch failure {
				case "검색 실행기 준비":
					cfg.SearchExecution = "invalid"
				case "인가 서버 준비":
					cfg.BcryptCost = 0
				}
				before := serverWorkerGoroutines()
				if err := runConfigured(cfg); err == nil || !strings.Contains(err.Error(), failure) {
					t.Fatalf("의도한 기동 실패가 아니다: %v", err)
				}
				// done을 닫은 고루틴의 마지막 반환까지 잠깐 기다리되, 살아 있는 소비 루프는
				// 없어져야 한다. 스케줄링 시점 차이를 작업자 누수로 오인하지 않는다.
				deadline := time.Now().Add(time.Second)
				for serverWorkerGoroutines() > before && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if after := serverWorkerGoroutines(); after > before {
					t.Fatalf("기동 실패 뒤 작업자 고루틴이 남았다: 이전 %d, 이후 %d", before, after)
				}
			})
		}
	}
}

func serverWorkerGoroutines() int {
	buffer := make([]byte, 1<<20)
	stacks := string(buffer[:runtime.Stack(buffer, true)])
	count := 0
	for stack := range strings.SplitSeq(stacks, "\n\n") {
		if strings.Contains(stack, "agent_context_sharing/internal/index.(*Worker).Start.func") || strings.Contains(stack, "agent_context_sharing/cmd/server.(*periodicWorker).Start.func") || strings.Contains(stack, "agent_context_sharing/cmd/server.(*periodicWorker).runPeriodically") {
			count++
		}
	}
	return count
}
