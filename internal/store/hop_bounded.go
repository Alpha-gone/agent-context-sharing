package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// hopWork는 DB 내부 스캔이 아니라 애플리케이션이 받은 행·본문의 측정값이다.
type hopWork struct {
	identifierRows, bodyRows, edgeRows, bodyBytes int
	// referenceRows는 근거·구성원 묶음 조회 행이다. 내부 간선과 겹쳐도 별도로 센다.
	referenceRows int
}

func (s *Store) hopContextsBounded(ctx context.Context, graphID model.ID, starts []model.Context, hops int, direction string, filter []string, limit int) (HopResult, error) {
	if !graphID.IsV7() || hops < 0 {
		return HopResult{}, fmt.Errorf("홉 탐색 인자가 올바르지 않다")
	}
	labels, err := traversalLabels(filter)
	if err != nil {
		return HopResult{}, err
	}
	result := HopResult{Distances: make(map[model.ID]int), Contexts: make([]model.Context, 0)}
	ids := make([]model.ID, 0, min(limit, len(starts)))
	frontier := make([]model.ID, 0, min(limit, len(starts)))
	for _, start := range starts {
		if _, seen := result.Distances[start.ID]; seen {
			continue
		}
		if len(ids) == limit {
			result.Truncated = true
			continue
		}
		result.Distances[start.ID] = 0
		ids = append(ids, start.ID)
		frontier = append(frontier, start.ID)
		result.Contexts = append(result.Contexts, start)
	}
	for depth := 0; depth < hops && len(frontier) > 0 && !result.Truncated; depth++ {
		next := make([]model.ID, 0)
		for _, label := range labels {
			remaining := limit - len(ids)
			// 1개 더 읽어야 정확히 상한과 같은 결과를 절단으로 오인하지 않는다.
			candidates, err := s.hopCandidateIDs(ctx, graphID, frontier, ids, label, direction, remaining)
			if err != nil {
				return HopResult{}, err
			}
			result.expansionQueries++
			result.work.identifierRows += len(candidates)
			if len(candidates) > remaining {
				if !result.Truncated {
					result.Truncated, result.Boundary = true, depth+1
				}
				candidates = candidates[:remaining]
			}
			for _, id := range candidates {
				result.Distances[id] = depth + 1
				ids = append(ids, id)
				next = append(next, id)
			}
			if result.Truncated {
				break
			}
		}
		frontier = next
	}
	// 시작 노드는 호출자가 이미 읽었으므로 새로 선택한 본문만 묶음으로 읽는다.
	selected := ids[len(result.Contexts):]
	byID, err := s.contextVerticesByIDs(ctx, s.reader(ctx), graphID, selected)
	if err != nil {
		return HopResult{}, err
	}
	for _, id := range selected {
		value, found := byID[id]
		if !found || value.DeletedAt != nil {
			return HopResult{}, missingContextError{ID: id}
		}
		result.Contexts = append(result.Contexts, value)
		result.work.bodyRows++
		result.work.bodyBytes += len(value.Body)
	}
	for _, label := range labels {
		edges, err := s.hopIncludedEdges(ctx, graphID, ids, label)
		if err != nil {
			return HopResult{}, err
		}
		result.work.edgeRows += len(edges)
		result.Edges = append(result.Edges, edges...)
	}
	slices.SortFunc(result.Edges, compareHopEdges)
	result.Edges = slices.Compact(result.Edges)
	result.work.referenceRows, err = s.fillHopReferences(ctx, graphID, result.Contexts)
	if err != nil {
		return HopResult{}, err
	}
	if err := s.touchEmbeddings(ctx, graphID, ids, nowUTC()); err != nil {
		return HopResult{}, err
	}
	return result, nil
}

