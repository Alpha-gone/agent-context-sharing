package main

import (
	"slices"
	"strings"
	"testing"
)

// hippoFixture는 HippoRAG 2 재현 세트의 구조를 그대로 줄인 것이다. 문단 본문 필드
// 이름이 세트마다 `text`와 `paragraph_text`로 갈리므로 둘을 함께 담는다.
const hippoFixture = `[
 {
  "id": "q1",
  "question": "Messi가 비교된 선수는 언제 바르셀로나와 계약했는가?",
  "answerable": true,
  "paragraphs": [
   {"idx": 0, "title": "Lionel Messi", "paragraph_text": "메시는 라 마시아에서 성장했다.", "is_supporting": false},
   {"idx": 1, "title": "Copa del Rey", "paragraph_text": "메시의 골은 마라도나의 골에 비교됐다.", "is_supporting": true},
   {"idx": 2, "title": "Diego Maradona", "paragraph_text": "마라도나는 1982년 6월 바르셀로나와 계약했다.", "is_supporting": true}
  ],
  "question_decomposition": [
   {"question": "메시의 골은 누구의 골에 비교됐는가?", "answer": "마라도나", "paragraph_support_idx": 1},
   {"question": "#1은 언제 바르셀로나와 계약했는가?", "answer": "1982년 6월", "paragraph_support_idx": 2}
  ]
 },
 {
  "id": "q2",
  "question": "답이 없는 질의",
  "answerable": false,
  "paragraphs": [
   {"idx": 0, "title": "Unused", "text": "쓰이지 않는 문단이다.", "is_supporting": true}
  ],
  "question_decomposition": []
 },
 {
  "id": "q3",
  "question": "근거가 하나인 질의",
  "answerable": true,
  "paragraphs": [
   {"idx": 0, "title": "Diego Maradona", "paragraph_text": "마라도나는 1982년 6월 바르셀로나와 계약했다.", "is_supporting": true}
  ],
  "question_decomposition": []
 }
]`

func TestConvertHippoRAGSet(t *testing.T) {
	path := writeFile(t, "musique.json", hippoFixture)
	contexts, queries, err := convertHippoRAGSet(path, "musique", 0)
	if err != nil {
		t.Fatalf("변환: %v", err)
	}

	// 같은 본문은 질의가 달라도 원천 하나로 합쳐진다. q1의 마라도나 문단과 q3의 문단이
	// 같으므로 넷이 아니라 셋이다.
	if len(contexts.Contexts) != 3 {
		t.Fatalf("원천 수 = %d; 중복 본문이 합쳐져 3이어야 한다", len(contexts.Contexts))
	}
	if contexts.Version != "hipporag2-musique-3q" {
		t.Fatalf("데이터셋 판 = %q", contexts.Version)
	}
	for _, spec := range contexts.Contexts {
		if spec.Layer != "source" || spec.Source == nil {
			t.Fatalf("모든 컨텍스트가 원천이어야 한다: %+v", spec)
		}
		// 외부에서 가져온 내용이므로 출처 구분이 external_content여야 한다.
		if spec.Source.OriginKind != "external_content" {
			t.Fatalf("출처 구분 = %q", spec.Source.OriginKind)
		}
		if !strings.Contains(spec.Body, "\n\n") {
			t.Fatalf("본문에 제목이 붙어야 한다: %q", spec.Body)
		}
	}

	ids := make([]string, 0, len(queries.Queries))
	for _, query := range queries.Queries {
		ids = append(ids, query.ID)
	}
	// q2는 답이 없다고 표시되어 빠지고, q1의 두 번째 하위 질의는 `#1` 자리 표시가 있어
	// 빠진다. 남는 것은 q1 전체, q1의 첫 하위 질의, q3 전체 셋이다.
	want := []string{"q1", "q1#hop1", "q3"}
	if !slices.Equal(ids, want) {
		t.Fatalf("질의 목록 = %v; %v여야 한다", ids, want)
	}

	byID := map[string]querySpec{}
	for _, query := range queries.Queries {
		byID[query.ID] = query
	}
	if byID["q1"].UseCase != useCaseAssociative || len(byID["q1"].Answers) != 2 {
		t.Fatalf("근거가 둘인 질의는 연상 검색이어야 한다: %+v", byID["q1"])
	}
	if byID["q1#hop1"].UseCase != useCaseFact || len(byID["q1#hop1"].Answers) != 1 {
		t.Fatalf("자립형 하위 질의는 정답 하나의 사실 검색이어야 한다: %+v", byID["q1#hop1"])
	}
	if byID["q3"].UseCase != useCaseFact {
		t.Fatalf("근거가 하나인 질의는 사실 검색이어야 한다: %+v", byID["q3"])
	}
	// 정답은 컨텍스트 집합 안을 가리켜야 한다. 적재 전 검증이 보는 것과 같은 조건이다.
	keys := map[string]struct{}{}
	for _, spec := range contexts.Contexts {
		keys[spec.Key] = struct{}{}
	}
	for _, query := range queries.Queries {
		for _, answer := range query.Answers {
			if _, known := keys[answer]; !known {
				t.Fatalf("질의 %q의 정답 %q가 컨텍스트 집합에 없다", query.ID, answer)
			}
		}
	}
}

// 질의 수를 줄이면 그만큼만 변환해야 한다. 시험 규모로 먼저 재는 경로가 이 인자다.
func TestConvertHippoRAGSetLimitsQuestions(t *testing.T) {
	path := writeFile(t, "musique.json", hippoFixture)
	_, queries, err := convertHippoRAGSet(path, "musique", 1)
	if err != nil {
		t.Fatalf("변환: %v", err)
	}
	for _, query := range queries.Queries {
		if !strings.HasPrefix(query.ID, "q1") {
			t.Fatalf("첫 질의만 남아야 한다: %q", query.ID)
		}
	}
}

// 본문이 다른데 제목이 같은 문단은 서로 다른 원천이어야 한다. 제목을 키로 쓰면 둘이
// 하나로 합쳐져 정답이 엉뚱한 문단을 가리킨다.
func TestContextKeyDistinguishesSameTitle(t *testing.T) {
	left := contextKey("같은 제목\n\n본문 하나")
	right := contextKey("같은 제목\n\n본문 둘")
	if left == right {
		t.Fatalf("본문이 다르면 키가 달라야 한다: %q", left)
	}
}
