package contract

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"uuid"
)

func manifestTools(t *testing.T) []Tool {
	t.Helper()
	var tools []Tool
	if err := json.Unmarshal(Manifest(), &tools); err != nil {
		t.Fatal(err)
	}
	return tools
}

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := ValidateTools(manifestTools(t))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func testPolicy(t *testing.T, mode string, names ...string) Policy {
	t.Helper()
	policy, err := NewPolicy(mode, names)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestClassificationAndPolicies(t *testing.T) {
	reads, writes, injections := 0, 0, 0
	for _, tool := range manifestTools(t) {
		kind, ok := Classify(tool.Name)
		if !ok {
			t.Fatalf("manifest 도구가 분류되지 않았습니다: %s", tool.Name)
		}
		if kind == Read {
			reads++
		} else if kind == Write {
			writes++
		} else {
			t.Fatalf("잘못된 분류: %s", kind)
		}
		if InjectsAgent(tool.Name) {
			injections++
		}
	}
	if reads != 5 || writes != 8 || injections != 6 {
		t.Fatalf("읽기·쓰기·주입 도구 수 = %d·%d·%d", reads, writes, injections)
	}
	catalog := testCatalog(t)
	for _, tc := range []struct {
		mode string
		list []string
		want int
	}{
		{"", nil, 13}, {"all", nil, 13}, {"read_only", nil, 5},
		{"allowlist", []string{"node_get", "graph_create"}, 2},
	} {
		policy := testPolicy(t, tc.mode, tc.list...)
		tools := catalog.Tools(policy)
		if len(tools) != tc.want {
			t.Fatalf("%s 목록 수 = %d", tc.mode, len(tools))
		}
		for _, tool := range manifestTools(t) {
			listed := slices.ContainsFunc(tools, func(got Tool) bool { return got.Name == tool.Name })
			if listed != policy.Allows(tool.Name) {
				t.Fatalf("%s의 목록·호출 정책이 다릅니다", tool.Name)
			}
			if tc.mode == "read_only" && listed {
				if kind, _ := Classify(tool.Name); kind != Read {
					t.Fatalf("쓰기 도구가 공개되었습니다: %s", tool.Name)
				}
			}
		}
		for _, name := range []string{"cmd/audit", "cmd/eval", "audit", "eval", "unknown"} {
			if _, ok := Classify(name); ok || policy.Allows(name) || InjectsAgent(name) {
				t.Fatalf("계약 밖 도구가 허용되었습니다: %s", name)
			}
		}
	}
	for _, tc := range []struct {
		mode string
		list []string
	}{
		{"other", nil}, {"all", []string{"node_get"}}, {"read_only", []string{"node_get"}},
		{"allowlist", nil}, {"allowlist", []string{""}}, {"allowlist", []string{"audit"}},
		{"allowlist", []string{"node_get", "node_get"}},
	} {
		if _, err := NewPolicy(tc.mode, tc.list); !errors.Is(err, ErrProtocol) {
			t.Fatalf("잘못된 정책을 허용했습니다: %+v", tc)
		}
	}
}

func TestValidateToolsRejectsEntireChangedCatalog(t *testing.T) {
	mutations := map[string]func([]Tool) []Tool{
		"missing":           func(tools []Tool) []Tool { return tools[:12] },
		"extra":             func(tools []Tool) []Tool { return append(tools, Tool{Name: "audit"}) },
		"duplicate":         func(tools []Tool) []Tool { tools[1] = tools[0]; return tools },
		"renamed":           func(tools []Tool) []Tool { tools[0].Name = "other"; return tools },
		"empty_description": func(tools []Tool) []Tool { tools[0].Description = " "; return tools },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			catalog, err := ValidateTools(change(manifestTools(t)))
			if catalog != nil || !errors.Is(err, ErrProtocol) {
				t.Fatalf("부분 목록이 공개되었습니다: %v, %v", catalog, err)
			}
		})
	}
	for _, tc := range []struct{ name, from, to string }{
		{"type", `"type":"integer"`, `"type":"string"`},
		{"required", `"required":[]`, `"required":["cursor"]`},
		{"enum", `"owner","editor","viewer"`, `"owner","editor"`},
		{"length", `"maxLength":200`, `"maxLength":199`},
		{"items", `"maxItems":3`, `"maxItems":4`},
		{"minimum", `"minimum":1`, `"minimum":0`},
		{"precision", `"minimum":1`, `"minimum":1.0000000000000001`},
		{"additional", `"additionalProperties":false`, `"additionalProperties":true`},
		{"unknown_root", `"type":"object"`, `"type":"object","oneOf":[]`},
		{"unknown_nested", `"type":"integer"`, `"type":"integer","maximum":99`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := manifestTools(t)
			tools[0].InputSchema = jsontext.Value(strings.Replace(string(tools[0].InputSchema), tc.from, tc.to, 1))
			catalog, err := ValidateTools(tools)
			if catalog != nil || !errors.Is(err, ErrProtocol) {
				t.Fatalf("변경된 스키마가 공개되었습니다: %v", err)
			}
		})
	}
}

