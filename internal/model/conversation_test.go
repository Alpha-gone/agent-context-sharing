package model

import (
	json "encoding/json/v2"
	"strings"
	"testing"
)

func TestConversationSourceContract(t *testing.T) {
	for _, sample := range []struct {
		name, conversation, field string
		turn                      *int64
		channel                   SourceChannel
	}{
		{"legacy", "", "", nil, SourceChannelConversation},
		{"first_turn", "대화", "", new(int64(0)), SourceChannelConversation},
		{"max_length", strings.Repeat("가", 128), "", new(int64(9)), SourceChannelConversation},
		{"missing_turn", "대화", "turn_index", nil, SourceChannelConversation},
		{"missing_conversation", "", "turn_index", new(int64(0)), SourceChannelConversation},
		{"negative_turn", "대화", "turn_index", new(int64(-1)), SourceChannelConversation},
		{"blank", " \t", "conversation_id", new(int64(0)), SourceChannelConversation},
		{"too_long", strings.Repeat("가", 129), "conversation_id", new(int64(0)), SourceChannelConversation},
		{"file", "대화", "conversation_id", new(int64(0)), SourceChannelFile},
		{"invalid_text", "a\x00b", "conversation_id", new(int64(0)), SourceChannelConversation},
	} {
		t.Run(sample.name, func(t *testing.T) {
			value := sourceContext(t, contractTime())
			value.Source.Reference.Channel = sample.channel
			value.Source.ConversationID, value.Source.TurnIndex = sample.conversation, sample.turn
			err := value.Validate()
			if sample.field != "" {
				assertField(t, err, sample.field)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	source := sourceContext(t, contractTime())
	source.Source.ConversationID, source.Source.TurnIndex = "a", new(int64(0))
	next := cloneContext(source)
	next.Source.TurnIndex = new(int64(1))
	assertField(t, ValidateUpdate(source, next), "conversation_id")
}

func TestConversationEventContract(t *testing.T) {
	source := sourceContext(t, contractTime())
	source.Source.ConversationID, source.Source.TurnIndex = "a", new(int64(0))
	event := eventContext(t, contractTime(), contractTime())
	event.GraphID = source.GraphID
	event.Event.ConversationID = "a"
	event.Event.MemberIDs = []ID{source.ID}
	if err := ValidateEventMembers(event, []Context{source}); err != nil {
		t.Fatal(err)
	}
	source.Source.ConversationID = "b"
	assertField(t, ValidateEventMembers(event, []Context{source}), "member_refs")
	source.Source.ConversationID, source.Source.TurnIndex = "", nil
	assertField(t, ValidateEventMembers(event, []Context{source}), "member_refs")
	next := cloneContext(event)
	next.Version++
	next.Event.ConversationID = "b"
	assertField(t, ValidateUpdate(event, next), "conversation_id")
	next.Event.ConversationID = "a"
	next.Event.PreviousEventID = new(newTestID(t, contractTime()))
	assertField(t, ValidateUpdate(event, next), "previous_event_id")
	encoded, err := json.Marshal(next.Event)
	if err != nil || strings.Contains(string(encoded), next.Event.PreviousEventID.String()) {
		t.Fatalf("생성 입력이 직렬화됐다: %s / %v", encoded, err)
	}
	next.Event.PreviousEventID = new(next.ID)
	assertField(t, next.Validate(), "previous_event_id")
	next.Event.PreviousEventID = new(newTestID(t, contractTime()))
	next.Event.ConversationID = ""
	assertField(t, next.Validate(), "previous_event_id")
}
