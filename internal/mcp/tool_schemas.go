package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverInstructions = `graph_list로 그래프를 선택하세요. 여섯 노드·관계 쓰기 전 agent_register에서 agent_id를 받으세요. 등록 키는 계정·OAuth client별 재사용하며 대화 신원·권한을 부여하지 않습니다.
graph_get.entry_context_id는 대표 요약입니다. navigation_refs는 안내, derived_from은 근거입니다. node_get으로 탐색하고 conversation_id·turn_index·previous_event_id로 대화 순서를 명시하세요.
graph_update·node_update는 현재 expected_version이 필요합니다. 충돌하면 재조회하세요. 원천 정정은 새 원천, 파생 근거·종류 변경은 새 파생과 supersedes_context_id를 사용하세요.
여덟 업무 쓰기의 request_id는 선택입니다. 같은 키·인자는 최초 결과를 24시간 재생하며 새 키는 새 쓰기입니다. 키 없는 불확정 결과·키 유실은 재조회하세요. 내부 장애에 반복 로그인하지 마세요.
서버는 매 요청 인증·등급·agent 결합을 검사합니다. annotations·호스트 승인이 권한을 대신하지 않습니다. 삭제·권한·팀 관리는 웹 전용입니다. node_restore는 연산 폐기만 복구하며 웹 삭제는 웹에서 복구하세요.
검색은 context_flow_get입니다. 검색 채널 일부 실패·예산/홉 절단은 정상 부분 결과일 수 있습니다.`

