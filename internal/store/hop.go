package store

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// HopEdge는 반환한 부분 그래프 안의 참조 또는 확정 사건 관계다.
type HopEdge struct {
	FromID model.ID
	ToID   model.ID
	Kind   string
}

// HopResult는 시작 노드에서 지정한 범위까지의 컨텍스트와 연결을 담는다.
//
// Contexts는 깊이별로 나뉘지 않은 너비 우선 순서의 평평한 목록이므로 목록 위치로는 홉
// 거리를 복원할 수 없다. 호출자가 거리를 응답에 실을 수 있도록 Distances에 노드별 최단
// 홉 거리를 함께 둔다. 시작 노드의 거리는 0이다.
type HopResult struct {
	Contexts  []model.Context
	Distances map[model.ID]int
	Edges     []HopEdge
	Truncated bool
	Boundary  int
}

// HopContexts는 graph_id 안에서 확정 참조와 관계를 따라 너비 우선으로 탐색한다.
func (s *Store) HopContexts(ctx context.Context, graphID, startID model.ID, hops int, direction string, filter []string, limit int) (HopResult, error) {
	if !graphID.IsV7() || !startID.IsV7() {
		return HopResult{}, fmt.Errorf("홉 탐색 인자가 올바르지 않다")
	}
	start, err := s.Context(ctx, graphID, startID)
	if err != nil {
		return HopResult{}, err
	}
	if start.DeletedAt != nil {
		return HopResult{}, ErrNotFound
	}
	return s.HopContextsFrom(ctx, graphID, []model.Context{start}, hops, direction, filter, limit)
}

// HopContextsFrom은 시작 노드 여럿에서 한 번에 너비 우선으로 탐색한다.
//
// 시작 노드마다 따로 호출하지 않는 이유는 두 가지다. 「검색 채널 실행기」의 2단계가 진입
// 채널의 결과를 모아 확장 채널의 시작 노드로 넘기라고 확정했고, 따로 호출하면 「채널
// 구현」이 하나로 두기로 한 결과 상한이 시작 노드 수만큼 겹쳐 어느 것이 잘랐는지 알 수
// 없게 된다. 거리는 가장 가까운 시작 노드까지의 최단 홉 거리다.
func (s *Store) HopContextsFrom(ctx context.Context, graphID model.ID, starts []model.Context, hops int, direction string, filter []string, limit int) (HopResult, error) {
	// limit 0은 「계정 플랜」이 선언한 대로 한도 없음이다. 값을 그대로 내려받아 여기에서
	// 해석하지 않으면 한도를 푸는 설정이 연산을 죽인다.
	if !graphID.IsV7() || hops < 0 || limit < 0 {
		return HopResult{}, fmt.Errorf("홉 탐색 인자가 올바르지 않다")
	}
	labels, err := traversalLabels(filter)
	if err != nil {
		return HopResult{}, err
	}
	// 방문 여부와 최단 홉 거리는 같은 판정에서 나오므로 맵 하나로 둔다. 너비 우선이라
	// 처음 방문한 깊이가 곧 최단 거리다.
	distances := make(map[model.ID]int, len(starts))
	result := HopResult{Contexts: make([]model.Context, 0, len(starts))}
	frontier := make([]model.ID, 0, len(starts))
	for _, start := range starts {
		if _, found := distances[start.ID]; found {
			continue
		}
		if limit > 0 && len(result.Contexts) == limit {
			// 시작 노드에서 이미 상한을 넘으면 확장 전에 자른 것이므로 경계는 0이다.
			if !result.Truncated {
				result.Truncated, result.Boundary = true, 0
			}
			continue
		}
		distances[start.ID] = 0
		result.Contexts = append(result.Contexts, start)
		frontier = append(frontier, start.ID)
	}
	edges := make(map[string]HopEdge)
	for depth := 1; depth <= hops && len(frontier) > 0; depth++ {
		next := make([]model.ID, 0)
		for _, label := range labels {
			neighbors, err := s.hopNeighbors(ctx, graphID, frontier, label, direction)
			if err != nil {
				return HopResult{}, err
			}
			for _, neighbor := range neighbors {
				if neighbor.context.DeletedAt != nil {
					continue
				}
				edgeKey := neighbor.fromID.String() + "|" + neighbor.toID.String() + "|" + label.kind
				edges[edgeKey] = HopEdge{FromID: neighbor.fromID, ToID: neighbor.toID, Kind: label.kind}
				if _, found := distances[neighbor.context.ID]; found {
					continue
				}
				if limit > 0 && len(result.Contexts) == limit {
					// 경계는 처음 자른 깊이다. 덮어쓰면 마지막 깊이가 남아 어디에서
					// 잘렸는지 알 수 없다. 상한에 닿아도 남은 label의 질의를 계속 내는
					// 이유는 「홉 범위 조회」가 방문한 노드 사이의 참조와 관계를 모두
					// 반환하라고 확정했기 때문이다. 여기에서 빠져나가면 이미 담은 노드를
					// 잇는 간선이 빠져 부분 그래프를 복원할 수 없다.
					if !result.Truncated {
						result.Truncated, result.Boundary = true, depth
					}
					continue
				}
				distances[neighbor.context.ID] = depth
				result.Contexts = append(result.Contexts, neighbor.context)
				next = append(next, neighbor.context.ID)
			}
		}
		frontier = next
	}
	result.Distances = distances
	result.Edges = slices.SortedFunc(edgesValues(edges, distances), compareHopEdges)
	if err := s.fillHopReferences(ctx, graphID, result.Contexts); err != nil {
		return HopResult{}, err
	}
	accessed := make([]model.ID, 0, len(result.Contexts))
	for _, value := range result.Contexts {
		if value.DeletedAt == nil {
			accessed = append(accessed, value.ID)
		}
	}
	if err := s.touchEmbeddings(ctx, graphID, accessed, nowUTC()); err != nil {
		return HopResult{}, err
	}
	return result, nil
}

