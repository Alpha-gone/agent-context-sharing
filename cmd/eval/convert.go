// 공개 벤치마크를 평가 데이터셋 형식으로 옮긴다.
//
// 변환기를 실행기와 같은 패키지에 두는 이유는 데이터셋 형식이 하나여야 하기 때문이다.
// 다른 자리에 두면 같은 구조체를 두 벌 갖게 되고, 한쪽만 고쳐도 알아차릴 방법이 없다.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"time"
)

// 지금 다루는 공개 벤치마크 형식은 하나다. 형식을 인자로 받는 이유는 다른 세트를
// 더할 때 변환 규칙이 이름으로 갈라져야 하기 때문이다.
const convertHippoRAG = "hipporag"

// convertOwn은 사건 관계를 포함한 자체 세트의 생성기 이름이다.
const convertOwn = "own"

// convertOwnGlobal은 전역 요약과 그 근거를 포함한 자체 세트의 생성기 이름이다.
const convertOwnGlobal = "own-global"

// 자체 세트의 재료다. 그룹마다 주제와 세부 어휘가 달라져 그래프 단계 비교에 쓸
// 어휘적 거리가 만들어진다. 낱말은 고정이고 그룹 번호로만 고르므로 생성은 결정적이다.
var (
	ownTopics = []string{
		"배포 파이프라인", "인증 서버", "데이터 마이그레이션", "검색 품질", "비용 최적화",
		"장애 대응", "권한 관리", "로그 수집", "캐시 전략", "테스트 자동화",
		"문서화", "온보딩", "API 버전 관리", "모니터링", "보안 감사",
		"데이터 백업", "성능 튜닝", "협업 워크플로", "요구사항 수집", "릴리스 관리",
		"개발 환경", "코드 리뷰", "기술 부채", "외부 연동", "알림 채널",
		"스키마 변경", "트래픽 대응", "실험 설계", "평가 지표", "운영 절차",
	}
	ownModules   = []string{"결제", "알림", "검색", "계정", "리포트", "게이트웨이", "작업 큐", "설정", "미터기", "웹훅"}
	ownArtefacts = []string{"설계안", "회의록", "점검표", "배포 note", "장애 보고서", "검토 의견"}
	ownEarlyActs = []string{"초기 요구사항을 정리했다", "현황을 조사했다", "초안을 작성했다", "제약 조건을 검토했다", "이해관계자 의견을 모았다"}
	ownMidActs   = []string{"구현 방향을 확정했다", "일정을 조정했다", "리스크를 검토했다", "범위를 합의했다"}
	ownLateActs  = []string{"안정화 작업을 마무리했다", "후속 조치를 정리했다", "재정리를 완료했다", "모니터링을 강화했다"}
	ownFindings  = []string{"예산 상한", "담당자 지정", "일정 지연 사유", "환경 설정 값", "승인 대기 항목", "외부 의존 목록", "임계값 조정", "회차별 참석자", "데이터 보관 기간", "실패 사례 요약", "버전 호환 조건", "검토 기준"}
)

