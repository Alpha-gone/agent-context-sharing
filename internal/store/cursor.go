package store

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
)

// graphCursor는 그래프 목록의 마지막 행을 나타내는 내부 커서다.
type graphCursor struct {
	// activity 필드는 마지막 그래프의 최근 활동 UTC 시각이다.
	activity time.Time
	// graphID 필드는 활동 시각이 같은 행을 구분하는 보조 정렬 키다.
	graphID model.ID
}

// encodeGraphCursor는 마지막 활동 시각과 그래프 식별자를 불투명 문자열로 만든다.
func encodeGraphCursor(cursor graphCursor) string {
	raw := cursor.activity.UTC().Format(time.RFC3339Nano) + "|" + cursor.graphID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeGraphCursor는 graph_list 커서의 형식과 UUIDv7 식별자를 검증한다.
func decodeGraphCursor(raw string) (graphCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return graphCursor{}, fmt.Errorf("그래프 목록 커서 해독: %w", err)
	}
	activityText, graphIDText, found := strings.Cut(string(decoded), "|")
	if !found || strings.Contains(graphIDText, "|") {
		return graphCursor{}, fmt.Errorf("그래프 목록 커서 형식이 올바르지 않다")
	}
	activity, err := time.Parse(time.RFC3339Nano, activityText)
	if err != nil || activity.Location() != time.UTC {
		return graphCursor{}, fmt.Errorf("그래프 목록 커서 시각이 올바르지 않다")
	}
	graphID, err := model.ParseID(graphIDText)
	if err != nil {
		return graphCursor{}, fmt.Errorf("그래프 목록 커서 식별자: %w", err)
	}
	return graphCursor{activity: activity, graphID: graphID}, nil
}

// encodeRelationCursor는 마지막 관계 식별자를 불투명 문자열로 만든다.
func encodeRelationCursor(relationID model.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(relationID.String()))
}

// decodeRelationCursor는 relation_list 커서의 형식과 UUIDv7 식별자를 검증한다.
func decodeRelationCursor(raw string) (model.ID, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return model.ID{}, fmt.Errorf("관계 목록 커서 해독: %w", err)
	}
	relationID, err := model.ParseID(string(decoded))
	if err != nil {
		return model.ID{}, fmt.Errorf("관계 목록 커서 식별자: %w", err)
	}
	return relationID, nil
}
