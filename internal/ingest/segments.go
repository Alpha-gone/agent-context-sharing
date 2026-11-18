package ingest

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
)

// SegmentConfig는 닫힌 구간 판정과 본문 발췌 상한이다.
type SegmentConfig struct {
	Gap                               time.Duration
	MaxTurns, ExcerptChars, BodyChars int
}

// Segment는 시간 순서가 아닌 턴 순서로 정렬한 원천과 사건 생성 입력이다.
type Segment struct {
	First, Last int64
	Start, End  time.Time
	Body        string
	Members     []model.ID
}

// ClosedSegments는 DB 기준 시각에서 닫힌 새 구간을 결정한다. 기존 구간 및 늦은 턴은 호출자가 분리한다.
func ClosedSegments(turns []model.Context, cfg SegmentConfig, databaseNow time.Time) ([]Segment, error) {
	if cfg.Gap < time.Minute || cfg.Gap > 24*time.Hour || cfg.MaxTurns < 2 || cfg.MaxTurns > 100 || cfg.ExcerptChars < 1 || cfg.BodyChars < cfg.ExcerptChars || cfg.BodyChars > 8000 || databaseNow.IsZero() {
		return nil, fmt.Errorf("구간화 구성이 올바르지 않다")
	}
	ordered := slices.Clone(turns)
	for _, turn := range ordered {
		if !turn.ID.IsV7() || turn.Source == nil || turn.Source.TurnIndex == nil || *turn.Source.TurnIndex < 0 || turn.Source.OccurredAt.IsZero() || turn.RecordedAt.IsZero() || !utf8.ValidString(turn.Body) || turn.Body == "" || (turn.Source.OriginKind != model.OriginKindUserUtterance && turn.Source.OriginKind != model.OriginKindAgentOutput) {
			return nil, fmt.Errorf("구간 원천이 올바르지 않다")
		}
	}
	slices.SortFunc(ordered, func(a, b model.Context) int { return cmp.Compare(*a.Source.TurnIndex, *b.Source.TurnIndex) })
	for i := 1; i < len(ordered); i++ {
		if *ordered[i-1].Source.TurnIndex == *ordered[i].Source.TurnIndex {
			return nil, fmt.Errorf("대화 턴 순서가 중복됐다")
		}
	}
	var result []Segment
	for start := 0; start < len(ordered); {
		end := start + 1
		for end < len(ordered) && end-start < cfg.MaxTurns && ordered[end].Source.OccurredAt.Sub(ordered[end-1].Source.OccurredAt) <= cfg.Gap {
			end++
		}
		if end == len(ordered) && end-start < cfg.MaxTurns && databaseNow.Before(ordered[end-1].RecordedAt.Add(cfg.Gap)) {
			break
		}
		segment, err := SummarizeSegment(ordered[start:end], cfg)
		if err != nil {
			return nil, err
		}
		result = append(result, segment)
		start = end
	}
	return result, nil
}

// SummarizeSegment는 기존 구간의 구성원이 늘었을 때 같은 규칙으로 사건 입력을 만든다.
func SummarizeSegment(turns []model.Context, cfg SegmentConfig) (Segment, error) {
	if len(turns) == 0 || len(turns) > 1000 || cfg.ExcerptChars < 1 || cfg.BodyChars < cfg.ExcerptChars || cfg.BodyChars > 8000 {
		return Segment{}, fmt.Errorf("구간 본문 구성이 올바르지 않다")
	}
	chunk := slices.Clone(turns)
	for _, turn := range chunk {
		if !turn.ID.IsV7() || turn.Source == nil || turn.Source.TurnIndex == nil || *turn.Source.TurnIndex < 0 || turn.Source.OccurredAt.IsZero() || !utf8.ValidString(turn.Body) || turn.Body == "" || (turn.Source.OriginKind != model.OriginKindUserUtterance && turn.Source.OriginKind != model.OriginKindAgentOutput) {
			return Segment{}, fmt.Errorf("구간 구성원이 올바르지 않다")
		}
	}
	slices.SortFunc(chunk, func(a, b model.Context) int { return cmp.Compare(*a.Source.TurnIndex, *b.Source.TurnIndex) })
	for i := 1; i < len(chunk); i++ {
		if *chunk[i-1].Source.TurnIndex == *chunk[i].Source.TurnIndex {
			return Segment{}, fmt.Errorf("구간 턴 순서가 중복됐다")
		}
	}
	segment := Segment{First: *chunk[0].Source.TurnIndex, Last: *chunk[len(chunk)-1].Source.TurnIndex, Start: chunk[0].Source.OccurredAt, End: chunk[0].Source.OccurredAt, Members: make([]model.ID, 0, len(chunk))}
	hasUser := false
	for _, turn := range chunk {
		segment.Members = append(segment.Members, turn.ID)
		if turn.Source.OccurredAt.Before(segment.Start) {
			segment.Start = turn.Source.OccurredAt
		}
		if turn.Source.OccurredAt.After(segment.End) {
			segment.End = turn.Source.OccurredAt
		}
		if turn.Source.OriginKind == model.OriginKindUserUtterance {
			hasUser = true
		}
	}
	var parts []string
	remaining := cfg.BodyChars
	for _, turn := range chunk {
		if hasUser && turn.Source.OriginKind != model.OriginKindUserUtterance {
			continue
		}
		if len(parts) > 0 {
			if remaining <= 1 {
				break
			}
			remaining--
		}
		text := []rune(turn.Body)
		length := min(len(text), cfg.ExcerptChars, remaining)
		if length == 0 {
			break
		}
		parts = append(parts, string(text[:length]))
		remaining -= length
		if remaining == 0 {
			break
		}
	}
	segment.Body = strings.Join(parts, "\n")
	return segment, nil
}
