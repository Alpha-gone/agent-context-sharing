package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolDefinition struct {
	tool       *sdk.Tool
	resolved   *jsonschema.Resolved
	properties map[string]*jsonschema.Resolved
}

// toolCatalog의 스키마는 생성 후 읽기 전용이다. 서버 목록과 입력 검증이 같은
// 정의를 사용하며, 상태·등급·행위자 결합 판정은 주입한 업무 처리기의 책임이다.
type toolCatalog struct {
	definitions []toolDefinition
}

func newToolCatalog() (*toolCatalog, error) {
	catalog := &toolCatalog{}
	for _, tool := range toolDefinitions() {
		schema := tool.InputSchema.(*jsonschema.Schema)
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("%s 입력 스키마: %w", tool.Name, err)
		}
		definition := toolDefinition{tool: tool, resolved: resolved, properties: map[string]*jsonschema.Resolved{}}
		for name, property := range schema.Properties {
			propertySchema, err := property.Resolve(nil)
			if err != nil {
				return nil, fmt.Errorf("%s.%s 입력 스키마: %w", tool.Name, name, err)
			}
			definition.properties[name] = propertySchema
		}
		catalog.definitions = append(catalog.definitions, definition)
	}
	return catalog, nil
}

func (catalog *toolCatalog) validate(name string, arguments []byte) (map[string]any, error) {
	for _, definition := range catalog.definitions {
		if definition.tool.Name == name {
			return definition.validate(arguments)
		}
	}
	return nil, inputFieldError("name")
}

// register는 모든 업무 처리기가 준비됐는지 먼저 검사한다. 임시 성공 처리기로
// 미구현 도구를 노출하지 않으며 인증 경로나 업무 구현을 여기서 만들지 않는다.
func (catalog *toolCatalog) register(server *sdk.Server, handlers map[string]sdk.ToolHandler, observer diagnostic.Observer) error {
	if server == nil || len(handlers) != len(catalog.definitions) {
		return fmt.Errorf("14종 업무 처리기가 필요하다")
	}
	for _, definition := range catalog.definitions {
		if handlers[definition.tool.Name] == nil {
			return fmt.Errorf("%s 업무 처리기가 필요하다", definition.tool.Name)
		}
	}
	for _, definition := range catalog.definitions {
		handler := handlers[definition.tool.Name]
		tool := *definition.tool
		tool.InputSchema = tool.InputSchema.(*jsonschema.Schema).CloneSchemas()
		server.AddTool(&tool, func(ctx context.Context, request *sdk.CallToolRequest) (result *sdk.CallToolResult, err error) {
			// SDK는 HTTP 복구 경계 밖의 goroutine에서 호출한다. panic 값은 보관하지 않는다.
			defer func() {
				if recover() != nil {
					result, err = writeErrorResult(&DependencyFailure{})
					// 진단 의존의 추가 panic도 이미 정한 internal 결과를 바꾸지 않는다.
					defer func() { _ = recover() }()
					diagnostic.Observe(observer, ctx, "ERROR", "response", "failed", "handler_panic", false,
						diagnostic.Fields{Request: diagnostic.RequestFields{Route: "mcp", Tool: tool.Name}})
				}
			}()
			if _, err := definition.validate(request.Params.Arguments); err != nil {
				field := "arguments"
				if failure, ok := errors.AsType[model.FieldError](err); ok {
					field = failure.Field
				}
				// 공급자 검증 오류는 입력 원문을 포함할 수 있으므로 고정 메시지와 필드만 노출한다.
				value := map[string]any{"code": "invalid_argument", "message": "도구 입력을 확인해 주십시오.", "field": field}
				text, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				return &sdk.CallToolResult{IsError: true, StructuredContent: value, Content: []sdk.Content{&sdk.TextContent{Text: string(text)}}}, nil
			}
			return handler(ctx, request)
		})
	}
	return nil
}

