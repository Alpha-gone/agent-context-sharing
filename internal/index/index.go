// Package index는 비동기 색인 작업과 임베딩 제공자 호출을 소유한다.
package index

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent_context_sharing/internal/store"
)

// reindexProgressInterval은 재색인이 남아 있는 동안 진행률을 구조화 로그에 남기는 간격이다.
const reindexProgressInterval = time.Minute

// Config는 하나의 배포가 쓰는 임베딩 제공자와 벡터 계약이다.
type Config struct {
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
// 늘린다. 차원 상한인 4096개의 float를 넉넉히 담을 수 있는 크기로 둔다.
const maxEmbeddingResponseBytes = 8 << 20

// Worker는 작업 행 잠금 안에서 임베딩 제공자를 호출하는 비동기 소비자다.
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
	if config.BaseURL == nil || config.BaseURL.Scheme == "" || config.BaseURL.Host == "" {
		return nil, fmt.Errorf("임베딩 제공자 주소가 올바르지 않다")
	}
	if strings.TrimSpace(config.Model) == "" || strings.TrimSpace(config.VectorType) == "" || config.Dimension < 1 {
		return nil, fmt.Errorf("임베딩 모델, 저장 타입과 차원이 필요하다")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: database, config: config, httpClient: client, logger: logger, done: make(chan struct{})}, nil
}

// ModelID는 모델과 벡터 표현이 같은지 비교할 수 있는 현재 색인 식별자다.
func (worker *Worker) ModelID() string {
	return worker.config.Model + ":" + worker.config.VectorType + ":" + fmt.Sprint(worker.config.Dimension)
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
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
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
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
	result, err := worker.store.ProcessNextIndexTask(ctx, func(ctx context.Context, task store.IndexTask) store.IndexTaskResult {
		embedding, err := worker.Embed(ctx, task.Body)
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
			return store.IndexTaskResult{Failure: failure, Retryable: !dimensionMismatch}
		}
		return store.IndexTaskResult{Embedding: embedding, ModelID: worker.ModelID()}
	})
	if err != nil {
		return false, err
	}
	if result.Succeeded {
		if err := worker.store.ProposeSimilarEventRelations(ctx, result.Task.GraphID, result.Task.ContextID, worker.ModelID()); err != nil {
			worker.logger.ErrorContext(ctx, "의미 관계 후보 제안 실패", "graph_id", result.Task.GraphID.String(), "context_id", result.Task.ContextID.String(), "error", err)
		}
	}
	return result.Found, nil
}

// Embed는 본문 색인과 질의 임베딩이 같은 제공자·모델을 쓰게 하는 유일한 호출 경계다.
func (worker *Worker) Embed(ctx context.Context, input string) ([]float64, error) {
	body, err := json.Marshal(struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}{Model: worker.config.Model, Input: input})
	if err != nil {
		return nil, fmt.Errorf("임베딩 요청 직렬화: %w", err)
	}
	endpoint := worker.config.BaseURL.Clone()
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/embed"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("임베딩 요청 생성: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := worker.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("임베딩 제공자 요청: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("임베딩 제공자 상태: %d", response.StatusCode)
	}
	var decoded struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	// 본문을 상한까지만 읽는다. 상한에 정확히 닿으면 잘린 것이므로 해석 결과를 믿지 않는다.
	limited := &io.LimitedReader{R: response.Body, N: maxEmbeddingResponseBytes + 1}
	if err := json.UnmarshalRead(limited, &decoded); err != nil {
		if limited.N <= 0 {
			return nil, fmt.Errorf("임베딩 응답이 %d바이트 상한을 넘었다", maxEmbeddingResponseBytes)
		}
		return nil, fmt.Errorf("임베딩 응답 해석: %w", err)
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("임베딩 응답이 %d바이트 상한을 넘었다", maxEmbeddingResponseBytes)
	}
	if len(decoded.Embeddings) != 1 {
		return nil, fmt.Errorf("임베딩 응답 벡터 개수가 올바르지 않다")
	}
	embedding := decoded.Embeddings[0]
	if len(embedding) != worker.config.Dimension {
		return nil, DimensionError{Got: len(embedding), Want: worker.config.Dimension}
	}
	return embedding, nil
}
