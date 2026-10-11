package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// createHopReferenceSample은 정상 상한의 근거 100개·구성원 1,000개를 만든다.
// 원천 정점만 묶음 생성하고, 파생·사건은 정식 생성 경로로 참조 검증을 거친다.
func createHopReferenceSample(t *testing.T, database *Store) (model.ID, model.Context, model.Context, model.Context, []model.ID) {
	t.Helper()
	graph, hub := createHopHub(t, database, 0)
	members := []model.ID{hub.ID}
	for len(members) < 1000 {
		properties := make([]string, 0, 100)
		for range min(100, 1000-len(members)) {
			value := testSourceContext(t, graph, hub.CreatedBy, fmt.Sprintf("api://hop-references/%s/%d", graph, len(members)))
			encoded, err := encodeProperties(contextProperties(value))
			if err != nil {
				t.Fatal(err)
			}
			properties = append(properties, encoded)
			members = append(members, value.ID)
		}
		query := "UNWIND [" + strings.Join(properties, ",") + "] AS props CREATE (node:Context) SET node = props RETURN count(node)"
		var count string
		if err := database.pool.QueryRow(t.Context(), database.cypherSQL(query, "count agtype"), pgx.QueryExecModeExec).Scan(&count); err != nil {
			t.Fatalf("원천 참조 표본 생성: %v", err)
		}
	}
	derived, err := database.CreateContext(t.Context(), graph, testDerivedContext(t, graph, hub.CreatedBy), members[:100])
	if err != nil {
		t.Fatal(err)
	}
	eventInput := testEventContext(t, graph, hub.CreatedBy, hub.ID)
	eventInput.Event.MemberIDs = slices.Clone(members)
	event, err := database.CreateContext(t.Context(), graph, eventInput, nil)
	if err != nil {
		t.Fatal(err)
	}
	return graph, hub, derived, event, members
}

func TestHopMaxReferencesWorkIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, hub, derived, event, members := createHopReferenceSample(t, database)
	for _, test := range []struct {
		name      string
		starts    []model.Context
		filter    []string
		limit     int
		wantNodes int
		wantWork  hopWork
	}{
		{"source-only", []model.Context{hub}, nil, 1, 1, hopWork{identifierRows: 1}},
		{"selected-derived", []model.Context{hub}, []string{"derived_from"}, 2, 2, hopWork{identifierRows: 1, bodyRows: 1, bodyBytes: len(derived.Body), edgeRows: 1, referenceRows: 100}},
		{"selected-event", []model.Context{hub}, []string{"has_member"}, 2, 2, hopWork{identifierRows: 1, bodyRows: 1, bodyBytes: len(event.Body), edgeRows: 1, referenceRows: 1000}},
		{"both-selected", []model.Context{hub}, nil, 3, 3, hopWork{identifierRows: 2, bodyRows: 2, bodyBytes: len(derived.Body) + len(event.Body), edgeRows: 2, referenceRows: 1100}},
		{"derived-start", []model.Context{derived}, []string{"derived_from"}, 1, 1, hopWork{identifierRows: 1, referenceRows: 100}},
		{"event-start", []model.Context{event}, []string{"has_member"}, 1, 1, hopWork{identifierRows: 1, referenceRows: 1000}},
		{"both-starts", []model.Context{derived, event}, nil, 2, 2, hopWork{identifierRows: 1, referenceRows: 1100}},
		// 중복 label은 내부 간선 수신 행만 늘리고 참조는 한 번 조립한다.
		{"duplicate-label", []model.Context{hub}, []string{"derived_from", "derived_from"}, 2, 2, hopWork{identifierRows: 1, bodyRows: 1, bodyBytes: len(derived.Body), edgeRows: 2, referenceRows: 100}},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now()
			bounded, err := database.HopContextsFrom(t.Context(), graph, test.starts, 1, "both", test.filter, test.limit)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			full, err := hopFullBaseline(t.Context(), database, graph, test.starts, 1, "both", test.filter, test.limit)
			if err != nil {
				t.Fatal(err)
			}
			compareHopResults(t, test.name, full, bounded)
			if bounded.work != test.wantWork || len(bounded.Contexts) != test.wantNodes || full.work.referenceRows != test.wantWork.referenceRows {
				t.Fatalf("작업량 대조: bounded=%+v full-refs=%d want=%+v nodes=%d", bounded.work, full.work.referenceRows, test.wantWork, len(bounded.Contexts))
			}
			for _, value := range bounded.Contexts {
				var got, want []model.ID
				switch value.Layer {
				case model.LayerDerived:
					got, want = value.Derived.DerivedFrom, slices.Clone(members[:100])
				case model.LayerEvent:
					got, want = value.Event.MemberIDs, slices.Clone(members)
				default:
					continue
				}
				slices.SortFunc(want, func(a, b model.ID) int { return strings.Compare(a.String(), b.String()) })
				if !slices.Equal(got, want) {
					t.Fatalf("정상 상한의 참조 완전성: got=%d want=%d", len(got), len(want))
				}
			}
			t.Logf("limit=%d nodes=%d edges=%d work=%+v elapsed=%s", test.limit, len(bounded.Contexts), len(bounded.Edges), bounded.work, elapsed)
		})
	}
}