func (definition toolDefinition) validate(arguments []byte) (map[string]any, error) {
	var raw map[string]jsontext.Value
	if len(arguments) == 0 {
		arguments = []byte("{}")
	}
	if json.Unmarshal(arguments, &raw) != nil || raw == nil {
		return nil, inputFieldError("arguments")
	}
	schema := definition.tool.InputSchema.(*jsonschema.Schema)
	for _, name := range schema.Required {
		if _, present := raw[name]; !present {
			return nil, inputFieldError(name)
		}
	}
	values := make(map[string]any, len(raw))
	// 여러 필드가 잘못된 경우에도 같은 필드부터 안내한다.
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		property, known := definition.properties[name]
		if !known {
			return nil, inputFieldError(name)
		}
		var value any
		if schema.Properties[name].Type == "integer" {
			integer, ok := parseToolInteger(raw[name])
			if !ok {
				return nil, inputFieldError(name)
			}
			value = integer
		} else if json.Unmarshal(raw[name], &value) != nil {
			return nil, inputFieldError(name)
		}
		if property.Validate(value) != nil {
			return nil, inputFieldError(name)
		}
		if err := validateInputFormats(name, value, schema.Properties[name]); err != nil {
			return nil, err
		}
		values[name] = value
	}
	if definition.resolved.Validate(values) != nil {
		// 조건부 필수 필드도 공개 스키마에서 찾아 안내한다.
		for _, condition := range schema.AllOf {
			if condition.If == nil || condition.Then == nil {
				continue
			}
			predicate, err := condition.If.Resolve(nil)
			if err != nil || predicate.Validate(values) != nil {
				continue
			}
			for _, name := range condition.Then.Required {
				if _, present := values[name]; !present {
					return nil, inputFieldError(name)
				}
			}
		}
		if definition.tool.Name == "node_create" {
			return nil, inputFieldError("layer")
		}
		if definition.tool.Name == "node_update" {
			return nil, inputFieldError("management_action")
		}
		return nil, inputFieldError("arguments")
	}
	return values, nil
}

// JSON Schema의 정수는 1.0·1e3도 포함한다. float64를 거치지 않고 판 번호를
// 보존하며, 지수가 커도 원문 길이 밖으로 0을 무제한 할당하지 않는다.
func parseToolInteger(raw jsontext.Value) (int64, bool) {
	if raw.Kind() != '0' {
		return 0, false
	}
	text := string(raw)
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	mantissa, exponentText, found := strings.Cut(text, "e")
	if !found {
		mantissa, exponentText, found = strings.Cut(text, "E")
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return 0, true
	}
	var exponent int64
	if found {
		var err error
		exponent, err = strconv.ParseInt(exponentText, 10, 64)
		if err != nil {
			return 0, false
		}
	}
	if exponent > int64(len(fraction))+19 || exponent < int64(len(fraction))-int64(len(digits)) {
		return 0, false
	}
	scale := exponent - int64(len(fraction))
	if scale < 0 {
		cut := len(digits) + int(scale)
		if strings.Trim(digits[cut:], "0") != "" {
			return 0, false
		}
		digits, scale = digits[:cut], 0
	}
	if int64(len(digits))+scale > 19 {
		return 0, false
	}
	digits += strings.Repeat("0", int(scale))
	if negative {
		digits = "-" + digits
	}
	value, err := strconv.ParseInt(digits, 10, 64)
	return value, err == nil
}

// 적용 판 라이브러리에서 format은 주석이므로 외부 I/O 없이 보완한다.
func validateInputFormats(field string, value any, schema *jsonschema.Schema) error {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		switch schema.Format {
		case "uuid-v7":
			if _, err := model.ParseID(text); err != nil {
				return inputFieldError(field)
			}
		case "date-time":
			parsed, err := time.Parse(time.RFC3339Nano, text)
			if err != nil || parsed.IsZero() {
				return inputFieldError(field)
			}
			_, offset := parsed.Zone()
			if offset != 0 {
				return inputFieldError(field)
			}
		case "uri":
			parsed, err := url.Parse(text)
			if err != nil || parsed.Scheme == "" {
				return inputFieldError(field)
			}
		case "base64url":
			// 내부 값의 구조는 커서를 소유한 저장 계층에서 재검증한다.
			if _, err := base64.RawURLEncoding.DecodeString(text); err != nil {
				return inputFieldError(field)
			}
		}
	}
	if object, ok := value.(map[string]any); ok {
		for _, name := range slices.Sorted(maps.Keys(object)) {
			if property := schema.Properties[name]; property != nil {
				if err := validateInputFormats(field+"."+name, object[name], property); err != nil {
					return err
				}
			}
		}
	}
	if array, ok := value.([]any); ok && schema.Items != nil {
		for _, item := range array {
			if err := validateInputFormats(field, item, schema.Items); err != nil {
				return err
			}
		}
	}
	return nil
}

func inputFieldError(field string) error {
	return model.FieldError{Field: field, Message: "도구 입력의 형식·길이·필수 조건을 확인해 주십시오."}
}
