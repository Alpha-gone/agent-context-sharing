package store

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func eventMemberEdgeIDs(t *testing.T, database *Store, graph, event model.ID) map[model.ID]string {
	t.Helper()
	table := func(label string) string { return pgx.Identifier{database.graphName, label}.Sanitize() }
	query := `SELECT member.properties ->> 'context_id'::text, edge.id::text
 FROM ` + table("HAS_MEMBER") + ` edge
 JOIN ` + table("Context") + ` owner ON owner.id=edge.start_id
 JOIN ` + table("Context") + ` member ON member.id=edge.end_id
 WHERE owner.properties ->> 'context_id'::text=$1 AND owner.properties ->> 'graph_id'::text=$2
 AND edge.properties ->> 'graph_id'::text=$2 AND member.properties ->> 'graph_id'::text=$2`
	rows, err := database.pool.Query(t.Context(), query, event.String(), graph.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[model.ID]string{}
	for rows.Next() {
		var raw, id string
		if err := rows.Scan(&raw, &id); err != nil {
			t.Fatal(err)
		}
		member, err := model.ParseID(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := result[member]; exists {
			t.Fatal("중복 구성원 간선")
		}
		result[member] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestEventUnchangedMembersKeepEdgesIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, _, _, event, _ := createHopReferenceSample(t, database)
	original := eventMemberEdgeIDs(t, database, graph, event.ID)
	if len(original) != 1000 {
		t.Fatal("구성원 상한 표본", len(original))
	}
	for _, change := range []string{"body", "time", "reorder", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			next := attachReferences(event, nil, slices.Clone(event.Event.MemberIDs))
			next.Version++
			switch change {
			case "body":
				next.Body += " 본문만 변경"
			case "time":
				next.Event.Start = next.Event.Start.Add(-time.Second)
			case "reorder":
				slices.Reverse(next.Event.MemberIDs)
			}
			input := slices.Clone(next.Event.MemberIDs)
			stored, err := database.UpdateContextWithOperation(t.Context(), graph, event.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(original, eventMemberEdgeIDs(t, database, graph, event.ID)) {
				t.Fatal("같은 구성원 집합의 간선 물리 ID 변경")
			}
			if !slices.Equal(next.Event.MemberIDs, input) || !slices.Equal(stored.Event.MemberIDs, sortedMemberIDs(input)) || stored.Version != next.Version || stored.Body != next.Body {
				t.Fatal("입력 불변/응답/판 위반")
			}
			event = stored
		})
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestEventChangedMembersReplaceEdgesIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	graph, actor, source := boundaryGraph(t, database)
	derived := lifeDerived(t, database, graph, actor, source.ID)
	other, err := database.CreateContext(t.Context(), graph, testSourceContext(t, graph, actor, "api://263/other"), nil)
	if err != nil {
		t.Fatal(err)
	}
	event, err := database.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, source.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name    string
		members []model.ID
	}{
		{"addition", []model.ID{source.ID, derived.ID}},
		{"removal", []model.ID{derived.ID}},
		{"replacement", []model.ID{other.ID}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			before := eventMemberEdgeIDs(t, database, graph, event.ID)
			next := attachReferences(event, nil, slices.Clone(sample.members))
			next.Version++
			stored, err := database.UpdateContextWithOperation(t.Context(), graph, event.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
			if err != nil {
				t.Fatal(err)
			}
			after := eventMemberEdgeIDs(t, database, graph, event.ID)
			if len(after) != len(sample.members) || !slices.Equal(stored.Event.MemberIDs, sortedMemberIDs(sample.members)) {
				t.Fatal("변경된 구성원 집합 불일치")
			}
			for _, member := range sample.members {
				if after[member] == "" {
					t.Fatal("새 구성원 간선 누락")
				}
				for _, old := range before {
					if after[member] == old {
						t.Fatal("구성원 변경 때 간선 교체 누락")
					}
				}
			}
			event = stored
		})
	}
	before := eventMemberEdgeIDs(t, database, graph, event.ID)
	if _, err := database.DiscardContext(t.Context(), graph, other.ID, registrationOperation(other, OperationDiscard), WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	next := attachReferences(event, nil, slices.Clone(event.Event.MemberIDs))
	next.Version++
	next.Body += " 거부할 본문"
	_, err = database.UpdateContextWithOperation(t.Context(), graph, event.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{})
	failure, ok := errors.AsType[model.FieldError](err)
	if !ok || failure.Field != "member_refs" {
		t.Fatalf("폐기 구성원 재검증=%v", err)
	}
	loaded, err := database.Context(t.Context(), graph, event.ID)
	if err != nil || loaded.Version != event.Version || loaded.Body != event.Body || !maps.Equal(before, eventMemberEdgeIDs(t, database, graph, event.ID)) {
		t.Fatal("거부한 갱신이 사건/간선을 변경했다")
	}
	next = attachReferences(event, nil, []model.ID{source.ID})
	next.Version++
	if _, err := database.UpdateContextWithOperation(t.Context(), graph, event.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{}); err != nil {
		t.Fatal("활성 구성원으로 수정 실패", err)
	}
	assertConnectionRecovery(t, database, nil, 0)
}
