package main

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

type longMemoryQuestion struct {
	ID             string             `json:"question_id"`
	Type           string             `json:"question_type"`
	Question       string             `json:"question"`
	Answer         jsontext.Value     `json:"answer"`
	Date           string             `json:"question_date"`
	Dates          []string           `json:"haystack_dates"`
	SessionIDs     []string           `json:"haystack_session_ids"`
	Sessions       [][]longMemoryTurn `json:"haystack_sessions"`
	AnswerSessions []string           `json:"answer_session_ids"`
}

type longMemoryTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer *bool  `json:"has_answer"`
}

func prepareLongMemory(questions []longMemoryQuestion, pin *publicDataPin, expected int) (preparedData, error) {
	data := newPreparedData("longmemeval-s", pin, len(questions))
	if len(questions) != expected || expected < 1 {
		return data, fmt.Errorf("LongMemEval 문항 수 불일치")
	}
	tasks := map[string]string{}
	seen := map[string]bool{}
	for i, q := range questions {
		if q.ID == "" || seen[q.ID] || q.Question == "" || !validPreparedAnswer(q.Answer) || !slices.Contains([]string{"single-session-user", "single-session-assistant", "single-session-preference", "multi-session", "temporal-reasoning", "knowledge-update"}, q.Type) {
			return data, fmt.Errorf("LongMemEval 문항 계약 오류")
		}
		seen[q.ID] = true
		taskID := strings.TrimSuffix(q.ID, "_abs")
		if tasks[taskID] == "" {
			tasks[taskID] = fmt.Sprintf("lme-t%04d", len(tasks)+1)
		}
		unit := tasks[taskID]
		graph := fmt.Sprintf("lme-g%04d", i+1)
		code := fmt.Sprintf("lme-q%04d", i+1)
		asOf, err := time.Parse("2006/01/02 (Mon) 15:04", q.Date)
		if err != nil {
			return data, fmt.Errorf("LongMemEval 질의 시각 오류")
		}
		if len(q.Sessions) == 0 || len(q.Sessions) != len(q.Dates) || len(q.Sessions) != len(q.SessionIDs) {
			return data, fmt.Errorf("LongMemEval 세션 매핑 오류")
		}
		group := preparedGroup{Code: graph, Contexts: contextSet{Version: preparationVersion}}
		query := preparedQuery{Code: code, Group: graph, Unit: unit, Type: q.Type, Question: q.Question, Answer: q.Answer, AnswerStatus: "provided", EvidenceStatus: "unmeasured", EvidenceReason: "missing_annotation", AsOf: asOf}
		abstention := strings.HasSuffix(q.ID, "_abs")
		if abstention {
			query.Type = "abstention"
			query.Answer = nil
			query.AnswerStatus = "unanswerable"
			query.EvidenceStatus = "not_applicable"
			query.EvidenceReason = "abstention"
		}
		sessions := map[string]bool{}
		annotatedSessions := map[string]bool{}
		for si, session := range q.Sessions {
			sessionID := q.SessionIDs[si]
			date, err := time.Parse("2006/01/02 (Mon) 15:04", q.Dates[si])
			if err != nil || sessionID == "" {
				return data, fmt.Errorf("LongMemEval 세션 계약 오류")
			}
			data.Report.SourceCounts["sessions"]++
			if sessions[sessionID] {
				data.Report.SourceCounts["repeated_session_ids"]++
			}
			if len(session) == 0 {
				data.Report.SourceCounts["empty_sessions"]++
			}
			sessions[sessionID] = true
			data.Mappings.Items = append(data.Mappings.Items, privateMapping{Code: fmt.Sprintf("%s-s%04d", graph, si+1), Unit: graph, Original: q.ID, Session: sessionID, Timestamp: q.Dates[si]})
			for ti, turn := range session {
				data.Report.SourceCounts["turns"]++
				if !slices.Contains([]string{"user", "assistant"}, turn.Role) {
					return data, fmt.Errorf("LongMemEval 발화 계약 오류")
				}
				key := fmt.Sprintf("%s-s%04d-t%04d", graph, si+1, ti+1)
				if strings.TrimSpace(turn.Content) == "" {
					if turn.HasAnswer != nil && *turn.HasAnswer {
						return data, fmt.Errorf("LongMemEval 빈 발화 근거 오류")
					}
					data.Report.SourceCounts["empty_turns"]++
					data.Mappings.Items = append(data.Mappings.Items, privateMapping{Code: key, Unit: graph, Original: q.ID, Session: sessionID, Turn: strconv.Itoa(ti + 1), Speaker: turn.Role, UnindexedText: new(turn.Content)})
					continue
				}
				group.Contexts.Contexts = append(group.Contexts.Contexts, contextSpec{Key: key, Layer: "source", Body: turn.Content, Source: &sourceSpec{Channel: "conversation", Locator: "urn:eval:" + key, OccurredAt: date, OriginKind: "external_content"}})
				data.Mappings.Items = append(data.Mappings.Items, privateMapping{Code: key, Unit: graph, Original: q.ID, Session: sessionID, Turn: strconv.Itoa(ti + 1), Speaker: turn.Role, HasAnswer: turn.HasAnswer})
				if !abstention && slices.Contains(q.AnswerSessions, sessionID) && turn.HasAnswer != nil && *turn.HasAnswer {
					query.Evidence = append(query.Evidence, key)
					annotatedSessions[sessionID] = true
				}
			}
		}
		for _, id := range q.AnswerSessions {
			if !sessions[id] {
				return data, fmt.Errorf("LongMemEval 정답 세션 참조 오류")
			}
		}
		complete := len(q.AnswerSessions) > 0
		for _, id := range q.AnswerSessions {
			complete = complete && annotatedSessions[id]
		}
		if !abstention && complete {
			query.EvidenceStatus = "annotated"
			query.EvidenceReason = ""
		} else if !abstention {
			if len(query.Evidence) > 0 {
				query.EvidenceReason = "partial_annotation"
			}
			query.Evidence = nil
		}
		data.Memory.Groups = append(data.Memory.Groups, group)
		data.Mappings.Items = append(data.Mappings.Items, privateMapping{Code: code, Unit: graph, Original: q.ID, EvidenceRefs: q.AnswerSessions, OriginalAnswer: q.Answer})
		data.addQuery(query, "", unit)
	}
	return data, nil
}

