package config

import (
	"testing"
	"time"
)

func TestConversationSegmentConfiguration(t *testing.T) {
	cfg, err := loadValues(validValues())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConversationSegmentGap != 30*time.Minute || cfg.ConversationSegmentMaxTurns != 40 || cfg.ConversationSegmentExcerptChars != 300 || cfg.ConversationSegmentBodyChars != 2000 {
		t.Fatal("구간화 기본값")
	}
	values := validValues()
	values["CONVERSATION_SEGMENT_GAP"] = "2h"
	values["CONVERSATION_SEGMENT_MAX_TURNS"] = "12"
	values["CONVERSATION_SEGMENT_EXCERPT_CHARS"] = "5"
	values["CONVERSATION_SEGMENT_BODY_CHARS"] = "10"
	cfg, err = loadValues(values)
	if err != nil || cfg.ConversationSegmentGap != 2*time.Hour || cfg.ConversationSegmentMaxTurns != 12 || cfg.ConversationSegmentExcerptChars != 5 || cfg.ConversationSegmentBodyChars != 10 {
		t.Fatal("구간화 구성", err)
	}
	for key, value := range map[string]string{"CONVERSATION_SEGMENT_GAP": "59s", "CONVERSATION_SEGMENT_MAX_TURNS": "1", "CONVERSATION_SEGMENT_EXCERPT_CHARS": "0", "CONVERSATION_SEGMENT_BODY_CHARS": "8001"} {
		t.Run(key, func(t *testing.T) { values := validValues(); values[key] = value; rejectValues(t, values) })
	}
	values = validValues()
	values["CONVERSATION_SEGMENT_EXCERPT_CHARS"] = "50"
	values["CONVERSATION_SEGMENT_BODY_CHARS"] = "49"
	rejectValues(t, values)
}
