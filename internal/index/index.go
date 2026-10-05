// Package index는 비동기 색인 작업과 임베딩 제공자 호출을 소유한다.
package index

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

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
	store      *store.Store
	config     Config
	httpClient *http.Client
	logger     *slog.Logger
	cancel     context.CancelFunc
	done       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
}

// New는 제공자 호출 경계와 작업 큐 소비자를 만든다.
func New(database *store.Store, config Config, client *http.Client, logger *slog.Logger) (*Worker, error) {
	if database == nil {
		return nil, fmt.Errorf("색인 저장소가 없다")
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
	config.BaseURL = config.BaseURL.Clone()
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: database, config: config, httpClient: &copy, logger: logger, done: make(chan struct{})}, nil
}

// ModelID는 모델과 벡터 표현이 같은지 비교할 수 있는 현재 색인 식별자다.
func (worker *Worker) ModelID() string {
	return worker.config.ModelID()
}

// ModelID는 임베딩 행의 model_id 형식을 만든다. 그래프 불변식 감사가 현재 모델과 대조할 때도
// 같은 형식을 써야 하므로 작업자 밖에 둔다.
func ModelID(model, vectorType string, dimension int) string {
	return model + ":" + vectorType + ":" + fmt.Sprint(dimension)
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
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (worker *Worker) run(ctx context.Context) {
	// 오래된 모델의 벡터를 다시 등록하는 일은 기동을 막을 이유가 없다. 대상이 많으면 그만큼
	// 기동이 늦어지고, 등록이 끝나기 전에 죽어도 upsert라 다음 기동이 같은 일을 다시 한다.
	remaining := -1
	if err := worker.store.ReindexOutdatedEmbeddings(ctx, worker.ModelID()); err != nil {
		worker.logger.ErrorContext(ctx, "이전 임베딩 재색인 등록 실패", "error", err)
	} else {
		remaining = worker.logReindexProgress(ctx)
	}
	nextProgress := time.Now().Add(reindexProgressInterval)
	idleInterval := idleIntervalMin
	timer := time.NewTimer(idleInterval)
	defer timer.Stop()
	for {
		// 남은 행이 0이 되면 더 늘지 않으므로 그때부터 진행률 조회를 멈춘다.
		if remaining != 0 && !time.Now().Before(nextProgress) {
			remaining = worker.logReindexProgress(ctx)
			nextProgress = time.Now().Add(reindexProgressInterval)
		}
		processed, err := worker.RunOnce(ctx)
		if err != nil {
			worker.logger.ErrorContext(ctx, "색인 작업 처리 실패", "error", err)
		}
		if processed {
			idleInterval = idleIntervalMin
			continue
		}
		timer.Reset(idleInterval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		idleInterval = min(idleInterval*2, idleIntervalMax)
	}
}

// logReindexProgress는 옛 model_id로 남은 임베딩 행 수를 재색인 진행률로 기록하고 그 수를
// 돌려준다. 조회에 실패하면 -1을 돌려 다음 간격에 다시 조회하게 한다.
func (worker *Worker) logReindexProgress(ctx context.Context) int {
	remaining, err := worker.store.OutdatedEmbeddingCount(ctx, worker.ModelID())
	if err != nil {
		worker.logger.ErrorContext(ctx, "이전 임베딩 진행률 조회 실패", "error", err)
		return -1
	}
	worker.logger.InfoContext(ctx, "이전 임베딩 재색인 진행률", "old_model_embeddings_remaining", remaining)
	return remaining
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
	acquire := worker.store.ProcessNextIndexTask
	if scope.IsV7() {
		acquire = func(ctx context.Context, processor store.IndexTaskProcessor) (store.IndexProcessResult, error) {
			return worker.store.ProcessNextIndexTaskInGraph(ctx, scope, processor)
		}
	}
	result, err := acquire(ctx, func(ctx context.Context, task store.IndexTask) store.IndexTaskResult {
		embedding, err := worker.EmbedDocument(ctx, task.Body)
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
				"correlation_id", task.CorrelationID, "attempts", task.Attempts,
				"dimension_mismatch", dimensionMismatch, "error", err.Error())
			return store.IndexTaskResult{Failure: failure, Retryable: retryableEmbeddingError(err)}
		}
		return store.IndexTaskResult{Embedding: embedding, ModelID: worker.ModelID()}
	})
	if err != nil {
		return result.Found, err
	}
	if result.Succeeded {
		if err := worker.store.ProposeSimilarEventRelations(ctx, result.Task.GraphID, result.Task.ContextID, worker.ModelID()); err != nil {
			worker.logger.ErrorContext(ctx, "의미 관계 후보 제안 실패", "graph_id", result.Task.GraphID.String(), "context_id", result.Task.ContextID.String(), "error", err)
		}
	}
	return result.Found, nil
}
