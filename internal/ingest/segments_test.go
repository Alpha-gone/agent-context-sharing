package ingest

import (
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestClosedSegments(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	id := func() model.ID { id, _ := model.NewID(); return id }
	turn := func(index int64, at time.Time, kind model.OriginKind, text string) model.Context {
		return model.Context{ID: id(), Body: text, RecordedAt: at, Source: &model.SourceAttributes{TurnIndex: new(index), OccurredAt: at, OriginKind: kind}}
	}
	turns := []model.Context{turn(2, now.Add(-time.Minute), model.OriginKindUserUtterance, "세번째"), turn(0, now.Add(-time.Hour), model.OriginKindAgentOutput, "에이전트"), turn(1, now.Add(-59*time.Minute), model.OriginKindUserUtterance, "가나다라")}
	got, err := ClosedSegments(turns, SegmentConfig{Gap: 30 * time.Minute, MaxTurns: 40, ExcerptChars: 3, BodyChars: 8}, now)
	if err != nil || len(got) != 1 {
		t.Fatalf("구간=%d 오류=%v", len(got), err)
	}
	if got[0].First != 0 || got[0].Last != 1 || got[0].Body != "가나다" || len(got[0].Members) != 2 {
		t.Fatalf("잘못된 구간: %+v", got[0])
	}
	if got[0].Start != turns[1].Source.OccurredAt || got[0].End != turns[2].Source.OccurredAt {
		t.Fatal("시간 범위")
	}
	if turns[0].Source.TurnIndex == nil || *turns[0].Source.TurnIndex != 2 {
		t.Fatal("입력 순서를 바꿈")
	}
}

func TestClosedSegmentsLimitsAndFallback(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var turns []model.Context
	for i := range 3 {
		id, _ := model.NewID()
		index := int64(i)
		turns = append(turns, model.Context{ID: id, Body: "가나다라", RecordedAt: now, Source: &model.SourceAttributes{TurnIndex: &index, OccurredAt: now, OriginKind: model.OriginKindAgentOutput}})
	}
	got, err := ClosedSegments(turns, SegmentConfig{Gap: time.Minute, MaxTurns: 2, ExcerptChars: 3, BodyChars: 5}, now)
	if err != nil || len(got) != 1 || got[0].Body != "가나다\n가" || len(got[0].Members) != 2 {
		t.Fatalf("상한 또는 대체=%+v err=%v", got, err)
	}
	got, err = ClosedSegments(turns, SegmentConfig{Gap: time.Minute, MaxTurns: 3, ExcerptChars: 3, BodyChars: 5}, now.Add(time.Minute))
	if err != nil || len(got) != 1 || len(got[0].Members) != 3 {
		t.Fatal("닫힌 경계", err)
	}
}