// convertOwnSet은 사건 관계를 포함한 자체 세트를 결정적으로 만든다.
//
// 공개 벤치마크에는 사건·관계 대응물이 없어 「검색 품질 평가」가 자체 세트를 요구한다.
// 생성기가 곧 원본이므로 같은 명령으로 같은 파일이 다시 만들어진다. 질의 그룹 하나는
// 원천 12개(초기·중간·마무리 넷씩), 파생 3개, 사건 4개(초기·중간·묶음·마무리)와 확정
// 관계 3개로 구성한다. 사건 관계의 그래프 효과는 연상 질의가 재는데, 질의 어휘는 마무리
// 사건과 겹치고 정답 사건과 준비 구간은 별도 작업 코드만 쓴다. 연상 정답은 마무리 사건과
// `precedes`로 연결된 준비 묶음 사건이라 관계를 켠 단계에서 한 홉으로 올라온다.
func convertOwnSet(name string, questions int) (contextSet, querySet, error) {
	if questions < 2 || questions%2 != 0 {
		return contextSet{}, querySet{}, fmt.Errorf("자체 세트의 질의 수는 2 이상의 짝수여야 한다")
	}
	contexts := make([]contextSpec, 0, questions*19)
	relations := make([]relationSpec, 0, questions*3)
	queries := make([]querySpec, 0, questions)
	for group := range questions {
		topic := ownTopics[group%len(ownTopics)]
		if group >= len(ownTopics) {
			topic += "·" + ownModules[(group/len(ownTopics))%len(ownModules)]
		}
		associative := group%2 != 0
		base := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC).Add(time.Duration(group) * 72 * time.Hour)
		contexts = append(contexts, ownGroupContexts(group, topic, associative, base)...)
		relations = append(relations,
			relationSpec{Type: "part_of", From: ownKey(group, "e1"), To: ownKey(group, "ew")},
			relationSpec{Type: "part_of", From: ownKey(group, "e2"), To: ownKey(group, "ew")},
			relationSpec{Type: "precedes", From: ownKey(group, "ew"), To: ownKey(group, "ef")},
		)
		if !associative {
			finding := ownFindings[(group*5+(group%4)*7)%len(ownFindings)]
			queries = append(queries, querySpec{ID: fmt.Sprintf("own-f-%04d", group), UseCase: useCaseFact,
				WorkContext: topic + " 논의에서 " + finding + " 내용이 담긴 기록을 찾아야 한다.",
				Answers:     []string{ownKey(group, fmt.Sprintf("s%02d", group%4))}})
		} else {
			queries = append(queries, querySpec{ID: fmt.Sprintf("own-a-%04d", group), UseCase: useCaseAssociative,
				WorkContext: ownMarker(group) + "로 표시한 " + topic + " 안정화 회차를 마쳤다. 이 회차와 연결된 준비 과정 사건이 필요하다.",
				Answers:     []string{ownKey(group, "ew")}})
		}
	}
	version := fmt.Sprintf("%s-%dq", name, questions)
	return contextSet{Version: version, Contexts: contexts, Relations: relations}, querySet{Version: version, Queries: queries}, nil
}

// convertOwnGlobalSet은 전역 요약 검색을 검증하는 자체 세트를 결정적으로 만든다.
//
// 전역 요약 후보는 질의 유사도가 아니라 유효 시점과 최근 기록 순으로 정해진다. 질의마다
// 겹치지 않는 유효 구간을 두어 해당 시점의 요약 하나만 진입점이 되게 한다. 정답은 요약
// 자체가 아니라 그 근거 원천 넷이다. 따라서 명시적 global 범위의 기준선은 요약만 내고,
// references 단계부터 derived_from을 따라 그래프 전반의 근거를 모으는 차이가 드러난다.
func convertOwnGlobalSet(name string, questions int) (contextSet, querySet, error) {
	if questions < 1 {
		return contextSet{}, querySet{}, fmt.Errorf("전역 요약 자체 세트의 질의 수는 1 이상이어야 한다")
	}
	contexts := make([]contextSpec, 0, questions*5)
	queries := make([]querySpec, 0, questions)
	for group := range questions {
		topic := ownTopics[group%len(ownTopics)]
		if group >= len(ownTopics) {
			topic += "·" + ownModules[(group/len(ownTopics))%len(ownModules)]
		}
		from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(group) * 24 * time.Hour)
		to := from.Add(12 * time.Hour)
		answers := make([]string, 0, 4)
		for index := range 4 {
			key := fmt.Sprintf("w%03d-s%d", group, index)
			answers = append(answers, key)
			body := fmt.Sprintf("%s 전반의 %s 영역에서 %s. 검토 항목은 %s이다.",
				topic, ownModules[(group+index)%len(ownModules)], ownEarlyActs[index%len(ownEarlyActs)],
				ownFindings[(group*3+index*5)%len(ownFindings)])
			contexts = append(contexts, contextSpec{Key: key, Layer: "source", Body: body,
				Source: &sourceSpec{Channel: "conversation", Locator: fmt.Sprintf("urn:own-global:%s:%03d:%d", ownSlug(topic), group, index),
					OccurredAt: from.Add(time.Duration(index+1) * time.Hour), OriginKind: "user_utterance"}})
		}
		summaryKey := fmt.Sprintf("w%03d-g", group)
		contexts = append(contexts, contextSpec{Key: summaryKey, Layer: "derived",
			Body: fmt.Sprintf("%s의 그래프 전반을 정리한 전역 요약이다. 네 영역의 결정과 검토 항목을 함께 다룬다.", topic),
			Derived: &derivedSpec{Kind: "summary", SummaryScope: "global", DerivedFrom: answers,
				EvidenceState: "observation", ValidFrom: &from, ValidTo: &to}})
		asOf := from.Add(6 * time.Hour)
		queries = append(queries, querySpec{ID: fmt.Sprintf("own-g-%04d", group), UseCase: useCaseGlobal,
			WorkContext: topic + "의 그래프 전반에 걸친 결정 근거를 모두 모아야 한다.", Answers: answers,
			Scope: "global", AsOf: &asOf})
	}
	version := fmt.Sprintf("%s-%dq", name, questions)
	return contextSet{Version: version, Contexts: contexts}, querySet{Version: version, Queries: queries}, nil
}

