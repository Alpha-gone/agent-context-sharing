package store

import (
	"cmp"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 「그래프 불변식 규칙과 감사」의 규칙 목록이다. 쓰기 검사와 전체 감사는 같은 목록을 쓰고
// 다른 것은 읽는 범위뿐이다. 규칙은 저장 상태를 읽은 스냅숏만 보고 판정하므로 저장 방식과
// 무관하게 같은 판정을 낸다. 위반은 보고할 뿐 저장 상태를 바꾸지 않는다.

// 규칙 식별자는 감사 출력과 테스트가 기대는 안정적인 이름이다.
const (
	RuleGraphMembership      = "graph_membership"
	RuleEndpointLayer        = "endpoint_layer"
	RuleReferenceCardinality = "reference_cardinality"
	RuleRelationIdentity     = "relation_identity"
	RuleRelationAcyclic      = "relation_acyclic"
	RuleRelationTime         = "relation_time"
	RuleEmbeddingLink        = "embedding_link"
)

// 위반 대상 종류다.
const (
	TargetContext   = "context"
	TargetReference = "reference"
	TargetRelation  = "relation"
	TargetEmbedding = "embedding"
)

// ErrInvariantViolation은 쓰기가 영향 주변부의 불변식을 깨 커밋하지 않았음을 나타낸다.
// 도메인 검증을 지난 뒤 저장 상태가 어긋난 것이므로 호출자에게는 내부 오류다.
var ErrInvariantViolation = errors.New("그래프 불변식 위반")

// InvariantViolation은 위반 하나의 위치다. 본문과 임베딩 값은 담지 않는다.
type InvariantViolation struct {
	Rule    string   `json:"rule"`
	GraphID string   `json:"graph_id"`
	Target  string   `json:"target"`
	Label   string   `json:"label,omitzero"`
	IDs     []string `json:"ids"`
}

// EmbeddingExpectation은 embedding_link가 대조할 현재 모델 식별자와 차원이다. 비어 있으면
// 모델과 차원은 보지 않고 연결 대상만 본다.
type EmbeddingExpectation struct {
	ModelID   string
	Dimension int
}

type auditContext struct {
	id, graphID, layer string
	deleted            bool
	start              time.Time
	end                *time.Time
	hasTime            bool
}

type auditEdge struct {
	label, graphID, relationID, state string
	from, to                          string
}

type auditEmbedding struct {
	contextID, graphID, modelID string
	dimension                   int
}

// auditSnapshot은 규칙이 판정할 저장 상태다. scope가 nil이면 그래프 전체이고, 아니면 쓰기가
// 바꾼 컨텍스트와 그 컨텍스트에 닿은 간선·임베딩만 담는다. 간선 끝의 이웃 컨텍스트도 속성
// 판정에 필요하므로 contexts에 함께 둔다.
type auditSnapshot struct {
	graphID    string
	scope      map[string]struct{}
	contexts   map[string]auditContext
	edges      []auditEdge
	embeddings []auditEmbedding
	expect     EmbeddingExpectation
	// pathExists는 확정 관계만으로 from에서 to로 닿는지 답한다. 전체 감사는 스냅숏 안에서,
	// 쓰기 검사는 스냅숏에 없는 경로까지 저장소에 물어 답한다.
	pathExists func(label, from, to string) (bool, error)
}

func (snapshot auditSnapshot) inScope(id string) bool {
	if snapshot.scope == nil {
		return snapshot.contexts[id].graphID == snapshot.graphID
	}
	_, ok := snapshot.scope[id]
	return ok
}

var referenceLabels = map[string]string{"DERIVED_FROM": "derived_from", "SUPERSEDES": "supersedes", "HAS_MEMBER": "has_member"}

var relationLabels = map[string]model.RelationType{"PRECEDES": model.RelationTypePrecedes, "CAUSES": model.RelationTypeCauses, "PART_OF": model.RelationTypePartOf, "RELATES_TO": model.RelationTypeRelatesTo}

func edgeTarget(label string) string {
	if _, ok := relationLabels[label]; ok {
		return TargetRelation
	}
	return TargetReference
}

func (snapshot auditSnapshot) edgeViolation(rule string, edge auditEdge) InvariantViolation {
	ids := []string{edge.from, edge.to}
	if edge.relationID != "" {
		ids = append([]string{edge.relationID}, ids...)
	}
	return InvariantViolation{Rule: rule, GraphID: snapshot.graphID, Target: edgeTarget(edge.label), Label: strings.ToLower(edge.label), IDs: ids}
}

// invariantRule은 규칙 하나다.
type invariantRule struct {
	id    string
	check func(auditSnapshot) ([]InvariantViolation, error)
}

// invariantRules는 쓰기 검사와 전체 감사가 공유하는 규칙 목록이다.
var invariantRules = []invariantRule{
	{RuleGraphMembership, checkGraphMembership},
	{RuleEndpointLayer, checkEndpointLayer},
	{RuleReferenceCardinality, checkReferenceCardinality},
	{RuleRelationIdentity, checkRelationIdentity},
	{RuleRelationAcyclic, checkRelationAcyclic},
	{RuleRelationTime, checkRelationTime},
	{RuleEmbeddingLink, checkEmbeddingLink},
}

// checkGraphMembership은 간선과 두 끝, 임베딩과 그 컨텍스트가 같은 그래프에 속하는지 본다.
func checkGraphMembership(snapshot auditSnapshot) ([]InvariantViolation, error) {
	var violations []InvariantViolation
	for _, edge := range snapshot.edges {
		from, to := snapshot.contexts[edge.from], snapshot.contexts[edge.to]
		if edge.graphID != snapshot.graphID || from.graphID != snapshot.graphID || to.graphID != snapshot.graphID {
			violations = append(violations, snapshot.edgeViolation(RuleGraphMembership, edge))
		}
	}
	for _, embedding := range snapshot.embeddings {
		owner, ok := snapshot.contexts[embedding.contextID]
		if embedding.graphID != snapshot.graphID || (ok && owner.graphID != embedding.graphID) {
			violations = append(violations, InvariantViolation{Rule: RuleGraphMembership, GraphID: snapshot.graphID, Target: TargetEmbedding, IDs: []string{embedding.contextID}})
		}
	}
	return violations, nil
}

// checkEndpointLayer는 참조와 관계의 양 끝 계층이 논리 모델을 따르는지 본다. 파생 근거는
// 원천 또는 파생, 대체는 파생끼리, 사건 구성원은 원천 또는 파생, 사건 관계는 사건끼리다.
func checkEndpointLayer(snapshot auditSnapshot) ([]InvariantViolation, error) {
	allowed := map[string][2][]string{
		"DERIVED_FROM": {{"derived"}, {"source", "derived"}},
		"SUPERSEDES":   {{"derived"}, {"derived"}},
		"HAS_MEMBER":   {{"event"}, {"source", "derived"}},
	}
	for label := range relationLabels {
		allowed[label] = [2][]string{{"event"}, {"event"}}
	}
	var violations []InvariantViolation
	for _, edge := range snapshot.edges {
		layers, known := allowed[edge.label]
		from, fromOK := snapshot.contexts[edge.from]
		to, toOK := snapshot.contexts[edge.to]
		if !known || !fromOK || !toOK || !slices.Contains(layers[0], from.layer) || !slices.Contains(layers[1], to.layer) {
			violations = append(violations, snapshot.edgeViolation(RuleEndpointLayer, edge))
		}
	}
	return violations, nil
}

// checkReferenceCardinality는 파생의 근거가 하나 이상이고 대체 대상이 하나 이하이며 사건의
// 구성원이 하나 이상인지, 같은 참조가 같은 끝점을 두 번 잇지 않는지 본다.
func checkReferenceCardinality(snapshot auditSnapshot) ([]InvariantViolation, error) {
	outgoing := map[string]map[string]int{}
	duplicates := map[[3]string]int{}
	for _, edge := range snapshot.edges {
		if _, ok := referenceLabels[edge.label]; !ok {
			continue
		}
		if outgoing[edge.from] == nil {
			outgoing[edge.from] = map[string]int{}
		}
		outgoing[edge.from][edge.label]++
		duplicates[[3]string{edge.label, edge.from, edge.to}]++
	}
	var violations []InvariantViolation
	for _, id := range slices.Sorted(maps.Keys(snapshot.contexts)) {
		value := snapshot.contexts[id]
		if !snapshot.inScope(id) {
			continue
		}
		counts := outgoing[id]
		broken := false
		switch value.layer {
		case "derived":
			broken = counts["DERIVED_FROM"] < 1 || counts["SUPERSEDES"] > 1
		case "event":
			broken = counts["HAS_MEMBER"] < 1
		}
		if broken {
			violations = append(violations, InvariantViolation{Rule: RuleReferenceCardinality, GraphID: snapshot.graphID, Target: TargetContext, IDs: []string{id}})
		}
	}
	for key, count := range duplicates {
		if count > 1 {
			violations = append(violations, InvariantViolation{Rule: RuleReferenceCardinality, GraphID: snapshot.graphID, Target: TargetReference, Label: strings.ToLower(key[0]), IDs: []string{key[1], key[2]}})
		}
	}
	return violations, nil
}

// checkRelationIdentity는 같은 유형·양 끝의 관계가 하나뿐이고 relates_to가 식별자 순서의
// 정규 방향으로 저장됐는지 본다. 관계 확정과 폐기는 같은 간선의 상태를 바꾸므로 상태와
// 무관하게 정체성당 간선 하나가 계약이다.
func checkRelationIdentity(snapshot auditSnapshot) ([]InvariantViolation, error) {
	groups := map[[3]string][]auditEdge{}
	var violations []InvariantViolation
	for _, edge := range snapshot.edges {
		if _, ok := relationLabels[edge.label]; !ok {
			continue
		}
		groups[[3]string{edge.label, edge.from, edge.to}] = append(groups[[3]string{edge.label, edge.from, edge.to}], edge)
		if edge.label == "RELATES_TO" && edge.from > edge.to {
			violations = append(violations, snapshot.edgeViolation(RuleRelationIdentity, edge))
		}
	}
	for _, edges := range groups {
		if len(edges) > 1 {
			for _, edge := range edges {
				violations = append(violations, snapshot.edgeViolation(RuleRelationIdentity, edge))
			}
		}
	}
	return violations, nil
}

// checkRelationAcyclic은 확정된 precedes·causes·part_of 간선마다 대상에서 시작으로 되돌아오는
// 같은 유형의 확정 경로가 있는지 본다. 있으면 그 간선이 순환에 속한다.
func checkRelationAcyclic(snapshot auditSnapshot) ([]InvariantViolation, error) {
	var violations []InvariantViolation
	for _, edge := range snapshot.edges {
		if edge.state != string(model.RelationStateConfirmed) || edge.label == "RELATES_TO" {
			continue
		}
		if _, ok := relationLabels[edge.label]; !ok {
			continue
		}
		cyclic, err := snapshot.pathExists(edge.label, edge.to, edge.from)
		if err != nil {
			return nil, err
		}
		if cyclic {
			violations = append(violations, snapshot.edgeViolation(RuleRelationAcyclic, edge))
		}
	}
	return violations, nil
}

// checkRelationTime은 폐기되지 않은 관계가 사건 시간 제약을 만족하는지 본다. 판정은 관계를
// 만들 때 쓰는 model.ValidateRelationTime을 그대로 쓴다.
func checkRelationTime(snapshot auditSnapshot) ([]InvariantViolation, error) {
	var violations []InvariantViolation
	for _, edge := range snapshot.edges {
		kind, ok := relationLabels[edge.label]
		if !ok || edge.state == string(model.RelationStateDiscarded) {
			continue
		}
		from, to := snapshot.contexts[edge.from], snapshot.contexts[edge.to]
		// 사건이 아니거나 시간이 없는 끝은 endpoint_layer가 보고한다.
		if !from.hasTime || !to.hasTime {
			continue
		}
		if model.ValidateRelationTime(kind, model.EventAttributes{Start: from.start, End: from.end}, model.EventAttributes{Start: to.start, End: to.end}) != nil {
			violations = append(violations, snapshot.edgeViolation(RuleRelationTime, edge))
		}
	}
	return violations, nil
}

// checkEmbeddingLink는 임베딩이 같은 그래프의 존재하는 컨텍스트를 가리키고 현재 모델·차원과
// 일치하는지 본다.
func checkEmbeddingLink(snapshot auditSnapshot) ([]InvariantViolation, error) {
	var violations []InvariantViolation
	for _, embedding := range snapshot.embeddings {
		owner, ok := snapshot.contexts[embedding.contextID]
		broken := !ok || owner.graphID != snapshot.graphID
		if snapshot.expect.ModelID != "" && embedding.modelID != snapshot.expect.ModelID {
			broken = true
		}
		if snapshot.expect.Dimension > 0 && embedding.dimension != snapshot.expect.Dimension {
			broken = true
		}
		if broken {
			violations = append(violations, InvariantViolation{Rule: RuleEmbeddingLink, GraphID: snapshot.graphID, Target: TargetEmbedding, IDs: []string{embedding.contextID}})
		}
	}
	return violations, nil
}

// runInvariantRules는 규칙 목록 전체를 실행하고 위반을 결정적인 순서로 돌려준다.
func runInvariantRules(snapshot auditSnapshot) ([]InvariantViolation, error) {
	var violations []InvariantViolation
	for _, rule := range invariantRules {
		found, err := rule.check(snapshot)
		if err != nil {
			return nil, fmt.Errorf("불변식 %s 검사: %w", rule.id, err)
		}
		violations = append(violations, found...)
	}
	slices.SortFunc(violations, func(left, right InvariantViolation) int {
		return cmp.Or(strings.Compare(left.Rule, right.Rule), strings.Compare(left.Target, right.Target), strings.Compare(left.Label, right.Label), slices.Compare(left.IDs, right.IDs))
	})
	return slices.CompactFunc(violations, func(left, right InvariantViolation) bool {
		return left.Rule == right.Rule && left.Target == right.Target && left.Label == right.Label && slices.Equal(left.IDs, right.IDs)
	}), nil
}

// memoryPaths는 스냅숏의 확정 관계만으로 경로를 답한다. 전체 감사는 그래프의 관계를 모두
// 읽었으므로 저장소에 다시 묻지 않는다.
func memoryPaths(edges []auditEdge) func(label, from, to string) (bool, error) {
	adjacency := map[string]map[string][]string{}
	for _, edge := range edges {
		if edge.state != string(model.RelationStateConfirmed) {
			continue
		}
		if adjacency[edge.label] == nil {
			adjacency[edge.label] = map[string][]string{}
		}
		adjacency[edge.label][edge.from] = append(adjacency[edge.label][edge.from], edge.to)
	}
	return func(label, from, to string) (bool, error) {
		seen := map[string]struct{}{from: {}}
		queue := []string{from}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if current == to {
				return true, nil
			}
			for _, next := range adjacency[label][current] {
				if _, ok := seen[next]; !ok {
					seen[next] = struct{}{}
					queue = append(queue, next)
				}
			}
		}
		return false, nil
	}
}

