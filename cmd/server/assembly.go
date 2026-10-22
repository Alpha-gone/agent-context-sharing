package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"sync"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

// serverAssembly는 수신 전 조립한 의존과 그 수명을 소유한다. 업무 객체와
// 공유하는 저장소·제공자는 각각 하나이며 DB 핸들을 패키지 밖으로 꺼내지 않는다.
type serverAssembly struct {
	app        *application
	database   *store.Store
	indexer    *index.Worker
	periodic   *periodicWorker
	recorder   *diagnostic.Recorder
	system     *diagnostic.Reporter
	jobContext context.Context
	logger     *slog.Logger
	placement  config.ComponentPlacement
	workers    []worker
	closeOnce  sync.Once
	closeErr   error
}

// assembleApplication은 검증된 구성을 받는다. providerClient의 수명과 복제 정책은
// index.New가 소유하며 nil이면 기본 TLS 클라이언트를 사용한다. 수신·소비 루프는 열지 않는다.
func assembleApplication(ctx context.Context, cfg config.Config, providerClient *http.Client, logger *slog.Logger) (_ *serverAssembly, resultErr error) {
	if logger == nil {
		logger = slog.Default()
	}
	a := &serverAssembly{logger: logger, placement: cfg.IndexWorkerPlacement}
	defer func() {
		if resultErr != nil {
			resultErr = a.close(resultErr, true)
		}
	}()
	source, err := diagnostic.NewSource(cfg.Diagnostic.InstanceID)
	if err != nil {
		return nil, err
	}
	a.recorder, err = diagnostic.NewRecorder(ctx, cfg.Diagnostic.Directory, source)
	if err != nil {
		return nil, fmt.Errorf("진단 파일 준비: %w", err)
	}
	sink, err := diagnostic.LevelSink(a.recorder, cfg.Diagnostic.Level)
	if err != nil {
		return nil, fmt.Errorf("진단 수준 준비: %w", err)
	}
	reports := make(map[diagnostic.Kind]*diagnostic.Reporter, 6)
	for _, kind := range []diagnostic.Kind{diagnostic.Request, diagnostic.Auth, diagnostic.Search, diagnostic.Index, diagnostic.Management, diagnostic.System} {
		reports[kind], err = diagnostic.NewReporter(kind, sink, source)
		if err != nil {
			return nil, fmt.Errorf("진단 종류 준비: %w", err)
		}
	}
	a.system = reports[diagnostic.System]
	a.jobContext = diagnostic.WithTrace(context.Background(), diagnostic.Trace{}.NewJob())
	a.system.RecordFields(a.jobContext, "INFO", "startup", "pending", "completed", false, diagnostic.Fields{System: diagnostic.SystemFields{Job: "startup"}})
	a.database, err = store.New(ctx, cfg.DatabaseURL, cfg.GraphName, &store.RelationProposalConfig{
		AdjacencyWindow: cfg.RelationAdjacencyWindow, SimilarityThreshold: cfg.RelationSimilarityThreshold, Limit: cfg.RelationProposalLimit,
	}, func(id model.ID) int { return cfg.AccountPlans.For(id).GraceDays }, store.IndexTargets(cfg.IndexTargetLayers))
	if err != nil {
		return nil, fmt.Errorf("데이터베이스 풀 준비: %w", err)
	}
	a.database.SetDiagnostics(a.system)
	a.database.SetOperationDiagnostics(reports[diagnostic.Management], reports[diagnostic.Index])
	a.periodic, err = newPeriodicWorker(a.database, cfg.AccountPlans, logger)
	if err != nil {
		return nil, fmt.Errorf("주기 작업 준비: %w", err)
	}
	a.workers = append(a.workers, a.periodic)
	a.periodic.diagnostics = a.system
	a.indexer, err = index.New(a.database, index.Config{
		Provider: cfg.EmbeddingProvider, APIKey: cfg.GeminiAPIKey, BaseURL: cfg.EmbeddingBaseURL,
		Model: cfg.EmbeddingModel, VectorType: cfg.EmbeddingVectorType, Dimension: cfg.EmbeddingDimension,
	}, providerClient, logger)
	if err != nil {
		return nil, fmt.Errorf("색인 작업자 준비: %w", err)
	}
	// external에서도 질의 임베딩 transport를 소유한다. 시작 여부와 별개로 정리한다.
	a.workers = append(a.workers, a.indexer)
	a.indexer.SetDiagnostics(reports[diagnostic.Index])
	if err := a.database.CheckEmbeddingSchema(ctx, cfg.EmbeddingVectorType, cfg.EmbeddingDimension); err != nil {
		return nil, fmt.Errorf("임베딩 DB 구성 확인: %w", err)
	}
	flow, err := search.New(a.database, a.indexer, search.Config{
		Execution: search.Execution(cfg.SearchExecution), CandidateLimit: cfg.SearchCandidateLimit,
		SemanticThreshold: cfg.SearchSemanticThreshold, FoldThreshold: cfg.SearchFoldThreshold,
		GraphStage: search.GraphStage(cfg.SearchGraphStage), GlobalFallback: cfg.SearchGlobalFallback,
		EvidencePathSelection: cfg.SearchEvidencePathSelection, AdaptiveRouting: cfg.SearchAdaptiveRouting,
		AdaptiveDirectThreshold: cfg.SearchAdaptiveDirectThreshold, AdaptiveMarginThreshold: cfg.SearchAdaptiveMarginThreshold,
		Consistency: search.ConsistencySnapshot,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("검색 실행기 준비: %w", err)
	}
	flow.SetDiagnostics(reports[diagnostic.Search])
	authorization, err := authz.New(a.database, authz.Config{
		Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: cfg.BcryptCost, OAuth: oauthConfiguration(cfg),
		AuthorizationCodes: a.database, AccessTokens: a.database, RefreshTokens: a.database, Connections: a.database,
	})
	if err != nil {
		return nil, fmt.Errorf("인가 서버 준비: %w", err)
	}
	authorization.SetDiagnostics(reports[diagnostic.Auth])
	transport := transportSecurity{directTLS: cfg.TLSMode == config.TLSModeDirect, trustedProxies: cfg.TrustedProxies}
	webServer, err := web.New(webAuthentication{service: authorization}, a.database, web.Config{
		Authorization: webAuthentication{service: authorization}, Connections: webAuthentication{service: authorization},
		Diagnostics: reports[diagnostic.Request], SecureCookie: transport.isTLSRequest, ClientAddress: transport.clientAddress, Plans: cfg.AccountPlans,
	})
	if err != nil {
		return nil, fmt.Errorf("웹 서버 준비: %w", err)
	}
	authorizationHandler, err := authz.NewHandler(authorization)
	if err != nil {
		return nil, fmt.Errorf("인가 서버 경로 준비: %w", err)
	}
	handlers, err := newMCPReadHandlers(a.database, cfg.AccountPlans, flow, reports[diagnostic.Request])
	if err != nil {
		return nil, fmt.Errorf("MCP 읽기 준비: %w", err)
	}
	writes, err := newMCPWriteHandlers(a.database, cfg.AccountPlans, reports[diagnostic.Request])
	if err != nil {
		return nil, fmt.Errorf("MCP 쓰기 준비: %w", err)
	}
	maps.Copy(handlers, writes)
	handlers["agent_register"], err = mcp.NewAgentHandler(a.database)
	if err != nil {
		return nil, fmt.Errorf("MCP 등록 준비: %w", err)
	}
	mcpHandler, err := newMCPHandler(authorizationHandler, cfg.MCPAllowedOrigins, handlers, reports[diagnostic.Request])
	if err != nil {
		return nil, fmt.Errorf("MCP 전송 준비: %w", err)
	}
	a.app = newApplication(a.database, logger, transport)
	a.app.diagnostics = reports[diagnostic.Request]
	a.app.web, a.app.publicKeys, a.app.oauthMetadata, a.app.oauthTokens = webServer, authorizationHandler, authorizationHandler, authorizationHandler
	a.app.mcp = mcpHandler
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *serverAssembly) startWorkers(ctx context.Context) {
	if a.placement == config.PlacementEmbedded {
		a.indexer.Start(ctx)
	}
	a.periodic.Start(ctx)
}

// 요청 drain 이후 또는 부분 조립 실패 때 호출한다. 호출자의 취소와 독립된
// 작업자/진단 예산을 사용하고 미반환 작업자가 있으면 풀을 강제로 닫지 않는다.
func (a *serverAssembly) close(cause error, poolCloseSafe bool) error {
	a.closeOnce.Do(func() {
		closePool := func() {}
		if a.database != nil {
			closePool = a.database.Close
		}
		resourceErr := closeServerResources(context.Background(), a.logger, closePool, poolCloseSafe, a.workers...)
		if a.system != nil {
			result, reason, level := "completed", "completed", "INFO"
			if cause != nil || resourceErr != nil {
				result, reason, level = "failed", "store_unavailable", "ERROR"
			}
			a.system.RecordFields(a.jobContext, level, "shutdown", result, reason, false, diagnostic.Fields{System: diagnostic.SystemFields{Job: "shutdown"}})
		}
		var diagnosticErr error
		if a.recorder != nil {
			diagnosticErr = a.recorder.Close(context.Background())
		}
		a.closeErr = errors.Join(resourceErr, diagnosticErr)
	})
	return errors.Join(cause, a.closeErr)
}
