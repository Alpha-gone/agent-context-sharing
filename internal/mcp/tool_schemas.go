package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverInstructions = `공유 컨텍스트 그래프를 사용하는 도구입니다. graph_list로 접근 가능한 그래프를 확인하고 graph_id를 선택하세요.
노드·관계 쓰기 전에 agent_register에 16~128자의 ASCII 등록 키를 보내 agent_id를 받으세요. 같은 계정·OAuth client에서 같은 등록 키를 재사용하고 반환한 agent_id를 후속 여섯 쓰기에 전달하세요. 등록은 개별 모델·대화의 신원 증명이나 그래프 권한 부여가 아닙니다.
graph_update·node_update는 조회한 현재 expected_version이 필요합니다. 충돌하면 재조회하고 변경을 검토하세요. 원천은 불변이며 정정은 새 원천, 파생의 근거·종류 변경은 새 파생과 supersedes_context_id로 표현하세요.
여덟 업무 쓰기의 request_id는 선택이며 16~128자의 ASCII 키입니다. 같은 논리 요청은 같은 키·인자로 재시도할 수 있고 최초 결과는 24시간 보존됩니다. 새 키는 새 쓰기입니다. 키 없는 쓰기의 결과가 불명확하거나 키를 잃었다면 현재 상태를 재조회하세요.
인증·현재 등급·agent 결합은 서버가 매 요청 확인합니다. annotations와 호스트 승인은 서버 권한 검사를 대신하지 않습니다. 내부 장애를 반복 로그인으로 해결하려 하지 마세요.
삭제·권한·팀 관리는 웹 전용입니다. node_discard는 소프트 폐기이며 node_restore는 연산으로 폐기한 노드만 복구합니다. 웹 삭제 복구는 웹을 사용하세요. 검색은 context_flow_get으로 요청하며 별도 검색 도구는 없습니다. 결과 절단·일부 검색 채널 실패는 정상 부분 결과일 수 있습니다.`

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
		return map[string]*jsonschema.Schema{"graph_id": idSchema("대상 그래프 UUIDv7. graph_list에서 확인한 접근 가능한 그래프를 선택한다.")}
	}
	node := func() map[string]*jsonschema.Schema {
		p := graph()
		p["context_id"] = idSchema("대상 컨텍스트 UUIDv7. graph_id 안의 현재 상태를 조회하여 사용한다.")
		return p
	}

	registration := objectSchema(map[string]*jsonschema.Schema{"registration_key": keySchema("계정·검증된 OAuth client에 등록을 결합하는 재사용 키. 자격 증명·대화 제목·본문을 넣지 않는다.")}, "registration_key")
	list := objectSchema(map[string]*jsonschema.Schema{
		"name_filter":  textSchema("대소문자를 무시한 이름 부분 일치. 정규식이 아니다.", 0, 0),
		"grade_filter": enumArray("요청 계정의 현재 유효 등급으로 필터한다. 빈 배열·중복을 허용하되 중복도 상한에 포함한다.", 3, "owner", "editor", "viewer"),
		"cursor":       cursorSchema(), "page_size": integerSchema("요청 페이지 크기. 생략하면 계정 플랜 기본값을 적용하며 양수·플랜 상한을 검사한다.", 1),
	})
	createGraph := objectSchema(map[string]*jsonschema.Schema{
		"name":        nonblankSchema("그래프 표시 이름. 공백만인 값은 허용하지 않으며 중복 이름은 허용한다.", 200),
		"description": textSchema("그래프 설명. 빈 문자열을 허용한다.", 0, 2000), "request_id": requestKeySchema(),
	}, "name")
	updateGraph := graph()
	updateGraph["expected_version"] = versionSchema()
	updateGraph["name"] = nonblankSchema("변경할 그래프 이름. 생략하면 유지한다.", 200)
	updateGraph["description"] = textSchema("변경할 설명. 빈 문자열이면 설명을 비운다. 생략하면 유지한다.", 0, 2000)
	updateGraph["request_id"] = requestKeySchema()

	createNode := graph()
	createNode["layer"] = enumSchema("생성할 계층. source·derived·event에 따라 필요한 속성이 다르다.", "source", "derived", "event")
	createNode["body"] = bodySchema()
	actorFields(createNode)
	createNode["source_ref"] = objectSchema(map[string]*jsonschema.Schema{
		"channel": enumSchema("원본을 얻은 수집 경로. 내용의 생성 주체인 origin_kind와 구분한다.", "conversation", "file", "web", "api", "other"),
		"locator": {Type: "string", Description: "scheme을 가진 원본 URI. 1~1024자이며 서버는 원본에 접근하지 않는다.", MinLength: new(1), MaxLength: new(1024), Pattern: "^[A-Za-z][A-Za-z0-9+.-]*:", Format: "uri"},
	}, "channel", "locator")
	createNode["source_ref"].Description = "원천의 자연 키인 출처 참조. 같은 그래프에서 같은 참조는 폐기·삭제 상태도 포함해 기존 원천을 반환한다. 삭제 시각과 creation_status=existing_deleted를 확인하며 자동 복구하지 않는다."
	createNode["occurred_at"] = utcSchema("원본 내용이 발생한 UTC 시각. 시스템 기록 시각과 구분한다.", false)
	createNode["origin_kind"] = enumSchema("원천 내용의 생성 주체. 생성 후 불변이다.", "user_utterance", "agent_output", "external_content")
	createNode["derived_from"] = refsSchema("파생의 원천·파생 근거 UUIDv7. 1~100개이며 생성 후 불변이다.", 100)
	createNode["derivation_kind"] = enumSchema("파생의 생성 목적. 생성 후 변경할 수 없다.", "proposition", "summary", "reflection")
	createNode["summary_scope"] = enumSchema("summary일 때만 필수인 요약 범위. 다른 파생에는 지정하지 않는다.", "local", "global")
	createNode["evidence_state"] = enumSchema("파생 근거 성격. 생성 후 불변이다.", "observation", "experience", "opinion")
	createNode["confidence_state"] = enumSchema("opinion일 때만 필수인 신뢰 상태. 다른 파생에는 지정하지 않는다.", "supported", "uncertain", "disputed")
	createNode["valid_from"] = utcSchema("파생의 유효 시작 UTC 시각. 생략하면 열린 시작이다.", false)
	createNode["valid_to"] = utcSchema("파생의 유효 종료 UTC 시각. 생략하면 계속 유효하다.", false)
	createNode["supersedes_context_id"] = idSchema("새 파생이 대체할 기존 파생 UUIDv7. 새 파생에서 이전 파생으로 대체 간선을 남긴다.")
	createNode["member_refs"] = refsSchema("사건의 원천·파생 구성원 UUIDv7. 1~1000개이며 사건을 구성원으로 넣지 않는다.", 1000)
	createNode["event_start"] = utcSchema("사건 시작 UTC 시각. 구성원과의 시간 관계는 모델·저장소에서 검사한다.", false)
	createNode["event_end"] = utcSchema("사건 종료 UTC 시각. 생략하면 진행 중이다.", false)
	creation := objectSchema(createNode, "graph_id", "layer", "body", "agent_id")
	sourceFields := []string{"source_ref", "occurred_at", "origin_kind"}
	derivedFields := []string{"derived_from", "derivation_kind", "summary_scope", "evidence_state", "confidence_state", "valid_from", "valid_to", "supersedes_context_id"}
	eventFields := []string{"member_refs", "event_start", "event_end"}
	creation.AllOf = []*jsonschema.Schema{
		conditional("layer", "source", sourceFields, append(append([]string{}, derivedFields...), eventFields...)),
		conditional("layer", "derived", []string{"derived_from", "derivation_kind", "evidence_state"}, append(append([]string{}, sourceFields...), eventFields...)),
		conditional("layer", "event", []string{"member_refs", "event_start"}, append(append([]string{}, sourceFields...), derivedFields...)),
		conditional("derivation_kind", "summary", []string{"summary_scope"}, nil),
		conditional("evidence_state", "opinion", []string{"confidence_state"}, nil),
		{If: &jsonschema.Schema{Not: requiredValue("derivation_kind", "summary")}, Then: forbidFields("summary_scope")},
		{If: &jsonschema.Schema{Not: requiredValue("evidence_state", "opinion")}, Then: forbidFields("confidence_state")},
	}

	getNode := node()
	getNode["hops"] = integerSchema("탐색 깊이. 0은 시작 노드만 반환하며 최대 홉 수는 계정 플랜이 제한한다.", 0)
	getNode["direction"] = enumSchema("탐색 방향. 생략하면 양방향 both다.", "in", "out", "both")
	getNode["traversal_filter"] = enumArray("탐색할 확정 참조·사건 관계. 빈 배열·중복을 허용하며 최대 7개다.", 7, "derived_from", "supersedes", "member_refs", "precedes", "causes", "part_of", "relates_to")
	updateNode := node()
	actorFields(updateNode)
	updateNode["expected_version"] = versionSchema()
	updateNode["management_action"] = enumSchema("생략/update는 일반 갱신. keep은 가변 필드 없이 현재 판을 검사하고 유지 판단만 기록한다.", "update", "keep")
	updateNode["body"] = bodySchema()
	updateNode["confidence_state"] = enumSchema("의견 파생의 새 신뢰 상태. 기존 계층·의견 여부는 모델에서 검사한다.", "supported", "uncertain", "disputed")
	updateNode["valid_from"] = utcSchema("파생의 새 유효 시작. null이면 열린 시작으로 비운다.", true)
	updateNode["valid_to"] = utcSchema("파생의 새 유효 종료. null이면 열린 종료로 비운다.", true)
	updateNode["member_refs"] = refsSchema("사건의 교체할 구성원 목록. 1~1000개다.", 1000)
	updateNode["event_start"] = utcSchema("사건의 새 시작 UTC 시각.", false)
	updateNode["event_end"] = utcSchema("사건의 새 종료 UTC 시각. null이면 진행 중으로 비운다.", true)
	updating := objectSchema(updateNode, "graph_id", "context_id", "expected_version", "agent_id")
	updating.AllOf = []*jsonschema.Schema{conditional("management_action", "keep", nil, []string{"body", "confidence_state", "valid_from", "valid_to", "member_refs", "event_start", "event_end"})}
	discard, restore := node(), node()
	actorFields(discard)
	actorFields(restore)
	flow := graph()
	flow["work_context"] = nonblankSchema("현재 작업 텍스트. 1~8000자이며 공백만인 값은 거부한다. 별도 시작 노드는 지정하지 않는다.", 8000)
	flow["as_of"] = utcSchema("검색 기준 UTC 시각. 생략하면 요청 시점 현재 UTC를 사용한다.", false)
	flow["scope"] = enumSchema("local·global·auto 중 검색 범위. 생략하면 설계의 기본 경로를 사용한다.", "local", "global", "auto")
	flow["budget"] = integerSchema("응답 body의 양수 문자 예산. 생략하면 계정 플랜 예산을 사용하며 0으로 한도를 우회할 수 없다.", 1)
	relations := node()
	relations["state_filter"] = enumArray("관계 상태 필터. 빈 배열·중복을 허용하되 최대 3개다.", 3, "proposed", "confirmed", "discarded")
	relations["type_filter"] = enumArray("사건 관계 유형 필터. 빈 배열·중복을 허용하되 최대 4개다.", 4, "precedes", "causes", "part_of", "relates_to")
	relations["cursor"], relations["page_size"] = cursorSchema(), integerSchema("양수 페이지 크기. 생략하면 계정 플랜 기본값이며 상한도 플랜이 정한다.", 1)
	confirm := graph()
	actorFields(confirm)
	confirm["relation_type"] = enumSchema("확정할 사건 관계 유형. 정체성은 그래프·유형·양 끝의 조합이며 재확정은 기존 현재 상태를 반환한다.", "precedes", "causes", "part_of", "relates_to")
	confirm["from_context_id"] = idSchema("같은 그래프의 활성 시작 사건(layer=event) UUIDv7. precedes·causes에서는 대상보다 늦게 시작할 수 없고 part_of에서는 부분 사건이다.")
	confirm["to_context_id"] = idSchema("같은 그래프의 활성 대상 사건(layer=event) UUIDv7. part_of에서는 시작 사건의 기간을 포함하는 전체 사건이다.")
	discardRelation := graph()
	actorFields(discardRelation)
	discardRelation["relation_id"] = idSchema("폐기할 사건 관계 UUIDv7. 이미 폐기된 관계의 재폐기는 입력 오류다.")
	return []*sdk.Tool{
		tool("agent_register", "인증 계정·OAuth client에 등록 키를 결합해 agent_id를 받는다. 같은 활성 키는 같은 ID를 반환하고 폐기 키를 암묵 복구하지 않는다. 등급·개별 대화 신원을 부여하지 않는다.", registration, write(false, true)),
		tool("graph_list", "현재 등급을 가진 활성 그래프를 최근 활동순으로 조회한다. 받은 불투명 커서를 그대로 사용하고 응답 등급으로 가능한 작업을 판단한다.", list, read()),
		tool("graph_create", "새 그래프를 만들고 인증 계정이 소유자가 된다. request_id 없는 불확정 결과는 재조회하며 새 키는 새 그래프를 만들 수 있다.", createGraph, write(false, false)),
		tool("graph_get", "선택한 graph_id의 현재 메타데이터·version을 조회한다. 현재 등급은 서버가 검사한다.", objectSchema(graph(), "graph_id"), read()),
		tool("graph_update", "현재 expected_version으로 이름·설명을 갱신한다. 충돌하면 재조회한다. 같은 request_id·인자의 24시간 최초 결과 재생과 새 키의 판 검사를 구분한다.", objectSchema(updateGraph, "graph_id", "expected_version"), write(true, false)),
		tool("node_create", "등록한 agent_id로 원천·파생·사건을 만든다. 계층별 필수 속성을 채운다. 원천은 source_ref로 중복 판정한다. 폐기·삭제된 기존 원천은 creation_status=existing_deleted와 warning을 반환하며 활성 근거로 사용할 수 없다. MCP 폐기는 node_restore로, 웹 삭제는 웹에서 복구한다. 파생 대체는 supersedes_context_id를 사용한다. 기존 노드와의 연결은 생성 때 파생의 derived_from이나 사건의 member_refs로 지정하며, 사건 사이 관계만 생성 뒤 relation_confirm으로 확정한다. request_id 재시도와 새 생성을 구분한다.", creation, write(true, false)),
		tool("node_get", "graph_id 안에서 시작 노드와 확정 간선의 부분 그래프를 조회한다. 0홉은 시작점만이며 정상 절단은 오류가 아니다.", objectSchema(getNode, "graph_id", "context_id"), read()),
		tool("node_update", "등록한 agent_id·현재 expected_version으로 파생/사건의 가변 필드만 갱신한다. 원천은 불변이고 keep은 가변 필드 없이 유지 판단을 기록한다. 충돌·불확정 결과는 재조회한다.", updating, write(true, false)),
		tool("node_discard", "등록한 agent_id로 노드를 소프트 폐기한다. 재폐기는 거부되며 연쇄 폐기는 하지 않는다. 같은 request_id의 결과 재생과 새 키의 상태 검사를 구분한다.", objectSchema(discard, "graph_id", "context_id", "agent_id"), write(true, false)),
		tool("node_restore", "등록한 agent_id로 연산 채널에서 폐기한 노드를 복구한다. 웹 삭제는 웹에서 복구하며 활성 노드의 재복구는 거부한다. 불확정 결과는 같은 키 재시도 또는 조회로 확인한다.", objectSchema(restore, "graph_id", "context_id", "agent_id"), write(true, false)),
		tool("context_flow_get", "현재 작업 텍스트로 공유 그래프의 평면 컨텍스트 흐름을 조회한다. UTC as_of·scope·문자 budget을 지정할 수 있다. 일부 채널 실패와 예산/홉 절단은 정상 부분 결과다.", objectSchema(flow, "graph_id", "work_context"), read()),
		tool("relation_list", "선택한 사건에 붙은 사건 관계를 상태·유형으로 필터하고 relation_id 내림차순으로 조회한다. 받은 커서는 변경하지 않는다.", objectSchema(relations, "graph_id", "context_id"), read()),
		tool("relation_confirm", "등록한 agent_id로 제안된 관계 또는 직접 판단한 사건 관계를 확정한다. 같은 정체성의 재확정은 현재 상태를 반환한다. 양 끝은 같은 그래프의 활성 사건(layer=event)이어야 하며 원천·파생은 node_create의 derived_from·member_refs로 연결한다. 거부되어도 앞선 노드 생성은 되돌리지 않는다. 그래프·사건 유형·시간·순환 제약은 서버가 검사한다.", objectSchema(confirm, "graph_id", "relation_type", "from_context_id", "to_context_id", "agent_id"), write(true, true)),
		tool("relation_discard", "등록한 agent_id로 사건 관계를 폐기한다. 재폐기는 거부되며 관계 복구는 relation_confirm 재확정으로 처리한다. 선택 request_id로 같은 논리 요청을 재생한다.", objectSchema(discardRelation, "graph_id", "relation_id", "agent_id"), write(true, false)),
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
	return keySchema("선택 멱등성 키. 같은 계정·같은 논리 요청은 같은 키·인자로 재시도한다. 최초 결과는 24시간 보존하며 키가 없으면 자동 재시도 안전성을 보장하지 않는다.")
}

