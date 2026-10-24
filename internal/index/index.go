// Package index는 비동기 색인 작업과 임베딩 제공자 호출을 소유한다.
package index

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// reindexProgressInterval은 재색인이 남아 있는 동안 진행률을 구조화 로그에 남기는 간격이다.
const reindexProgressInterval = time.Minute

// 대기 작업이 없는 회차의 간격이다. 회차마다 트랜잭션을 열어 대기 작업을 찾으므로 간격이
// 고정이면 유휴 상태에서도 쉬지 않고 초당 한 트랜잭션을 낸다. 빈 회차가 이어지면 간격을 두
// 배로 늘리고 작업을 처리한 회차에서 최소값으로 되돌려, 유휴 부하를 줄이면서 작업이 들어온
// 뒤의 처리 지연은 idleIntervalMax 안으로 묶는다.
const (
	idleIntervalMin = time.Second
	idleIntervalMax = 30 * time.Second
)

// Config는 하나의 배포가 쓰는 임베딩 제공자와 벡터 계약이다.
type Config struct {
	Provider   string
	APIKey     string
	BaseURL    *url.URL
	Model      string
	VectorType string
	Dimension  int
}

// DimensionError는 제공자가 현재 배포 구성과 다른 차원의 벡터를 돌려줬음을 나타낸다.
type DimensionError struct{ Got, Want int }

// Error는 재시도로 해결되지 않는 차원 불일치를 설명한다.
func (error DimensionError) Error() string {
	return fmt.Sprintf("임베딩 차원이 다르다: %d, 기대값 %d", error.Got, error.Want)
}

// maxEmbeddingResponseBytes는 제공자 응답 본문에서 읽을 최대 크기다.
//
// 상한이 없으면 잘못 설정된 제공자가 보낸 큰 응답이 작업자와 검색 경로의 메모리를 그대로
// 늘린다. HNSW가 허용하는 벡터를 넉넉히 담을 수 있는 크기로 둔다.
const maxEmbeddingResponseBytes = 8 << 20

// Worker는 짧은 확보·저장 트랜잭션 사이에서 제공자를 호출하는 비동기 소비자다.
type Worker struct {
	diagnostics diagnostic.Observer
	store       *store.Store
	proposer    RelationProposer
	config      Config
	httpClient  *http.Client
	logger      *slog.Logger
	cancel      context.CancelFunc
	done        chan struct{}
	startOnce   sync.Once
	stopOnce    sync.Once
}

// RelationProposer는 사건의 색인 공개 커밋 뒤 관계 후보 생성을 알리는 경계다.
// 오류는 이미 커밋한 색인을 되돌리거나 색인 작업을 다시 등록하지 않는다.
type RelationProposer interface {
	ProposeSimilarEventRelations(context.Context, model.ID, model.ID, string) error
}

// New는 제공자 호출 경계와 작업 큐 소비자를 만든다.
func New(database *store.Store, config Config, client *http.Client, logger *slog.Logger) (*Worker, error) {
	return NewWithRelationProposer(database, config, client, logger, database)
}

// NewWithRelationProposer는 공개된 사건 색인의 관계 후보 신호를 조립 지점에서 주입한다.
func NewWithRelationProposer(database *store.Store, config Config, client *http.Client, logger *slog.Logger, proposer RelationProposer) (*Worker, error) {
	if database == nil {
		return nil, fmt.Errorf("색인 저장소가 없다")
	}
	if proposer == nil {
		return nil, fmt.Errorf("관계 후보 신호 수신기가 없다")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// 호출자가 공유하는 client와 URL은 변경하지 않는다. 인증 헤더의 재전송을 막고
	// 주입한 client에도 유한 상한을 적용한다.
	copy := *client
	if copy.Timeout <= 0 || copy.Timeout > 30*time.Second {
		copy.Timeout = 30 * time.Second
	}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if transport, ok := copy.Transport.(*http.Transport); ok {
		if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
			return nil, fmt.Errorf("임베딩 제공자의 TLS 인증서 검증을 비활성화할 수 없다")
		}
		copy.Transport = transport.Clone()
	}
	config.BaseURL = config.BaseURL.Clone()
	config.APIKey = strings.TrimSpace(config.APIKey)
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: database, proposer: proposer, config: config, httpClient: &copy, logger: logger, done: make(chan struct{})}, nil
}

