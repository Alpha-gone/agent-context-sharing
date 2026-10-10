package store

import (
	"context"
	"fmt"
	"slices"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// hopWork는 DB 내부 스캔이 아니라 애플리케이션이 받은 행·본문의 측정값이다.
type hopWork struct {
	identifierRows, bodyRows, edgeRows, bodyBytes int
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
	if err := s.fillHopReferences(ctx, graphID, result.Contexts); err != nil {
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
	pattern := "-[edge:" + label.label + "]-"
	if direction == "out" {
		pattern += ">"
	} else if direction == "in" {
		pattern = "<" + pattern
	}
	graph := cypherString(graphID.String())
	query := "MATCH (anchor:Context)" + pattern + "(neighbor:Context) WHERE anchor.context_id IN " + anchors +
		" AND anchor.graph_id = " + graph + " AND neighbor.graph_id = " + graph + " AND edge.graph_id = " + graph +
		" AND neighbor.deleted_at IS NULL AND NOT neighbor.context_id IN " + seen
	if label.confirmed {
		query += " AND edge.state = 'confirmed'"
	}
	// 여러 기준 정점의 공통 이웃도 최초 발견 순서로 한 번만 선택한다.
	// limit=MaxInt에서도 초과 확인용 덧셈이 넘치지 않게 한다.
	probeLimit := remaining
	if remaining < int(^uint(0)>>1) {
		probeLimit++
	}
	query += " RETURN anchor.context_id, neighbor.context_id"
	// AGE의 정점 속성 GROUP BY는 정점 전체를 정렬할 수 있다. Cypher에서 식별자만
	// 내보내고 SQL 외피에서 합쳐 본문이 중복 제거·정렬 작업에 실리지 않게 한다.
	return "WITH candidates AS MATERIALIZED (" + s.cypherSQL(query, "anchor agtype, neighbor agtype") +
		") SELECT neighbor, min(anchor::text COLLATE \"C\") AS first_anchor FROM candidates GROUP BY neighbor ORDER BY first_anchor, neighbor LIMIT " + fmt.Sprint(probeLimit), nil
}

func (s *Store) hopIncludedEdges(ctx context.Context, graphID model.ID, ids []model.ID, label traversalLabel) ([]HopEdge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := cypherIDList(ids)
	if err != nil {
		return nil, err
	}
	graph := cypherString(graphID.String())
	query := "MATCH (a:Context)-[edge:" + label.label + "]->(b:Context) WHERE a.context_id IN " + list + " AND b.context_id IN " + list +
		" AND a.graph_id = " + graph + " AND b.graph_id = " + graph + " AND edge.graph_id = " + graph
	if label.confirmed {
		query += " AND edge.state = 'confirmed'"
	}
	query += " RETURN DISTINCT a.context_id, b.context_id"
	rows, err := s.reader(ctx).Query(ctx, s.cypherSQL(query, "source agtype, target agtype"), pgx.QueryExecModeExec)
	if err != nil {
		return nil, fmt.Errorf("홉 내부 간선 조회: %w", err)
	}
	defer rows.Close()
	var edges []HopEdge
	for rows.Next() {
		var fromRaw, toRaw string
		if err := rows.Scan(&fromRaw, &toRaw); err != nil {
			return nil, fmt.Errorf("홉 내부 간선 행 해석: %w", err)
		}
		from, err := parseAnchorID(fromRaw)
		if err != nil {
			return nil, err
		}
		to, err := parseAnchorID(toRaw)
		if err != nil {
			return nil, err
		}
		edges = append(edges, HopEdge{FromID: from, ToID: to, Kind: label.kind})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("홉 내부 간선 행 읽기: %w", err)
	}
	return edges, nil
}
