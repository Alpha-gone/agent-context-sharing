// Command indexer는 external 배치의 색인 소비자만 실행한다.
// 서버와 같은 배포 환경을 사용하며 HTTP 수신기와 주기 작업은 시작하지 않는다.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

const workerShutdownTimeout = 10 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := configureAndRun(ctx, os.Getenv, runConfigured); err != nil {
		slog.Error("색인 실행기 종료", "reason_code", "indexer_failed")
		os.Exit(1)
	}
}

func configureAndRun(ctx context.Context, env config.Environment, start func(context.Context, config.Config) error) error {
	cfg, err := config.Load(env)
	if err != nil {
		return fmt.Errorf("배포 구성 검증: %w", err)
	}
	if cfg.IndexWorkerPlacement != config.PlacementExternal {
		return fmt.Errorf("별도 색인 실행기는 INDEX_WORKER_PLACEMENT=external이 필요하다")
	}
	return start(ctx, cfg)
}

func runConfigured(ctx context.Context, cfg config.Config) (resultErr error) {
	source, err := diagnostic.NewSource(cfg.Diagnostic.InstanceID)
	if err != nil {
		return err
	}
	recorder, err := diagnostic.NewRecorder(ctx, cfg.Diagnostic.Directory, source)
	if err != nil {
		return fmt.Errorf("진단 파일 준비: %w", err)
	}
	sink, _ := diagnostic.LevelSink(recorder, cfg.Diagnostic.Level)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, recorder.Close(cleanup))
	}()
	indexReporter, _ := diagnostic.NewReporter(diagnostic.Index, sink, source)
	systemReporter, _ := diagnostic.NewReporter(diagnostic.System, sink, source)

	database, err := store.New(ctx, cfg.DatabaseURL, cfg.GraphName, &store.RelationProposalConfig{
		AdjacencyWindow: cfg.RelationAdjacencyWindow, SimilarityThreshold: cfg.RelationSimilarityThreshold, Limit: cfg.RelationProposalLimit,
	}, func(account model.ID) int { return cfg.AccountPlans.For(account).GraceDays }, store.IndexTargets(cfg.IndexTargetLayers))
	if err != nil {
		return fmt.Errorf("색인 DB 준비: %w", err)
	}
	database.SetDiagnostics(systemReporter)
	database.SetOperationDiagnostics(nil, indexReporter)
	var worker *index.Worker
	defer func() {
		if worker != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerShutdownTimeout)
			defer cancel()
			if err := worker.Close(cleanup); err != nil {
				// 미반환 연결을 기다리는 풀 종료로 실패 보고가 무기한 막히지 않게 한다.
				resultErr = errors.Join(resultErr, fmt.Errorf("색인 작업자 종료: %w", err))
				return
			}
		}
		database.Close()
	}()
	worker, err = index.New(database, index.Config{
		Provider: cfg.EmbeddingProvider, APIKey: cfg.GeminiAPIKey, BaseURL: cfg.EmbeddingBaseURL,
		Model: cfg.EmbeddingModel, VectorType: cfg.EmbeddingVectorType, Dimension: cfg.EmbeddingDimension,
	}, nil, slog.Default())
	if err != nil {
		return fmt.Errorf("색인 작업자 준비: %w", err)
	}
	if err := database.CheckEmbeddingSchema(ctx, cfg.EmbeddingVectorType, cfg.EmbeddingDimension); err != nil {
		return fmt.Errorf("임베딩 DB 구성 확인: %w", err)
	}
	worker.SetDiagnostics(indexReporter)
	worker.Start(ctx)
	<-ctx.Done()
	return nil
}
