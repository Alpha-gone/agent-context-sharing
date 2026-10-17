package store

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAccessRelationCodecStates(t *testing.T) {
	graph, actor := newTestID(t), newTestID(t)
	from, to := newTestID(t), newTestID(t)
	if from.Compare(to) > 0 {
		from, to = to, from
	}
	for _, kind := range []model.RelationType{model.RelationTypePrecedes, model.RelationTypeCauses, model.RelationTypePartOf, model.RelationTypeRelatesTo} {
		for _, state := range []model.RelationState{model.RelationStateProposed, model.RelationStateConfirmed, model.RelationStateDiscarded} {
			t.Run(string(kind)+"/"+string(state), func(t *testing.T) {
				relation := proposedRelation(graph, kind, from, to)
				relation.ID, relation.State = newTestID(t), state
				relation.ProposedAt = time.Now().UTC()
				if state != model.RelationStateProposed {
					relation.ConfirmedBy, relation.ConfirmedByAgent, relation.ConfirmedAt = actor, newTestID(t), new(relation.ProposedAt.Add(time.Second))
				}
				if state == model.RelationStateDiscarded {
					relation.DeletedAt = new(relation.ProposedAt.Add(2 * time.Second))
				}
				label, err := relationLabel(kind)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(map[string]any{"label": label, "properties": relationProperties(relation)})
				if err != nil {
					t.Fatal(err)
				}
				got, err := parseRelation(string(raw)+"::edge", graph)
				if err != nil || !reflect.DeepEqual(got, relation) {
					t.Fatalf("관계 왕복 = %+v, %v", got, err)
				}
			})
		}
	}
}

func TestAccessCodecRejectsMalformedStoredValues(t *testing.T) {
	graph, actor := newTestID(t), newTestID(t)
	value := testSourceContext(t, graph, actor, "api://data03/private-locator")
	value.Body = "private-body-must-not-appear"
	for _, scenario := range []string{"label", "body_type", "graph", "source_ref", "version", "json"} {
		t.Run(scenario, func(t *testing.T) {
			properties := contextProperties(value)
			label := "Context"
			switch scenario {
			case "label":
				label = "Other"
			case "body_type":
				properties["body"] = 123
			case "graph":
				properties["graph_id"] = newTestID(t).String()
			case "source_ref":
				properties["source_ref_channel"] = value.Body
			case "version":
				properties["version"] = 0
			}
			raw, err := json.Marshal(map[string]any{"label": label, "properties": properties})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "json" {
				raw = []byte(`{"properties":`)
			}
			got, err := parseContext(string(raw)+"::vertex", graph)
			if _, ok := errors.AsType[*StorageError](err); !ok || !got.ID.IsZero() {
				t.Fatalf("저장 값 거부 = %+v, %v", got, err)
			}
			if strings.Contains(err.Error(), value.Body) || strings.Contains(err.Error(), "private-locator") {
				t.Fatalf("저장 값 원문 노출: %s", err)
			}
		})
	}
	for _, scenario := range []string{"state", "confirmation", "direction"} {
		t.Run("relation/"+scenario, func(t *testing.T) {
			from, to := newTestID(t), newTestID(t)
			if from.Compare(to) > 0 {
				from, to = to, from
			}
			relation := proposedRelation(graph, model.RelationTypeRelatesTo, from, to)
			relation.ID = newTestID(t)
			switch scenario {
			case "state":
				relation.State = "private-invalid-state"
			case "confirmation":
				relation.State = model.RelationStateConfirmed
			case "direction":
				relation.FromContextID, relation.ToContextID = to, from
			}
			raw, _ := json.Marshal(map[string]any{"label": "RELATES_TO", "properties": relationProperties(relation)})
			got, err := parseRelation(string(raw)+"::edge", graph)
			if _, ok := errors.AsType[*StorageError](err); !ok || !got.ID.IsZero() {
				t.Fatalf("잘못된 관계 = %+v, %v", got, err)
			}
		})
	}
}

func TestAccessErrorPreservesCauses(t *testing.T) {
	pg := &pgconn.PgError{Code: "23505", Message: "private database payload"}
	wrapped := writeFailure(t.Context(), "context_write", fmt.Errorf("private wrapper: %w", pg))
	if _, ok := errors.AsType[*StorageError](wrapped); !ok || !errors.Is(wrapped, pg) || !isUniqueViolation(wrapped) {
		t.Fatalf("DB 오류 식별 = %v", wrapped)
	}
	if strings.Contains(wrapped.Error(), "private") {
		t.Fatalf("내부 오류 노출: %v", wrapped)
	}
	for _, cause := range []error{ErrNotFound, ErrInvalidState, VersionConflictError{Current: 3}, model.FieldError{Field: "body", Message: "본문 필요"}} {
		if got := writeFailure(t.Context(), "write", cause); got != cause {
			t.Fatalf("도메인 오류가 바뀌었다: %v", got)
		}
	}
	cause := errors.New("사용자 지정 취소")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	got := writeFailure(ctx, "write", context.Canceled)
	if !errors.Is(got, cause) || !errors.Is(got, context.Canceled) {
		t.Fatalf("취소 원인 = %v", got)
	}
}

func canonicalReferences(value model.Context) model.Context {
	value = attachReferences(value, previousReferences(value), func() []model.ID {
		if value.Event != nil {
			return value.Event.MemberIDs
		}
		return nil
	}())
	if value.Derived != nil {
		slices.SortFunc(value.Derived.DerivedFrom, func(a, b model.ID) int { return a.Compare(b) })
	}
	if value.Event != nil {
		slices.SortFunc(value.Event.MemberIDs, func(a, b model.ID) int { return a.Compare(b) })
	}
	return value
}

func TestAccessReferencesDoNotAlias(t *testing.T) {
	value := testDerivedContext(t, newTestID(t), newTestID(t))
	ids := []model.ID{newTestID(t)}
	stored := attachReferences(value, ids, nil)
	ids[0] = newTestID(t)
	stored.Derived.DerivedFrom[0] = newTestID(t)
	stored.Derived.EvidenceInvalidated = true
	if value.Derived.EvidenceInvalidated || len(value.Derived.DerivedFrom) != 0 {
		t.Fatal("조립 결과가 입력 속성과 참조를 공유한다")
	}
}
