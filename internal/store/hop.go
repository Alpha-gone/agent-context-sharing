package store

import (
	"bytes"
	"cmp"
	"context"
	json "encoding/json/v2"
	"fmt"
	"math"
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
	// expansionQueries 필드에는 확장에 쓴 데이터베이스 질의 수를 둔다. 「홉 탐색 구현 비교」가
	// 두 구현의 왕복 횟수를 견주는 측정 지점이며 응답에는 싣지 않는다.
	expansionQueries int
}

// hopNeighborSource는 한 깊이의 기준 정점 전체를 label 하나로 확장한 이웃을 돌려준다.
// 기준 정점 context_id와 이웃 context_id의 오름차순이어야 결과 상한이 같은 노드에서 자른다.
type hopNeighborSource func(frontier []model.ID, label traversalLabel) ([]hopNeighbor, error)

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
	queries := 0
	return s.traverseHops(ctx, graphID, starts, hops, filter, limit, &queries, func(frontier []model.ID, label traversalLabel) ([]hopNeighbor, error) {
		queries++
		return s.hopNeighbors(ctx, graphID, frontier, label, direction)
	})
}

// hopContextsPrefetched는 「홉 탐색 구현 비교」의 후보다. 첫 확장 전에 가변 길이 간선 질의
// 한 번으로 도달 가능한 정점의 인접 간선을 모두 가져오고, 깊이·label별 확장은 그 결과에서
// 기준선과 같은 조건과 순서로 만든다. 너비 우선 규칙은 기준선과 같은 루프를 공유한다.
func (s *Store) hopContextsPrefetched(ctx context.Context, graphID model.ID, starts []model.Context, hops int, direction string, filter []string, limit int) (HopResult, error) {
	queries := 0
	var subgraph hopSubgraph
	return s.traverseHops(ctx, graphID, starts, hops, filter, limit, &queries, func(frontier []model.ID, label traversalLabel) ([]hopNeighbor, error) {
		if subgraph == nil {
			// 첫 호출의 기준 정점은 상한 안에 담긴 시작 노드다. 기준선도 이 노드들만 확장한다.
			queries++
			fetched, err := s.fetchHopSubgraph(ctx, graphID, frontier, hops)
			if err != nil {
				return nil, err
			}
			subgraph = fetched
		}
		return subgraph.neighbors(frontier, label, direction), nil
	})
}