func (s *Store) hopCandidateIDs(ctx context.Context, graphID model.ID, frontier, visited []model.ID, label traversalLabel, direction string, remaining int) ([]model.ID, error) {
	query, err := s.hopCandidateSQL(graphID, frontier, visited, label, direction, remaining)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader(ctx).Query(ctx, query, pgx.QueryExecModeExec)
	if err != nil {
		return nil, fmt.Errorf("홉 후보 식별자 조회: %w", err)
	}
	defer rows.Close()
	var ids []model.ID
	for rows.Next() {
		var raw, rawAnchor string
		if err := rows.Scan(&raw, &rawAnchor); err != nil {
			return nil, fmt.Errorf("홉 후보 식별자 행 해석: %w", err)
		}
		id, err := parseAnchorID(raw)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("홉 후보 식별자 행 읽기: %w", err)
	}
	return ids, nil
}

func (s *Store) hopCandidateSQL(graphID model.ID, frontier, visited []model.ID, label traversalLabel, direction string, remaining int) (string, error) {
	anchors, err := cypherIDList(frontier)
	if err != nil {
		return "", err
	}
	seen, err := cypherIDList(visited)
	if err != nil {
		return "", err
	}
	graph := cypherString(graphID.String())
	directedPairs := func(incoming bool) string {
		pattern := "-[edge:" + label.label + "]->"
		if incoming {
			pattern = "<-[edge:" + label.label + "]-"
		}
		query := "MATCH (anchor:Context)" + pattern + "(neighbor:Context) WHERE anchor.context_id IN " + anchors +
			" AND anchor.graph_id = " + graph + " AND neighbor.graph_id = " + graph + " AND edge.graph_id = " + graph +
			" AND neighbor.deleted_at IS NULL AND NOT neighbor.context_id IN " + seen
		if label.confirmed {
			query += " AND edge.state = 'confirmed'"
		}
		return s.cypherSQL(query+" RETURN anchor.context_id, neighbor.context_id", "anchor agtype, neighbor agtype")
	}
	pairs := directedPairs(direction == "in")
	if direction != "in" && direction != "out" {
		// AGE 무방향 패턴의 끝점 OR는 상관 존재 확인에서 인덱스 선택을 막는다.
		// 두 방향을 같은 SQL 안에서 합치고 뒤에서 중복·발견 순서를 정한다.
		pairs = "(" + directedPairs(false) + ") UNION ALL (" + directedPairs(true) + ")"
	}
	// 여러 기준 정점의 공통 이웃도 최초 발견 순서로 한 번만 선택한다.
	// limit=MaxInt에서도 초과 확인용 덧셈이 넘치지 않게 한다.
	probeLimit := remaining
	if remaining < int(^uint(0)>>1) {
		probeLimit++
	}
	if remaining == 0 {
		// 초과 여부만 필요하므로 최초 기준 정점이나 이웃의 순서를 계산하지 않는다.
		return "SELECT neighbor, anchor::text AS first_anchor FROM (" + pairs + ") AS pairs LIMIT 1", nil
	}
	anchorQuery := "MATCH (anchor:Context) WHERE anchor.graph_id = " + graph +
		" AND anchor.context_id IN " + anchors + " RETURN anchor.context_id"
	contextTable := pgx.Identifier{s.graphName, "Context"}.Sanitize()
	seenText := make([]string, len(visited))
	for index, id := range visited {
		seenText[index] = sqlString(id.String())
	}
	// 작은 인접 집합은 정렬하고, 큰 집합은 정렬된 정점에서 관계를 확인한다.
	// 각 기준 정점의 첫 K개 합집합에는 전체 발견 순서의 첫 K개가 모두 있다.
	probe := max(64, probeLimit)
	sampleLimit := probe
	if probe < int(^uint(0)>>1) {
		sampleLimit++
	}
	seenClause := ""
	if len(seenText) > 0 {
		seenClause = " AND node.properties ->> 'context_id'::text NOT IN (" + strings.Join(seenText, ",") + ")"
	}
	return fmt.Sprintf(`WITH pairs AS NOT MATERIALIZED (%s),
anchors AS MATERIALIZED (%s),
candidates AS (
 SELECT anchors.anchor, candidate.neighbor
 FROM anchors CROSS JOIN LATERAL (
  WITH sample AS MATERIALIZED (
   SELECT neighbor FROM pairs WHERE pairs.anchor = anchors.anchor LIMIT %d
  )
  (SELECT neighbor FROM sample WHERE (SELECT count(*) FROM sample) <= %d
   GROUP BY neighbor ORDER BY neighbor LIMIT %d)
  UNION ALL
  (SELECT ag_catalog.agtype_access_operator(VARIADIC ARRAY[node.properties, '"context_id"'::agtype]) AS neighbor
   FROM %s AS node
   WHERE (SELECT count(*) FROM sample) > %d
    AND node.properties ->> 'graph_id'::text = %s
    AND node.properties ->> 'deleted_at'::text IS NULL
    %s
    AND EXISTS (SELECT 1 FROM pairs WHERE pairs.anchor = anchors.anchor
      AND pairs.neighbor = ag_catalog.agtype_access_operator(VARIADIC ARRAY[node.properties, '"context_id"'::agtype]) OFFSET 0)
   ORDER BY (node.properties ->> 'context_id'::text) COLLATE "C" LIMIT %d)
 ) AS candidate
)
SELECT neighbor, min(anchor::text COLLATE "C") AS first_anchor
FROM candidates GROUP BY neighbor ORDER BY first_anchor, neighbor LIMIT %d`,
		pairs, s.cypherSQL(anchorQuery, "anchor agtype"), sampleLimit, probe, probeLimit,
		contextTable, probe, sqlString(graphID.String()), seenClause, probeLimit, probeLimit), nil
}

