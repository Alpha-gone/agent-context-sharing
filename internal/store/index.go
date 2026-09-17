package store

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

const maxIndexAttempts = 5

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

// IndexTaskProcessor는 행 잠금을 유지하는 동안 임베딩 제공자를 호출하는 경계다.
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
}

// ProcessNextIndexTask는 대기 중인 작업 하나를 잠금으로 확보하고 제공자 결과를 저장한다.
// 처리 상태를 따로 저장하지 않고 트랜잭션 잠금이 살아 있는 동안만 작업을 소유한다.
func (s *Store) ProcessNextIndexTask(ctx context.Context, processor IndexTaskProcessor) (IndexProcessResult, error) {
	if processor == nil {
		return IndexProcessResult{}, fmt.Errorf("색인 작업 처리기가 없다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return IndexProcessResult{}, fmt.Errorf("색인 작업 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	var rawID, rawContextID, rawGraphID, correlationID string
	var attempts int
	err = tx.QueryRow(ctx, `
		SELECT task_id::text, context_id::text, graph_id::text, attempts, COALESCE(correlation_id, '')
		FROM public.index_task
		WHERE state = 'pending' AND next_attempt_at <= now()
		ORDER BY enqueued_at, task_id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).Scan(&rawID, &rawContextID, &rawGraphID, &attempts, &correlationID)
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
	result := processor(ctx, task)
	if result.Failure != "" {
		if err := s.recordIndexFailure(ctx, tx, task, result); err != nil {
			return IndexProcessResult{}, err
		}
		return IndexProcessResult{Found: true, Task: task}, tx.Commit(ctx)
	}
	if len(result.Embedding) == 0 || result.ModelID == "" {
		return IndexProcessResult{}, fmt.Errorf("색인 처리기가 빈 임베딩 또는 모델 식별자를 반환했다")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.context_embedding (context_id, graph_id, embedding, model_id, indexed_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (context_id) DO UPDATE SET
			graph_id = EXCLUDED.graph_id, embedding = EXCLUDED.embedding,
			model_id = EXCLUDED.model_id, indexed_at = EXCLUDED.indexed_at`,
		contextID.String(), graphID.String(), vectorText(result.Embedding), result.ModelID); err != nil {
		return IndexProcessResult{}, fmt.Errorf("임베딩 저장: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.index_task WHERE task_id = $1`, rawID); err != nil {
		return IndexProcessResult{}, fmt.Errorf("완료 색인 작업 제거: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IndexProcessResult{}, fmt.Errorf("색인 작업 커밋: %w", err)
	}
	return IndexProcessResult{Found: true, Succeeded: true, Task: task}, nil
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
	defer rows.Close()
	for rows.Next() {
		var rawGraphID string
		if err := rows.Scan(&rawGraphID); err != nil {
			return fmt.Errorf("재색인 그래프 행 해석: %w", err)
		}
		graphID, err := model.ParseID(rawGraphID)
		if err != nil {
			return fmt.Errorf("재색인 그래프 식별자 해석: %w", err)
		}
		if err := s.ReindexGraph(ctx, graphID); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("재색인 그래프 행 읽기: %w", err)
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

// ReindexGraph는 그래프의 모든 컨텍스트를 기존 upsert 규칙으로 다시 등록한다.
func (s *Store) ReindexGraph(ctx context.Context, graphID model.ID) error {
	if !graphID.IsV7() {
		return fmt.Errorf("그래프 식별자가 UUIDv7이 아니다")
	}
	rows, err := s.pool.Query(ctx, `SELECT properties ->> 'context_id'::text FROM `+s.contextTable()+` WHERE properties ->> 'graph_id'::text = $1`, graphID.String())
	if err != nil {
		return fmt.Errorf("재색인 대상 조회: %w", err)
	}
	defer rows.Close()
	ids := make([]model.ID, 0)
	for rows.Next() {
		var rawID string
		if err := rows.Scan(&rawID); err != nil {
			return fmt.Errorf("재색인 대상 행 해석: %w", err)
		}
		contextID, err := model.ParseID(rawID)
		if err != nil {
			return fmt.Errorf("재색인 대상 식별자 해석: %w", err)
		}
		ids = append(ids, contextID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("재색인 대상 행 읽기: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("재색인 등록 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	for _, contextID := range ids {
		if err := s.enqueueIndexTask(ctx, tx, graphID, contextID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("재색인 등록 커밋: %w", err)
	}
	return nil
}

// KeywordCandidates는 PostgreSQL simple 전문 검색으로 활성 기본 검색 후보를 읽는다.
func (s *Store) KeywordCandidates(ctx context.Context, graphID model.ID, query string, current time.Time, limit int) ([]SearchCandidate, error) {
	if !graphID.IsV7() || !current.UTC().Equal(current) || limit < 1 {
		return nil, fmt.Errorf("키워드 검색 인자가 올바르지 않다")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT properties ->> 'context_id'::text
		FROM `+s.contextTable()+`
		WHERE properties ->> 'graph_id'::text = $1
			AND properties ->> 'deleted_at'::text IS NULL
			AND ((properties ->> 'layer'::text) <> 'derived'
				OR NULLIF(properties ->> 'valid_to'::text, '') IS NULL
				OR (properties ->> 'valid_to'::text)::timestamptz >= $3)
			AND to_tsvector('simple', properties ->> 'body'::text) @@ plainto_tsquery('simple', $2)
		ORDER BY ts_rank_cd(to_tsvector('simple', properties ->> 'body'::text), plainto_tsquery('simple', $2)) DESC,
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
	rows, err := s.pool.Query(ctx, `
		SELECT candidate.context_id::text
		FROM public.context_embedding AS candidate
		JOIN `+s.contextTable()+` AS node ON (node.properties ->> 'context_id'::text)::uuid = candidate.context_id
		WHERE candidate.graph_id = $1 AND candidate.model_id = $2
			AND node.properties ->> 'graph_id'::text = $1::text
			AND node.properties ->> 'deleted_at'::text IS NULL
			AND ((node.properties ->> 'layer'::text) <> 'derived'
				OR NULLIF(node.properties ->> 'valid_to'::text, '') IS NULL
				OR (node.properties ->> 'valid_to'::text)::timestamptz >= $4)
		ORDER BY candidate.embedding <=> $3::vector, candidate.context_id
		LIMIT $5`, graphID.String(), modelID, vectorText(embedding), current, limit)
	if err != nil {
		return nil, fmt.Errorf("의미 유사도 검색: %w", err)
	}
	ids, err := searchCandidateIDs(rows)
	if err != nil {
		return nil, err
	}
	return s.searchCandidates(ctx, graphID, ids)
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
	rows, err := s.pool.Query(ctx, `
		SELECT candidate.context_id::text, target.embedding <=> candidate.embedding
		FROM public.context_embedding AS target
		JOIN public.context_embedding AS candidate ON candidate.graph_id = target.graph_id
		JOIN `+s.contextTable()+` AS node ON (node.properties ->> 'context_id'::text)::uuid = candidate.context_id
		WHERE target.context_id = $1 AND target.graph_id = $2 AND target.model_id = $3
			AND candidate.context_id <> target.context_id AND candidate.model_id = $3
			AND node.properties ->> 'layer'::text = 'event' AND node.properties ->> 'deleted_at'::text IS NULL
		ORDER BY target.embedding <=> candidate.embedding, candidate.context_id
		LIMIT $4`, eventID.String(), graphID.String(), modelID, s.relationProposals.Limit)
	if err != nil {
		return fmt.Errorf("의미 관계 후보 조회: %w", err)
	}
	// 후보를 먼저 모아 행을 놓아준다. 행을 연 채 관계를 만들면 같은 풀에서 트랜잭션용
	// 연결을 하나 더 잡아 색인 작업자와 요청 경로가 서로의 연결을 기다린다.
	similar := make([]model.ID, 0)
	for rows.Next() {
		var rawID string
		var distance float64
		if err := rows.Scan(&rawID, &distance); err != nil {
			rows.Close()
			return fmt.Errorf("의미 관계 후보 행 해석: %w", err)
		}
		if 1-distance < s.relationProposals.SimilarityThreshold {
			continue
		}
		otherID, err := model.ParseID(rawID)
		if err != nil {
			rows.Close()
			return fmt.Errorf("의미 관계 후보 식별자 해석: %w", err)
		}
		similar = append(similar, otherID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("의미 관계 후보 행 읽기: %w", err)
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

func vectorText(values []float64) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = fmt.Sprintf("%g", value)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
