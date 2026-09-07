package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Graph{}, ErrNotFound
		}
		return model.Graph{}, fmt.Errorf("그래프 조회: %w", err)
	}
	return graph, nil
}

// ListGraphs는 요청 계정에 유효한 등급이 있는 그래프만 최근 활동 순서로 읽는다.
func (s *Store) ListGraphs(ctx context.Context, accountID model.ID, filter model.GraphListFilter, cursor string, limit int) ([]model.GraphListItem, string, error) {
	if !accountID.IsV7() {
		return nil, "", fmt.Errorf("요청 계정 식별자가 UUIDv7이 아니다")
	}
	if limit <= 0 {
		return nil, "", fmt.Errorf("그래프 목록 페이지 크기는 양수여야 한다")
	}
	gradeRanks, err := graphGradeRanks(filter.Grades)
	if err != nil {
		return nil, "", err
	}
	var cursorActivity any
	var cursorGraphID any
	if cursor != "" {
		decoded, err := decodeGraphCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		cursorActivity = decoded.activity
		cursorGraphID = decoded.graphID.String()
	}
	arguments := []any{accountID.String(), likeContains(filter.Name), gradeRanks, cursorActivity, cursorGraphID, limit + 1}
	rows, err := s.pool.Query(ctx, `
		WITH grant_candidate AS (
			SELECT graph_id, grade
			FROM public.graph_grant
			WHERE subject_type = 'account' AND subject_id = $1::uuid
			UNION ALL
			SELECT permission.graph_id, permission.grade
			FROM public.graph_grant AS permission
			JOIN public.team_member AS membership ON membership.team_id = permission.subject_id
			JOIN public.team ON team.team_id = membership.team_id AND team.deleted_at IS NULL
			WHERE permission.subject_type = 'team' AND membership.account_id = $1::uuid
		), effective_grade AS (
			SELECT graph_id,
			       MAX(CASE grade WHEN 'owner' THEN 3 WHEN 'editor' THEN 2 WHEN 'viewer' THEN 1 END) AS rank
			FROM grant_candidate
			GROUP BY graph_id
		)
		SELECT graph.graph_id, graph.name, graph.description, graph.last_activity_at,
		       CASE effective_grade.rank WHEN 3 THEN 'owner' WHEN 2 THEN 'editor' ELSE 'viewer' END AS grade
		FROM public.context_graph AS graph
		JOIN effective_grade ON effective_grade.graph_id = graph.graph_id
		WHERE graph.deleted_at IS NULL
		  AND ($2::text IS NULL OR graph.name ILIKE $2 ESCAPE '\')
		  AND (cardinality($3::smallint[]) = 0 OR effective_grade.rank = ANY($3::smallint[]))
		  AND ($4::timestamptz IS NULL OR (
			graph.last_activity_at < $4 OR (graph.last_activity_at = $4 AND graph.graph_id < $5::uuid)
		  ))
		ORDER BY graph.last_activity_at DESC, graph.graph_id DESC
		LIMIT $6`, arguments...)

	if err != nil {
		return nil, "", fmt.Errorf("그래프 목록 조회: %w", err)
	}
	defer rows.Close()

	graphs := make([]model.GraphListItem, 0, limit)
	for rows.Next() {
		graph, err := scanGraphListItem(rows)
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

// graphGradeRanks는 등급 필터를 SQL 정렬값으로 바꾸고 허용 값을 검증한다.
func graphGradeRanks(grades []model.GraphGrade) ([]int16, error) {
	ranks := make([]int16, 0, len(grades))
	for _, grade := range grades {
		switch grade {
		case model.GraphGradeOwner:
			ranks = append(ranks, 3)
		case model.GraphGradeEditor:
			ranks = append(ranks, 2)
		case model.GraphGradeViewer:
			ranks = append(ranks, 1)
		default:
			return nil, fmt.Errorf("그래프 목록 등급 필터 %q가 올바르지 않다", grade)
		}
	}
	return ranks, nil
}

// likeContains는 SQL 와일드카드를 이스케이프해 이름 필터가 리터럴 부분 일치만 하게 한다.
func likeContains(value string) any {
	if value == "" {
		return nil
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
	return "%" + escaped + "%"
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

// scanGraphListItem은 목록에 노출할 그래프 필드와 요청 계정의 유효 등급만 해석한다.
func scanGraphListItem(row pgx.Row) (model.GraphListItem, error) {
	var graphID string
	var item model.GraphListItem
	var description *string
	if err := row.Scan(&graphID, &item.Name, &description, &item.LastActivityAt, &item.Grade); err != nil {
		return model.GraphListItem{}, err
	}
	var err error
	item.ID, err = model.ParseID(graphID)
	if err != nil {
		return model.GraphListItem{}, fmt.Errorf("graph_id: %w", err)
	}
	if !item.Grade.Valid() {
		return model.GraphListItem{}, fmt.Errorf("유효 등급 %q가 올바르지 않다", item.Grade)
	}
	if description != nil {
		item.Description = *description
	}
	item.LastActivityAt = item.LastActivityAt.UTC()
	return item, nil
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
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("현재 그래프 판 번호 조회: %w", err)
}

// nowUTC는 그래프 활동 시각을 한곳에서 UTC로 만든다.
func nowUTC() time.Time {
	return time.Now().UTC()
}
