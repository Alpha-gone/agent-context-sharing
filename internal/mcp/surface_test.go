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

// TestNodeCreateRejectsOtherLayerArguments는 계층에 맞지 않는 인자를 조용히 버리지 않고
// 거부하는지 확인한다. 버리면 호출자가 보낸 값이 저장되지 않았다는 사실을 알 수 없다.
func TestNodeCreateRejectsOtherLayerArguments(t *testing.T) {
	base := func(layer string) map[string]any {
		return map[string]any{
			"graph_id": newTestID(t), "created_by_agent": newTestID(t),
			"layer": layer, "body": "본문",
		}
	}
	cases := map[string]struct {
		layer string
		name  string
		value any
	}{
		"원천에 붙인 derived_from":   {"source", "derived_from", []any{newTestID(t)}},
		"원천에 붙인 member_refs":    {"source", "member_refs", []any{newTestID(t)}},
		"원천에 붙인 start":          {"source", "start", "2026-09-19T00:00:00Z"},
		"파생에 붙인 source_channel": {"derived", "source_channel", "api"},
		"파생에 붙인 occurred_at":    {"derived", "occurred_at", "2026-09-19T00:00:00Z"},
		"사건에 붙인 evidence_state": {"event", "evidence_state", "observation"},
	}
	for name, testCase := range cases {
		arguments := base(testCase.layer)
		arguments[testCase.name] = testCase.value
		err := validateToolCall("node_create", arguments)
		if err == nil {
			t.Fatalf("%s이 통과했다", name)
		}
		if err.Field != testCase.name {
			t.Fatalf("%s의 거부 필드 = %q, want %q", name, err.Field, testCase.name)
		}
	}
	// 계층에 맞는 인자는 그대로 통과한다.
	source := base("source")
	source["source_channel"] = "api"
	source["occurred_at"] = "2026-09-19T00:00:00Z"
	if err := validateToolCall("node_create", source); err != nil {
		t.Fatalf("계층에 맞는 인자를 거부했다: %#v", err)
	}
}

// TestIntegerArgumentsHaveUpperBound는 정수 인자에 상한이 있는지 확인한다. 상한이 없으면
// 한도를 풀어 둔 플랜에서 저장소가 상한에 1을 더하는 자리가 넘쳐 음수가 된다.
func TestIntegerArgumentsHaveUpperBound(t *testing.T) {
	for _, value := range []float64{maxIntegerArgument + 1, 1 << 62, 1 << 63} {
		if err := validateToolCall("graph_list", map[string]any{"page_size": value}); err == nil {
			t.Fatalf("page_size %v가 통과했다", value)
		}
		arguments := map[string]any{"graph_id": newTestID(t), "context_id": newTestID(t), "hops": value}
		if err := validateToolCall("node_get", arguments); err == nil {
			t.Fatalf("hops %v가 통과했다", value)
		}
	}
	if err := validateToolCall("graph_list", map[string]any{"page_size": float64(maxIntegerArgument)}); err != nil {
		t.Fatalf("상한 값을 거부했다: %#v", err)
	}
}
