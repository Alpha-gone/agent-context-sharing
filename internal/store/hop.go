package store

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"agent_context_sharing/internal/model"
)

// HopEdge는 반환한 부분 그래프 안의 참조 또는 확정 사건 관계다.
type HopEdge struct {
	FromID model.ID
	ToID   model.ID
	Kind   string
}

// HopResult는 시작 노드에서 지정한 범위까지의 컨텍스트와 연결을 담는다.
type HopResult struct {
	Contexts  []model.Context
	Edges     []HopEdge
	Truncated bool
	Boundary  int
}

// HopContexts는 graph_id 안에서 확정 참조와 관계를 따라 너비 우선으로 탐색한다.
func (s *Store) HopContexts(ctx context.Context, graphID, startID model.ID, hops int, direction string, filter []string, limit int) (HopResult, error) {
	if !graphID.IsV7() || !startID.IsV7() || hops < 0 || limit < 1 {
		return HopResult{}, fmt.Errorf("홉 탐색 인자가 올바르지 않다")
	}
	start, err := s.Context(ctx, graphID, startID)
	if err != nil {
		return HopResult{}, err
	}
	if start.DeletedAt != nil {
		return HopResult{}, ErrNotFound
	}
	labels, err := traversalLabels(filter)
	if err != nil {
		return HopResult{}, err
	}
	visited := map[model.ID]struct{}{start.ID: {}}
	result := HopResult{Contexts: []model.Context{start}}
	frontier := []model.ID{start.ID}
	edges := make(map[string]HopEdge)
	for depth := 1; depth <= hops && len(frontier) > 0; depth++ {
		next := make([]model.ID, 0)
		for _, label := range labels {
			for _, reverse := range traversalDirections(direction) {
				neighbors, err := s.hopNeighbors(ctx, graphID, frontier, label, reverse)
				if err != nil {
					return HopResult{}, err
				}
				for _, neighbor := range neighbors {
					if neighbor.context.DeletedAt != nil {
						continue
					}
					edgeKey := neighbor.fromID.String() + "|" + neighbor.toID.String() + "|" + label.kind
					edges[edgeKey] = HopEdge{FromID: neighbor.fromID, ToID: neighbor.toID, Kind: label.kind}
					if _, found := visited[neighbor.context.ID]; found {
						continue
					}
					if len(result.Contexts) == limit {
						// 경계는 처음 자른 깊이다. 덮어쓰면 마지막 깊이가 남아 어디에서
						// 잘렸는지 알 수 없다.
						if !result.Truncated {
							result.Truncated, result.Boundary = true, depth
						}
						continue
					}
					visited[neighbor.context.ID] = struct{}{}
					result.Contexts = append(result.Contexts, neighbor.context)
					next = append(next, neighbor.context.ID)
				}
			}
		}
		frontier = next
	}
	result.Edges = slices.SortedFunc(edgesValues(edges, visited), compareHopEdges)
	return result, nil
}

type traversalLabel struct {
	label     string
	kind      string
	confirmed bool
}

func traversalLabels(filter []string) ([]traversalLabel, error) {
	all := []traversalLabel{
		{label: "DERIVED_FROM", kind: "derived_from"}, {label: "SUPERSEDES", kind: "supersedes"}, {label: "HAS_MEMBER", kind: "has_member"},
		{label: "PRECEDES", kind: "precedes", confirmed: true}, {label: "CAUSES", kind: "causes", confirmed: true}, {label: "PART_OF", kind: "part_of", confirmed: true}, {label: "RELATES_TO", kind: "relates_to", confirmed: true},
	}
	if len(filter) == 0 {
		return all, nil
	}
	labels := make([]traversalLabel, 0, len(filter))
	for _, kind := range filter {
		index := slices.IndexFunc(all, func(label traversalLabel) bool { return label.kind == kind })
		if index == -1 {
			return nil, fmt.Errorf("탐색 필터 %q가 올바르지 않다", kind)
		}
		labels = append(labels, all[index])
	}
	return labels, nil
}

func traversalDirections(direction string) []bool {
	switch direction {
	case "in":
		return []bool{true}
	case "out":
		return []bool{false}
	default:
		return []bool{false, true}
	}
}