func actorFields(properties map[string]*jsonschema.Schema) {
	properties["agent_id"] = idSchema("agent_register에서 발급받은 활성 agent_id. 현재 인증 계정·OAuth client와의 결합은 서버가 검사한다.")
	properties["request_id"] = requestKeySchema()
	properties["judgment_input"] = textSchema("선택 판단 입력. 1~8000자이며 같은 재구성의 요청에 같은 값을 넣을 수 있다. 생략하면 검증된 인자 묶음을 기록한다.", 1, 8000)
}

func bodySchema() *jsonschema.Schema {
	return textSchema("컨텍스트 본문. 유효한 UTF-8의 1~8000자이며 U+0000을 허용하지 않는다. 공백만인 본문도 허용하고 원문 그대로 저장·색인한다.", 1, 8000)
}
func versionSchema() *jsonschema.Schema {
	return integerSchema("조회한 현재 양수 version. 충돌하면 대상을 재조회하며 자동 병합하지 않는다.", 1)
}
func integerSchema(description string, minimum int) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: description, Minimum: new(float64(minimum))}
}
func refsSchema(description string, maximum int) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: idSchema("참조할 컨텍스트 UUIDv7. 실제 존재·계층·같은 그래프 여부는 저장 상태로 판정한다."), MinItems: new(1), MaxItems: new(maximum)}
}
func enumArray(description string, maximum int, values ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: enumSchema("허용된 필터 값.", values...), MaxItems: new(maximum)}
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
	return &jsonschema.Schema{Type: "string", Description: "앞 응답의 불투명 커서를 그대로 반환한다. 내부 값을 구성하거나 해석하지 않는다. 생략·빈 문자열은 첫 페이지다.", Pattern: "^[A-Za-z0-9_-]*$", Format: "base64url"}
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
