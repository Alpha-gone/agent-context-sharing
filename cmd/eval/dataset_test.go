package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile은 검사할 데이터셋을 임시 파일로 남긴다.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("데이터셋 기록: %v", err)
	}
	return path
}

const validContexts = `{
  "version": "test-1",
  "contexts": [
    {"key": "s1", "layer": "source", "body": "원천 본문", "source": {"channel": "conversation", "locator": "urn:test:1", "occurred_at": "2026-09-01T00:00:00Z", "origin_kind": "user_utterance"}},
    {"key": "d1", "layer": "derived", "body": "파생 본문", "derived": {"kind": "proposition", "derived_from": ["s1"], "evidence_state": "observation"}},
    {"key": "e1", "layer": "event", "body": "사건 본문", "event": {"members": ["s1", "d1"], "start": "2026-09-01T00:00:00Z"}}
  ]
}`

func TestLoadContextSet(t *testing.T) {
	set, err := loadContextSet(writeFile(t, "contexts.json", validContexts))
	if err != nil {
		t.Fatalf("컨텍스트 집합 읽기: %v", err)
	}
	if set.Version != "test-1" || len(set.Contexts) != 3 {
		t.Fatalf("판 또는 개수가 다르다: %q %d", set.Version, len(set.Contexts))
	}
}

func TestLoadContextSetRejects(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"중복 key": {
			body: `{"version":"v","contexts":[
				{"key":"s1","layer":"source","body":"a","source":{"channel":"conversation","locator":"urn:test:1","occurred_at":"2026-09-01T00:00:00Z","origin_kind":"user_utterance"}},
				{"key":"s1","layer":"source","body":"b","source":{"channel":"conversation","locator":"urn:test:2","occurred_at":"2026-09-01T00:00:00Z","origin_kind":"user_utterance"}}]}`,
			want: "중복된다",
		},
		"없는 근거": {
			body: `{"version":"v","contexts":[
				{"key":"d1","layer":"derived","body":"a","derived":{"kind":"proposition","derived_from":["없음"],"evidence_state":"observation"}}]}`,
			want: "가리킨다",
		},
		"계층 속성 둘": {
			body: `{"version":"v","contexts":[
				{"key":"x","layer":"source","body":"a",
				 "source":{"channel":"conversation","locator":"urn:test:1","occurred_at":"2026-09-01T00:00:00Z","origin_kind":"user_utterance"},
				 "event":{"members":[],"start":"2026-09-01T00:00:00Z"}}]}`,
			want: "하나만",
		},
		"판 없음": {
			body: `{"contexts":[{"key":"s1","layer":"source","body":"a","source":{"channel":"conversation","locator":"urn:test:1","occurred_at":"2026-09-01T00:00:00Z","origin_kind":"user_utterance"}}]}`,
			want: "데이터셋 판",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadContextSet(writeFile(t, "contexts.json", testCase.body))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("%q를 담은 오류를 기대했으나 %v", testCase.want, err)
			}
		})
	}
}

func TestLoadQuerySet(t *testing.T) {
	contexts, err := loadContextSet(writeFile(t, "contexts.json", validContexts))
	if err != nil {
		t.Fatalf("컨텍스트 집합 읽기: %v", err)
	}
	body := `{"version":"q-1","queries":[{"id":"q1","use_case":"fact","work_context":"질의","answers":["s1"]}]}`
	set, err := loadQuerySet(writeFile(t, "queries.json", body), contexts)
	if err != nil {
		t.Fatalf("질의 집합 읽기: %v", err)
	}
	if len(set.Queries) != 1 || set.Queries[0].UseCase != useCaseFact {
		t.Fatalf("질의 해석이 다르다: %+v", set.Queries)
	}

	unknownAnswer := `{"version":"q-1","queries":[{"id":"q1","use_case":"fact","work_context":"질의","answers":["없음"]}]}`
	if _, err := loadQuerySet(writeFile(t, "bad.json", unknownAnswer), contexts); err == nil {
		t.Fatal("컨텍스트 집합에 없는 정답을 받아들였다")
	}
	unknownUseCase := `{"version":"q-1","queries":[{"id":"q1","use_case":"other","work_context":"질의","answers":["s1"]}]}`
	if _, err := loadQuerySet(writeFile(t, "case.json", unknownUseCase), contexts); err == nil {
		t.Fatal("알 수 없는 사용 사례를 받아들였다")
	}
}