func toolDefinitions() []*sdk.Tool {
	read := func() *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: new(false), OpenWorldHint: new(false)}
	}
	write := func(destructive, idempotent bool) *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: idempotent, DestructiveHint: new(destructive), OpenWorldHint: new(false)}
	}
	tool := func(name, description string, schema *jsonschema.Schema, annotations *sdk.ToolAnnotations) *sdk.Tool {
		return &sdk.Tool{Name: name, Description: description, InputSchema: schema, Annotations: annotations}
	}
	graph := func() map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{"graph_id": idSchema("graph_list에서 선택한 그래프 UUIDv7.")}
	}
	node := func() map[string]*jsonschema.Schema {
		p := graph()
		p["context_id"] = idSchema("대상 컨텍스트 UUIDv7.")
		return p
	}

	registration := objectSchema(map[string]*jsonschema.Schema{"registration_key": keySchema("재사용 등록 키: ASCII 영숫자·_·- 16~128자. 자격 증명·본문 금지.")}, "registration_key")
	list := objectSchema(map[string]*jsonschema.Schema{
		"name_filter":  textSchema("이름 부분 일치(대소문자 무시).", 0, 0),
		"grade_filter": enumArray("현재 등급 필터. 빈 배열·중복 허용.", 3, "owner", "editor", "viewer"),
		"cursor":       cursorSchema(), "page_size": integerSchema("페이지 크기. 기본값·상한은 플랜 기준.", 1),
	})
	createGraph := objectSchema(map[string]*jsonschema.Schema{
		"name":        nonblankSchema("표시 이름. 공백만은 금지, 중복 허용.", 200),
		"description": textSchema("설명. 빈 문자열 허용.", 0, 2000), "request_id": requestKeySchema(),
	}, "name")
	updateGraph := graph()
	updateGraph["expected_version"] = versionSchema()
	updateGraph["name"] = nonblankSchema("새 이름. 생략 시 유지, 공백만은 금지.", 200)
	updateGraph["description"] = textSchema("새 설명. 생략 시 유지, 빈 문자열로 삭제.", 0, 2000)
	updateGraph["request_id"] = requestKeySchema()
	updateGraph["entry_context_id"] = idSchema("대표 활성 요약. 생략 시 유지, null로 해제.")
	updateGraph["entry_context_id"].Type = ""
	updateGraph["entry_context_id"].Types = []string{"string", "null"}

	createNode := graph()
	createNode["layer"] = enumSchema("계층. 계층별 필수·금지 필드는 조건식 참조.", "source", "derived", "event")
	createNode["body"] = bodySchema()
	actorFields(createNode)
	createNode["source_ref"] = objectSchema(map[string]*jsonschema.Schema{
		"channel": enumSchema("수집 경로(origin_kind와 별개).", "conversation", "file", "web", "api", "other"),
		"locator": {Type: "string", Description: "원본 URI. 서버는 접근하지 않는다.", MinLength: new(1), MaxLength: new(1024), Pattern: "^[A-Za-z][A-Za-z0-9+.-]*:", Format: "uri"},
	}, "channel", "locator")
	createNode["source_ref"].Description = "원천 중복 키. 폐기·삭제 상태도 기존 원천을 반환하며 자동 복구하지 않는다."
	createNode["occurred_at"] = utcSchema("원본 발생 UTC 시각(기록 시각과 별개).", false)
	createNode["origin_kind"] = enumSchema("원천 생성 주체. 불변.", "user_utterance", "agent_output", "external_content")
	createNode["conversation_id"] = nonblankSchema("source·event의 대화 식별자. 불변·공백만 금지. source는 conversation 출처·turn_index 필수.", 128)
	createNode["turn_index"] = integerSchema("대화 원천의 턴 순서. conversation_id와 쌍, 불변.", 0)
	createNode["previous_event_id"] = idSchema("같은 그래프·conversation_id의 활성 앞 사건. confirmed precedes를 원자 생성하며 속성 저장은 하지 않는다.")
	createNode["navigation_refs"] = refsSchema("summary 안내 참조(근거 아님). 같은 그래프의 사건·요약·원천. 자기 참조 금지·불변.", 100)
	createNode["navigation_refs"].MinItems = new(0)
	createNode["navigation_refs"].UniqueItems = true
	createNode["derived_from"] = refsSchema("같은 그래프의 원천·파생 근거. 불변.", 100)
	createNode["derivation_kind"] = enumSchema("파생 목적. 불변.", "proposition", "summary", "reflection")
	createNode["summary_scope"] = enumSchema("summary 전용 필수 범위.", "local", "global")
	createNode["evidence_state"] = enumSchema("파생 근거 성격. 불변.", "observation", "experience", "opinion")
	createNode["confidence_state"] = enumSchema("opinion 전용 필수 신뢰 상태.", "supported", "uncertain", "disputed")
	createNode["valid_from"] = utcSchema("유효 시작 UTC. 생략 시 열린 시작.", false)
	createNode["valid_to"] = utcSchema("유효 종료 UTC. 생략 시 열린 종료.", false)
	createNode["supersedes_context_id"] = idSchema("대체할 같은 계열의 이전 파생. 새 파생→이전 파생 간선이며 근거와 별개.")
	createNode["member_refs"] = refsSchema("같은 그래프의 원천·파생 구성원(사건 제외).", 1000)
	createNode["event_start"] = utcSchema("사건 시작 UTC. 원천 구성원의 occurred_at을 시작~종료 범위에 포함해야 한다.", false)
	createNode["event_end"] = utcSchema("사건 종료 UTC. 생략 시 진행 중.", false)
	creation := objectSchema(createNode, "graph_id", "layer", "body", "agent_id")
	sourceFields := []string{"source_ref", "occurred_at", "origin_kind"}
	sourceOnlyFields := append(append([]string{}, sourceFields...), "turn_index")
	derivedFields := []string{"derived_from", "derivation_kind", "summary_scope", "evidence_state", "confidence_state", "valid_from", "valid_to", "supersedes_context_id", "navigation_refs"}
	eventFields := []string{"member_refs", "event_start", "event_end", "previous_event_id"}
	creation.AllOf = []*jsonschema.Schema{
		conditional("layer", "source", sourceFields, append(append([]string{}, derivedFields...), eventFields...)),
		conditional("layer", "derived", []string{"derived_from", "derivation_kind", "evidence_state"}, append(append(append([]string{}, sourceOnlyFields...), eventFields...), "conversation_id")),
		conditional("layer", "event", []string{"member_refs", "event_start"}, append(append([]string{}, sourceOnlyFields...), derivedFields...)),
		conditional("derivation_kind", "summary", []string{"summary_scope"}, nil),
		conditional("evidence_state", "opinion", []string{"confidence_state"}, nil),
		{If: &jsonschema.Schema{Required: []string{"turn_index"}}, Then: &jsonschema.Schema{Required: []string{"conversation_id"}}},
		{If: &jsonschema.Schema{AllOf: []*jsonschema.Schema{requiredValue("layer", "source"), {Required: []string{"conversation_id"}}}}, Then: &jsonschema.Schema{Required: []string{"turn_index"}, Properties: map[string]*jsonschema.Schema{"source_ref": {Properties: map[string]*jsonschema.Schema{"channel": {Const: new(any("conversation"))}}}}}},
		{If: &jsonschema.Schema{Required: []string{"previous_event_id"}}, Then: &jsonschema.Schema{Required: []string{"conversation_id"}}},
		{If: &jsonschema.Schema{Not: requiredValue("derivation_kind", "summary")}, Then: forbidFields("summary_scope", "navigation_refs")},
		{If: &jsonschema.Schema{Not: requiredValue("evidence_state", "opinion")}, Then: forbidFields("confidence_state")},
	}

	getNode := node()
	getNode["hops"] = integerSchema("탐색 홉 수. 기본 0은 시작점만, 상한은 플랜 기준.", 0)
	getNode["direction"] = enumSchema("기본 both. out은 파생→근거·이전 파생, 요약→안내, 사건→구성원·관계 대상이며 in은 역방향.", "in", "out", "both")
	getNode["traversal_filter"] = enumArray("탐색 간선 필터. 생략·빈 배열은 전체, 중복 허용.", 8, "derived_from", "supersedes", "member_refs", "navigation_refs", "precedes", "causes", "part_of", "relates_to")
	updateNode := node()
	actorFields(updateNode)
	updateNode["expected_version"] = versionSchema()
	updateNode["management_action"] = enumSchema("기본 update. keep은 가변 필드 없이 현재 판을 검사하고 유지 판단만 기록.", "update", "keep")
	updateNode["body"] = bodySchema()
	updateNode["confidence_state"] = enumSchema("opinion 파생의 새 신뢰 상태.", "supported", "uncertain", "disputed")
	updateNode["valid_from"] = utcSchema("새 유효 시작 UTC. null로 해제.", true)
	updateNode["valid_to"] = utcSchema("새 유효 종료 UTC. null로 해제.", true)
	updateNode["member_refs"] = refsSchema("교체할 원천·파생 구성원(사건 제외).", 1000)
	updateNode["event_start"] = utcSchema("새 사건 시작 UTC. 원천 구성원의 occurred_at을 범위에 포함해야 한다.", false)
	updateNode["event_end"] = utcSchema("새 사건 종료 UTC. null이면 진행 중.", true)
	updating := objectSchema(updateNode, "graph_id", "context_id", "expected_version", "agent_id")
	updating.AllOf = []*jsonschema.Schema{conditional("management_action", "keep", nil, []string{"body", "confidence_state", "valid_from", "valid_to", "member_refs", "event_start", "event_end"})}
	discard, restore := node(), node()
	actorFields(discard)
	actorFields(restore)
	flow := graph()
	flow["work_context"] = nonblankSchema("현재 작업 텍스트. 공백만은 금지.", 8000)
	flow["as_of"] = utcSchema("검색 기준 UTC(과거 상태 복원 아님). 기본 현재.", false)
	flow["scope"] = enumSchema("검색 범위. 기본 auto.", "local", "global", "auto")
	flow["budget"] = integerSchema("응답 body 문자 예산(토큰 아님). 기본값·상한은 플랜 기준.", 1)
	relations := node()
	relations["state_filter"] = enumArray("상태 필터. 빈 배열·중복 허용.", 3, "proposed", "confirmed", "discarded")
	relations["type_filter"] = enumArray("유형 필터. 빈 배열·중복 허용.", 4, "precedes", "causes", "part_of", "relates_to")
	relations["cursor"], relations["page_size"] = cursorSchema(), integerSchema("페이지 크기. 기본값·상한은 플랜 기준.", 1)
	confirm := graph()
	actorFields(confirm)
	confirm["relation_type"] = enumSchema("precedes: 앞→뒤, causes: 원인→결과, part_of: 부분→전체, relates_to: 무방향. 같은 그래프·유형·양 끝의 재확정은 현재 상태 반환.", "precedes", "causes", "part_of", "relates_to")
	confirm["from_context_id"] = idSchema("같은 그래프의 활성 시작 사건. precedes·causes는 대상보다 늦게 시작할 수 없다.")
	confirm["to_context_id"] = idSchema("같은 그래프의 활성 대상 사건. part_of는 시작 사건의 기간을 포함해야 한다.")
	discardRelation := graph()
	actorFields(discardRelation)
	discardRelation["relation_id"] = idSchema("폐기할 관계 UUIDv7. 재폐기 금지.")
	return []*sdk.Tool{
		tool("agent_register", "등록 키로 agent_id 발급. 같은 활성 키는 같은 ID이며 폐기 키는 복구하지 않는다.", registration, write(false, true)),
		tool("graph_list", "접근 가능한 활성 그래프를 최근 활동순으로 조회.", list, read()),
		tool("graph_create", "인증 계정을 소유자로 하는 그래프 생성.", createGraph, write(false, false)),
		tool("graph_get", "그래프 메타데이터·현재 version·대표 진입점 조회.", objectSchema(graph(), "graph_id"), read()),
		tool("graph_update", "현재 expected_version으로 이름·설명·대표 요약 갱신.", objectSchema(updateGraph, "graph_id", "expected_version"), write(true, false)),
		tool("node_create", "원천·파생·사건 생성. creation_status=existing_deleted는 활성 근거로 사용 불가. 사건 연결은 previous_event_id 또는 relation_confirm 사용.", creation, write(true, false)),
		tool("node_get", "시작점과 확정 간선의 부분 그래프 조회. 정상 절단은 오류가 아니다.", objectSchema(getNode, "graph_id", "context_id"), read()),
		tool("node_update", "파생·사건의 가변 필드 갱신 또는 keep 판단 기록. 원천은 불변. 생략 필드는 유지.", updating, write(true, false)),
		tool("node_discard", "노드 소프트 폐기. 재폐기 거부, 연쇄 폐기 없음.", objectSchema(discard, "graph_id", "context_id", "agent_id"), write(true, false)),
		tool("node_restore", "연산으로 폐기한 노드 복구. 웹 삭제는 웹에서 복구. 활성 노드 재복구 금지.", objectSchema(restore, "graph_id", "context_id", "agent_id"), write(true, false)),
		tool("context_flow_get", "작업 텍스트로 평면 컨텍스트 흐름 검색. 채널 일부 실패·예산/홉 절단은 정상 부분 결과.", objectSchema(flow, "graph_id", "work_context"), read()),
		tool("relation_list", "사건 관계를 상태·유형별로 relation_id 내림차순 조회.", objectSchema(relations, "graph_id", "context_id"), read()),
		tool("relation_confirm", "같은 그래프의 활성 사건 간 관계 확정·재확정. 원천·파생 연결은 derived_from·member_refs 사용. 실패해도 앞선 노드 생성은 유지.", objectSchema(confirm, "graph_id", "relation_type", "from_context_id", "to_context_id", "agent_id"), write(true, true)),
		tool("relation_discard", "사건 관계 폐기. 재폐기 금지, 복구는 relation_confirm 사용.", objectSchema(discardRelation, "graph_id", "relation_id", "agent_id"), write(true, false)),
	}
}

