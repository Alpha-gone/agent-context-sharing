package store

import (
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestCursorRoundTrip은 두 목록 커서가 UUIDv7과 정렬 시각을 손실 없이 보존하는지 확인한다.
func TestCursorRoundTrip(t *testing.T) {
	graphID := mustID(t, "0199f2bd-9db5-7a33-9e3e-5cda2deabcef")
	activity := time.Date(2026, time.September, 6, 10, 20, 30, 400, time.UTC)

	decodedGraph, err := decodeGraphCursor(encodeGraphCursor(graphCursor{activity: activity, graphID: graphID}))
	if err != nil {
		t.Fatalf("그래프 커서 해석: %v", err)
	}
	if !decodedGraph.activity.Equal(activity) || decodedGraph.graphID != graphID {
		t.Fatalf("그래프 커서 왕복 결과가 다르다: %#v", decodedGraph)
	}

	decodedRelation, err := decodeRelationCursor(encodeRelationCursor(graphID))
	if err != nil {
		t.Fatalf("관계 커서 해석: %v", err)
	}
	if decodedRelation != graphID {
		t.Fatalf("관계 커서 왕복 결과가 다르다: %s", decodedRelation)
	}
}

// TestCursorRejectsInvalidValue는 손상되거나 형식이 다른 커서를 거부하는지 확인한다.
func TestCursorRejectsInvalidValue(t *testing.T) {
	if _, err := decodeGraphCursor("not-a-cursor"); err == nil {
		t.Fatal("손상된 그래프 커서를 거부하지 않았다")
	}
	if _, err := decodeRelationCursor("bm90LWF1dWlk"); err == nil {
		t.Fatal("UUIDv7이 아닌 관계 커서를 거부하지 않았다")
	}
}

// TestDollarStringAvoidsQueryDelimiterCollision은 본문에 구분자가 있어도 AGE 호출 SQL이 닫히지 않는지 확인한다.
func TestDollarStringAvoidsQueryDelimiterCollision(t *testing.T) {
	value := "RETURN '$store0$'"
	quoted := dollarString(value)
	if quoted != "$store1$"+value+"$store1$" {
		t.Fatalf("dollar quote 구분자 충돌을 피하지 못했다: %s", quoted)
	}
}

// TestEncodePropertiesBuildsCypherMap은 AGE 속성값은 이스케이프하면서 key를 Cypher 식별자로 만드는지 확인한다.
func TestEncodePropertiesBuildsCypherMap(t *testing.T) {
	encoded, err := encodeProperties(map[string]any{"body": "'본문'", "version": int64(1)})
	if err != nil {
		t.Fatalf("AGE property 인코딩: %v", err)
	}
	if encoded != `{body: "'본문'", version: 1}` {
		t.Fatalf("Cypher map 결과가 다르다: %s", encoded)
	}
}

// TestParseContextVerifiesGraphIsolation은 AGE 응답 조립이 다른 graph_id 정점을 거부하는지 확인한다.
func TestParseContextVerifiesGraphIsolation(t *testing.T) {
	requestedGraphID := mustID(t, "0199f2bd-9db5-7a33-9e3e-5cda2deabcef")
	foreignGraphID := mustID(t, "0199f2bd-9db6-7a33-9e3e-5cda2deabcef")
	contextID := mustID(t, "0199f2bd-9db7-7a33-9e3e-5cda2deabcef")
	createdBy := mustID(t, "0199f2bd-9db8-7a33-9e3e-5cda2deabcef")
	createdByAgent := mustID(t, "0199f2bd-9db9-7a33-9e3e-5cda2deabcef")
	raw := `{"id":1,"label":"Context","properties":{"context_id":"` + contextID.String() + `","graph_id":"` + foreignGraphID.String() + `","layer":"source","body":"본문","recorded_at":"2026-09-06T10:20:30Z","created_by":"` + createdBy.String() + `","created_by_agent":"` + createdByAgent.String() + `","version":1,"source_ref_channel":"api","source_ref_locator":"https://example.test/source","occurred_at":"2026-09-06T10:20:30Z","origin_kind":"external_content"}}::vertex`

	_, err := parseContext(raw, requestedGraphID)
	if err == nil || !strings.Contains(err.Error(), "그래프 격리 위반") {
		t.Fatalf("다른 그래프 정점이 격리 위반으로 거부되지 않았다: %v", err)
	}
}

// TestParseContextDecodesSource는 source_ref를 평탄화한 AGE property에서 도메인 모델을 복원하는지 확인한다.
func TestParseContextDecodesSource(t *testing.T) {
	graphID := mustID(t, "0199f2bd-9db5-7a33-9e3e-5cda2deabcef")
	contextID := mustID(t, "0199f2bd-9db7-7a33-9e3e-5cda2deabcef")
	createdBy := mustID(t, "0199f2bd-9db8-7a33-9e3e-5cda2deabcef")
	createdByAgent := mustID(t, "0199f2bd-9db9-7a33-9e3e-5cda2deabcef")
	raw := `{"id":1,"label":"Context","properties":{"context_id":"` + contextID.String() + `","graph_id":"` + graphID.String() + `","layer":"source","body":"본문","recorded_at":"2026-09-06T10:20:30Z","created_by":"` + createdBy.String() + `","created_by_agent":"` + createdByAgent.String() + `","version":1,"source_ref_channel":"api","source_ref_locator":"https://example.test/source","occurred_at":"2026-09-06T10:20:30Z","origin_kind":"external_content"}}::vertex`

	context, err := parseContext(raw, graphID)
	if err != nil {
		t.Fatalf("원천 정점 해석: %v", err)
	}
	if context.Source == nil || context.Source.Reference.Locator != "https://example.test/source" || context.GraphID != graphID {
		t.Fatalf("원천 정점 해석 결과가 다르다: %#v", context)
	}
}

// mustID는 테스트에서 유효한 UUIDv7 리터럴을 해석한다.
func mustID(t *testing.T, raw string) model.ID {
	t.Helper()
	id, err := model.ParseID(raw)
	if err != nil {
		t.Fatalf("UUIDv7 해석: %v", err)
	}
	return id
}
