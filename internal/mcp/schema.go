package mcp

import (
	"math"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
)

const (
	maxNameRunes        = 200
	maxDescriptionRunes = 2_000
	maxBodyRunes        = 8_000
	maxReferences       = 100
	maxMembers          = 1_000
)

// argumentError는 MCP 입력 계층에서 거절할 인자의 이름만 노출한다.
type argumentError struct{ Field string }

// Error는 상세 입력값을 포함하지 않는 오류 메시지를 만든다.
func (error argumentError) Error() string { return "invalid MCP tool argument" }

// validateToolCall은 tools/call에 허용한 도구와 연산 표면의 형식·상한을 확인한다.
// 계층별 필수 속성과 속성 사이의 규칙은 model 패키지가 맡는다.
func validateToolCall(name string, arguments map[string]any) *argumentError {
	if _, ok := toolSchemas[name]; !ok {
		return &argumentError{Field: "name"}
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	for key := range arguments {
		if _, ok := toolSchemas[name][key]; !ok {
			return &argumentError{Field: key}
		}
	}
	for key, rule := range toolSchemas[name] {
		value, present := arguments[key]
		if rule.required && !present {
			return &argumentError{Field: key}
		}
		if !present {
			continue
		}
		if !rule.valid(value) {
			return &argumentError{Field: key}
		}
	}
	return nil
}

type argumentRule struct {
	required bool
	valid    func(any) bool
}

var toolSchemas = map[string]map[string]argumentRule{
	"graph_list": {
		"name_filter":  optional(textAtMost(maxNameRunes)),
		"grade_filter": optional(stringArray("owner", "editor", "viewer")),
		"cursor":       optional(stringValue),
		"page_size":    optional(positiveInteger),
	},
	"graph_create": {
		"name":        required(textRange(1, maxNameRunes)),
		"description": optional(textAtMost(maxDescriptionRunes)),
	},
	"graph_get": graphIDRules(),
	"graph_update": {
		"graph_id":         required(idString),
		"expected_version": required(positiveInteger),
		"name":             required(textRange(1, maxNameRunes)),
		"description":      optional(textAtMost(maxDescriptionRunes)),
	},
	"node_create": {
		"graph_id":         required(idString),
		"created_by_agent": required(idString),
		"layer":            required(oneOf("source", "derived", "event")),
		"body":             required(textRange(1, maxBodyRunes)),
		"source_channel":   optional(oneOf("conversation", "file", "web", "api", "other")),
		"locator":          optional(uriString),
		"occurred_at":      optional(dateTimeString),
		"origin_kind":      optional(oneOf("user_utterance", "agent_output", "external_content")),
		"derivation_kind":  optional(oneOf("proposition", "summary", "reflection")),
		"summary_scope":    optional(oneOf("local", "global")),
		"evidence_state":   optional(oneOf("observation", "experience", "opinion")),
		"confidence_state": optional(oneOf("supported", "uncertain", "disputed")),
		"valid_from":       optional(dateTimeString),
		"valid_to":         optional(dateTimeString),
		"derived_from":     optional(idArray(maxReferences)),
		"member_refs":      optional(idArray(maxMembers)),
		"start":            optional(dateTimeString),
		"end":              optional(dateTimeString),
	},
	"node_get": {
		"graph_id":         required(idString),
		"context_id":       required(idString),
		"hops":             required(nonNegativeInteger),
		"direction":        optional(oneOf("in", "out", "both")),
		"traversal_filter": optional(stringArray("derived_from", "has_member", "supersedes", "precedes", "causes", "part_of", "relates_to")),
	},
	"node_update": {
		"graph_id":         required(idString),
		"context_id":       required(idString),
		"expected_version": required(positiveInteger),
		"created_by_agent": required(idString),
		"body":             optional(textRange(1, maxBodyRunes)),
		"confidence_state": optional(oneOf("supported", "uncertain", "disputed")),
		"valid_from":       optional(dateTimeString),
		"valid_to":         optional(dateTimeString),
		"member_refs":      optional(idArray(maxMembers)),
		"start":            optional(dateTimeString),
		"end":              optional(dateTimeString),
	},
	"node_discard": {
		"graph_id":         required(idString),
		"context_id":       required(idString),
		"created_by_agent": required(idString),
	},
	"node_restore": {
		"graph_id":         required(idString),
		"context_id":       required(idString),
		"created_by_agent": required(idString),
	},
	"context_flow_get": {
		"graph_id":     required(idString),
		"work_context": required(nonBlankText(maxBodyRunes)),
		"as_of":        optional(dateTimeString),
		"scope":        optional(oneOf("auto", "local", "global")),
		"budget":       optional(positiveInteger),
	},
	"relation_list": {
		"graph_id":     required(idString),
		"context_id":   required(idString),
		"state_filter": optional(stringArray("proposed", "confirmed", "discarded")),
		"type_filter":  optional(stringArray("precedes", "causes", "part_of", "relates_to")),
		"cursor":       optional(stringValue),
		"page_size":    optional(positiveInteger),
	},
	"relation_confirm": {
		"graph_id":        required(idString),
		"relation_type":   required(oneOf("precedes", "causes", "part_of", "relates_to")),
		"from_context_id": required(idString),
		"to_context_id":   required(idString),
	},
	"relation_discard": {
		"graph_id":    required(idString),
		"relation_id": required(idString),
	},
}

func required(valid func(any) bool) argumentRule { return argumentRule{required: true, valid: valid} }

func optional(valid func(any) bool) argumentRule { return argumentRule{valid: valid} }

func graphIDRules() map[string]argumentRule {
	return map[string]argumentRule{"graph_id": required(idString)}
}

func contextIDRules() map[string]argumentRule {
	return map[string]argumentRule{"graph_id": required(idString), "context_id": required(idString)}
}

func textRange(minimum, maximum int) func(any) bool {
	return func(value any) bool {
		text, ok := value.(string)
		length := utf8.RuneCountInString(text)
		return ok && minimum <= length && length <= maximum
	}
}

func textAtMost(maximum int) func(any) bool { return textRange(0, maximum) }

func nonBlankText(maximum int) func(any) bool {
	return func(value any) bool {
		text, ok := value.(string)
		return ok && strings.TrimSpace(text) != "" && utf8.RuneCountInString(text) <= maximum
	}
}

func oneOf(values ...string) func(any) bool {
	return func(value any) bool {
		text, ok := value.(string)
		for _, allowed := range values {
			if ok && text == allowed {
				return true
			}
		}
		return false
	}
}

func stringValue(value any) bool { _, ok := value.(string); return ok }

func dateTimeString(value any) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	_, err := time.Parse(time.RFC3339, text)
	return err == nil
}