// ModelID는 모델과 벡터 표현이 같은지 비교할 수 있는 현재 색인 식별자다.
func (worker *Worker) ModelID() string {
	return worker.config.ModelID()
}

// Start는 서버 요청 경로와 분리된 색인 소비 루프를 시작한다.
func (worker *Worker) Start(parent context.Context) {
	worker.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		worker.cancel = cancel
		go func() {
			defer close(worker.done)
			worker.run(ctx)
		}()
	})
}

// Close는 처리 중인 작업의 컨텍스트를 취소하고 작업자 종료를 기다린다.
// 시작하지 않은 작업자는 시작 기회를 먼저 소비해, 이후 Start가 done을 다시 닫지 않게 한다.
func (worker *Worker) Close(ctx context.Context) error {
	worker.startOnce.Do(func() { close(worker.done) })
	worker.stopOnce.Do(func() {
		if worker.cancel != nil {
			worker.cancel()
		}
	})
	select {
	case <-worker.done:
		// New가 복제한 표준 transport만 정리한다. 기본 전역 transport나
		// 호출자가 주입한 다른 RoundTripper의 수명은 변경하지 않는다.
		if transport, ok := worker.httpClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (worker *Worker) run(ctx context.Context) {
	ctx = diagnostic.ResumeJob(ctx, "")
	if ctx.Err() != nil {
		return
	}
	// 오래된 모델의 벡터를 다시 등록하는 일은 기동을 막을 이유가 없다. 대상이 많으면 그만큼
	// 기동이 늦어지고, 등록이 끝나기 전에 죽어도 upsert라 다음 기동이 같은 일을 다시 한다.
	if err := worker.store.ReindexOutdatedEmbeddings(ctx, worker.ModelID()); err != nil {
		worker.logger.ErrorContext(ctx, "이전 임베딩 재색인 등록 실패", "reason_code", "reindex_registration_failed")
	} else {
		worker.logReindexProgress(ctx)
	}
	nextProgress := time.Now().Add(reindexProgressInterval)
	worker.consume(ctx, func(ctx context.Context) (bool, error) {
		// 차원 전환은 옛 벡터를 비우며, 실행 중 재등록도 생길 수 있다.
		// 옛 모델 행이 0이어도 미색인·대기·실패를 계속 관측한다.
		if !time.Now().Before(nextProgress) {
			worker.logReindexProgress(ctx)
			nextProgress = time.Now().Add(reindexProgressInterval)
		}
		return worker.RunOnce(ctx)
	})
}

// consume는 DB·제공자 한 회차와 조회 간격·취소의 경계를 분리한다.
func (worker *Worker) consume(ctx context.Context, once func(context.Context) (bool, error)) {
	interval := idleIntervalMin
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for ctx.Err() == nil {
		processed, err := once(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			worker.logger.ErrorContext(ctx, "색인 작업 처리 실패", "reason_code", "index_cycle_failed")
		}
		if processed {
			interval = idleIntervalMin
		} else {
			interval = min(interval*2, idleIntervalMax)
		}
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// logReindexProgress는 현재 대상의 옛 벡터·현재 모델 누락과 큐 상태를 함께 기록한다.
func (worker *Worker) logReindexProgress(ctx context.Context) {
	progress, err := worker.store.ReindexProgress(ctx, model.ID{}, worker.ModelID())
	if err != nil {
		worker.logger.ErrorContext(ctx, "이전 임베딩 진행률 조회 실패", "reason_code", "reindex_progress_failed")
		return
	}
	worker.logger.InfoContext(ctx, "이전 임베딩 재색인 진행률",
		"old_model_embeddings_remaining", progress.OldModelEmbeddings,
		"missing_current_embeddings", progress.Missing, "pending", progress.Pending, "failed", progress.Failed)
}

// RunOnce는 현재 가능한 색인 작업 하나를 처리하고, 완료된 사건의 의미 관계 후보를 제안한다.
func (worker *Worker) RunOnce(ctx context.Context) (bool, error) {
	return worker.runOnce(ctx, model.ID{})
}

// RunOnceInGraph는 확보 대상을 그래프 하나로 좁혀 한 회차를 돈다. 「검증」의 측정이
// 자기 그래프의 색인만 비우고 시작해야 할 때 쓴다.
func (worker *Worker) RunOnceInGraph(ctx context.Context, graphID model.ID) (bool, error) {
	return worker.runOnce(ctx, graphID)
}

func (worker *Worker) runOnce(ctx context.Context, scope model.ID) (bool, error) {
	ctx = diagnostic.ResumeJob(ctx, "")
	acquire := worker.store.ProcessNextIndexTask
	if scope.IsV7() {
		acquire = func(ctx context.Context, processor store.IndexTaskProcessor) (store.IndexProcessResult, error) {
			return worker.store.ProcessNextIndexTaskInGraph(ctx, scope, processor)
		}
	}
	result, err := acquire(ctx, func(ctx context.Context, task store.IndexTask) store.IndexTaskResult {
		started := time.Now()
		embedding, status, err := worker.embedResponse(ctx, task.Body, true)
		result, reason, level := "completed", "completed", "INFO"
		if err != nil {
			result, reason, level = "failed", "provider_unavailable", "WARN"
		}
		diagnostic.Observe(worker.diagnostics, ctx, level, "provider", result, reason, true, diagnostic.Fields{Identity: diagnostic.IdentityFields{GraphID: task.GraphID.String(), ContextID: task.ContextID.String()}, Index: diagnostic.IndexFields{TaskID: task.ID.String(), Generation: uint64(task.Generation.UnixMicro()), Attempts: uint64(task.Attempts), ProviderStatus: status, LatencyMS: uint64(time.Since(started).Milliseconds())}})
		if err != nil {
			// 사유를 그대로 남긴다. 고정 문구만 남기면 「색인 재시도」가 운영자 개입이
			// 필요하다고 한 차원 불일치를 실패한 행에서 구분할 수 없다.
			mismatch, dimensionMismatch := errors.AsType[DimensionError](err)
			failure := err.Error()
			if dimensionMismatch {
				failure = mismatch.Error()
			}
			worker.logger.ErrorContext(ctx, "임베딩 제공자 호출 실패",
				"context_id", task.ContextID.String(), "graph_id", task.GraphID.String(),
				"job_id", task.ID.String(),
				"correlation_id", task.CorrelationID, "attempts", task.Attempts,
				"dimension_mismatch", dimensionMismatch, "error", err.Error())
			return store.IndexTaskResult{Failure: failure, Retryable: retryableEmbeddingError(err)}
		}
		return store.IndexTaskResult{Embedding: embedding, ModelID: worker.ModelID()}
	})
	if result.Found {
		outcome := "not_published"
		if err != nil {
			outcome = "unknown"
		} else if result.Succeeded {
			outcome = "committed"
		}
		worker.logger.InfoContext(ctx, "색인 결과 공개 회차",
			"job_id", result.Task.ID.String(), "correlation_id", result.Task.CorrelationID,
			"graph_id", result.Task.GraphID.String(), "context_id", result.Task.ContextID.String(),
			"stage", "result_publication", "result", outcome)
	}
	if err != nil {
		return result.Found, err
	}
	if result.Succeeded && result.Task.Layer == model.LayerEvent {
		if err := worker.proposer.ProposeSimilarEventRelations(ctx, result.Task.GraphID, result.Task.ContextID, worker.ModelID()); err != nil {
			worker.logger.ErrorContext(ctx, "의미 관계 후보 제안 실패",
				"job_id", result.Task.ID.String(), "correlation_id", result.Task.CorrelationID,
				"graph_id", result.Task.GraphID.String(), "context_id", result.Task.ContextID.String(),
				"stage", "relation_proposal", "result", "failed", "reason_code", "proposal_failed")
		}
	}
	return result.Found, nil
}

// SetDiagnostics는 Start 전에 주입한다. 진단 실패는 색인 공개 결과를 바꾸지 않는다.
func (worker *Worker) SetDiagnostics(observer diagnostic.Observer) { worker.diagnostics = observer }
