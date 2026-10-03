// Package contract는 공유 manifest에 따른 도구 검증과 호스트 공개 정책을 소유한다.
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"sync"
	"uuid"
)

// Tool은 이름·설명과 원문 JSON 입력 스키마를 보관한다.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema jsontext.Value `json:"inputSchema"`
}

// Kind는 도구 호출의 읽기·쓰기 분류다.
type Kind string

const (
	// Read는 도메인 데이터를 변경하지 않는 도구다.
	Read Kind = "read"
	// Write는 도메인 데이터를 변경하는 도구다.
	Write Kind = "write"
)

var toolTraits = map[string]struct {
	kind  Kind
	agent bool
}{
	"graph_list":       {Read, false},
	"graph_get":        {Read, false},
	"node_get":         {Read, false},
	"context_flow_get": {Read, false},
	"relation_list":    {Read, false},
	"graph_create":     {Write, false},
	"graph_update":     {Write, false},
	"node_create":      {Write, true},
	"node_update":      {Write, true},
	"node_discard":     {Write, true},
	"node_restore":     {Write, true},
	"relation_confirm": {Write, true},
	"relation_discard": {Write, true},
}

// Classify는 공개 13종만 읽기 또는 쓰기로 분류한다.
func Classify(name string) (Kind, bool) {
	trait, ok := toolTraits[name]
	return trait.kind, ok
}

// InjectsAgent는 구성의 행위 에이전트 식별자를 주입하는 여섯 도구를 구분한다.
func InjectsAgent(name string) bool {
	return toolTraits[name].agent
}

// Policy는 목록과 호출에서 함께 사용하는 불변 공개 정책이다.
type Policy struct{ allowed map[string]bool }

// NewPolicy는 세 공개 모드와 명시적인 allowlist를 검증한다.
func NewPolicy(mode string, allowlist []string) (Policy, error) {
	p := Policy{allowed: make(map[string]bool)}
	if mode == "" {
		mode = "all"
	}
	if mode != "all" && mode != "read_only" && mode != "allowlist" {
		return Policy{}, ErrConfiguration
	}
	if mode != "allowlist" {
		if len(allowlist) != 0 {
			return Policy{}, ErrConfiguration
		}
		for name, trait := range toolTraits {
			if mode == "all" || trait.kind == Read {
				p.allowed[name] = true
			}
		}
		return p, nil
	}
	if len(allowlist) == 0 {
		return Policy{}, ErrConfiguration
	}
	for _, name := range allowlist {
		if _, ok := Classify(name); !ok || p.allowed[name] {
			return Policy{}, ErrConfiguration
		}
		p.allowed[name] = true
	}
	return p, nil
}

// Allows는 목록 공개와 호출 허용에 같은 판정을 제공한다.
func (p Policy) Allows(name string) bool { return p.allowed[name] }

var expectedTools = sync.OnceValues(func() ([]Tool, error) {
	var tools []Tool
	if err := json.Unmarshal(toolManifest, &tools); err != nil {
		return nil, ErrProtocol
	}
	return tools, nil
})

// Catalog는 완전히 검증한 원격 목록과 호스트 입력 스키마를 보관한다.
// 내부 JSON은 밖으로 직접 노출하지 않는다.
type Catalog struct {
	tools         []Tool
	remoteSchemas map[string]*inputSchema
	hostSchemas   map[string]*inputSchema
	fingerprint   [sha256.Size]byte
}