// ownSlug는 주제를 로케이터에 쓸 수 있는 표기로 바꾼다. 로케이터가 scheme을 가진 URI여야
// 하므로 주제의 빈칸과 가운뎃점을 하이픈으로 바꾼다.
func ownSlug(topic string) string {
	return strings.NewReplacer(" ", "-", "·", "-").Replace(topic)
}

// ownKey는 그룹 안 컨텍스트의 파일 식별자를 만든다.
func ownKey(group int, suffix string) string {
	return fmt.Sprintf("g%03d-%s", group, suffix)
}

// ownMarker는 연상 질의가 관계 경로의 시작 사건 하나를 안정적으로 찾게 하는 표식이다.
func ownMarker(group int) string {
	return fmt.Sprintf("완료표식 Z%03d", group)
}

// ownGroupContexts는 질의 그룹 하나의 원천·파생·사건을 만든다. 사건 시간은 관계 검증이
// 요구하는 순서(초기·중간은 묶음 안, 마무리는 묶음 뒤)에 맞춘다.
func ownGroupContexts(group int, topic string, associative bool, base time.Time) []contextSpec {
	contexts := make([]contextSpec, 0, 19)
	preparationTopic := topic
	if associative {
		// 연상 질의의 준비 구간에 공개 주제어를 넣으면 의미·키워드 채널이 정답을 직접
		// 찾아 관계 단계의 효과가 사라진다. 작업 코드는 관계를 거쳐야만 뜻을 알 수 있다.
		preparationTopic = fmt.Sprintf("작업 코드 R%03d", group)
	}
	windows := []struct {
		topic string
		start time.Time
		acts  []string
	}{
		{preparationTopic, base.Add(time.Hour), ownEarlyActs},
		{preparationTopic, base.Add(25 * time.Hour), ownMidActs},
		{topic, base.Add(49 * time.Hour), ownLateActs},
	}
	for phase, window := range windows {
		for index := range 4 {
			id := phase*4 + index
			body := fmt.Sprintf("%s 관련 %d차 논의에서 %s. %s %s에 %s 내용이 남았다.",
				window.topic, id+1, window.acts[index%len(window.acts)],
				ownModules[(group+id*3)%len(ownModules)], ownArtefacts[(group+id)%len(ownArtefacts)],
				ownFindings[(group*5+id*7)%len(ownFindings)])
			contexts = append(contexts, contextSpec{Key: ownKey(group, fmt.Sprintf("s%02d", id)), Layer: "source", Body: body,
				Source: &sourceSpec{Channel: "conversation", Locator: fmt.Sprintf("urn:own:%s:%03d:%02d", ownSlug(topic), group, id),
					OccurredAt: window.start.Add(time.Duration(index+1) * 10 * time.Minute), OriginKind: "user_utterance"}})
		}
	}
	for index, members := range [][]string{
		{ownKey(group, "s00"), ownKey(group, "s01"), ownKey(group, "s02"), ownKey(group, "s03")},
		{ownKey(group, "s04"), ownKey(group, "s05"), ownKey(group, "s06"), ownKey(group, "s07")},
		{ownKey(group, "s08"), ownKey(group, "s09"), ownKey(group, "s10"), ownKey(group, "s11")},
	} {
		summaryTopic := preparationTopic
		if index == 2 {
			summaryTopic = topic
		}
		contexts = append(contexts, contextSpec{Key: ownKey(group, fmt.Sprintf("d%d", index)), Layer: "derived",
			Body:    summaryTopic + " 회차별 기록을 묶은 요약이다. 핵심 항목과 결정 흐름을 정리했다.",
			Derived: &derivedSpec{Kind: "summary", SummaryScope: "local", DerivedFrom: members, EvidenceState: "observation"}})
	}
	eventTimes := []struct {
		key   string
		start time.Time
	}{
		{"e1", base.Add(time.Hour)},
		{"e2", base.Add(25 * time.Hour)},
		{"ew", base.Add(time.Hour)},
		{"ef", base.Add(49 * time.Hour)},
	}
	eventMembers := map[string][]string{
		"e1": {ownKey(group, "s00"), ownKey(group, "s01"), ownKey(group, "s02"), ownKey(group, "s03")},
		"e2": {ownKey(group, "s04"), ownKey(group, "s05"), ownKey(group, "s06"), ownKey(group, "s07")},
		"ew": {ownKey(group, "s00"), ownKey(group, "s01"), ownKey(group, "s02"), ownKey(group, "s03"), ownKey(group, "s04"), ownKey(group, "s05"), ownKey(group, "s06"), ownKey(group, "s07")},
		"ef": {ownKey(group, "s08"), ownKey(group, "s09"), ownKey(group, "s10"), ownKey(group, "s11")},
	}
	eventBodies := map[string]string{
		"e1": preparationTopic + " 초기 논의 회차다. 요구사항과 제약을 다뤘다.",
		"e2": preparationTopic + " 중간 조율 회차다. 구현 방향과 일정을 확정했다.",
		"ew": preparationTopic + " 준비 과정을 묶은 전체 회차다. 초기 논의와 중간 조율을 함께 다룬다.",
		"ef": ownMarker(group) + "를 붙인 " + topic + " 안정화 회차다. 후속 조치와 재정리를 마무리했다.",
	}
	for _, event := range eventTimes {
		end := event.start.Add(time.Hour)
		if event.key == "ew" {
			end = base.Add(26 * time.Hour)
		}
		contexts = append(contexts, contextSpec{Key: ownKey(group, event.key), Layer: "event", Body: eventBodies[event.key],
			Event: &eventSpec{Members: eventMembers[event.key], Start: event.start, End: &end}})
	}
	return contexts
}

