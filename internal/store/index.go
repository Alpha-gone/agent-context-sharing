package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

const maxIndexAttempts = 5

// indexTaskLease는 확보한 작업을 다른 작업자에게 숨겨 두는 시간이다.
//
// 제공자 호출을 트랜잭션 밖으로 빼면 그동안 작업 행의 잠금이 없으므로, 대기 조건인
// next_attempt_at을 미뤄 소유를 표현한다. 「색인 작업 큐」가 처리 중 상태를 두지 않기로
// 했으므로 열을 더하지 않고 이미 있는 열을 쓴다. 제공자 타임아웃보다 넉넉해야 같은 작업이
// 겹쳐 처리되지 않고, 작업자가 죽어도 이 시간이 지나면 저절로 다시 대기가 된다.
const indexTaskLease = 2 * time.Minute

// IndexTask는 외부 임베딩 제공자에 넘길 색인 대기 작업이다.
type IndexTask struct {
	ID            model.ID
	ContextID     model.ID
	GraphID       model.ID
	Body          string
	Attempts      int
	CorrelationID string
}

// IndexTaskResult는 제공자 호출 결과와 저장·재시도 판단을 분리해 전달한다.
type IndexTaskResult struct {
	Embedding []float64
	ModelID   string
	Failure   string
	Retryable bool
}

// IndexTaskProcessor는 트랜잭션 밖에서 임베딩 제공자를 호출하는 경계다.
type IndexTaskProcessor func(context.Context, IndexTask) IndexTaskResult

// IndexProcessResult는 한 번의 작업자 회차 결과다.
type IndexProcessResult struct {
	Found     bool
	Succeeded bool
	Task      IndexTask
}

// SearchCandidate는 검색 채널이 순위를 부여하기 전 저장소가 돌려주는 컨텍스트다.
type SearchCandidate struct {
	Context model.Context
	// Similarity는 의미 유사도 채널에서만 코사인 유사도를 담는다. 다른 채널은 0이다.
	Similarity float64
}

// ProcessNextIndexTask는 대기 중인 작업 하나를 확보하고 제공자 결과를 저장한다.
//
// 확보와 저장을 서로 다른 짧은 트랜잭션으로 나누고 제공자 호출은 그 사이에서 한다.
// 「색인 처리」가 임베딩 생성을 트랜잭션 밖으로 빼기로 했고, 잠금을 쥔 채 부르면 같은
// 컨텍스트의 저장이 제공자 응답까지 막혀 대역이 늦어질 때 저장 지연이 함께 늘어난다.
// 확보한 작업은 next_attempt_at을 미뤄 다른 작업자에게 숨긴다.
func (s *Store) ProcessNextIndexTask(ctx context.Context, processor IndexTaskProcessor) (IndexProcessResult, error) {
	return s.processNextIndexTask(ctx, model.ID{}, processor)
}

// ProcessNextIndexTaskInGraph는 확보 대상을 그래프 하나로 좁힌다.
//
// 「검증」의 측정은 잰 그래프의 색인이 끝난 상태에서 시작해야 하고, 그 판정에 다른
// 그래프의 대기 작업이 섞이면 회차마다 시작 상태가 달라진다. 좁혀서 확보하면 평가가
// 남의 작업을 대신 처리하지 않고 자기 그래프만 비운다. 운영 경로의 색인 작업자는
// 그래프를 가리지 않으므로 이 함수를 쓰지 않는다.
func (s *Store) ProcessNextIndexTaskInGraph(ctx context.Context, graphID model.ID, processor IndexTaskProcessor) (IndexProcessResult, error) {
	if !graphID.IsV7() {
		return IndexProcessResult{}, fmt.Errorf("그래프 식별자가 UUIDv7이 아니다")
	}
	return s.processNextIndexTask(ctx, graphID, processor)
}