// loadAuditSnapshot은 범위의 간선, 간선 끝과 범위의 컨텍스트, 임베딩을 읽는다. contextIDs가
// nil이면 그래프 전체다.
func (s *Store) loadAuditSnapshot(ctx context.Context, queryer cypherQueryer, graphID model.ID, contextIDs []model.ID, expect EmbeddingExpectation) (auditSnapshot, error) {
	graph := cypherString(graphID.String())
	snapshot := auditSnapshot{graphID: graphID.String(), contexts: map[string]auditContext{}, expect: expect}
	var scopeList string
	if contextIDs != nil {
		snapshot.scope = make(map[string]struct{}, len(contextIDs))
		quoted := make([]string, 0, len(contextIDs))
		for _, id := range contextIDs {
			snapshot.scope[id.String()] = struct{}{}
			quoted = append(quoted, cypherString(id.String()))
		}
		scopeList = "[" + strings.Join(quoted, ", ") + "]"
	}
	// 쓰기 검사는 나가는 간선과 들어오는 간선을 나눠 읽는다. 두 끝의 조건을 OR로 묶으면 조인
	// 뒤에야 판정되어 context_id 인덱스를 쓰지 못하고 모든 그래프의 간선을 훑는다. 양 끝이 모두
	// 범위인 간선은 두 질의에 함께 나오므로 간선 식별자로 한 번만 담는다.
	edgeFilters := []string{"a.graph_id = " + graph + " OR b.graph_id = " + graph + " OR e.graph_id = " + graph}
	if contextIDs != nil {
		edgeFilters = []string{"a.context_id IN " + scopeList, "b.context_id IN " + scopeList}
	}
	endpoints := map[string]struct{}{}
	seen := map[string]struct{}{}
	for _, filter := range edgeFilters {
		if err := s.loadAuditEdges(ctx, queryer, filter, &snapshot, endpoints, seen); err != nil {
			return auditSnapshot{}, err
		}
	}

	nodeFilter := "n.graph_id = " + graph
	if contextIDs != nil {
		quoted := make([]string, 0, len(endpoints)+len(contextIDs))
		for id := range snapshot.scope {
			endpoints[id] = struct{}{}
		}
		for id := range endpoints {
			quoted = append(quoted, cypherString(id))
		}
		nodeFilter = "n.context_id IN [" + strings.Join(quoted, ", ") + "]"
	}
	if err := s.loadAuditContexts(ctx, queryer, nodeFilter, snapshot.contexts); err != nil {
		return auditSnapshot{}, err
	}
	// 전체 감사는 그래프의 정점만 읽었으므로 다른 그래프에 속한 간선 끝을 따로 읽는다. 쓰기
	// 검사처럼 끝의 계층을 알아야 graph_membership 위반이 endpoint_layer로 번지지 않는다.
	if contextIDs == nil {
		missing := make([]string, 0)
		for id := range endpoints {
			if _, ok := snapshot.contexts[id]; !ok {
				missing = append(missing, cypherString(id))
			}
		}
		if len(missing) > 0 {
			if err := s.loadAuditContexts(ctx, queryer, "n.context_id IN ["+strings.Join(missing, ", ")+"]", snapshot.contexts); err != nil {
				return auditSnapshot{}, err
			}
		}
	}

	embeddingIDs := make([]string, 0, len(snapshot.contexts))
	for id, value := range snapshot.contexts {
		if snapshot.scope == nil && value.graphID == snapshot.graphID || snapshot.scope != nil && snapshot.inScope(id) {
			embeddingIDs = append(embeddingIDs, id)
		}
	}
	// 전체 감사는 다른 그래프 컨텍스트를 가리키는 이 그래프의 임베딩도 찾아야 하므로 그래프
	// 조건을 더한다. 쓰기 검사는 범위 컨텍스트의 임베딩만 본다.
	query, arguments := `SELECT context_id::text, graph_id::text, model_id, vector_dims(embedding) FROM public.context_embedding WHERE context_id = ANY($1::uuid[])`, []any{embeddingIDs}
	if contextIDs == nil {
		query, arguments = query+` OR graph_id = $2`, []any{embeddingIDs, graphID.String()}
	}
	embeddingRows, err := queryer.Query(ctx, query, arguments...)
	if err != nil {
		return auditSnapshot{}, fmt.Errorf("불변식 임베딩 조회: %w", err)
	}
	defer embeddingRows.Close()
	for embeddingRows.Next() {
		var embedding auditEmbedding
		if err := embeddingRows.Scan(&embedding.contextID, &embedding.graphID, &embedding.modelID, &embedding.dimension); err != nil {
			return auditSnapshot{}, fmt.Errorf("불변식 임베딩 행 해석: %w", err)
		}
		snapshot.embeddings = append(snapshot.embeddings, embedding)
	}
	if err := embeddingRows.Err(); err != nil {
		return auditSnapshot{}, fmt.Errorf("불변식 임베딩 행 읽기: %w", err)
	}
	return snapshot, nil
}