// hippoQuestion은 HippoRAG 2 재현 세트의 질의 하나다. 쓰지 않는 필드는 읽지 않는다.
type hippoQuestion struct {
	ID            string           `json:"id"`
	Question      string           `json:"question"`
	Answerable    bool             `json:"answerable"`
	Paragraphs    []hippoParagraph `json:"paragraphs"`
	Decomposition []hippoStep      `json:"question_decomposition"`
}

// hippoParagraph는 질의에 딸린 문단이다. 본문 필드 이름이 세트마다 다르므로 둘 다 받는다.
type hippoParagraph struct {
	Index         int    `json:"idx"`
	Title         string `json:"title"`
	Text          string `json:"text"`
	ParagraphText string `json:"paragraph_text"`
	IsSupporting  bool   `json:"is_supporting"`
}

func (paragraph hippoParagraph) body() string {
	text := paragraph.ParagraphText
	if text == "" {
		text = paragraph.Text
	}
	if paragraph.Title == "" {
		return text
	}
	return paragraph.Title + "\n\n" + text
}

// hippoStep은 다홉 질의를 한 홉씩 나눈 하위 질의다.
type hippoStep struct {
	Question     string `json:"question"`
	SupportIndex *int   `json:"paragraph_support_idx"`
}

// convertHippoRAGSet은 벤치마크 파일 하나에서 컨텍스트 집합과 질의 집합을 만든다.
//
// 문단을 원천 계층으로 옮기는 이유는 그것이 우리가 수집한 원본이 아니라 외부에서
// 가져온 내용이기 때문이다. 「출처 구분」이 그런 내용을 `external_content`로 받도록
// 확정했고, 그 값은 생성 뒤 바뀌지 않는다.
//
// 파생과 사건은 만들지 않는다. 질의문이나 정답에서 파생을 만들면 찾아야 할 답이 색인
// 대상 안으로 들어와 검색이 스스로를 맞히게 된다. 「검색 품질 평가」도 사건 관계를
// 포함한 세트를 공개 벤치마크가 아니라 자체 세트의 몫으로 두었다.
func convertHippoRAGSet(sourcePath, name string, limit int) (contextSet, querySet, error) {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return contextSet{}, querySet{}, fmt.Errorf("벤치마크 읽기: %w", err)
	}
	var questions []hippoQuestion
	if err := json.Unmarshal(raw, &questions); err != nil {
		return contextSet{}, querySet{}, fmt.Errorf("%s: 벤치마크 해석: %w", sourcePath, err)
	}
	if limit > 0 && limit < len(questions) {
		questions = questions[:limit]
	}
	if len(questions) == 0 {
		return contextSet{}, querySet{}, fmt.Errorf("%s: 질의가 없다", sourcePath)
	}

	version := fmt.Sprintf("hipporag2-%s-%dq", name, len(questions))
	contexts := contextSet{Version: version}
	queries := querySet{Version: version}
	keys := map[string]string{}

	for _, question := range questions {
		// 답이 없다고 표시된 질의는 정답 컨텍스트가 없어 재현율을 잴 수 없다.
		if !question.Answerable {
			continue
		}
		byIndex := make(map[int]string, len(question.Paragraphs))
		supporting := make([]string, 0, len(question.Paragraphs))
		for _, paragraph := range question.Paragraphs {
			body := paragraph.body()
			if strings.TrimSpace(body) == "" {
				continue
			}
			key := contextKey(body)
			if _, known := keys[key]; !known {
				keys[key] = body
				contexts.Contexts = append(contexts.Contexts, sourceSpecOf(key, body, name, len(contexts.Contexts)))
			}
			byIndex[paragraph.Index] = key
			if paragraph.IsSupporting {
				supporting = append(supporting, key)
			}
		}
		if len(supporting) == 0 {
			continue
		}
		// 근거가 여럿인 질의는 한 문단만 찾아서는 답이 되지 않으므로 연상 검색으로 둔다.
		// 근거가 하나뿐인 질의는 그 문단을 정확히 찾는 문제이므로 사실 검색이다.
		useCase := useCaseAssociative
		if len(supporting) == 1 {
			useCase = useCaseFact
		}
		queries.Queries = append(queries.Queries, querySpec{
			ID:          question.ID,
			UseCase:     useCase,
			WorkContext: question.Question,
			Answers:     supporting,
		})
		queries.Queries = append(queries.Queries, factQueries(question, byIndex)...)
	}
	if len(queries.Queries) == 0 {
		return contextSet{}, querySet{}, fmt.Errorf("%s: 정답을 가진 질의가 없다", sourcePath)
	}
	return contexts, queries, nil
}

