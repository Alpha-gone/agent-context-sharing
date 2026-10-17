package model

import (
	"context"
	"errors"
	"testing"
)

func TestCorrelationContext(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	id, _ := NewID()
	ctx, err := WithCorrelationID(parent, id)
	if err != nil || CorrelationID(ctx) != id || !CorrelationID(parent).IsZero() {
		t.Fatalf("상관 ID 전달=%v", err)
	}
	other, _ := NewID()
	child, err := WithCorrelationID(ctx, other)
	if err != nil || CorrelationID(child) != other || CorrelationID(ctx) != id {
		t.Fatalf("요청별 상관 ID 격리=%v", err)
	}
	cancel()
	if !errors.Is(child.Err(), context.Canceled) {
		t.Fatal("상관 ID 전달이 요청 취소를 보존하지 않았다")
	}
	for _, test := range []struct {
		name string
		id   ID
	}{
		{"zero", ID{}}, {"v4", ID{6: 0x40, 8: 0x80}}, {"variant", ID{6: 0x70}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if value, err := WithCorrelationID(t.Context(), test.id); err == nil || value != nil {
				t.Fatal("잘못된 상관 ID를 허용했다")
			}
		})
	}
}
