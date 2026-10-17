package perm

import (
	"context"
	"errors"
	"testing"

	"agent_context_sharing/internal/model"
)

type gradeLookup func(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error)

func (lookup gradeLookup) EffectiveGrade(ctx context.Context, graphID, accountID model.ID) (model.GraphGrade, bool, error) {
	return lookup(ctx, graphID, accountID)
}

func TestRequireGradeContract(t *testing.T) {
	graphID, accountID := permissionID(t), permissionID(t)
	grades := []model.GraphGrade{model.GraphGradeViewer, model.GraphGradeEditor, model.GraphGradeOwner}
	for actualRank, actual := range grades {
		for requiredRank, required := range grades {
			t.Run(string(actual)+"/"+string(required), func(t *testing.T) {
				calls := 0
				source := gradeLookup(func(ctx context.Context, graph, account model.ID) (model.GraphGrade, bool, error) {
					calls++
					if ctx != t.Context() || graph != graphID || account != accountID {
						t.Fatal("요청과 다른 컨텍스트 또는 식별자가 전달됐다")
					}
					return actual, true, nil
				})
				err := Require(t.Context(), source, graphID, accountID, required)
				if actualRank >= requiredRank {
					if err != nil {
						t.Fatalf("허용할 등급이 거부됐다: %v", err)
					}
				} else if denied, ok := errors.AsType[DeniedError](err); !ok || denied.Required != required {
					t.Fatalf("등급 부족 = %v; 필요 등급 %s", err, required)
				}
				if calls != 1 {
					t.Fatalf("유효 등급 조회 횟수 = %d, want 1", calls)
				}
			})
		}
	}
}

func TestRequireRejectsInvalidInput(t *testing.T) {
	id := permissionID(t)
	for _, invalid := range []struct {
		name           string
		graph, account model.ID
		required       model.GraphGrade
	}{
		{"그래프 식별자", model.ID{}, id, model.GraphGradeViewer},
		{"계정 식별자", id, model.ID{}, model.GraphGradeViewer},
		{"요구 등급", id, id, "admin"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			source := gradeLookup(func(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
				t.Fatal("잘못된 입력으로 저장소를 호출했다")
				return "", false, nil
			})
			if err := Require(t.Context(), source, invalid.graph, invalid.account, invalid.required); err == nil {
				t.Fatal("잘못된 권한 검사 입력이 허용됐다")
			}
		})
	}
	if err := Require(t.Context(), nil, id, id, model.GraphGradeViewer); err == nil {
		t.Fatal("없는 저장소가 허용됐다")
	}
}

func TestRequireLookupFailures(t *testing.T) {
	id := permissionID(t)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("저장소 장애")} {
		t.Run(cause.Error(), func(t *testing.T) {
			source := gradeLookup(func(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
				return model.GraphGradeOwner, true, cause
			})
			if err := Require(t.Context(), source, id, id, model.GraphGradeViewer); !errors.Is(err, cause) {
				t.Fatalf("저장소 오류가 보존되지 않았다: %v", err)
			}
		})
	}
	for _, grade := range []model.GraphGrade{"", "admin"} {
		t.Run("잘못된 저장 등급/"+string(grade), func(t *testing.T) {
			err := Require(t.Context(), gradeStore{grade: grade, found: true}, id, id, model.GraphGradeViewer)
			_, denied := errors.AsType[DeniedError](err)
			_, missing := errors.AsType[NotFoundError](err)
			if err == nil || denied || missing {
				t.Fatalf("잘못된 저장 등급을 정상 권한 판정으로 처리했다: %v", err)
			}
		})
	}
}

func permissionID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
