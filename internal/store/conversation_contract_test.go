package store

import (
	json "encoding/json/v2"
	"errors"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func conversationSource(t *testing.T, graph, actor model.ID, conversation string, turn int64) model.Context {
	t.Helper()
	value := testSourceContext(t, graph, actor, "conversation://"+newTestID(t).String())
	value.Source.Reference.Channel = model.SourceChannelConversation
	value.Source.ConversationID, value.Source.TurnIndex = conversation, new(turn)
	return value
}

func conversationEvent(t *testing.T, graph, actor, member model.ID, conversation string, start time.Time, end *time.Time) model.Context {
	t.Helper()
	value := testEventContext(t, graph, actor, member)
	value.Event.ConversationID, value.Event.Start, value.Event.End = conversation, start, end
	return value
}

func TestConversationProperties(t *testing.T) {
	graph, actor := newTestID(t), newTestID(t)
	source := conversationSource(t, graph, actor, "dialogue", 0)
	event := conversationEvent(t, graph, actor, source.ID, "dialogue", source.Source.OccurredAt, nil)
	event.Event.PreviousEventID = new(newTestID(t))
	for _, value := range []model.Context{source, event} {
		properties := contextProperties(value)
		if _, present := properties["previous_event_id"]; present {
			t.Fatal("생성 입력이 속성에 저장됐다")
		}
		raw, err := json.Marshal(map[string]any{"label": "Context", "properties": properties})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := parseContext(string(raw), graph)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Source != nil && (decoded.Source.ConversationID != "dialogue" || decoded.Source.TurnIndex == nil || *decoded.Source.TurnIndex != 0) {
			t.Fatalf("원천 순서 왕복=%+v", decoded.Source)
		}
		if decoded.Event != nil && (decoded.Event.ConversationID != "dialogue" || decoded.Event.PreviousEventID != nil) {
			t.Fatalf("사건 대화 왕복=%+v", decoded.Event)
		}
	}
	other := event
	other.ID = newTestID(t)
	other.Event = new(*event.Event)
	other.Event.ConversationID = "another"
	other.Event.Start = event.Event.Start.Add(time.Minute)
	event.Event.End = new(event.Event.Start)
	if _, _, ok := adjacentPrecedesCandidate(graph, event, other, time.Hour); ok {
		t.Fatal("다른 명시적 대화의 시간 후보가 생성됐다")
	}
}

func TestConversationPreviousIsolationIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, member := lifeProposalFixture(t, database)
	otherGraph, otherActor, otherMember := lifeProposalFixture(t, database)
	at := time.Now().UTC().Truncate(time.Second)
	foreign := conversationEvent(t, otherGraph, otherActor, otherMember.ID, "same-name", at, nil)
	foreign, err := database.CreateContext(t.Context(), otherGraph, foreign, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted := conversationEvent(t, graph, actor, member.ID, "same-name", at, nil)
	deleted, err = database.CreateContext(t.Context(), graph, deleted, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DiscardContext(t.Context(), graph, deleted.ID, nil, WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	for _, previous := range []model.ID{foreign.ID, deleted.ID} {
		value := conversationEvent(t, graph, actor, member.ID, "same-name", at.Add(time.Minute), nil)
		value.Event.PreviousEventID = new(previous)
		if _, err := database.CreateContext(t.Context(), graph, value, nil); err == nil {
			t.Fatal("다른 그래프 또는 폐기된 앞 사건을 연결했다")
		}
		if _, err := database.Context(t.Context(), graph, value.ID); !errors.Is(err, ErrNotFound) || indexTaskCount(t, database, value.ID) != 0 {
			t.Fatalf("격리 거부 뒤 사건/큐가 남았다: %v", err)
		}
	}
}

func TestConversationCreateAtomicIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, member := lifeProposalFixture(t, database)
	now := time.Now().UTC().Truncate(time.Second)
	previous := conversationEvent(t, graph, actor, member.ID, "a", now, new(now.Add(10*time.Minute)))
	previous, err := database.CreateContext(t.Context(), graph, previous, nil)
	if err != nil {
		t.Fatal(err)
	}
	current := conversationEvent(t, graph, actor, member.ID, "a", now.Add(time.Minute), nil)
	current.Event.PreviousEventID = new(previous.ID)
	operation := &OperationRecord{Kind: OperationAdd, GraphID: graph, ContextID: current.ID, AccountID: actor, AgentID: actor, JudgmentInput: "explicit previous event"}
	stored, err := database.CreateContextWithOperation(t.Context(), graph, current, nil, operation, WriteLimits{})
	if err != nil || stored.Event.PreviousEventID != nil {
		t.Fatalf("시간이 겹치는 명시적 연결=%+v / %v", stored.Event, err)
	}
	relation := lifeProposalIdentity(t, database, graph, model.RelationTypePrecedes, previous.ID, current.ID)
	if relation.State != model.RelationStateConfirmed || relation.ConfirmedBy != actor || relation.ConfirmedByAgent != actor {
		t.Fatal(relation)
	}
	var audits int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log WHERE graph_id=$1 AND relation_id=$2 AND actor_account_id=$3 AND actor_agent_id=$3 AND result='applied'`, graph.String(), relation.ID.String(), actor.String()).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("관계 감사=%d / %v", audits, err)
	}
	for _, conversation := range []string{"b", "a"} {
		bad := conversationEvent(t, graph, actor, member.ID, conversation, now.Add(-time.Minute), nil)
		bad.Event.PreviousEventID = new(previous.ID)
		if _, err := database.CreateContext(t.Context(), graph, bad, nil); err == nil {
			t.Fatal("다른 대화 또는 역순 연결이 허용됐다")
		}
		if _, err := database.Context(t.Context(), graph, bad.ID); !errors.Is(err, ErrNotFound) || indexTaskCount(t, database, bad.ID) != 0 {
			t.Fatalf("실패한 생성이 남았다: %v", err)
		}
	}
	remove := lifeFailGraphWrite(t, database, graph, "operation_log", "INSERT", false)
	failed := conversationEvent(t, graph, actor, member.ID, "a", now.Add(2*time.Minute), nil)
	failed.Event.PreviousEventID = new(previous.ID)
	if _, err := database.CreateContext(t.Context(), graph, failed, nil); err == nil {
		t.Fatal("감사 실패가 성공했다")
	}
	remove()
	if _, err := database.Context(t.Context(), graph, failed.ID); !errors.Is(err, ErrNotFound) || indexTaskCount(t, database, failed.ID) != 0 {
		t.Fatalf("감사 실패 뒤 정점/큐가 남았다: %v", err)
	}
	if _, err := database.relationByIdentity(t.Context(), database.pool, graph, model.RelationTypePrecedes, previous.ID, failed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("감사 실패 뒤 관계가 남았다: %v", err)
	}
}

func TestConversationTurnUniquenessIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, _ := boundaryGraph(t, database)
	first := conversationSource(t, graph, actor, "a", 0)
	if _, err := database.CreateContext(t.Context(), graph, first, nil); err != nil {
		t.Fatal(err)
	}
	duplicate := conversationSource(t, graph, actor, "a", 0)
	if _, err := database.CreateContext(t.Context(), graph, duplicate, nil); err == nil {
		t.Fatal("같은 대화와 턴 순서가 허용됐다")
	} else if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != "turn_index" {
		t.Fatalf("중복 오류=%v", err)
	}
	for _, value := range []model.Context{conversationSource(t, graph, actor, "a", 1), conversationSource(t, graph, actor, "b", 0)} {
		if _, err := database.CreateContext(t.Context(), graph, value, nil); err != nil {
			t.Fatal(err)
		}
	}
	foreignGraph := createTestGraph(t, database, actor)
	if _, err := database.CreateContext(t.Context(), foreignGraph, conversationSource(t, foreignGraph, actor, "a", 0), nil); err != nil {
		t.Fatalf("다른 그래프의 같은 대화 턴이 거부됐다: %v", err)
	}
	immutable := conversationSource(t, graph, actor, "b", 1)
	immutable.Source.Reference.Locator = first.Source.Reference.Locator
	if stored, err := database.CreateContext(t.Context(), graph, immutable, nil); err != nil || stored.ID != first.ID || stored.Source.ConversationID != "a" || stored.Source.TurnIndex == nil || *stored.Source.TurnIndex != 0 {
		t.Fatalf("같은 원본 위치의 기존 원천 반환 계약이 바뀌었다: %+v / %v", stored.Source, err)
	}
}

func TestConversationNearestProposalIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, member := lifeProposalFixture(t, database)
	now := time.Now().UTC()
	var events []model.Context
	for i := range 4 {
		start := now.Add(time.Duration(i) * 2 * time.Minute)
		value := conversationEvent(t, graph, actor, member.ID, "a", start, new(start.Add(time.Minute)))
		stored, err := database.CreateContext(t.Context(), graph, value, nil)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, stored)
	}
	database.relationProposals = lifeProposalConfig(10)
	for range 2 {
		if err := database.ProposeEventRelations(t.Context(), graph, events[2].ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(proposalRelations(t, database, graph)); got != 2 {
		t.Fatalf("앞뒤 1개 제한=%d", got)
	}
	for _, relation := range proposalRelations(t, database, graph) {
		if relation.Type != model.RelationTypePrecedes || relation.FromContextID == events[0].ID {
			t.Fatalf("지름길 후보=%+v", relation)
		}
	}
	// 명시적으로 건너뛴 연결이 있으면 사건 0의 다음 방향에 새 후보를 만들지 않는다.
	if _, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypePrecedes, events[0].ID, events[2].ID), nil); err != nil {
		t.Fatal(err)
	}
	if err := database.ProposeEventRelations(t.Context(), graph, events[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := len(proposalRelations(t, database, graph)); got != 3 {
		t.Fatalf("확정 방향의 새 후보가 추가됐다: %d", got)
	}
}