func objectSchema(properties map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: properties, Required: required, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
}

func textSchema(description string, minimum, maximum int) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "string", Description: description, MinLength: new(minimum), Pattern: "^[^\x00]*$"}
	if maximum > 0 {
		s.MaxLength = new(maximum)
	}
	return s
}

func nonblankSchema(description string, maximum int) *jsonschema.Schema {
	s := textSchema(description, 1, maximum)
	s.AllOf = []*jsonschema.Schema{{Pattern: "[^\x09-\x0d\x20\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]"}}
	return s
}

func enumSchema(description string, values ...string) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "string", Description: description}
	for _, value := range values {
		s.Enum = append(s.Enum, value)
	}
	return s
}

func idSchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: description, Format: "uuid-v7", Pattern: "^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-7[0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|[0-9a-fA-F]{12}7[0-9a-fA-F]{3}[89aAbB][0-9a-fA-F]{15})$"}
}

func keySchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: description, MinLength: new(16), MaxLength: new(128), Pattern: "^[A-Za-z0-9_-]+$"}
}

func requestKeySchema() *jsonschema.Schema {
	return keySchema("선택 재시도 키: ASCII 영숫자·_·- 16~128자. 같은 키·인자는 24시간 최초 결과 재생.")
}

func actorFields(properties map[string]*jsonschema.Schema) {
	properties["agent_id"] = idSchema("agent_register의 활성 agent_id.")
	properties["request_id"] = requestKeySchema()
	properties["judgment_input"] = textSchema("판단 입력. 생략 시 검증된 인자를 기록.", 1, 8000)
}