func validPreparedAnswer(answer jsontext.Value) bool {
	if answer.Kind() == '"' {
		var text string
		return json.Unmarshal(answer, &text) == nil && strings.TrimSpace(text) != ""
	}
	return answer.Kind() == '0' && answer.IsValid()
}

type locomoConversation struct {
	ID           string                    `json:"sample_id"`
	Conversation map[string]jsontext.Value `json:"conversation"`
	Questions    []locomoQuestion          `json:"qa"`
	// 원본의 평가자가 만든 요약·관찰은 기억으로 쓰지 않는다.
	EventSummary   jsontext.Value `json:"event_summary"`
	Observation    jsontext.Value `json:"observation"`
	SessionSummary jsontext.Value `json:"session_summary"`
}

type locomoQuestion struct {
	Question          string         `json:"question"`
	Answer            jsontext.Value `json:"answer"`
	AdversarialAnswer jsontext.Value `json:"adversarial_answer"`
	Category          int            `json:"category"`
	Evidence          []string       `json:"evidence"`
}

type locomoTurn struct {
	ID         string         `json:"dia_id"`
	Speaker    string         `json:"speaker"`
	Text       string         `json:"text"`
	Image      jsontext.Value `json:"img_url"`
	Caption    jsontext.Value `json:"blip_caption"`
	ImageQuery jsontext.Value `json:"query"`
	Redownload jsontext.Value `json:"re-download"`
}

