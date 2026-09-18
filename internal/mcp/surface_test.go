package mcp

import (
	"encoding/base64"
	"errors"
	"testing"

	"agent_context_sharing/internal/store"
)

// TestGraphNameRejectsBlankText는 공백만으로 된 그래프 이름을 거부하는지 확인한다.
// SRS는 빈 이름과 마찬가지로 공백만 있는 이름을 invalid_argument로 둔다.
func TestGraphNameRejectsBlankText(t *testing.T) {
	for _, name := range []string{"   ", "\t", "\n", "   "} {
		if err := validateToolCall("graph_create", map[string]any{"name": name}); err == nil {
			t.Fatalf("graph_create가 공백 이름 %q를 받았다", name)
		}
		arguments := map[string]any{"graph_id": newTestID(t), "expected_version": float64(1), "name": name}
		if err := validateToolCall("graph_update", arguments); err == nil {
			t.Fatalf("graph_update가 공백 이름 %q를 받았다", name)
		}
	}
	// 앞뒤 공백이 있어도 내용이 있으면 받는다. 잘라내기는 이 계층의 일이 아니다.
	if err := validateToolCall("graph_create", map[string]any{"name": " 이름 "}); err != nil {
		t.Fatalf("내용이 있는 이름을 거부했다: %#v", err)
	}
}

// TestCursorFormatValidatedInMCP는 형식이 깨진 커서를 접근 계층이 거르는지 확인한다.
// 「입력 검증」이 커서 형식 검증을 이 패키지의 책임으로 두었고, 거르지 못하면 저장소의
// 해독 오류가 internal로 나간다.
func TestCursorFormatValidatedInMCP(t *testing.T) {
	for _, cursor := range []string{"!!", "a b", "", "====", "가나다"} {
		if err := validateToolCall("graph_list", map[string]any{"cursor": cursor}); err == nil {
			t.Fatalf("graph_list가 잘못된 커서 %q를 받았다", cursor)
		}
		arguments := map[string]any{"graph_id": newTestID(t), "context_id": newTestID(t), "cursor": cursor}
		if err := validateToolCall("relation_list", arguments); err == nil {
			t.Fatalf("relation_list가 잘못된 커서 %q를 받았다", cursor)
		}
	}
	valid := base64.RawURLEncoding.EncodeToString([]byte("2026-09-19T00:00:00Z|0199f0a0-0000-7000-8000-000000000000"))
	if err := validateToolCall("graph_list", map[string]any{"cursor": valid}); err != nil {
		t.Fatalf("정상 커서를 거부했다: %#v", err)
	}
}

// TestStoreCursorErrorMapsToInvalidArgument는 형식을 지났지만 내용이 잘못된 커서도
// invalid_argument로 나가는지 확인한다. SRS가 유효하지 않은 커서를 그렇게 정했다.
func TestStoreCursorErrorMapsToInvalidArgument(t *testing.T) {
	domain, ok := errors.AsType[*Error](mapError(store.ErrInvalidCursor))
	if !ok || domain.Code != "invalid_argument" {
		t.Fatalf("커서 오류 사상 = %#v, want invalid_argument", domain)
	}
	if field, _ := domain.Data["field"].(string); field != "cursor" {
		t.Fatalf("field = %q, want cursor", field)
	}
}