type hopNeighbor struct {
	context      model.Context
	fromID, toID model.ID
}

// hopNeighbors는 한 깊이의 기준 정점 전체를 한 질의로 확장한다. 정점마다 따로 물으면
// 왕복이 기준 정점 수만큼 늘어나므로 IN 목록으로 묶고, 어느 정점에서 나온 이웃인지는
// 기준 정점의 context_id를 함께 받아 구분한다.
func (s *Store) hopNeighbors(ctx context.Context, graphID model.ID, anchorIDs []model.ID, label traversalLabel, reverse bool) ([]hopNeighbor, error) {
	if len(anchorIDs) == 0 {
		return nil, nil
	}
	anchor, neighbor := "from", "to"
	if reverse {
		anchor, neighbor = neighbor, anchor
	}
	identifiers := make([]string, 0, len(anchorIDs))
	for _, anchorID := range anchorIDs {
		identifiers = append(identifiers, cypherString(anchorID.String()))
	}
	query := "MATCH (from:Context)-[edge:" + label.label + "]->(to:Context) WHERE " + anchor + ".context_id IN [" + strings.Join(identifiers, ", ") + "]" +
		" AND " + anchor + ".graph_id = " + cypherString(graphID.String()) +
		" AND " + neighbor + ".graph_id = " + cypherString(graphID.String()) + " AND edge.graph_id = " + cypherString(graphID.String())
	if label.confirmed {
		query += " AND edge.state = 'confirmed'"
	}
	query += " RETURN " + anchor + ".context_id, " + neighbor + " ORDER BY " + anchor + ".context_id, " + neighbor + ".context_id"
	rows, err := s.pool.Query(ctx, s.cypherSQL(query, "anchor agtype, node agtype"))
	if err != nil {
		return nil, fmt.Errorf("홉 이웃 조회: %w", err)
	}
	defer rows.Close()
	neighbors := make([]hopNeighbor, 0)
	for rows.Next() {
		var rawAnchor, raw string
		if err := rows.Scan(&rawAnchor, &raw); err != nil {
			return nil, fmt.Errorf("홉 이웃 행 해석: %w", err)
		}
		anchorID, err := parseAnchorID(rawAnchor)
		if err != nil {
			return nil, err
		}
		value, err := parseContext(raw, graphID)
		if err != nil {
			return nil, err
		}
		fromID, toID := anchorID, value.ID
		if reverse {
			fromID, toID = value.ID, anchorID
		}
		neighbors = append(neighbors, hopNeighbor{context: value, fromID: fromID, toID: toID})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("홉 이웃 행 읽기: %w", err)
	}
	return neighbors, nil
}

// parseAnchorID는 agtype 문자열로 돌아온 기준 정점의 context_id를 식별자로 바꾼다.
func parseAnchorID(raw string) (model.ID, error) {
	var text string
	if err := json.Unmarshal([]byte(raw), &text); err != nil {
		return model.ID{}, fmt.Errorf("홉 기준 정점 식별자 해석: %w", err)
	}
	anchorID, err := model.ParseID(text)
	if err != nil {
		return model.ID{}, fmt.Errorf("홉 기준 정점 식별자 해석: %w", err)
	}
	return anchorID, nil
}

func edgesValues(edges map[string]HopEdge, visited map[model.ID]struct{}) func(func(HopEdge) bool) {
	return func(yield func(HopEdge) bool) {
		for _, edge := range edges {
			if _, fromFound := visited[edge.FromID]; !fromFound {
				continue
			}
			if _, toFound := visited[edge.ToID]; !toFound {
				continue
			}
			if !yield(edge) {
				return
			}
		}
	}
}

// compareHopEdges는 응답 안의 참조와 관계 순서를 양 끝 context_id 오름차순으로 고정한다.
// 같은 두 노드가 서로 다른 종류로 이어질 수 있으므로 종류를 마지막 비교 기준으로 둔다.
func compareHopEdges(left, right HopEdge) int {
	if order := bytes.Compare(left.FromID[:], right.FromID[:]); order != 0 {
		return order
	}
	if order := bytes.Compare(left.ToID[:], right.ToID[:]); order != 0 {
		return order
	}
	return strings.Compare(left.Kind, right.Kind)
}