func (s *Store) hopIncludedEdges(ctx context.Context, graphID model.ID, ids []model.ID, label traversalLabel) ([]HopEdge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query, err := s.hopIncludedEdgeSQL(graphID, ids, label)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader(ctx).Query(ctx, query, pgx.QueryExecModeExec)
	if err != nil {
		return nil, fmt.Errorf("홉 내부 간선 조회: %w", err)
	}
	defer rows.Close()
	var edges []HopEdge
	for rows.Next() {
		var fromRaw, toRaw, rawEdge string
		if err := rows.Scan(&fromRaw, &toRaw, &rawEdge); err != nil {
			return nil, fmt.Errorf("홉 내부 간선 행 해석: %w", err)
		}
		from, to, err := parseStoredEndpoints(rawEdge, fromRaw, toRaw, graphID)
		if err != nil {
			return nil, err
		}
		if label.confirmed {
			relation, err := parseRelation(rawEdge, graphID)
			if err != nil {
				return nil, err
			}
			if relation.FromContextID != from.ID || relation.ToContextID != to.ID {
				return nil, storageError("hop_relation_identity", ErrInvariantViolation)
			}
		}
		edges = append(edges, HopEdge{FromID: from.ID, ToID: to.ID, Kind: label.kind})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("홉 내부 간선 행 읽기: %w", err)
	}
	return edges, nil
}

func (s *Store) hopIncludedEdgeSQL(graphID model.ID, ids []model.ID, label traversalLabel) (string, error) {
	list, err := cypherIDList(ids)
	if err != nil {
		return "", err
	}
	graph := cypherString(graphID.String())
	query := "MATCH (a:Context)-[edge:" + label.label + "]->(b:Context) WHERE a.context_id IN " + list + " AND b.context_id IN " + list +
		" AND a.graph_id = " + graph + " AND b.graph_id = " + graph + " AND edge.graph_id = " + graph
	if label.confirmed {
		query += " AND edge.state = 'confirmed'"
	}
	return s.cypherSQL(query+" RETURN DISTINCT "+endpointProjection("a")+", "+endpointProjection("b")+", edge", "source agtype, target agtype, edge agtype"), nil
}
