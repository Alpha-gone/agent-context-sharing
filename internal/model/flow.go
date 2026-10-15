package model

import "time"

// SearchCandidate는 저장 방식과 무관한 채널 후보와 의미 유사도다.
type SearchCandidate struct {
	Context    Context
	Similarity float64
}

// HopEdge는 반환한 부분 그래프 안의 참조 또는 확정 사건 관계다.
type HopEdge struct {
	FromID ID
	ToID   ID
	Kind   string
}

// HopResult의 Distances는 각 노드까지의 최단 홉 거리이며 시작 노드는 0이다.
// 저장소 내부의 질의·적재 계측은 이 결과에 포함하지 않는다.
type HopResult struct {
	Contexts  []Context
	Distances map[ID]int
	Edges     []HopEdge
	Truncated bool
	Boundary  int
}

// ReadStats는 읽기 경계에서 사용한 자원의 집계이며 연결 핸들을 포함하지 않는다.
type ReadStats struct {
	Connections     int
	PeakConnections int
	Hold            time.Duration
}

// SearchRoute는 검색기가 선택한 경로다. 선택 규칙은 search가 소유한다.
type SearchRoute string

// FlowInput은 context_flow_get이 내부 검색기에 넘기는 확정된 입력이다.
type FlowInput struct {
	GraphID     ID
	WorkContext string
	AsOf        time.Time
	Budget      int
	Scope       string
	MaxHops     int
	MaxHopNodes int
}

// FlowContext는 흐름 응답을 만들기 위해 컨텍스트에 검색 메타데이터를 붙인 값이다.
type FlowContext struct {
	Value           Context
	OriginKinds     []OriginKind
	MatchedChannels []string
	Rank            int
	FoldedCount     int
	EntryDistance   int
	// CandidateDegree 필드에는 후보 부분 그래프에서의 차수를 둔다. 응답에는 싣지 않으며
	// 「검색 품질 평가」의 허브 편중을 계산하는 원자료다.
	CandidateDegree int
}

// FlowChannel은 채널별 후보 수·지연·실패와 응답 기여를 담는다.
type FlowChannel struct {
	Candidates   int
	Latency      time.Duration
	Contribution int
	Failure      string
}

// FlowTruncation은 예산으로 제외된 항목의 수를 나타낸다.
type FlowTruncation struct {
	Reason   string
	Excluded int
}

// ContextFlow는 MCP가 그대로 직렬화할 수 있는 비그래프 검색 결과다.
type ContextFlow struct {
	Contexts    []FlowContext
	Edges       []HopEdge
	EntryPoints []ID
	BudgetUsed  int
	Budget      int
	Channels    map[string]FlowChannel
	Truncation  *FlowTruncation
	// HopBoundary 필드에는 그래프 확장이 결과 상한으로 잘린 홉 경계를 둔다. 예산 절단과
	// 다른 원인이므로 따로 싣고, 경계 0과 절단 없음을 구분하려고 포인터로 둔다.
	HopBoundary *int
	Selection   FlowSelection
	// Route와 RouteReason 필드에는 실제 실행한 검색 경로와 그 이유 코드를 둔다.
	Route       SearchRoute
	RouteReason string
	// RouteSignals 필드에는 적응형 라우팅이 판정에 쓴 진입 채널 요약을 둔다. 라우터를
	// 실행하지 않았으면 0 값이다.
	RouteSignals RouteSignals
	// Read 필드에는 동기화 스냅숏이 쓴 연결 자원을 둔다. Read Committed 검색이면 0 값이다.
	Read ReadStats
}

// RouteSignals는 라우터가 보는 진입 채널 결과의 요약이다. 같은 신호와 임계값이면 항상 같은
// 경로가 나오므로, 평가 실행기는 이 값으로 임계값 후보를 다시 실행하지 않고 견줄 수 있다.
type RouteSignals struct {
	// RelevantLocal 필드에는 유사도 하한을 넘은 의미 후보나 키워드 후보가 있는지를 둔다.
	RelevantLocal bool
	// TopIsEvent 필드에는 진입 채널 통합 순위 1위가 사건인지를 둔다.
	TopIsEvent bool
	// CrossChannelAgreement 필드에는 진입 채널 통합 순위 1위가 의미·키워드 양쪽 후보인지를 둔다.
	CrossChannelAgreement bool
	// SemanticCount 필드에는 의미 후보 수를 둔다. 채널이 실패하면 0이다.
	SemanticCount int
	// SemanticTop과 SemanticSecond 필드에는 의미 후보 1위와 2위의 유사도를 둔다.
	SemanticTop    float64
	SemanticSecond float64
}

// FlowSelection은 「측정」이 요구한 근거 경로 선택 전후 후보 수와 연결 노드 수다. 선택이
// 비활성이면 선택 단위가 후보 하나이므로 연결 노드와 단절 후보는 항상 0이다.
type FlowSelection struct {
	// Candidates 필드에는 중복 파생 접기 뒤 선택에 들어간 후보 수를 둔다.
	Candidates int
	// Selected 필드에는 예산 적용까지 마친 뒤 남은 후보 수를 둔다.
	Selected int
	// Connectors 필드에는 연결 근거 집합을 완성하려고 대표 후보와 함께 담은 경로 노드 수를 둔다.
	Connectors int
	// Unreachable 필드에는 진입점까지 경로가 없어 선택하지 않은 대표 후보 수를 둔다.
	Unreachable int
}