// AuditGraph는 그래프 하나를 읽기 전용 트랜잭션에서 전체 감사한다. 쓰기 검사와 같은 규칙
// 목록을 쓰고 저장 상태를 바꾸지 않는다.
func (s *Store) AuditGraph(ctx context.Context, graphID model.ID, expect EmbeddingExpectation) ([]InvariantViolation, error) {
	if !graphID.IsV7() {
		return nil, fmt.Errorf("그래프 식별자는 UUIDv7이어야 한다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("감사 트랜잭션 시작: %w", err)
	}
	defer tx.Rollback(ctx)
	snapshot, err := s.loadAuditSnapshot(ctx, tx, graphID, nil, expect)
	if err != nil {
		return nil, err
	}
	snapshot.pathExists = memoryPaths(snapshot.edges)
	return runInvariantRules(snapshot)
}

// AuditGraphIDs는 전체 감사의 기본 범위인 활성·삭제 그래프 식별자를 모두 돌려준다.
func (s *Store) AuditGraphIDs(ctx context.Context) ([]model.ID, error) {
	rows, err := s.pool.Query(ctx, `SELECT graph_id::text FROM public.context_graph ORDER BY graph_id`)
	if err != nil {
		return nil, fmt.Errorf("감사 그래프 목록 조회: %w", err)
	}
	defer rows.Close()
	var ids []model.ID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("감사 그래프 행 해석: %w", err)
		}
		id, err := model.ParseID(raw)
		if err != nil {
			return nil, fmt.Errorf("감사 그래프 식별자 해석: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// checkWriteInvariants는 쓰기 트랜잭션이 커밋하기 전에 영향 주변부에 규칙 목록을 실행한다.
// contextIDs는 쓰기가 만들거나 바꾼 컨텍스트와 바뀐 간선의 양 끝이다.
func (s *Store) checkWriteInvariants(ctx context.Context, tx pgx.Tx, graphID model.ID, contextIDs []model.ID, expect EmbeddingExpectation) error {
	violations, err := s.writeInvariantViolations(ctx, tx, graphID, contextIDs, expect)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		first := violations[0]
		return fmt.Errorf("%w: %s %s %v 외 %d건", ErrInvariantViolation, first.Rule, first.Target, first.IDs, len(violations)-1)
	}
	return nil
}

// writeInvariantViolations는 영향 주변부의 위반을 모두 돌려준다.
func (s *Store) writeInvariantViolations(ctx context.Context, tx pgx.Tx, graphID model.ID, contextIDs []model.ID, expect EmbeddingExpectation) ([]InvariantViolation, error) {
	snapshot, err := s.loadAuditSnapshot(ctx, tx, graphID, contextIDs, expect)
	if err != nil {
		return nil, err
	}
	// 순환은 영향 주변부 밖의 경로로도 닫히므로 그래프의 확정 순환 대상 간선을 처음 필요할
	// 때 한 번 읽어 전체 감사와 같은 방식으로 답한다.
	var paths func(label, from, to string) (bool, error)
	snapshot.pathExists = func(label, from, to string) (bool, error) {
		if paths == nil {
			edges, err := s.loadAcyclicEdges(ctx, tx, graphID)
			if err != nil {
				return false, err
			}
			paths = memoryPaths(edges)
		}
		return paths(label, from, to)
	}
	return runInvariantRules(snapshot)
}

// loadAuditEdges는 filter에 맞는 간선을 snapshot에 더하고 양 끝을 endpoints에 모은다. seen에
// 이미 있는 간선은 건너뛴다.
func (s *Store) loadAuditEdges(ctx context.Context, queryer cypherQueryer, filter string, snapshot *auditSnapshot, endpoints, seen map[string]struct{}) error {
	rows, err := queryer.Query(ctx, s.cypherSQL("MATCH (a:Context)-[e]->(b:Context) WHERE "+filter+
		" RETURN id(e), label(e), coalesce(e.graph_id, ''), coalesce(e.relation_id, ''), coalesce(e.state, ''), a.context_id, b.context_id",
		"edge agtype, label agtype, graph agtype, relation agtype, state agtype, source agtype, target agtype"), pgx.QueryExecModeExec)
	if err != nil {
		return fmt.Errorf("불변식 간선 조회: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var edgeID string
		var raw [6]string
		if err := rows.Scan(&edgeID, &raw[0], &raw[1], &raw[2], &raw[3], &raw[4], &raw[5]); err != nil {
			return fmt.Errorf("불변식 간선 행 해석: %w", err)
		}
		if _, ok := seen[edgeID]; ok {
			continue
		}
		seen[edgeID] = struct{}{}
		var values [6]string
		for index := range raw {
			if err := json.Unmarshal([]byte(raw[index]), &values[index]); err != nil {
				return fmt.Errorf("불변식 간선 값 해석: %w", err)
			}
		}
		snapshot.edges = append(snapshot.edges, auditEdge{label: values[0], graphID: values[1], relationID: values[2], state: values[3], from: values[4], to: values[5]})
		endpoints[values[4]], endpoints[values[5]] = struct{}{}, struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("불변식 간선 행 읽기: %w", err)
	}
	return nil
}

// loadAuditContexts는 filter에 맞는 정점의 판정 속성만 읽어 contexts에 더한다. 본문은 규칙에
// 쓰지 않으므로 읽지 않는다.
func (s *Store) loadAuditContexts(ctx context.Context, queryer cypherQueryer, filter string, contexts map[string]auditContext) error {
	rows, err := queryer.Query(ctx, s.cypherSQL("MATCH (n:Context) WHERE "+filter+
		" RETURN n.context_id, coalesce(n.graph_id, ''), coalesce(n.layer, ''), coalesce(n.deleted_at, ''), coalesce(n.event_start, ''), coalesce(n.event_end, '')",
		"id agtype, graph agtype, layer agtype, deleted agtype, start agtype, finish agtype"), pgx.QueryExecModeExec)
	if err != nil {
		return fmt.Errorf("불변식 정점 조회: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw [6]string
		if err := rows.Scan(&raw[0], &raw[1], &raw[2], &raw[3], &raw[4], &raw[5]); err != nil {
			return fmt.Errorf("불변식 정점 행 해석: %w", err)
		}
		var values [6]string
		for index := range raw {
			if err := json.Unmarshal([]byte(raw[index]), &values[index]); err != nil {
				return fmt.Errorf("불변식 정점 값 해석: %w", err)
			}
		}
		value := auditContext{id: values[0], graphID: values[1], layer: values[2], deleted: values[3] != ""}
		if values[4] != "" {
			start, err := time.Parse(time.RFC3339Nano, values[4])
			if err != nil {
				return fmt.Errorf("사건 시작 시각 해석: %w", err)
			}
			value.start, value.hasTime = start, true
			if values[5] != "" {
				end, err := time.Parse(time.RFC3339Nano, values[5])
				if err != nil {
					return fmt.Errorf("사건 종료 시각 해석: %w", err)
				}
				value.end = &end
			}
		}
		contexts[value.id] = value
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("불변식 정점 행 읽기: %w", err)
	}
	return nil
}

// loadAcyclicEdges는 그래프의 확정 precedes·causes·part_of 간선을 label과 상태로만 읽는다.
// 쓰기 검사의 순환 판정이 전체 감사와 같은 간선 집합으로 경로를 답하게 한다. AGE 1.8.0이
// label 교대를 받지 않아 label마다 한 번 묻는다.
func (s *Store) loadAcyclicEdges(ctx context.Context, queryer cypherQueryer, graphID model.ID) ([]auditEdge, error) {
	var edges []auditEdge
	for _, label := range []string{"PRECEDES", "CAUSES", "PART_OF"} {
		rows, err := queryer.Query(ctx, s.cypherSQL("MATCH (a:Context)-[e:"+label+"]->(b:Context) WHERE e.graph_id = "+cypherString(graphID.String())+
			" AND e.state = 'confirmed' RETURN a.context_id, b.context_id", "source agtype, target agtype"), pgx.QueryExecModeExec)
		if err != nil {
			return nil, fmt.Errorf("순환 검사 간선 조회: %w", err)
		}
		for rows.Next() {
			var rawFrom, rawTo, from, to string
			if err := rows.Scan(&rawFrom, &rawTo); err != nil {
				rows.Close()
				return nil, fmt.Errorf("순환 검사 간선 행 해석: %w", err)
			}
			if err := json.Unmarshal([]byte(rawFrom), &from); err != nil {
				rows.Close()
				return nil, fmt.Errorf("순환 검사 간선 값 해석: %w", err)
			}
			if err := json.Unmarshal([]byte(rawTo), &to); err != nil {
				rows.Close()
				return nil, fmt.Errorf("순환 검사 간선 값 해석: %w", err)
			}
			edges = append(edges, auditEdge{label: label, state: string(model.RelationStateConfirmed), from: from, to: to})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("순환 검사 간선 행 읽기: %w", err)
		}
	}
	return edges, nil
}
