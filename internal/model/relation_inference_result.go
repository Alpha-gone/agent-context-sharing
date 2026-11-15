package model

// InferenceDecision은 검증된 한 후보·유형 판정과 발췌 인용이다.
// 본문에서 나온 값은 관리 진단으로 출력하지 않는다.
type InferenceDecision struct {
	CandidateID                               ID
	CandidateVersion                          int64
	RelationType                              RelationType
	Decision, Direction                       string
	TargetEvidence, CandidateEvidence, Reason string
	InvalidReason                             string
}

// InferenceApplication은 한 확보분과 결과 반영 정책이다.
type InferenceApplication struct {
	Task                                            RelationInferenceTask
	RunID                                           ID
	ProfileFingerprint, ProfileVersions, Candidates string
	Mode, AcceptedProfile                           string
	AutoConfirmTypes                                []RelationType
	Decisions                                       []InferenceDecision
	InputTokens, OutputTokens, LatencyMillis        *int64
	ProviderStatus                                  *string
	Attempts                                        int
	InvalidUnbound                                  int
}
