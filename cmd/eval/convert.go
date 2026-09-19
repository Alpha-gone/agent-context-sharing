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