func idString(value any) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	id, err := model.ParseID(text)
	return err == nil && id.IsV7()
}

func uriString(value any) bool {
	text, ok := value.(string)
	if !ok || utf8.RuneCountInString(text) == 0 || utf8.RuneCountInString(text) > 1_024 {
		return false
	}
	parsed, err := url.Parse(text)
	return err == nil && parsed.Scheme != ""
}

func positiveInteger(value any) bool { return integer(value, 1) }

func nonNegativeInteger(value any) bool { return integer(value, 0) }

func integer(value any, minimum float64) bool {
	number, ok := value.(float64)
	return ok && number >= minimum && number <= math.MaxInt && math.Trunc(number) == number
}

func stringArray(values ...string) func(any) bool {
	return func(value any) bool {
		items, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if !oneOf(values...)(item) {
				return false
			}
		}
		return true
	}
}

func idArray(maximum int) func(any) bool {
	return func(value any) bool {
		items, ok := value.([]any)
		if !ok || len(items) > maximum {
			return false
		}
		for _, item := range items {
			if !idString(item) {
				return false
			}
		}
		return true
	}
}

func schema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func stringSchema(extra map[string]any) map[string]any {
	schema := map[string]any{"type": "string"}
	for key, value := range extra {
		schema[key] = value
	}
	return schema
}

func integerSchema(minimum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum}
}

func arraySchema(items map[string]any, minimum, maximum int) map[string]any {
	return map[string]any{"type": "array", "items": items, "minItems": minimum, "maxItems": maximum}
}

func graphIDSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema()}, "graph_id")
}

func contextIDSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "context_id": idSchema()}, "graph_id", "context_id")
}

func nodeLifecycleSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "context_id": idSchema(), "created_by_agent": idSchema()}, "graph_id", "context_id", "created_by_agent")
}

