package store

import (
	"errors"
	"testing"

	"agent_context_sharing/internal/model"
)

// 연결 없는 저장소로 인자 오류가 DB 접근 전에 반환되는지도 확인한다.
func TestInvalidManagementInputReturnsFieldErrorBeforeDatabase(t *testing.T) {
	s := &Store{}
	actor, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"a\x00b", "a\xffb"} {
		_, accountErr := s.AccountByLoginID(t.Context(), value)
		_, teamErr := s.CreateTeam(t.Context(), actor, value)
		_, _, listErr := s.ListGraphs(t.Context(), actor, model.GraphListFilter{Name: value}, "", 10)
		for _, err := range []error{accountErr, teamErr, listErr} {
			if _, ok := errors.AsType[model.FieldError](err); !ok {
				t.Fatalf("error=%v, want FieldError", err)
			}
			if _, ok := errors.AsType[*StorageError](err); ok {
				t.Fatalf("input classified as storage error: %v", err)
			}
		}
	}
	for _, value := range []string{"", "ab", "Upper", "a-b"} {
		_, err := s.AccountByLoginID(t.Context(), value)
		if _, ok := errors.AsType[model.FieldError](err); !ok {
			t.Fatalf("login %q: %v", value, err)
		}
	}
}
