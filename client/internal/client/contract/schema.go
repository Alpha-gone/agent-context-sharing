package contract

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"
)

// 지원 키워드는 manifest에서 사용하는 제약으로 한정한다. 미지원 키워드를
// 무시하면 서버의 새로운 제약을 검증하지 않은 채 도구를 공개할 수 있다.
type inputSchema struct {
	Type                 string                  `json:"type"`
	Properties           map[string]*inputSchema `json:"properties,omitempty"`
	Required             []string                `json:"required,omitempty"`
	AdditionalProperties *bool                   `json:"additionalProperties,omitempty"`
	Enum                 []string                `json:"enum,omitempty"`
	MinLength            *int                    `json:"minLength,omitempty"`
	MaxLength            *int                    `json:"maxLength,omitempty"`
	MinItems             *int                    `json:"minItems,omitempty"`
	MaxItems             *int                    `json:"maxItems,omitempty"`
	Minimum              *int64                  `json:"minimum,omitempty"`
	Items                *inputSchema            `json:"items,omitempty"`
	Format               string                  `json:"format,omitempty"`
}

func parseSchema(raw jsontext.Value) (*inputSchema, error) {
	var schema inputSchema
	if err := json.Unmarshal(raw, &schema, json.RejectUnknownMembers(true)); err != nil {
		return nil, ErrProtocol
	}
	return &schema, nil
}

func normalizeSchema(raw jsontext.Value) (jsontext.Value, error) {
	if _, err := parseSchema(raw); err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, ErrProtocol
	}
	var normalize func(map[string]any) error
	normalize = func(schema map[string]any) error {
		for _, key := range []string{"required", "enum"} {
			if values, ok := schema[key].([]any); ok {
				for _, value := range values {
					if _, ok := value.(string); !ok {
						return ErrProtocol
					}
				}
				slices.SortFunc(values, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
			}
		}
		if properties, ok := schema["properties"].(map[string]any); ok {
			for _, property := range properties {
				object, ok := property.(map[string]any)
				if !ok || normalize(object) != nil {
					return ErrProtocol
				}
			}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			if err := normalize(items); err != nil {
				return err
			}
		}
		return nil
	}
	if err := normalize(value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return nil, ErrProtocol
	}
	return encoded, nil
}

func (s *inputSchema) valid(raw jsontext.Value) bool {
	if !raw.IsValid() {
		return false
	}
	switch s.Type {
	case "object":
		var object map[string]jsontext.Value
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return false
		}
		for _, key := range s.Required {
			if _, ok := object[key]; !ok {
				return false
			}
		}
		for key, value := range object {
			property := s.Properties[key]
			if property == nil {
				if s.AdditionalProperties == nil || !*s.AdditionalProperties {
					return false
				}
				continue
			}
			if !property.valid(value) {
				return false
			}
		}
		return true
	case "array":
		var items []jsontext.Value
		if err := json.Unmarshal(raw, &items); err != nil || items == nil {
			return false
		}
		if s.MinItems != nil && len(items) < *s.MinItems || s.MaxItems != nil && len(items) > *s.MaxItems {
			return false
		}
		for _, item := range items {
			if s.Items == nil || !s.Items.valid(item) {
				return false
			}
		}
		return true
	case "string":
		var value string
		if len(raw) == 0 || raw.Kind() != '"' || json.Unmarshal(raw, &value) != nil {
			return false
		}
		length := utf8.RuneCountInString(value)
		if s.MinLength != nil && length < *s.MinLength || s.MaxLength != nil && length > *s.MaxLength {
			return false
		}
		if len(s.Enum) != 0 && !slices.Contains(s.Enum, value) {
			return false
		}
		switch s.Format {
		case "":
			return true
		case "uuid":
			_, err := uuid.Parse(value)
			return err == nil && len(value) == 36
		case "date-time":
			_, err := time.Parse(time.RFC3339Nano, value)
			return err == nil
		case "uri":
			u, err := url.Parse(value)
			return err == nil && u.IsAbs()
		default:
			return false
		}
	case "integer":
		if raw.Kind() != '0' {
			return false
		}
		return integerAtLeast(string(raw), s.Minimum)
	default:
		return false
	}
}

// 지수 크기에 비례하는 정수·분모를 만들지 않고 소수부 유무와 하한만 판정한다.
// 호출자는 JSON 문법을 먼저 검증하므로 계수와 지수는 숫자 문법을 따른다.
func integerAtLeast(text string, minimum *int64) bool {
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	coefficient, exponentText, hasExponent := strings.Cut(text, "e")
	if !hasExponent {
		coefficient, exponentText, hasExponent = strings.Cut(text, "E")
	}
	var exponent int64
	if hasExponent {
		var err error
		exponent, err = strconv.ParseInt(exponentText, 10, 64)
		// 입력 자릿수를 넘는 지수는 정수 여부와 int64 하한 비교에 같은 결과를 낸다.
		limit := int64(len(text)) + 20
		if err != nil || exponent > limit || exponent < -limit {
			exponent = limit
			if strings.HasPrefix(exponentText, "-") {
				exponent = -limit
			}
		}
	}
	whole, fraction, _ := strings.Cut(coefficient, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return minimum == nil || *minimum <= 0
	}
	trimmed := strings.TrimRight(digits, "0")
	scale := exponent - int64(len(fraction)) + int64(len(digits)-len(trimmed))
	if scale < 0 {
		return false
	}
	if minimum == nil {
		return true
	}
	if int64(len(trimmed))+scale > 20 {
		return !negative
	}
	integer := trimmed + strings.Repeat("0", int(scale))
	if negative {
		integer = "-" + integer
	}
	number, ok := new(big.Int).SetString(integer, 10)
	return ok && number.Cmp(big.NewInt(*minimum)) >= 0
}