func graphListSchema() map[string]any {
	return schema(map[string]any{
		"name_filter":  stringSchema(map[string]any{"maxLength": maxNameRunes}),
		"grade_filter": arraySchema(map[string]any{"type": "string", "enum": []string{"owner", "editor", "viewer"}}, 0, 3),
		"cursor":       stringSchema(nil), "page_size": integerSchema(1),
	})
}

func graphCreateSchema() map[string]any {
	return schema(map[string]any{"name": stringSchema(map[string]any{"minLength": 1, "maxLength": maxNameRunes}), "description": stringSchema(map[string]any{"maxLength": maxDescriptionRunes})}, "name")
}

func graphUpdateSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "expected_version": integerSchema(1), "name": stringSchema(map[string]any{"minLength": 1, "maxLength": maxNameRunes}), "description": stringSchema(map[string]any{"maxLength": maxDescriptionRunes})}, "graph_id", "expected_version", "name")
}

func nodeCreateSchema() map[string]any {
	return schema(map[string]any{
		"graph_id": idSchema(), "created_by_agent": idSchema(), "layer": enumSchema("source", "derived", "event"),
		"body": stringSchema(map[string]any{"minLength": 1, "maxLength": maxBodyRunes}), "source_channel": enumSchema("conversation", "file", "web", "api", "other"),
		"locator": stringSchema(map[string]any{"minLength": 1, "maxLength": 1_024, "format": "uri"}), "occurred_at": dateTimeSchema(), "origin_kind": enumSchema("user_utterance", "agent_output", "external_content"),
		"derivation_kind": enumSchema("proposition", "summary", "reflection"), "summary_scope": enumSchema("local", "global"), "evidence_state": enumSchema("observation", "experience", "opinion"), "confidence_state": enumSchema("supported", "uncertain", "disputed"),
		"valid_from": dateTimeSchema(), "valid_to": dateTimeSchema(), "derived_from": arraySchema(idSchema(), 1, maxReferences), "member_refs": arraySchema(idSchema(), 1, maxMembers), "start": dateTimeSchema(), "end": dateTimeSchema(),
	}, "graph_id", "created_by_agent", "layer", "body")
}

func nodeGetSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "context_id": idSchema(), "hops": integerSchema(0), "direction": enumSchema("in", "out", "both"), "traversal_filter": arraySchema(enumSchema("derived_from", "has_member", "supersedes", "precedes", "causes", "part_of", "relates_to"), 0, 7)}, "graph_id", "context_id", "hops")
}

func nodeUpdateSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "context_id": idSchema(), "expected_version": integerSchema(1), "created_by_agent": idSchema(), "body": stringSchema(map[string]any{"minLength": 1, "maxLength": maxBodyRunes}), "confidence_state": enumSchema("supported", "uncertain", "disputed"), "valid_from": dateTimeSchema(), "valid_to": dateTimeSchema(), "member_refs": arraySchema(idSchema(), 1, maxMembers), "start": dateTimeSchema(), "end": dateTimeSchema()}, "graph_id", "context_id", "expected_version", "created_by_agent")
}

func contextFlowSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "work_context": stringSchema(map[string]any{"minLength": 1, "maxLength": maxBodyRunes}), "as_of": dateTimeSchema(), "scope": enumSchema("auto", "local", "global"), "budget": integerSchema(1)}, "graph_id", "work_context")
}

func relationListSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "context_id": idSchema(), "state_filter": arraySchema(enumSchema("proposed", "confirmed", "discarded"), 0, 3), "type_filter": arraySchema(enumSchema("precedes", "causes", "part_of", "relates_to"), 0, 4), "cursor": stringSchema(nil), "page_size": integerSchema(1)}, "graph_id", "context_id")
}

func relationConfirmSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "relation_type": enumSchema("precedes", "causes", "part_of", "relates_to"), "from_context_id": idSchema(), "to_context_id": idSchema()}, "graph_id", "relation_type", "from_context_id", "to_context_id")
}

func relationDiscardSchema() map[string]any {
	return schema(map[string]any{"graph_id": idSchema(), "relation_id": idSchema()}, "graph_id", "relation_id")
}

func idSchema() map[string]any { return stringSchema(map[string]any{"format": "uuid"}) }

func dateTimeSchema() map[string]any { return stringSchema(map[string]any{"format": "date-time"}) }

func enumSchema(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