func bodySchema() *jsonschema.Schema {
	return textSchema("본문. U+0000 금지, 공백만 허용. 원문 저장·색인.", 1, 8000)
}
func versionSchema() *jsonschema.Schema {
	return integerSchema("조회한 현재 version. 충돌 시 재조회.", 1)
}
func integerSchema(description string, minimum int) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: description, Minimum: new(float64(minimum))}
}
func refsSchema(description string, maximum int) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: idSchema("컨텍스트 UUIDv7."), MinItems: new(1), MaxItems: new(maximum)}
}
func enumArray(description string, maximum int, values ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: enumSchema("허용값.", values...), MaxItems: new(maximum)}
}
func utcSchema(description string, nullable bool) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "string", Description: description, Format: "date-time", Pattern: `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|\+00:00)$`}
	if nullable {
		s.Type = ""
		s.Types = []string{"string", "null"}
	}
	return s
}
func cursorSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: "앞 응답 커서를 그대로 사용. 생략·빈 문자열은 첫 페이지.", Pattern: "^[A-Za-z0-9_-]*$", Format: "base64url"}
}
func requiredValue(field, value string) *jsonschema.Schema {
	return &jsonschema.Schema{Properties: map[string]*jsonschema.Schema{field: {Const: new(any(value))}}, Required: []string{field}}
}
func forbidFields(fields ...string) *jsonschema.Schema {
	s := &jsonschema.Schema{}
	for _, field := range fields {
		s.AnyOf = append(s.AnyOf, &jsonschema.Schema{Required: []string{field}})
	}
	return &jsonschema.Schema{Not: s}
}
func conditional(field, value string, required, forbidden []string) *jsonschema.Schema {
	then := &jsonschema.Schema{Required: required}
	if len(forbidden) > 0 {
		then.AllOf = []*jsonschema.Schema{forbidFields(forbidden...)}
	}
	return &jsonschema.Schema{If: requiredValue(field, value), Then: then}
}