// factQueries는 하위 질의 중 자립형인 것만 사실 검색 질의로 옮긴다.
//
// `#1`처럼 앞선 답을 가리키는 자리 표시가 있는 하위 질의는 그 자체로 읽을 수 없으므로
// 버린다. 자리 표시를 앞 단계의 답으로 채우면 정답을 질의문에 넣는 셈이 되어 그 홉을
// 검색이 아니라 주입으로 푼다.
func factQueries(question hippoQuestion, byIndex map[int]string) []querySpec {
	steps := make([]querySpec, 0, len(question.Decomposition))
	for order, step := range question.Decomposition {
		if step.SupportIndex == nil || strings.Contains(step.Question, "#") {
			continue
		}
		key, known := byIndex[*step.SupportIndex]
		if !known {
			continue
		}
		steps = append(steps, querySpec{
			ID:          fmt.Sprintf("%s#hop%d", question.ID, order+1),
			UseCase:     useCaseFact,
			WorkContext: step.Question,
			Answers:     []string{key},
		})
	}
	return steps
}

// convertBaseTime은 원천의 발생 시각에 쓰는 기준 시각이다. 벤치마크 문단에는 시각이
// 없고, 시간 필터 채널이 기준 시각과 견줄 값을 요구한다.
var convertBaseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// sourceSpecOf는 문단 하나를 원천 컨텍스트로 만든다. 발생 시각을 첫 등장 순서로
// 1초씩 벌리는 이유는 같은 시각이 여럿이면 시간 필터의 순서가 회차마다 달라지기
// 때문이다. 순서 자체에는 의미가 없고 결정적이기만 하면 된다.
func sourceSpecOf(key, body, name string, order int) contextSpec {
	return contextSpec{
		Key:   key,
		Layer: "source",
		Body:  body,
		Source: &sourceSpec{
			Channel:    "web",
			Locator:    fmt.Sprintf("urn:eval:hipporag2:%s:%s", name, key),
			OccurredAt: convertBaseTime.Add(time.Duration(order) * time.Second),
			OriginKind: "external_content",
		},
	}
}

// contextKey는 본문으로 결정되는 데이터셋 안의 이름이다. 제목만으로는 모자란다.
// 같은 제목에 다른 본문을 담은 문단이 있어 제목을 키로 쓰면 서로 다른 원천이 하나로
// 합쳐진다.
func contextKey(body string) string {
	digest := sha256.Sum256([]byte(body))
	return "p_" + hex.EncodeToString(digest[:6])
}

// writeDataset은 변환 결과를 읽기 좋은 JSON으로 남긴다.
func writeDataset(path string, value any) error {
	raw, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("데이터셋 직렬화: %w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("데이터셋 기록: %w", err)
	}
	return nil
}