// processNextIndexTask는 확보 범위만 다른 두 진입점의 공통 구현이다. scope가 비어
// 있으면 그래프를 가리지 않는다.
func (s *Store) processNextIndexTask(ctx context.Context, scope model.ID, processor IndexTaskProcessor) (IndexProcessResult, error) {
	if processor == nil {
		return IndexProcessResult{}, fmt.Errorf("색인 작업 처리기가 없다")
	}
	scopeValue := any(nil)
	if scope.IsV7() {
		scopeValue = scope.String()
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return IndexProcessResult{}, fmt.Errorf("색인 작업 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	var rawID, rawContextID, rawGraphID, correlationID string
	var attempts int
	var enqueuedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT task_id::text, context_id::text, graph_id::text, attempts, COALESCE(correlation_id, ''), enqueued_at
		FROM public.index_task
		WHERE state = 'pending' AND next_attempt_at <= now()
		  AND ($1::uuid IS NULL OR graph_id = $1::uuid)
		ORDER BY enqueued_at, task_id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`, scopeValue).Scan(&rawID, &rawContextID, &rawGraphID, &attempts, &correlationID, &enqueuedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return IndexProcessResult{}, tx.Commit(ctx)
		}
		return IndexProcessResult{}, fmt.Errorf("색인 작업 확보: %w", err)
	}
	taskID, err := model.ParseID(rawID)
	if err != nil {
		return IndexProcessResult{}, fmt.Errorf("색인 작업 식별자 해석: %w", err)
	}
	contextID, err := model.ParseID(rawContextID)
	if err != nil {
		return IndexProcessResult{}, fmt.Errorf("색인 대상 식별자 해석: %w", err)
	}
	graphID, err := model.ParseID(rawGraphID)
	if err != nil {
		return IndexProcessResult{}, fmt.Errorf("색인 그래프 식별자 해석: %w", err)
	}
	value, err := s.context(ctx, tx, graphID, contextID)
	if err != nil {
		if err == ErrNotFound {
			if _, deleteErr := tx.Exec(ctx, `DELETE FROM public.index_task WHERE task_id = $1`, rawID); deleteErr != nil {
				return IndexProcessResult{}, fmt.Errorf("사라진 색인 작업 제거: %w", deleteErr)
			}
			return IndexProcessResult{Found: true}, tx.Commit(ctx)
		}
		return IndexProcessResult{}, fmt.Errorf("색인 대상 조회: %w", err)
	}
	task := IndexTask{ID: taskID, ContextID: contextID, GraphID: graphID, Body: value.Body, Attempts: attempts, CorrelationID: correlationID}
	// 확보를 커밋하면서 대기 시각을 미뤄 다른 작업자가 같은 작업을 집지 않게 한다.
	if _, err := tx.Exec(ctx, `UPDATE public.index_task SET next_attempt_at = $2 WHERE task_id = $1`, rawID, time.Now().UTC().Add(indexTaskLease)); err != nil {
		return IndexProcessResult{}, fmt.Errorf("색인 작업 확보 표시: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IndexProcessResult{}, fmt.Errorf("색인 작업 확보 커밋: %w", err)
	}

	// 트랜잭션 밖에서 제공자를 부른다. 이 구간에는 어떤 행 잠금도 쥐고 있지 않다.
	result := processor(ctx, task)

	if result.Failure != "" {
		if err := s.storeIndexFailure(ctx, task, result); err != nil {
			return IndexProcessResult{}, err
		}
		return IndexProcessResult{Found: true, Task: task}, nil
	}
	if len(result.Embedding) == 0 || result.ModelID == "" {
		return IndexProcessResult{}, fmt.Errorf("색인 처리기가 빈 임베딩 또는 모델 식별자를 반환했다")
	}
	if err := s.storeIndexResult(ctx, task, enqueuedAt, result); err != nil {
		return IndexProcessResult{}, err
	}
	return IndexProcessResult{Found: true, Succeeded: true, Task: task}, nil
}

// storeIndexResult는 제공자 결과를 저장하고 처리한 작업을 지운다.
//
// 지울 때 등록 시각을 함께 대조하는 이유는 확보한 뒤 본문이 바뀌었을 수 있기 때문이다.
// 그 경우 enqueueIndexTask의 upsert가 등록 시각을 새로 쓰므로 여기에서 지우지 않고 남겨
// 다음 회차가 새 본문으로 다시 색인한다.
func (s *Store) storeIndexResult(ctx context.Context, task IndexTask, enqueuedAt time.Time, result IndexTaskResult) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("색인 결과 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.context_embedding (context_id, graph_id, embedding, model_id, indexed_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (context_id) DO UPDATE SET
			graph_id = EXCLUDED.graph_id, embedding = EXCLUDED.embedding,
			model_id = EXCLUDED.model_id, indexed_at = EXCLUDED.indexed_at`,
		task.ContextID.String(), task.GraphID.String(), vectorText(result.Embedding), result.ModelID); err != nil {
		return fmt.Errorf("임베딩 저장: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.index_task WHERE task_id = $1 AND enqueued_at = $2`, task.ID.String(), enqueuedAt); err != nil {
		return fmt.Errorf("완료 색인 작업 제거: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("색인 작업 커밋: %w", err)
	}
	return nil
}

// storeIndexFailure는 제공자 실패를 작업 행에 기록한다.
func (s *Store) storeIndexFailure(ctx context.Context, task IndexTask, result IndexTaskResult) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("색인 실패 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := s.recordIndexFailure(ctx, tx, task, result); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("색인 실패 커밋: %w", err)
	}
	return nil
}

func (s *Store) recordIndexFailure(ctx context.Context, tx pgx.Tx, task IndexTask, result IndexTaskResult) error {
	attempts := task.Attempts + 1
	state := "pending"
	next := time.Now().UTC().Add(indexRetryDelay(attempts))
	if !result.Retryable || attempts >= maxIndexAttempts {
		state = "failed"
		next = time.Now().UTC()
	}
	if _, err := tx.Exec(ctx, `
		UPDATE public.index_task
		SET attempts = $2, state = $3, last_error = $4, next_attempt_at = $5
		WHERE task_id = $1`, task.ID.String(), attempts, state, result.Failure, next); err != nil {
		return fmt.Errorf("색인 작업 실패 기록: %w", err)
	}
	return nil
}

func indexRetryDelay(attempt int) time.Duration {
	// 1, 2, 4, 8분으로 늘리고 마지막 다섯 번째 실패는 failed로 남긴다.
	return time.Minute * time.Duration(1<<min(attempt-1, 3))
}

// ReindexOutdatedEmbeddings는 현재 모델과 다른 벡터가 남은 그래프 전체를 다시 대기열에 넣는다.
func (s *Store) ReindexOutdatedEmbeddings(ctx context.Context, modelID string) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT graph_id::text FROM public.context_embedding WHERE model_id <> $1`, modelID)
	if err != nil {
		return fmt.Errorf("재색인 그래프 조회: %w", err)
	}
	// 행을 끝까지 읽어 연결을 놓아준 뒤에 재등록한다. 행을 연 채 부르면 같은 풀에서
	// 연결을 하나 더 잡아 기동과 요청이 겹칠 때 서로의 연결을 기다린다.
	graphIDs := make([]model.ID, 0)
	for rows.Next() {
		var rawGraphID string
		if err := rows.Scan(&rawGraphID); err != nil {
			rows.Close()
			return fmt.Errorf("재색인 그래프 행 해석: %w", err)
		}
		graphID, err := model.ParseID(rawGraphID)
		if err != nil {
			rows.Close()
			return fmt.Errorf("재색인 그래프 식별자 해석: %w", err)
		}
		graphIDs = append(graphIDs, graphID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("재색인 그래프 행 읽기: %w", err)
	}
	for _, graphID := range graphIDs {
		if err := s.ReindexGraph(ctx, graphID); err != nil {
			return err
		}
	}
	return nil
}

// OutdatedEmbeddingCount는 현재 모델과 다른 임베딩 행 수를 돌려준다. 이 수는 재색인
// 진행 중인 행 수이므로 별도 상태 없이 재색인 진행률을 관측하는 값으로 쓴다.
func (s *Store) OutdatedEmbeddingCount(ctx context.Context, modelID string) (int, error) {
	if strings.TrimSpace(modelID) == "" {
		return 0, fmt.Errorf("현재 임베딩 모델 식별자가 없다")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.context_embedding WHERE model_id <> $1`, modelID).Scan(&count); err != nil {
		return 0, fmt.Errorf("이전 임베딩 수 조회: %w", err)
	}
	return count, nil
}

