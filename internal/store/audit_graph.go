package store

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 전체 감사의 정점 조건은 기존 text 표현식 인덱스와 맞춘다. 반환 값은 쓰기 검사와
// 같은 agtype 속성 판독기를 거쳐 시간·계층 판정의 의미를 유지한다.
func (s *Store) loadGraphAuditContexts(ctx context.Context, queryer cypherQueryer, graphID model.ID, contexts map[string]auditContext) ([]int64, error) {
	columns := make([]string, 0, 7)
	for _, key := range []string{"context_id", "graph_id", "layer", "deleted_at", "event_start", "event_end", "derivation_kind"} {
		columns = append(columns, auditPropertySQL("c", key))
	}
	columns = append(columns, "c.id::text")
	return s.readAuditContexts(ctx, queryer, "SELECT "+strings.Join(columns, ",")+" FROM "+s.contextTable()+" c WHERE c.properties ->> 'graph_id'::text=$1", contexts, graphID.String())
}

// 간선 소속과 양 끝의 소속을 각각 좁힌 뒤 합친다. LEFT JOIN은 이 범위 안에서만
// 수행하므로 고아 간선을 위해 그래프마다 모든 간선을 다시 읽지 않는다.
func (s *Store) loadGraphAuditEdges(ctx context.Context, queryer cypherQueryer, snapshot *auditSnapshot, vertexIDs []int64, endpoints map[string]struct{}) error {
	ids := make([]string, 0, len(vertexIDs))
	for _, id := range vertexIDs {
		ids = append(ids, sqlString(strconv.FormatInt(id, 10))+"::ag_catalog.graphid")
	}
	slices.Sort(ids)
	labels := []string{"DERIVED_FROM", "SUPERSEDES", "HAS_MEMBER", "NAVIGATES_TO", "PRECEDES", "CAUSES", "PART_OF", "RELATES_TO"}
	// 계약 밖 label도 기존 endpoint_layer 검사로 넘긴다. 부모는 ONLY로 자식 중복을 막는다.
	labelRows, err := queryer.Query(ctx, `SELECT l.name::text FROM ag_catalog.ag_label l
  JOIN ag_catalog.ag_graph g ON g.graphid=l.graph
  WHERE g.name=$1 AND l.kind='e' AND l.name::text <> ALL($2::text[]) ORDER BY l.name`, s.graphName, labels)
	if err != nil {
		return fmt.Errorf("감사 간선 label 조회: %w", err)
	}
	for labelRows.Next() {
		var label string
		if err := labelRows.Scan(&label); err != nil {
			labelRows.Close()
			return fmt.Errorf("감사 간선 label 해석: %w", err)
		}
		labels = append(labels, label)
	}
	labelRows.Close()
	if err := labelRows.Err(); err != nil {
		return fmt.Errorf("감사 간선 label 순회: %w", err)
	}
	var queries []string
	for _, label := range labels {
		table := pgx.Identifier{s.graphName, label}.Sanitize()
		selections := []string{"SELECT * FROM ONLY " + table + " WHERE properties ->> 'graph_id'::text=$1"}
		if len(ids) > 0 {
			values := "(" + strings.Join(ids, ",") + ")"
			selections = append(selections, "SELECT * FROM ONLY "+table+" WHERE start_id IN "+values, "SELECT * FROM ONLY "+table+" WHERE end_id IN "+values)
		}
		// UNION은 속성이 같은 중복 손상 간선도 물리 ID가 다르면 보존한다.
		name := label
		if name == "_ag_label_edge" {
			name = ""
		}
		queries = append(queries, `SELECT `+sqlString(name)+`, `+auditPropertySQL("e", "graph_id")+`, `+
			auditPropertySQL("e", "relation_id")+`, `+auditPropertySQL("e", "state")+`,
   CASE WHEN a.id IS NULL THEN to_json('age:' || e.start_id::text)::text::ag_catalog.agtype ELSE `+auditPropertySQL("a", "context_id")+` END,
   CASE WHEN b.id IS NULL THEN to_json('age:' || e.end_id::text)::text::ag_catalog.agtype ELSE `+auditPropertySQL("b", "context_id")+` END
   FROM (`+strings.Join(selections, " UNION ")+` OFFSET 0) e
   LEFT JOIN LATERAL (SELECT id, properties FROM `+s.contextTable()+` WHERE id=e.start_id OFFSET 0) a ON true
   LEFT JOIN LATERAL (SELECT id, properties FROM `+s.contextTable()+` WHERE id=e.end_id OFFSET 0) b ON true`)

	}
	rows, err := queryer.Query(ctx, strings.Join(queries, " UNION ALL "), snapshot.graphID)
	if err != nil {
		return fmt.Errorf("불변식 그래프 간선 조회: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		var raw, values [5]string
		if err := rows.Scan(&label, &raw[0], &raw[1], &raw[2], &raw[3], &raw[4]); err != nil {
			return fmt.Errorf("불변식 그래프 간선 행 해석: %w", err)
		}
		for i := range raw {
			if err := json.Unmarshal([]byte(raw[i]), &values[i]); err != nil {
				return fmt.Errorf("불변식 그래프 간선 값 해석: %w", err)
			}
		}
		edge := auditEdge{label: label, graphID: values[0], relationID: values[1], state: values[2], from: values[3], to: values[4]}
		snapshot.edges = append(snapshot.edges, edge)
		endpoints[edge.from], endpoints[edge.to] = struct{}{}, struct{}{}
	}
	return rows.Err()
}

// SQL의 범위 조건과 별개로 속성 자료형은 openCypher 판독과 같이 엄격하게 해석한다.
func auditPropertySQL(alias, key string) string {
	return fmt.Sprintf(`coalesce(ag_catalog.agtype_access_operator(VARIADIC ARRAY[%s.properties, '"%s"'::ag_catalog.agtype]), '""'::ag_catalog.agtype)`, alias, key)
}