// traverseHops는 두 구현이 공유하는 너비 우선 규칙이다. 이웃을 어디에서 가져오는지만 다르다.
func (s *Store) traverseHops(ctx context.Context, graphID model.ID, starts []model.Context, hops int, filter []string, limit int, queries *int, source hopNeighborSource) (HopResult, error) {
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
			neighbors, err := source(frontier, label)
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
	result.expansionQueries = *queries
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

// hopIncident는 가져온 부분 그래프에서 기준 정점 하나에 닿은 간선 하나다.
type hopIncident struct {
	neighbor model.Context
	label    string
	state    string
	// forward 필드는 기준 정점이 간선의 시작인지다.
	forward bool
}

// hopSubgraph는 기준 정점별 인접 간선이다.
type hopSubgraph map[model.ID][]hopIncident

// fetchHopSubgraph는 시작 노드에서 hops-1홉 안의 정점을 가변 길이 간선으로 모으고 그
// 정점들의 인접 간선을 같은 질의에서 가져온다. 기준선이 확장하는 frontier는 깊이 hops-1까지
// 이므로 이 범위의 인접 간선이면 기준선이 묻는 행을 모두 담는다.
//
// 가변 길이 간선에는 graph_id 조건만 건다. AGE 1.8.0의 가변 길이 간선은 label을 하나만 받고
// 경로 노드 조건을 지원하지 않아, label·관계 상태·삭제 노드 조건을 걸면 여러 label을 거치는
// 경로나 기준선이 지나는 경로를 잃는다. 조건을 빼고 모은 정점은 기준선이 도달하는 정점의
// 상위 집합이고, 조건 판정은 neighbors가 기준선과 같은 규칙으로 한다.
func (s *Store) fetchHopSubgraph(ctx context.Context, graphID model.ID, startIDs []model.ID, hops int) (hopSubgraph, error) {
	identifiers := make([]string, 0, len(startIDs))
	for _, startID := range startIDs {
		identifiers = append(identifiers, cypherString(startID.String()))
	}
	// 상한 없는 탐색은 가변 길이 간선의 상한을 비워 연결 요소 전체를 모은다.
	span := fmt.Sprintf("*0..%d", hops-1)
	if hops == math.MaxInt {
		span = "*0.."
	}
	graph := cypherString(graphID.String())
	query := "MATCH (start:Context)-[" + span + " {graph_id: " + graph + "}]-(anchor:Context)" +
		" WHERE start.context_id IN [" + strings.Join(identifiers, ", ") + "] AND start.graph_id = " + graph +
		" WITH DISTINCT anchor" +
		" MATCH (anchor)-[edge]-(neighbor:Context) WHERE edge.graph_id = " + graph + " AND neighbor.graph_id = " + graph +
		" RETURN anchor.context_id, neighbor, label(edge), coalesce(edge.state, ''), id(anchor) = id(startNode(edge))"
	rows, err := s.pool.Query(ctx, s.cypherSQL(query, "anchor agtype, node agtype, label agtype, state agtype, forward agtype"), pgx.QueryExecModeExec)
	if err != nil {
		return nil, fmt.Errorf("홉 부분 그래프 조회: %w", err)
	}
	defer rows.Close()
	subgraph := hopSubgraph{}
	for rows.Next() {
		var rawAnchor, raw, rawLabel, rawState, rawForward string
		if err := rows.Scan(&rawAnchor, &raw, &rawLabel, &rawState, &rawForward); err != nil {
			return nil, fmt.Errorf("홉 부분 그래프 행 해석: %w", err)
		}
		anchorID, err := parseAnchorID(rawAnchor)
		if err != nil {
			return nil, err
		}
		value, err := parseContext(raw, graphID)
		if err != nil {
			return nil, err
		}
		incident := hopIncident{neighbor: value}
		if err := json.Unmarshal([]byte(rawLabel), &incident.label); err != nil {
			return nil, fmt.Errorf("홉 부분 그래프 간선 label 해석: %w", err)
		}
		if err := json.Unmarshal([]byte(rawState), &incident.state); err != nil {
			return nil, fmt.Errorf("홉 부분 그래프 간선 상태 해석: %w", err)
		}
		if err := json.Unmarshal([]byte(rawForward), &incident.forward); err != nil {
			return nil, fmt.Errorf("홉 부분 그래프 간선 방향 해석: %w", err)
		}
		subgraph[anchorID] = append(subgraph[anchorID], incident)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("홉 부분 그래프 행 읽기: %w", err)
	}
	return subgraph, nil
}

// neighbors는 hopNeighbors가 같은 frontier와 label로 돌려줄 행을 같은 순서로 만든다.
func (subgraph hopSubgraph) neighbors(frontier []model.ID, label traversalLabel, direction string) []hopNeighbor {
	type row struct {
		anchor model.ID
		hopNeighbor
	}
	rows := make([]row, 0)
	for _, anchorID := range frontier {
		for _, incident := range subgraph[anchorID] {
			if incident.label != label.label || (label.confirmed && incident.state != "confirmed") {
				continue
			}
			if (direction == "out" && !incident.forward) || (direction == "in" && incident.forward) {
				continue
			}
			fromID, toID := anchorID, incident.neighbor.ID
			if !incident.forward {
				fromID, toID = incident.neighbor.ID, anchorID
			}
			rows = append(rows, row{anchor: anchorID, hopNeighbor: hopNeighbor{context: incident.neighbor, fromID: fromID, toID: toID}})
		}
	}
	slices.SortStableFunc(rows, func(left, right row) int {
		return cmp.Or(strings.Compare(left.anchor.String(), right.anchor.String()), strings.Compare(left.context.ID.String(), right.context.ID.String()))
	})
	neighbors := make([]hopNeighbor, len(rows))
	for index, value := range rows {
		neighbors[index] = value.hopNeighbor
	}
	return neighbors
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
