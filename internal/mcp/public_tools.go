package mcp

import (
	"encoding/json/v2"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// publicTool은 모델용 메타데이터를 엄격 검증 정의와 분리한다.
// JSON 왕복으로 CloneSchemas가 공유하는 일반 slice·map도 독립시킨다.
func publicTool(original *sdk.Tool) (*sdk.Tool, error) {
	data, err := json.Marshal(original)
	if err != nil {
		return nil, fmt.Errorf("공개 도구 정의 직렬화: %w", err)
	}
	var published sdk.Tool
	if err := json.Unmarshal(data, &published); err != nil {
		return nil, fmt.Errorf("공개 도구 정의 복사: %w", err)
	}
	schema, ok := published.InputSchema.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("공개 도구 입력은 객체 스키마여야 한다")
	}
	compactPublicSchema(schema)
	// Claude API는 루트 allOf를 받지 않는다. 조건은 설명과 내부 검증에 유지한다.
	delete(schema, "allOf")
	return &published, nil
}

// 스키마 값을 가진 키만 순회한다. const·enum·examples는 데이터다.
func compactPublicSchema(schema map[string]any) {
	delete(schema, "pattern")
	for _, key := range []string{"minLength", "minItems"} {
		if schema[key] == float64(0) {
			delete(schema, key)
		}
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"} {
		children, _ := schema[key].(map[string]any)
		for _, child := range children {
			if nested, ok := child.(map[string]any); ok {
				compactPublicSchema(nested)
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		children, _ := schema[key].([]any)
		for _, child := range children {
			if nested, ok := child.(map[string]any); ok {
				compactPublicSchema(nested)
			}
		}
	}
	for _, key := range []string{"items", "additionalItems", "contains", "unevaluatedItems", "additionalProperties", "propertyNames", "unevaluatedProperties", "not", "if", "then", "else", "contentSchema"} {
		if nested, ok := schema[key].(map[string]any); ok {
			compactPublicSchema(nested)
		}
	}
	// Draft-07의 items는 tuple 스키마 배열도 허용한다.
	if children, ok := schema["items"].([]any); ok {
		for _, child := range children {
			if nested, ok := child.(map[string]any); ok {
				compactPublicSchema(nested)
			}
		}
	}
	// 빈 스키마는 {} 또는 true로 직렬화된다. 다른 조합식의 참 분기는 유지한다.
	if terms, ok := schema["allOf"].([]any); ok {
		kept := terms[:0]
		for _, term := range terms {
			if term == true {
				continue
			}
			if nested, ok := term.(map[string]any); ok && len(nested) == 0 {
				continue
			}
			kept = append(kept, term)
		}
		if len(kept) == 0 {
			delete(schema, "allOf")
		} else {
			schema["allOf"] = kept
		}
	}
}

func (catalog *toolCatalog) publicTools() ([]*sdk.Tool, error) {
	tools := make([]*sdk.Tool, 0, len(catalog.definitions))
	for _, definition := range catalog.definitions {
		tool, err := publicTool(definition.tool)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}
