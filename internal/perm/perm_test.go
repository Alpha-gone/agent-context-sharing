package perm

import (
	"context"
	"errors"
	"testing"

	"agent_context_sharing/internal/model"
)

type gradeStore struct {
	grade model.GraphGrade
	found bool
}

func (store gradeStore) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	return store.grade, store.found, nil
}

func TestRequire(t *testing.T) {
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	err = Require(t.Context(), gradeStore{grade: model.GraphGradeViewer, found: true}, id, id, model.GraphGradeEditor)
	if _, ok := errors.AsType[DeniedError](err); !ok {
		t.Fatalf("등급 부족 오류 = %v", err)
	}
	err = Require(t.Context(), gradeStore{found: false}, id, id, model.GraphGradeViewer)
	if _, ok := errors.AsType[NotFoundError](err); !ok {
		t.Fatalf("부재 오류 = %v", err)
	}
	if err := Require(t.Context(), gradeStore{grade: model.GraphGradeOwner, found: true}, id, id, model.GraphGradeEditor); err != nil {
		t.Fatalf("소유자 권한이 거부됐다: %v", err)
	}
}