// ValidateTools는 13종 전체를 비교하고 알 수 없는 스키마 키워드를 거부한다.
// 설명은 최초 정상 조회값을 사용하고 fingerprint에 포함한다.
func ValidateTools(tools []Tool) (*Catalog, error) {
	expected, err := expectedTools()
	if err != nil || len(tools) != len(expected) {
		return nil, ErrProtocol
	}
	byName := make(map[string]Tool, len(tools))
	for _, tool := range tools {
		if _, ok := Classify(tool.Name); !ok || strings.TrimSpace(tool.Description) == "" {
			return nil, ErrProtocol
		}
		if _, exists := byName[tool.Name]; exists {
			return nil, ErrProtocol
		}
		byName[tool.Name] = tool
	}
	c := &Catalog{remoteSchemas: make(map[string]*inputSchema), hostSchemas: make(map[string]*inputSchema)}
	var normalized []Tool
	for _, want := range expected {
		got, ok := byName[want.Name]
		if !ok {
			return nil, ErrProtocol
		}
		gotSchema, err := normalizeSchema(got.InputSchema)
		if err != nil {
			return nil, ErrProtocol
		}
		wantSchema, err := normalizeSchema(want.InputSchema)
		if err != nil || !bytes.Equal(gotSchema, wantSchema) {
			return nil, ErrProtocol
		}
		remoteSchema, err := parseSchema(gotSchema)
		if err != nil {
			return nil, ErrProtocol
		}
		c.remoteSchemas[got.Name] = remoteSchema
		normalized = append(normalized, Tool{Name: got.Name, Description: got.Description, InputSchema: gotSchema})
		hostSchema := bytes.Clone(gotSchema)
		if InjectsAgent(got.Name) {
			var schema map[string]any
			if err := json.Unmarshal(hostSchema, &schema); err != nil {
				return nil, ErrProtocol
			}
			delete(schema["properties"].(map[string]any), "created_by_agent")
			required := schema["required"].([]any)
			schema["required"] = slices.DeleteFunc(required, func(value any) bool { return value == "created_by_agent" })
			hostSchema, err = json.Marshal(schema, json.Deterministic(true))
			if err != nil {
				return nil, ErrProtocol
			}
		}
		c.hostSchemas[got.Name], err = parseSchema(hostSchema)
		if err != nil {
			return nil, ErrProtocol
		}
		c.tools = append(c.tools, Tool{Name: got.Name, Description: got.Description, InputSchema: hostSchema})
	}
	encoded, err := json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return nil, ErrProtocol
	}
	c.fingerprint = sha256.Sum256(encoded)
	return c, nil
}

// Fingerprint는 도구 순서와 객체 키 순서에 무관한 이름·설명·스키마 지문이다.
func (c *Catalog) Fingerprint() [sha256.Size]byte { return c.fingerprint }

// Tools는 정책이 허용한 호스트 도구의 독립 복사본을 반환한다.
func (c *Catalog) Tools(policy Policy) []Tool {
	tools := make([]Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		if policy.Allows(tool.Name) {
			tool.InputSchema = bytes.Clone(tool.InputSchema)
			tools = append(tools, tool)
		}
	}
	return tools
}

// PrepareArguments는 호스트 입력을 선검증하고 새 객체에 구성값만 주입한다.
// 숫자·본문·커서를 일반 부동소수점이나 도메인 자료형으로 변환하지 않는다.
func (c *Catalog) PrepareArguments(name string, arguments jsontext.Value, policy Policy, agentID uuid.UUID) (jsontext.Value, error) {
	if !policy.Allows(name) {
		return nil, ErrProtocol
	}
	schema := c.hostSchemas[name]
	if schema == nil || !schema.valid(arguments) {
		return nil, ErrProtocol
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal(arguments, &object); err != nil || object == nil {
		return nil, ErrProtocol
	}
	if InjectsAgent(name) {
		if agentID[6]>>4 != 7 || agentID[8]&0xc0 != 0x80 {
			return nil, ErrProtocol
		}
		object["created_by_agent"], _ = json.Marshal(agentID.String())
	}
	result, err := json.Marshal(object, json.Deterministic(true))
	if err != nil {
		return nil, ErrProtocol
	}
	return result, nil
}

// ValidateRemoteArguments는 주입을 마친 인자가 원격 입력 스키마를 지키는지 확인한다.
func (c *Catalog) ValidateRemoteArguments(name string, arguments jsontext.Value) error {
	if schema := c.remoteSchemas[name]; schema == nil || !schema.valid(arguments) {
		return ErrProtocol
	}
	return nil
}