func TestCatalogFingerprintAndIndependentCopies(t *testing.T) {
	first := testCatalog(t)
	tools := manifestTools(t)
	slices.Reverse(tools)
	for i := range tools {
		var schema map[string]any
		if err := json.Unmarshal(tools[i].InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		slices.Reverse(schema["required"].([]any))
		for _, property := range schema["properties"].(map[string]any) {
			object := property.(map[string]any)
			if enum, ok := object["enum"].([]any); ok {
				slices.Reverse(enum)
			}
		}
		raw, err := json.Marshal(schema, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		tools[i].InputSchema = raw
	}
	second, err := ValidateTools(tools)
	if err != nil || first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("순서만 바꾼 계약의 지문이 다릅니다: %v", err)
	}
	tools[0].Description = "원격 서버가 제공하는 첫 정상 설명"
	changed, err := ValidateTools(tools)
	if err != nil || first.Fingerprint() == changed.Fingerprint() {
		t.Fatalf("원격 설명을 지문에 반영하지 않았습니다: %v", err)
	}
	policy := testPolicy(t, "all")
	exposed := first.Tools(policy)
	exposed[0].InputSchema[0] = ' '
	exposed[0].Name = "modified"
	if fresh := first.Tools(policy); fresh[0].Name == "modified" || bytes.Equal(fresh[0].InputSchema, exposed[0].InputSchema) {
		t.Fatal("공개 목록이 내부 계약을 변경했습니다")
	}
}

func TestPrepareAllToolsAndInjectAgent(t *testing.T) {
	const id = "019f0000-0000-7000-8000-000000000001"
	arguments := map[string]string{
		"graph_list": `{}`, "graph_create": `{"name":"그래프"}`,
		"graph_get":        `{"graph_id":"` + id + `"}`,
		"graph_update":     `{"graph_id":"` + id + `","expected_version":1,"name":"그래프"}`,
		"node_create":      `{"graph_id":"` + id + `","layer":"source","body":"본문"}`,
		"node_get":         `{"graph_id":"` + id + `","context_id":"` + id + `","hops":0}`,
		"node_update":      `{"graph_id":"` + id + `","context_id":"` + id + `","expected_version":1}`,
		"node_discard":     `{"graph_id":"` + id + `","context_id":"` + id + `"}`,
		"node_restore":     `{"graph_id":"` + id + `","context_id":"` + id + `"}`,
		"context_flow_get": `{"graph_id":"` + id + `","work_context":"작업"}`,
		"relation_list":    `{"graph_id":"` + id + `","context_id":"` + id + `"}`,
		"relation_confirm": `{"graph_id":"` + id + `","from_context_id":"` + id + `","to_context_id":"` + id + `","relation_type":"relates_to"}`,
		"relation_discard": `{"graph_id":"` + id + `","relation_id":"` + id + `"}`,
	}
	catalog, policy, agent := testCatalog(t), testPolicy(t, "all"), uuid.MustParse(id)
	for _, tool := range catalog.Tools(policy) {
		t.Run(tool.Name, func(t *testing.T) {
			input := jsontext.Value(arguments[tool.Name])
			before := bytes.Clone(input)
			prepared, err := catalog.PrepareArguments(tool.Name, input, policy, agent)
			if err != nil || catalog.ValidateRemoteArguments(tool.Name, prepared) != nil || !bytes.Equal(input, before) {
				t.Fatalf("호출 인자 검증·주입 실패: %v", err)
			}
			var schema map[string]jsontext.Value
			var object map[string]jsontext.Value
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(prepared, &object); err != nil {
				t.Fatal(err)
			}
			if InjectsAgent(tool.Name) {
				if bytes.Contains(schema["properties"], []byte("created_by_agent")) || bytes.Contains(schema["required"], []byte("created_by_agent")) || string(object["created_by_agent"]) != `"`+id+`"` {
					t.Fatal("호스트 스키마 제거·구성값 주입 계약이 다릅니다")
				}
				bypass := jsontext.Value(strings.TrimSuffix(string(input), "}") + `,"created_by_agent":"` + id + `"}`)
				if _, err := catalog.PrepareArguments(tool.Name, bypass, policy, agent); !errors.Is(err, ErrProtocol) {
					t.Fatal("created_by_agent 우회 입력이 허용되었습니다")
				}
				if _, err := catalog.PrepareArguments(tool.Name, input, policy, uuid.UUID{}); !errors.Is(err, ErrProtocol) {
					t.Fatal("잘못된 행위 에이전트 식별자가 허용되었습니다")
				}
			} else if _, ok := object["created_by_agent"]; ok {
				t.Fatal("주입 대상 밖의 도구에 식별자가 추가되었습니다")
			}
		})
	}
}

func TestInputConstraintsAndRawNumbers(t *testing.T) {
	catalog, policy := testCatalog(t), testPolicy(t, "all")
	for _, input := range []string{
		`null`, `[]`, `{"unknown":1}`, `{"page_size":null}`, `{"page_size":"1"}`, `{"page_size":true}`,
		`{"page_size":0}`, `{"page_size":-1}`, `{"page_size":1.01}`, `{"page_size":1.0000000000000001}`,
		`{"page_size":1e-999999999999999999999}`, `{"grade_filter":["admin"]}`,
		`{"grade_filter":[1]}`, `{"grade_filter":["owner","owner","owner","owner"]}`,
		`{"name_filter":"` + strings.Repeat("가", 201) + `"}`,
		`{"page_size":1,"page_size":2}`,
	} {
		if _, err := catalog.PrepareArguments("graph_list", jsontext.Value(input), policy, uuid.UUID{}); !errors.Is(err, ErrProtocol) {
			t.Fatalf("제약 위반 입력을 허용했습니다: %.80s", input)
		}
	}
	for _, number := range []string{"1", "1.0", "10e-1", "9007199254740993", "18446744073709551616", "1e999999999999999999999"} {
		input := jsontext.Value(`{"page_size":` + number + `}`)
		prepared, err := catalog.PrepareArguments("graph_list", input, policy, uuid.UUID{})
		if err != nil || !bytes.Equal(input, prepared) {
			t.Fatalf("유효한 정수의 원문이 바뀌었습니다: %s, %v", number, err)
		}
	}
	for _, input := range []string{`{}`, `{"grade_filter":[]}`, `{"name_filter":"` + strings.Repeat("가", 200) + `"}`} {
		if _, err := catalog.PrepareArguments("graph_list", jsontext.Value(input), policy, uuid.UUID{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := catalog.PrepareArguments("graph_create", jsontext.Value(`{"name":""}`), policy, uuid.UUID{}); !errors.Is(err, ErrProtocol) {
		t.Fatal("빈 필수 문자열을 허용했습니다")
	}
	if _, err := catalog.PrepareArguments("graph_get", jsontext.Value(`{}`), policy, uuid.UUID{}); !errors.Is(err, ErrProtocol) {
		t.Fatal("필수 필드 누락을 허용했습니다")
	}
	if _, err := catalog.PrepareArguments("graph_create", jsontext.Value(`{"name":"그래프"}`), testPolicy(t, "read_only"), uuid.UUID{}); !errors.Is(err, ErrProtocol) {
		t.Fatal("정책 밖 입력을 허용했습니다")
	}
}

func TestInputFormatsAndArrayBounds(t *testing.T) {
	catalog, policy := testCatalog(t), testPolicy(t, "all")
	agent := uuid.MustParse("019f0000-0000-7000-8000-000000000001")
	base := map[string]any{"graph_id": agent.String(), "layer": "derived", "body": "본문"}
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"graph_id", "not-a-uuid", false}, {"graph_id", "urn:uuid:" + agent.String(), false},
		{"occurred_at", "2026-10-03T12:00:00+09:00", true}, {"occurred_at", "2026-10-03", false},
		{"locator", "https://example.test/context", true}, {"locator", "relative/file", false},
		{"derived_from", []string{}, false}, {"derived_from", []string{agent.String()}, true},
		{"derived_from", slices.Repeat([]string{agent.String()}, 100), true},
		{"derived_from", slices.Repeat([]string{agent.String()}, 101), false},
		{"member_refs", slices.Repeat([]string{agent.String()}, 1000), true},
		{"member_refs", slices.Repeat([]string{agent.String()}, 1001), false},
	} {
		t.Run(tc.name+"/"+strconv.FormatBool(tc.valid), func(t *testing.T) {
			input := maps.Clone(base)
			input[tc.name] = tc.value
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			_, err = catalog.PrepareArguments("node_create", raw, policy, agent)
			if (err == nil) != tc.valid {
				t.Fatalf("입력 형식·배열 제약 판정 = %v, 기대 유효성 %v", err, tc.valid)
			}
		})
	}
}

func TestResultPreservesRawContent(t *testing.T) {
	raw := jsontext.Value(`{ "structuredContent":{"contexts":[{"version":9007199254740993}],"references":[],"relations":[],"entry_points":[2,1],"budget":{"used":1},"channels":{"failed":["semantic"]},"truncated":true,"cursor":"opaque+/=","status":"result_truncated"},"content":[{"type":"text","text":"본문"}],"isError":false,"future":{"x":1.00} }`)
	before := bytes.Clone(raw)
	result, err := NewResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = ' '
	if !bytes.Equal(result.Raw(), before) {
		t.Fatal("결과의 숫자·순서·커서·부분 상태가 바뀌었습니다")
	}
	copyOfResult := result.Raw()
	copyOfResult[0] = ' '
	if !bytes.Equal(result.Raw(), before) {
		t.Fatal("결과의 내부 저장소가 노출되었습니다")
	}
	for _, invalid := range []string{"null", "[]", "true", `{`, `{"a":1,"a":2}`} {
		if _, err := NewResult(jsontext.Value(invalid)); !errors.Is(err, ErrProtocol) {
			t.Fatalf("잘못된 결과를 허용했습니다: %s", invalid)
		}
	}
}