// fillHopReferences는 확장으로 가져온 파생과 사건에 근거와 구성원 목록을 채운다.
//
// hopNeighbors는 정점 속성만 읽어 오는데 참조는 속성이 아니라 간선에 있다. 비우면 흐름
// 응답의 확장 노드에서 `derived_from`과 `member_refs`가 빈 목록으로 나가고, 「예산 적용과
// 절단」이 근거가 다르면 접지 않기로 한 중복 파생 접기가 근거를 구분하지 못한다.
// 노드마다 묶어 읽지 않고 두 질의로 한 번에 읽는다.
func (s *Store) fillHopReferences(ctx context.Context, graphID model.ID, contexts []model.Context) error {
	needs := make([]string, 0, len(contexts))
	for _, value := range contexts {
		if value.Layer == model.LayerDerived || value.Layer == model.LayerEvent {
			needs = append(needs, cypherString(value.ID.String()))
		}
	}
	if len(needs) == 0 {
		return nil
	}
	list := "[" + strings.Join(needs, ", ") + "]"
	references, err := s.edgeTargetsBySource(ctx, s.pool, graphID, "DERIVED_FROM", list)
	if err != nil {
		return err
	}
	members, err := s.edgeTargetsBySource(ctx, s.pool, graphID, "HAS_MEMBER", list)
	if err != nil {
		return err
	}
	for index, value := range contexts {
		switch value.Layer {
		case model.LayerDerived:
			if value.Derived != nil {
				contexts[index].Derived.DerivedFrom = references[value.ID]
			}
		case model.LayerEvent:
			if value.Event != nil {
				contexts[index].Event.MemberIDs = members[value.ID]
			}
		}
	}
	return nil
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

type hopNeighbor struct {
	context      model.Context
	fromID, toID model.ID
}

// hopNeighbors는 한 깊이의 기준 정점 전체를 label마다 한 질의로 확장한다.
//
// 정점마다 따로 물으면 왕복이 기준 정점 수만큼 늘어나므로 IN 목록으로 묶고, 어느 정점에서
// 나온 이웃인지는 기준 정점의 context_id를 함께 받아 구분한다. 양방향은 방향마다 질의를
// 나누지 않고 무방향 패턴 하나로 묻는다. AGE는 무방향 패턴에서 같은 간선을 양쪽 방향으로
// 한 번씩 돌려주므로 결과가 방향별 두 질의와 같고, 깊이마다 왕복이 절반으로 줄어든다.
// label 교대는 쓰지 않는다. 배포한 AGE 1.8.0이 `[e:A|B]` 문법을 문법 오류로 거부한다.
//
// 간선의 실제 방향은 기준 정점이 간선의 시작인지로 판정한다. 무방향 패턴에서는 패턴의
// from과 to가 기준 정점과 이웃 중 어느 쪽인지 고정되지 않기 때문이다.
func (s *Store) hopNeighbors(ctx context.Context, graphID model.ID, anchorIDs []model.ID, label traversalLabel, direction string) ([]hopNeighbor, error) {
	if len(anchorIDs) == 0 {
		return nil, nil
	}
	anchor, neighbor, pattern := "anchor", "neighbor", "-[edge:"+label.label+"]-"
	switch direction {
	case "out":
		pattern = "-[edge:" + label.label + "]->"
	case "in":
		anchor, neighbor = neighbor, anchor
		pattern = "-[edge:" + label.label + "]->"
	}
	identifiers := make([]string, 0, len(anchorIDs))
	for _, anchorID := range anchorIDs {
		identifiers = append(identifiers, cypherString(anchorID.String()))
	}
	query := "MATCH (anchor:Context)" + pattern + "(neighbor:Context) WHERE " + anchor + ".context_id IN [" + strings.Join(identifiers, ", ") + "]" +
		" AND " + anchor + ".graph_id = " + cypherString(graphID.String()) +
		" AND " + neighbor + ".graph_id = " + cypherString(graphID.String()) + " AND edge.graph_id = " + cypherString(graphID.String())
	if label.confirmed {
		query += " AND edge.state = 'confirmed'"
	}
	query += " RETURN " + anchor + ".context_id, " + neighbor + ", id(" + anchor + ") = id(startNode(edge))" +
		" ORDER BY " + anchor + ".context_id, " + neighbor + ".context_id"
	rows, err := s.pool.Query(ctx, s.cypherSQL(query, "anchor agtype, node agtype, forward agtype"), pgx.QueryExecModeExec)
	if err != nil {
		return nil, fmt.Errorf("홉 이웃 조회: %w", err)
	}
	defer rows.Close()
	neighbors := make([]hopNeighbor, 0)
	for rows.Next() {
		var rawAnchor, raw, rawForward string
		if err := rows.Scan(&rawAnchor, &raw, &rawForward); err != nil {
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
		var forward bool
		if err := json.Unmarshal([]byte(rawForward), &forward); err != nil {
			return nil, fmt.Errorf("홉 이웃 간선 방향 해석: %w", err)
		}
		fromID, toID := anchorID, value.ID
		if !forward {
			fromID, toID = value.ID, anchorID
		}
		neighbors = append(neighbors, hopNeighbor{context: value, fromID: fromID, toID: toID})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("홉 이웃 행 읽기: %w", err)
	}
	return neighbors, nil
}

// parseAnchorID는 agtype 문자열로 돌아온 정점의 context_id를 식별자로 바꾼다.
// 홉 탐색의 기준 정점뿐 아니라 간선 목록 질의가 받은 양쪽 정점의 식별자도 이 함수를 지난다.
func parseAnchorID(raw string) (model.ID, error) {
	var text string
	if err := json.Unmarshal([]byte(raw), &text); err != nil {
		return model.ID{}, fmt.Errorf("정점 식별자 해석: %w", err)
	}
	anchorID, err := model.ParseID(text)
	if err != nil {
		return model.ID{}, fmt.Errorf("정점 식별자 해석: %w", err)
	}
	return anchorID, nil
}

func edgesValues(edges map[string]HopEdge, visited map[model.ID]int) func(func(HopEdge) bool) {
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
