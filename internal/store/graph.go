package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// CreateGraph는 관계형 그래프 메타데이터 행을 만든다.
func (s *Store) CreateGraph(ctx context.Context, graph model.Graph) (model.Graph, error) {
	if err := graph.Validate(); err != nil {
		return model.Graph{}, fmt.Errorf("그래프 검증: %w", err)
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO public.context_graph (
			graph_id, name, description, created_by, created_at, last_activity_at,
			grace_started_at, stored_chars, version, deleted_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING graph_id, name, description, created_by, created_at, last_activity_at,
		          grace_started_at, stored_chars, version, deleted_at`,
		graph.ID.String(), graph.Name, nullableString(graph.Description), graph.CreatedBy.String(),
		graph.CreatedAt, graph.LastActivityAt, graph.GraceStartedAt, graph.StoredChars, graph.Version,
		graph.DeletedAt,
	)
	stored, err := scanGraph(row)
	if err != nil {
		return model.Graph{}, fmt.Errorf("그래프 생성: %w", err)
	}
	return stored, nil
}

// Graph는 graph_id에 해당하는 그래프 메타데이터를 읽는다.
func (s *Store) Graph(ctx context.Context, graphID model.ID) (model.Graph, error) {
	if !graphID.IsV7() {
		return model.Graph{}, fmt.Errorf("그래프 식별자가 UUIDv7이 아니다")
	}
	row := s.pool.QueryRow(ctx, `
		SELECT graph_id, name, description, created_by, created_at, last_activity_at,
		       grace_started_at, stored_chars, version, deleted_at
		FROM public.context_graph
		WHERE graph_id = $1`, graphID.String())
	graph, err := scanGraph(row)
	if err != nil {
		return model.Graph{}, fmt.Errorf("그래프 조회: %w", err)
	}
	return graph, nil
}

// ListGraphs는 최근 활동 순서로 그래프 메타데이터 한 페이지를 읽는다.
func (s *Store) ListGraphs(ctx context.Context, cursor string, limit int) ([]model.Graph, string, error) {
	if limit <= 0 {
		return nil, "", fmt.Errorf("그래프 목록 페이지 크기는 양수여야 한다")
	}
	arguments := []any{limit + 1}
	query := `
		SELECT graph_id, name, description, created_by, created_at, last_activity_at,
		       grace_started_at, stored_chars, version, deleted_at
		FROM public.context_graph
		WHERE deleted_at IS NULL`
	if cursor != "" {
		decoded, err := decodeGraphCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		query += ` AND (last_activity_at < $2 OR (last_activity_at = $2 AND graph_id < $3::uuid))`
		arguments = append(arguments, decoded.activity, decoded.graphID.String())
	}
	query += ` ORDER BY last_activity_at DESC, graph_id DESC LIMIT $1`

	rows, err := s.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, "", fmt.Errorf("그래프 목록 조회: %w", err)
	}
	defer rows.Close()

	graphs := make([]model.Graph, 0, limit)
	for rows.Next() {
		graph, err := scanGraph(rows)
		if err != nil {
			return nil, "", fmt.Errorf("그래프 목록 해석: %w", err)
		}
		graphs = append(graphs, graph)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("그래프 목록 행 읽기: %w", err)
	}
	if len(graphs) <= limit {
		return graphs, "", nil
	}
	graphs = graphs[:limit]
	last := graphs[len(graphs)-1]
	return graphs, encodeGraphCursor(graphCursor{activity: last.LastActivityAt, graphID: last.ID}), nil
}

// UpdateGraph은 같은 트랜잭션에서 그래프 판 번호를 비교하고 하나 증가시킨다.
func (s *Store) UpdateGraph(ctx context.Context, graphID model.ID, expectedVersion int64, name, description string) (model.Graph, error) {
	if !graphID.IsV7() || expectedVersion < 1 || name == "" {
		return model.Graph{}, fmt.Errorf("그래프 갱신 인자가 올바르지 않다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.Graph{}, fmt.Errorf("그래프 갱신 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE public.context_graph
		SET name = $1, description = $2, version = version + 1
		WHERE graph_id = $3 AND version = $4
		RETURNING graph_id, name, description, created_by, created_at, last_activity_at,
		          grace_started_at, stored_chars, version, deleted_at`,
		name, nullableString(description), graphID.String(), expectedVersion,
	)
	graph, err := scanGraph(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return model.Graph{}, fmt.Errorf("그래프 갱신: %w", err)
		}
		if err := s.versionConflict(ctx, tx, graphID, expectedVersion); err != nil {
			return model.Graph{}, err
		}
		return model.Graph{}, fmt.Errorf("그래프 갱신: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Graph{}, fmt.Errorf("그래프 갱신 커밋: %w", err)
	}
	return graph, nil
}

// scanGraph은 관계형 행을 model.Graph로 해석한다.
func scanGraph(row pgx.Row) (model.Graph, error) {
	var graphID, createdBy string
	var graph model.Graph
	var description *string
	if err := row.Scan(
		&graphID, &graph.Name, &description, &createdBy, &graph.CreatedAt, &graph.LastActivityAt,
		&graph.GraceStartedAt, &graph.StoredChars, &graph.Version, &graph.DeletedAt,
	); err != nil {
		return model.Graph{}, err
	}
	var err error
	graph.ID, err = model.ParseID(graphID)
	if err != nil {
		return model.Graph{}, fmt.Errorf("graph_id: %w", err)
	}
	graph.CreatedBy, err = model.ParseID(createdBy)
	if err != nil {
		return model.Graph{}, fmt.Errorf("created_by: %w", err)
	}
	if description != nil {
		graph.Description = *description
	}
	graph.CreatedAt = graph.CreatedAt.UTC()
	graph.LastActivityAt = graph.LastActivityAt.UTC()
	if graph.GraceStartedAt != nil {
		graph.GraceStartedAt = new(graph.GraceStartedAt.UTC())
	}
	if graph.DeletedAt != nil {
		graph.DeletedAt = new(graph.DeletedAt.UTC())
	}
	if err := graph.Validate(); err != nil {
		return model.Graph{}, fmt.Errorf("저장된 그래프 값: %w", err)
	}
	return graph, nil
}

// nullableString은 빈 선택 문자열을 SQL NULL로 바꾼다.
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// versionConflict은 같은 트랜잭션에서 현재 판 번호를 읽어 충돌 또는 부재를 구분한다.
func (s *Store) versionConflict(ctx context.Context, tx pgx.Tx, graphID model.ID, expectedVersion int64) error {
	var current int64
	err := tx.QueryRow(ctx, `SELECT version FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&current)
	if err == nil {
		return VersionConflictError{Current: current}
	}
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	return fmt.Errorf("현재 그래프 판 번호 조회: %w", err)
}

// nowUTC는 그래프 활동 시각을 한곳에서 UTC로 만든다.
func nowUTC() time.Time {
	return time.Now().UTC()
}
