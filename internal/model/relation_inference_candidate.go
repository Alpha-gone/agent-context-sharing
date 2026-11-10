package model

import "time"

// InferenceCandidate는 같은 스냅숏에서 읽은 활성 사건과 미정 판정 유형이다.
type InferenceCandidate struct {
	Event           Context
	Allowed         []RelationType
	PartOfDirection string
	CauseDirections []string
}

// InferenceChannel은 독립 순위로 정렬된 사건 후보 목록이다.
type InferenceChannel struct {
	Name       string
	Candidates []InferenceCandidate
}

// InferenceSnapshot은 사건·채널과 대상 색인 대기 상태의 일관 읽기 결과다.
type InferenceSnapshot struct {
	Target              Context
	Channels            []InferenceChannel
	SemanticUnavailable bool
	IndexWaitUntil      *time.Time
}

// InferenceCandidateRequest는 하나의 확보된 사건과 후보 스냅숏 설정이다.
type InferenceCandidateRequest struct {
	Task                               RelationInferenceTask
	ProfileFingerprint, EmbeddingModel string
	Channels                           []string
	Limit                              int
	IndexWait, AdjacencyWindow         time.Duration
}