func prepareLocomo(conversations []locomoConversation, pin *publicDataPin, expectedConversations, expectedQuestions int) (preparedData, error) {
	data := newPreparedData("locomo10-text", pin, expectedQuestions)
	if len(conversations) != expectedConversations || expectedConversations < 1 {
		return data, fmt.Errorf("LoCoMo 대화 수 불일치")
	}
	seen := map[string]bool{}
	total := 0
	types := []string{"", "multi-hop", "temporal", "commonsense", "single-hop", "abstention"}
	for ci, c := range conversations {
		if c.ID == "" || seen[c.ID] || len(c.Questions) == 0 {
			return data, fmt.Errorf("LoCoMo 대화 계약 오류")
		}
		seen[c.ID] = true
		unit := fmt.Sprintf("loco-c%02d", ci+1)
		var speakerA, speakerB string
		if json.Unmarshal(c.Conversation["speaker_a"], &speakerA) != nil || json.Unmarshal(c.Conversation["speaker_b"], &speakerB) != nil || speakerA == "" || speakerB == "" || speakerA == speakerB {
			return data, fmt.Errorf("LoCoMo 발화자 매핑 오류")
		}
		group := preparedGroup{Code: unit, Contexts: contextSet{Version: preparationVersion}}
		turns := map[string]locomoTurn{}
		keys := map[string]string{}
		var sessions []int
		for field := range c.Conversation {
			if field == "speaker_a" || field == "speaker_b" {
				continue
			}
			number, ok := strings.CutPrefix(field, "session_")
			if !ok {
				return data, fmt.Errorf("LoCoMo 대화 필드 오류")
			}
			if strings.HasSuffix(number, "_date_time") {
				number = strings.TrimSuffix(number, "_date_time")
			}
			n, err := strconv.Atoi(number)
			if err != nil || n < 1 || strconv.Itoa(n) != number {
				return data, fmt.Errorf("LoCoMo 세션 번호 오류")
			}
			if field == "session_"+number {
				sessions = append(sessions, n)
			}
		}
		slices.Sort(sessions)
		if len(sessions) == 0 {
			return data, fmt.Errorf("LoCoMo 세션/시각 누락")
		}
		for _, field := range slices.Sorted(maps.Keys(c.Conversation)) {
			raw := c.Conversation[field]
			if !strings.HasSuffix(field, "_date_time") {
				continue
			}
			if _, exists := c.Conversation[strings.TrimSuffix(field, "_date_time")]; exists {
				continue
			}
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return data, fmt.Errorf("LoCoMo 빈 세션 시각 형식 오류")
			}
			if _, err := time.Parse("3:04 pm on 2 January, 2006", value); err != nil {
				return data, fmt.Errorf("LoCoMo 빈 세션 시각 오류")
			}
			data.Report.SourceCounts["timestamp_only_sessions"]++
			data.Mappings.Items = append(data.Mappings.Items, privateMapping{Code: unit + "-" + strings.TrimSuffix(field, "_date_time"), Unit: unit, Original: c.ID, Session: strings.TrimSuffix(field, "_date_time"), Timestamp: value})
		}
		var latest time.Time
		for _, si := range sessions {
			data.Report.SourceCounts["sessions"]++
			field := "session_" + strconv.Itoa(si)
			var dateText string
			var session []locomoTurn
			if json.Unmarshal(c.Conversation[field+"_date_time"], &dateText) != nil || json.Unmarshal(c.Conversation[field], &session, json.RejectUnknownMembers(true)) != nil {
				return data, fmt.Errorf("LoCoMo 세션 형식 오류")
			}
			date, err := time.Parse("3:04 pm on 2 January, 2006", dateText)
			if err != nil || len(session) == 0 {
				return data, fmt.Errorf("LoCoMo 세션 시각 오류")
			}
			if date.After(latest) {
				latest = date
			}
			data.Mappings.Items = append(data.Mappings.Items, privateMapping{Code: unit + "-" + field, Unit: unit, Original: c.ID, Session: field, Timestamp: dateText})
			for ti, turn := range session {
				data.Report.SourceCounts["turns"]++
				if turn.ID == "" || turns[turn.ID].ID != "" || strings.TrimSpace(turn.Text) == "" || !slices.Contains([]string{speakerA, speakerB}, turn.Speaker) {
					return data, fmt.Errorf("LoCoMo 턴 계약 오류")
				}
				key := fmt.Sprintf("%s-s%03d-t%03d", unit, si, ti+1)
				turns[turn.ID] = turn
				keys[turn.ID] = key
				group.Contexts.Contexts = append(group.Contexts.Contexts, contextSpec{Key: key, Layer: "source", Body: turn.Text, Source: &sourceSpec{Channel: "conversation", Locator: "urn:eval:" + key, OccurredAt: date, OriginKind: "external_content"}})
				data.Mappings.Items = append(data.Mappings.Items, privateMapping{Code: key, Unit: unit, Original: c.ID, Session: field, Turn: turn.ID, Speaker: turn.Speaker})
			}
		}
		data.Memory.Groups = append(data.Memory.Groups, group)
		for qi, q := range c.Questions {
			total++
			if q.Question == "" || q.Category < 1 || q.Category > 5 || (q.Category != 5 && !validPreparedAnswer(q.Answer)) {
				return data, fmt.Errorf("LoCoMo 문항 계약 오류")
			}
			code := fmt.Sprintf("%s-q%04d", unit, qi+1)
			query := preparedQuery{Code: code, Group: unit, Unit: unit, Type: types[q.Category], Question: q.Question, Answer: q.Answer, AnswerStatus: "provided", EvidenceStatus: "annotated", AsOf: latest}
			image := false
			missing := len(q.Evidence) == 0
			for _, ref := range q.Evidence {
				turn, exists := turns[ref]
				// 원본의 비정규 참조를 분할·추정하지 않는다.
				if !exists {
					missing = true
					continue
				}
				if len(turn.Image) > 0 && string(turn.Image) != "null" && string(turn.Image) != "\"\"" {
					image = true
				}
				query.Evidence = append(query.Evidence, keys[ref])
			}
			data.Mappings.Items = append(data.Mappings.Items, privateMapping{Code: code, Unit: unit, Original: fmt.Sprintf("%s:qa:%d", c.ID, qi), EvidenceRefs: q.Evidence, OriginalAnswer: q.Answer, AdversarialAnswer: q.AdversarialAnswer})
			if image {
				data.Report.Exclusions.Items = append(data.Report.Exclusions.Items, manifestExclusion{Dataset: data.Report.Dataset, Query: code, Reason: "image_evidence"})
				continue
			}
			if q.Category == 5 {
				query.Answer = nil
				query.AnswerStatus = "unanswerable"
				query.Evidence = nil
				query.EvidenceStatus = "not_applicable"
				query.EvidenceReason = "abstention"
			} else if missing {
				query.Evidence = nil
				query.EvidenceStatus = "unmeasured"
				query.EvidenceReason = "invalid_annotation"
				if len(q.Evidence) == 0 {
					query.EvidenceReason = "missing_annotation"
				}
			}
			query.Evidence = slices.Compact(query.Evidence)
			data.addQuery(query, unit, "")
		}
	}
	if total != expectedQuestions {
		return data, fmt.Errorf("LoCoMo 문항 수 불일치")
	}
	return data, nil
}