// ReindexGraph는 그래프의 컨텍스트를 현재 색인 대상 구성에 맞게 다시 맞춘다.
//
// 대상인 계층은 기존 upsert 규칙으로 다시 등록하고, 대상에서 빠진 계층은 남아 있던
// 임베딩과 대기 작업을 지운다. 지우지 않으면 「색인 대상 비교」의 원천 제외 구성으로
// 바꿔도 예전 구성이 남긴 원천 임베딩이 의미 유사도 채널에 계속 올라와 두 구성이
// 등록 조건 말고도 달라진다.
func (s *Store) ReindexGraph(ctx context.Context, graphID model.ID) error {
	if !graphID.IsV7() {
		return fmt.Errorf("그래프 식별자가 UUIDv7이 아니다")
	}
	rows, err := s.pool.Query(ctx, `SELECT properties ->> 'context_id'::text, properties ->> 'layer'::text FROM `+s.contextTable()+` WHERE properties ->> 'graph_id'::text = $1`, graphID.String())
	if err != nil {
		return fmt.Errorf("재색인 대상 조회: %w", err)
	}
	defer rows.Close()
	type reindexTarget struct {
		id    model.ID
		layer model.Layer
	}
	targets := make([]reindexTarget, 0)
	excluded := make([]string, 0)
	for rows.Next() {
		var rawID, rawLayer string
		if err := rows.Scan(&rawID, &rawLayer); err != nil {
			return fmt.Errorf("재색인 대상 행 해석: %w", err)
		}
		contextID, err := model.ParseID(rawID)
		if err != nil {
			return fmt.Errorf("재색인 대상 식별자 해석: %w", err)
		}
		if s.indexTargets.indexes(model.Layer(rawLayer)) {
			targets = append(targets, reindexTarget{id: contextID, layer: model.Layer(rawLayer)})
			continue
		}
		excluded = append(excluded, contextID.String())
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("재색인 대상 행 읽기: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("재색인 등록 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	for _, target := range targets {
		if err := s.enqueueIndexTask(ctx, tx, graphID, target.id, target.layer); err != nil {
			return err
		}
	}
	if len(excluded) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM public.context_embedding WHERE graph_id = $1 AND context_id = ANY($2::uuid[])`, graphID.String(), excluded); err != nil {
			return fmt.Errorf("색인 대상에서 빠진 임베딩 제거: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM public.index_task WHERE graph_id = $1 AND context_id = ANY($2::uuid[])`, graphID.String(), excluded); err != nil {
			return fmt.Errorf("색인 대상에서 빠진 대기 작업 제거: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("재색인 등록 커밋: %w", err)
	}
	return nil
}

// KeywordCandidates는 PostgreSQL simple 전문 검색으로 활성 기본 검색 후보를 읽는다.
// websearchOrQuery는 자연어 질의의 어휘를 websearch 구문의 OR로 묶는다.
//
// plainto와 websearch 모두 어휘를 AND로 묶는데 work_context는 문장 단위라 전부를 담은
// 문단이 없으면 후보가 나오지 않는다. 후보를 내는 이 채널은 겹치는 어휘만으로도
// 문단을 찾아야 하므로 어휘를 OR로 조립하고 구문 해석은 websearch_to_tsquery에 맡긴다.
func websearchOrQuery(query string) string {
	return strings.Join(strings.Fields(query), " OR ")
}

func (s *Store) KeywordCandidates(ctx context.Context, graphID model.ID, query string, current time.Time, limit int) ([]SearchCandidate, error) {
	query = websearchOrQuery(query)
	if !graphID.IsV7() || !current.UTC().Equal(current) || limit < 1 {
		return nil, fmt.Errorf("키워드 검색 인자가 올바르지 않다")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT properties ->> 'context_id'::text
		FROM `+s.contextTable()+`
		WHERE properties ->> 'graph_id'::text = $1
			AND properties ->> 'deleted_at'::text IS NULL
			AND NOT (properties ->> 'layer'::text = 'derived'
				AND properties ->> 'derivation_kind'::text = 'summary'
				AND properties ->> 'summary_scope'::text = 'global')
			AND ((properties ->> 'layer'::text) <> 'derived'
				OR NULLIF(properties ->> 'valid_to'::text, '') IS NULL
				OR (properties ->> 'valid_to'::text)::timestamptz >= $3)
			AND to_tsvector('simple', properties ->> 'body'::text) @@ websearch_to_tsquery('simple', $2)
		ORDER BY ts_rank_cd(to_tsvector('simple', properties ->> 'body'::text), websearch_to_tsquery('simple', $2)) DESC,
			(properties ->> 'recorded_at'::text)::timestamptz DESC, properties ->> 'context_id'::text ASC
		LIMIT $4`, graphID.String(), query, current, limit)
	if err != nil {
		return nil, fmt.Errorf("키워드 검색: %w", err)
	}
	ids, err := searchCandidateIDs(rows)
	if err != nil {
		return nil, err
	}
	return s.searchCandidates(ctx, graphID, ids)
}

// TimeCandidates는 지정 시점에 유효했던 활성 컨텍스트를 최근 기록 순으로 읽는다.
func (s *Store) TimeCandidates(ctx context.Context, graphID model.ID, asOf time.Time, limit int) ([]SearchCandidate, error) {
	if !graphID.IsV7() || !asOf.UTC().Equal(asOf) || limit < 1 {
		return nil, fmt.Errorf("시간 검색 인자가 올바르지 않다")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT properties ->> 'context_id'::text
		FROM `+s.contextTable()+`
		WHERE properties ->> 'graph_id'::text = $1 AND properties ->> 'deleted_at'::text IS NULL
			AND NOT (properties ->> 'layer'::text = 'derived'
				AND properties ->> 'derivation_kind'::text = 'summary'
				AND properties ->> 'summary_scope'::text = 'global')
			AND ((properties ->> 'layer'::text) <> 'derived'
				OR (NULLIF(properties ->> 'valid_from'::text, '') IS NULL OR (properties ->> 'valid_from'::text)::timestamptz <= $2)
				AND (NULLIF(properties ->> 'valid_to'::text, '') IS NULL OR (properties ->> 'valid_to'::text)::timestamptz >= $2))
		ORDER BY (properties ->> 'recorded_at'::text)::timestamptz DESC, properties ->> 'context_id'::text ASC
		LIMIT $3`, graphID.String(), asOf, limit)
	if err != nil {
		return nil, fmt.Errorf("시간 검색: %w", err)
	}
	ids, err := searchCandidateIDs(rows)
	if err != nil {
		return nil, err
	}
	return s.searchCandidates(ctx, graphID, ids)
}

// GlobalSummaryCandidates는 전역 범위 흐름의 시작점이 될 활성 전역 요약 파생을 최근 순으로 읽는다.
func (s *Store) GlobalSummaryCandidates(ctx context.Context, graphID model.ID, asOf time.Time, limit int) ([]SearchCandidate, error) {
	if !graphID.IsV7() || !asOf.UTC().Equal(asOf) || limit < 1 {
		return nil, fmt.Errorf("전역 요약 검색 인자가 올바르지 않다")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT properties ->> 'context_id'::text
		FROM `+s.contextTable()+`
		WHERE properties ->> 'graph_id'::text = $1 AND properties ->> 'deleted_at'::text IS NULL
			AND properties ->> 'layer'::text = 'derived'
			AND properties ->> 'derivation_kind'::text = 'summary'
			AND properties ->> 'summary_scope'::text = 'global'
			AND (NULLIF(properties ->> 'valid_from'::text, '') IS NULL OR (properties ->> 'valid_from'::text)::timestamptz <= $2)
			AND (NULLIF(properties ->> 'valid_to'::text, '') IS NULL OR (properties ->> 'valid_to'::text)::timestamptz >= $2)
		ORDER BY (properties ->> 'recorded_at'::text)::timestamptz DESC, properties ->> 'context_id'::text ASC
		LIMIT $3`, graphID.String(), asOf, limit)
	if err != nil {
		return nil, fmt.Errorf("전역 요약 검색: %w", err)
	}
	ids, err := searchCandidateIDs(rows)
	if err != nil {
		return nil, err
	}
	return s.searchCandidates(ctx, graphID, ids)
}

// SemanticCandidates는 현재 모델과 같은 벡터만 사용해 활성 기본 검색 후보를 읽는다.
func (s *Store) SemanticCandidates(ctx context.Context, graphID model.ID, modelID string, embedding []float64, current time.Time, limit int) ([]SearchCandidate, error) {
	if !graphID.IsV7() || modelID == "" || len(embedding) == 0 || !current.UTC().Equal(current) || limit < 1 {
		return nil, fmt.Errorf("의미 유사도 검색 인자가 올바르지 않다")
	}
	// 후보 식별자만 트랜잭션 안에서 읽고, 조립은 트랜잭션을 닫은 뒤에 한다. 반복 탐색
	// 설정이 트랜잭션 범위라 질의가 그 안에 있어야 하고, 조립까지 안에 두면 연결을 오래
	// 쥔 채 정점 조회가 같은 풀에서 연결을 하나 더 잡는다.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("의미 유사도 검색 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := enableIterativeScan(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT candidate.context_id::text, 1 - (candidate.embedding <=> $3::vector) AS similarity
		FROM public.context_embedding AS candidate
		JOIN `+s.contextTable()+` AS node ON (node.properties ->> 'context_id'::text)::uuid = candidate.context_id
		WHERE candidate.graph_id = $1 AND candidate.model_id = $2
			AND node.properties ->> 'graph_id'::text = $1::text
			AND node.properties ->> 'deleted_at'::text IS NULL
			AND NOT (node.properties ->> 'layer'::text = 'derived'
				AND node.properties ->> 'derivation_kind'::text = 'summary'
				AND node.properties ->> 'summary_scope'::text = 'global')
			AND ((node.properties ->> 'layer'::text) <> 'derived'
				OR NULLIF(node.properties ->> 'valid_to'::text, '') IS NULL
				OR (node.properties ->> 'valid_to'::text)::timestamptz >= $4)
		ORDER BY candidate.embedding <=> $3::vector, candidate.context_id
		LIMIT $5`, graphID.String(), modelID, vectorText(embedding), current, limit)
	if err != nil {
		return nil, fmt.Errorf("의미 유사도 검색: %w", err)
	}
	ids := make([]model.ID, 0)
	similarities := make(map[model.ID]float64)
	for rows.Next() {
		var rawID string
		var similarity float64
		if err := rows.Scan(&rawID, &similarity); err != nil {
			rows.Close()
			return nil, fmt.Errorf("의미 유사도 후보 행 해석: %w", err)
		}
		contextID, err := model.ParseID(rawID)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("의미 유사도 후보 식별자 해석: %w", err)
		}
		ids = append(ids, contextID)
		similarities[contextID] = similarity
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("의미 유사도 후보 행 읽기: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("의미 유사도 검색 커밋: %w", err)
	}
	candidates, err := s.searchCandidates(ctx, graphID, ids)
	if err != nil {
		return nil, err
	}
	for index := range candidates {
		candidates[index].Similarity = similarities[candidates[index].Context.ID]
	}
	return candidates, nil
}

// searchCandidateIDs는 결과 행을 끝까지 읽어 식별자만 모으고 연결을 놓아준다.
// 행을 연 채 컨텍스트를 조회하면 같은 풀에서 연결을 하나 더 잡아 동시 요청이 서로를 기다린다.
func searchCandidateIDs(rows pgx.Rows) ([]model.ID, error) {
	defer rows.Close()
	ids := make([]model.ID, 0)
	for rows.Next() {
		var rawID string
		if err := rows.Scan(&rawID); err != nil {
			return nil, fmt.Errorf("검색 후보 행 해석: %w", err)
		}
		contextID, err := model.ParseID(rawID)
		if err != nil {
			return nil, fmt.Errorf("검색 후보 식별자 해석: %w", err)
		}
		ids = append(ids, contextID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("검색 후보 행 읽기: %w", err)
	}
	return ids, nil
}

// searchCandidates는 모은 식별자를 순서를 지켜 한 묶음으로 조립한다.
func (s *Store) searchCandidates(ctx context.Context, graphID model.ID, ids []model.ID) ([]SearchCandidate, error) {
	values, err := s.ContextsByIDs(ctx, graphID, ids)
	if err != nil {
		return nil, err
	}
	result := make([]SearchCandidate, 0, len(values))
	for _, value := range values {
		result = append(result, SearchCandidate{Context: value})
	}
	return result, nil
}

// ContextOriginKinds는 여러 컨텍스트의 출처 구분을 한 번에 모은다.
//
// 컨텍스트마다 근거 트리를 따로 타면 같은 원천을 후보 수만큼 다시 읽고 질의가 후보 수에
// 비례해 늘어난다. 흐름 응답은 담긴 컨텍스트 전부의 출처가 필요하므로, 한 깊이씩 넓혀 가며
// 묶어 읽고 방문한 정점을 호출 전체에서 공유한다.
func (s *Store) ContextOriginKinds(ctx context.Context, graphID model.ID, contextIDs []model.ID) (map[model.ID][]model.OriginKind, error) {
	result := make(map[model.ID][]model.OriginKind, len(contextIDs))
	if len(contextIDs) == 0 {
		return result, nil
	}
	// 근거를 따라가며 만나는 모든 정점을 한 번씩만 읽는다.
	loaded := make(map[model.ID]model.Context)
	frontier := slices.Clone(contextIDs)
	for len(frontier) > 0 {
		pending := make([]model.ID, 0, len(frontier))
		for _, id := range frontier {
			if _, found := loaded[id]; !found {
				pending = append(pending, id)
			}
		}
		if len(pending) == 0 {
			break
		}
		slices.SortFunc(pending, func(left, right model.ID) int { return cmp.Compare(left.String(), right.String()) })
		pending = slices.Compact(pending)
		values, err := s.ContextsByIDs(ctx, graphID, pending)
		if err != nil {
			return nil, err
		}
		next := make([]model.ID, 0)
		for _, value := range values {
			loaded[value.ID] = value
			switch value.Layer {
			case model.LayerDerived:
				next = append(next, value.Derived.DerivedFrom...)
			case model.LayerEvent:
				next = append(next, value.Event.MemberIDs...)
			}
		}
		frontier = next
	}
	for _, contextID := range contextIDs {
		origins := make(map[model.OriginKind]struct{})
		visited := make(map[model.ID]struct{})
		collectOriginKinds(loaded, contextID, visited, origins)
		result[contextID] = slices.Sorted(maps.Keys(origins))
	}
	return result, nil
}

// collectOriginKinds는 이미 읽어 둔 정점만 따라가며 출처 구분을 모은다.
func collectOriginKinds(loaded map[model.ID]model.Context, contextID model.ID, visited map[model.ID]struct{}, origins map[model.OriginKind]struct{}) {
	if _, found := visited[contextID]; found {
		return
	}
	visited[contextID] = struct{}{}
	value, found := loaded[contextID]
	if !found {
		return
	}
	switch value.Layer {
	case model.LayerSource:
		origins[value.Source.OriginKind] = struct{}{}
	case model.LayerDerived:
		for _, referenceID := range value.Derived.DerivedFrom {
			collectOriginKinds(loaded, referenceID, visited, origins)
		}
	case model.LayerEvent:
		for _, memberID := range value.Event.MemberIDs {
			collectOriginKinds(loaded, memberID, visited, origins)
		}
	}
}

// ProposeSimilarEventRelations는 색인을 마친 사건과 같은 모델의 사건 벡터를 비교한다.
func (s *Store) ProposeSimilarEventRelations(ctx context.Context, graphID, eventID model.ID, modelID string) error {
	if s.relationProposals == nil {
		return nil
	}
	target, err := s.Context(ctx, graphID, eventID)
	if err != nil {
		return err
	}
	if target.Layer != model.LayerEvent || target.DeletedAt != nil {
		return nil
	}
	// 대상 벡터를 먼저 한 행으로 읽어 파라미터로 넘긴다. 임베딩끼리 조인해 두 열의 연산으로
	// 정렬하면 정렬 기준이 열이 아니라서 벡터 인덱스를 쓸 수 없고, 같은 그래프의 모든 임베딩과
	// 거리를 계산한다. 파라미터에 타입을 박지 않는 이유는 「임베딩 저장」이 열 타입을 배포
	// 구성으로 두었기 때문이며, 거리 연산자가 왼쪽 열 타입으로 파라미터 타입을 결정한다.
	var targetVector string
	err = s.pool.QueryRow(ctx, `
		SELECT embedding::text FROM public.context_embedding
		WHERE context_id = $1 AND graph_id = $2 AND model_id = $3`,
		eventID.String(), graphID.String(), modelID).Scan(&targetVector)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 색인이 아직 없거나 모델이 다르면 비교할 벡터가 없다. 제안도 없다.
			return nil
		}
		return fmt.Errorf("의미 관계 대상 벡터 조회: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("의미 관계 후보 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := enableIterativeScan(ctx, tx); err != nil {
		return err
	}
	// 유사도 하한을 질의의 조건으로 내린다. Go에서 걸러내면 상한까지 뽑은 후보가 모두 임계값
	// 미달일 때 기준을 넘는 후보가 더 있어도 놓친다. 코사인 거리는 1에서 유사도를 뺀 값이다.
	rows, err := tx.Query(ctx, `
		SELECT candidate.context_id::text
		FROM public.context_embedding AS candidate
		JOIN `+s.contextTable()+` AS node ON (node.properties ->> 'context_id'::text)::uuid = candidate.context_id
		WHERE candidate.graph_id = $2 AND candidate.model_id = $3
			AND candidate.context_id <> $1::uuid
			AND candidate.embedding <=> $4 <= $5
			AND node.properties ->> 'layer'::text = 'event' AND node.properties ->> 'deleted_at'::text IS NULL
		ORDER BY candidate.embedding <=> $4, candidate.context_id
		LIMIT $6`,
		eventID.String(), graphID.String(), modelID, targetVector,
		1-s.relationProposals.SimilarityThreshold, s.relationProposals.Limit)
	if err != nil {
		return fmt.Errorf("의미 관계 후보 조회: %w", err)
	}
	// 후보를 먼저 모아 행과 트랜잭션을 놓아준다. 행을 연 채 관계를 만들면 같은 풀에서
	// 트랜잭션용 연결을 하나 더 잡아 색인 작업자와 요청 경로가 서로의 연결을 기다린다.
	similar, err := searchCandidateIDs(rows)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("의미 관계 후보 커밋: %w", err)
	}
	for _, otherID := range similar {
		relation := normalizeRelation(proposedRelation(graphID, model.RelationTypeRelatesTo, eventID, otherID))
		if _, err := s.createProposedRelation(ctx, graphID, relation); err != nil {
			return fmt.Errorf("의미 관계 후보 저장: %w", err)
		}
	}
	return nil
}

func (s *Store) contextTable() string { return `"` + s.graphName + `"."Context"` }

// vectorText는 float64 목록을 pgvector 리터럴로 만든다.
//
// 검색 한 번과 색인 한 번마다 차원 수만큼 도는 자리다. 값마다 문자열을 만들지 않고 버퍼
// 하나에 이어 붙인다. %g와 같은 표현을 얻으려면 AppendFloat의 정밀도를 -1로 둔다.
func vectorText(values []float64) string {
	// 값 하나에 부호와 소수점, 지수까지 넉넉히 잡아 재할당을 없앤다.
	buffer := make([]byte, 0, len(values)*24+2)
	buffer = append(buffer, '[')
	for index, value := range values {
		if index > 0 {
			buffer = append(buffer, ',')
		}
		buffer = strconv.AppendFloat(buffer, value, 'g', -1, 64)
	}
	return string(append(buffer, ']'))
}

// enableIterativeScan은 이 트랜잭션의 벡터 인덱스 질의가 조건에 걸러진 만큼 더 훑게 한다.
//
// HNSW는 조건을 보지 않고 전역 근접 이웃을 먼저 꺼내므로, graph_id·model_id·삭제·유효 기간
// 조건이 뒤에서 걸러내면 상한을 채우지 못하거나 0건이 될 수 있다. 여러 그래프를 한 테이블에
// 논리로 격리하는 이 배포에서는 그래프가 작을수록 잘 걸린다. pgvector의 반복 탐색은 상한을
// 채울 때까지 인덱스를 이어 훑고, strict_order는 그러면서도 거리 순서를 정확히 지킨다.
//
// 반복 탐색은 pgvector 0.8.0에서 들어왔고 「버전 요구」의 하한은 0.7.0이다. 그래서 설정이
// 없는 배포에서는 아무것도 하지 않도록 WHERE로 막는다. SET LOCAL 대신 set_config를 쓰는
// 이유가 이것이며, 없는 설정에 SET을 보내면 오류로 트랜잭션이 끊긴다. is_local을 참으로
// 두어 설정이 이 트랜잭션에서만 살고 풀의 연결에 남지 않게 한다.
func enableIterativeScan(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `
		SELECT set_config('hnsw.iterative_scan', 'strict_order', true)
		WHERE current_setting('hnsw.iterative_scan', true) IS NOT NULL`); err != nil {
		return fmt.Errorf("벡터 반복 탐색 설정: %w", err)
	}
	return nil
}
