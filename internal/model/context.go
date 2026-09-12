package model

import (
	"fmt"
	"net/url"
	"slices"
	"time"
)

// Layer는 컨텍스트의 논리 계층을 나타낸다.
type Layer string

const (
	// LayerSource는 외부 원본을 보존하는 불변 원천 계층이다.
	LayerSource Layer = "source"
	// LayerDerived는 원천을 근거로 만든 파생 계층이다.
	LayerDerived Layer = "derived"
	// LayerEvent는 상호작용을 시간 범위로 묶는 사건 계층이다.
	LayerEvent Layer = "event"
)

// SourceChannel은 원천을 얻은 경로를 나타낸다.
type SourceChannel string

const (
	// SourceChannelConversation은 대화에서 얻은 원천을 뜻한다.
	SourceChannelConversation SourceChannel = "conversation"
	// SourceChannelFile은 파일에서 얻은 원천을 뜻한다.
	SourceChannelFile SourceChannel = "file"
	// SourceChannelWeb은 웹에서 얻은 원천을 뜻한다.
	SourceChannelWeb SourceChannel = "web"
	// SourceChannelAPI는 API에서 얻은 원천을 뜻한다.
	SourceChannelAPI SourceChannel = "api"
	// SourceChannelOther는 닫힌 목록 밖의 원천 경로를 뜻한다.
	SourceChannelOther SourceChannel = "other"
)

// OriginKind는 원천 내용을 만든 주체의 종류를 나타낸다.
type OriginKind string

const (
	// OriginKindUserUtterance는 사용자의 발화를 뜻한다.
	OriginKindUserUtterance OriginKind = "user_utterance"
	// OriginKindAgentOutput은 에이전트 출력을 뜻한다.
	OriginKindAgentOutput OriginKind = "agent_output"
	// OriginKindExternalContent는 외부에서 가져온 내용을 뜻한다.
	OriginKindExternalContent OriginKind = "external_content"
)

// DerivationKind는 파생 컨텍스트의 생성 목적을 나타낸다.
type DerivationKind string

const (
	// DerivationKindProposition은 원천에서 도출한 명제를 뜻한다.
	DerivationKindProposition DerivationKind = "proposition"
	// DerivationKindSummary는 원천을 압축한 요약을 뜻한다.
	DerivationKindSummary DerivationKind = "summary"
	// DerivationKindReflection은 작업 경과를 해석한 성찰을 뜻한다.
	DerivationKindReflection DerivationKind = "reflection"
)

// SummaryScope는 요약 파생이 다루는 범위를 나타낸다.
type SummaryScope string

const (
	// SummaryScopeLocal은 국소 원천·파생 묶음의 요약을 뜻한다.
	SummaryScopeLocal SummaryScope = "local"
	// SummaryScopeGlobal은 그래프 전역 맥락의 요약을 뜻한다.
	SummaryScopeGlobal SummaryScope = "global"
)

// EvidenceState는 파생이 근거에서 얼마나 떨어졌는지 나타낸다.
type EvidenceState string

const (
	// EvidenceStateObservation은 원천에서 직접 확인할 수 있는 파생을 뜻한다.
	EvidenceStateObservation EvidenceState = "observation"
	// EvidenceStateExperience는 여러 원천의 경과를 정리한 파생을 뜻한다.
	EvidenceStateExperience EvidenceState = "experience"
	// EvidenceStateOpinion은 원천을 넘어선 판단이나 권고를 뜻한다.
	EvidenceStateOpinion EvidenceState = "opinion"
)

// ConfidenceState는 의견 파생의 신뢰 상태를 나타낸다.
type ConfidenceState string

const (
	// ConfidenceStateSupported는 근거가 주장을 뒷받침하는 상태다.
	ConfidenceStateSupported ConfidenceState = "supported"
	// ConfidenceStateUncertain은 근거가 충분하지 않은 상태다.
	ConfidenceStateUncertain ConfidenceState = "uncertain"
	// ConfidenceStateDisputed는 상반된 파생이 확인된 상태다.
	ConfidenceStateDisputed ConfidenceState = "disputed"
)

// SourceReference는 원천 원본을 다시 찾기 위한 수집 경로와 위치다.
type SourceReference struct {
	// Channel 필드는 수집 경로의 닫힌 분류다.
	Channel SourceChannel
	// Locator 필드는 scheme을 포함하는 원본 위치 URI다.
	Locator string
}

// SourceAttributes는 원천 계층에만 필요한 불변 속성이다.
type SourceAttributes struct {
	// Reference 필드는 원본을 다시 찾는 출처 참조다.
	Reference SourceReference
	// OccurredAt 필드는 원본 발화나 내용이 실제로 발생한 UTC 시각이다.
	OccurredAt time.Time
	// OriginKind 필드는 원천 내용을 만든 주체의 종류다.
	OriginKind OriginKind
}

// DerivedAttributes는 파생 계층에만 필요한 속성이다.
type DerivedAttributes struct {
	// Kind 필드는 파생의 정체성을 정하는 생성 목적이다.
	Kind DerivationKind
	// SummaryScope 필드는 요약 파생일 때만 다루는 범위다.
	SummaryScope SummaryScope
	// DerivedFrom 필드는 근거가 된 원천 또는 파생 컨텍스트 식별자다.
	// DERIVED_FROM 간선에서 다시 조립한 응답용 값이다.
	DerivedFrom []ID
	// EvidenceState 필드는 생성 뒤 바꿀 수 없는 근거 성격이다.
	EvidenceState EvidenceState
	// ConfidenceState 필드는 의견 파생일 때만 지정하는 신뢰 상태다.
	ConfidenceState ConfidenceState
	// ValidFrom 필드는 사실로 간주하는 시작 UTC 시각이다.
	ValidFrom *time.Time
	// ValidTo 필드는 사실로 간주하는 종료 UTC 시각이다.
	ValidTo *time.Time
	// EvidenceInvalidated 필드는 근거가 폐기됐다는 표시다.
	EvidenceInvalidated bool
}

// EventAttributes는 사건 계층에만 필요한 시간 범위와 구성원을 담는다.
type EventAttributes struct {
	// MemberIDs 필드는 사건을 구성하는 원천 또는 파생 컨텍스트 식별자다.
	MemberIDs []ID
	// Start 필드는 사건이 시작된 UTC 시각이다.
	Start time.Time
	// End 필드는 사건이 끝난 UTC 시각이며 없으면 진행 중이다.
	End *time.Time
}

// Context는 세 계층이 공유하는 속성과 계층별 속성을 함께 표현한다.
type Context struct {
	// ID 필드는 컨텍스트의 전역 UUIDv7 식별자다.
	ID ID
	// GraphID 필드는 컨텍스트가 속한 그래프 식별자다.
	GraphID ID
	// Layer 필드는 아래 속성 묶음 중 하나를 고르는 계층이다.
	Layer Layer
	// Body 필드는 컨텍스트 본문이며 길이는 MCP 입력 계층이 검증한다.
	Body string
	// RecordedAt 필드는 시스템이 컨텍스트를 기록한 UTC 시각이다.
	RecordedAt time.Time
	// CreatedBy 필드는 컨텍스트를 기록한 계정 식별자다.
	CreatedBy ID
	// CreatedByAgent 필드는 계정을 대행해 기록한 에이전트 식별자다.
	CreatedByAgent ID
	// Version 필드는 1에서 시작하는 낙관적 잠금용 판 번호다.
	Version int64
	// DeletedAt 필드는 소프트 삭제 시각이며 원천에서도 이 표시만 바꿀 수 있다.
	DeletedAt *time.Time
	// Source 필드는 원천 계층에서만 채우는 속성 묶음이다.
	Source *SourceAttributes
	// Derived 필드는 파생 계층에서만 채우는 속성 묶음이다.
	Derived *DerivedAttributes
	// Event 필드는 사건 계층에서만 채우는 속성 묶음이다.
	Event *EventAttributes
}

// Graph는 컨텍스트 그래프의 도메인 메타데이터다.
type Graph struct {
	// ID 필드는 그래프의 전역 UUIDv7 식별자다.
	ID ID
	// Name 필드는 그래프의 표시 이름이며 다른 그래프와 중복될 수 있다.
	Name string
	// Description 필드는 그래프를 설명하는 선택 속성이다.
	Description string
	// CreatedBy 필드는 그래프를 만든 계정 식별자다.
	CreatedBy ID
	// CreatedAt 필드는 그래프 생성 UTC 시각이다.
	CreatedAt time.Time
	// LastActivityAt 필드는 컨텍스트가 마지막으로 추가·수정된 UTC 시각이다.
	LastActivityAt time.Time
	// StoredChars 필드는 활성 컨텍스트 본문의 유니코드 문자 수 누계다.
	StoredChars int64
	// Version 필드는 그래프 갱신에 쓰는 판 번호다.
	Version int64
	// DeletedAt 필드는 사용자 또는 수명주기 소프트 삭제 시각이다.
	DeletedAt *time.Time
	// GraceStartedAt 필드는 마지막 접근 가능 계정이 사라진 UTC 시각이다.
	GraceStartedAt *time.Time
}

// GraphGrade는 계정이 그래프에 대해 가진 유효 권한 등급이다.
type GraphGrade string

const (
	// GraphGradeOwner는 권한 관리와 소프트 삭제를 수행할 수 있는 최고 등급이다.
	GraphGradeOwner GraphGrade = "owner"
	// GraphGradeEditor는 그래프와 컨텍스트를 수정할 수 있는 등급이다.
	GraphGradeEditor GraphGrade = "editor"
	// GraphGradeViewer는 그래프와 컨텍스트를 조회할 수 있는 등급이다.
	GraphGradeViewer GraphGrade = "viewer"
)

// Valid는 권한 등급이 서비스에서 허용하는 값인지 확인한다.
func (grade GraphGrade) Valid() bool {
	switch grade {
	case GraphGradeOwner, GraphGradeEditor, GraphGradeViewer:
		return true
	default:
		return false
	}
}

// GraphListFilter는 그래프 목록에 적용할 이름과 등급 조건이다.
type GraphListFilter struct {
	// Name 필드는 대소문자를 구분하지 않는 리터럴 부분 일치 조건이다.
	Name string
	// Grades 필드는 반환할 요청 계정의 유효 등급 목록이다.
	Grades []GraphGrade
}

// GraphListItem은 요청 계정에 노출 가능한 그래프 목록 항목이다.
type GraphListItem struct {
	// ID 필드는 그래프의 전역 UUIDv7 식별자다.
	ID ID
	// Name 필드는 그래프의 표시 이름이다.
	Name string
	// Description 필드는 그래프를 설명하는 선택 속성이다.
	Description string
	// LastActivityAt 필드는 컨텍스트가 마지막으로 추가·수정된 UTC 시각이다.
	LastActivityAt time.Time
	// Grade 필드는 요청 계정의 직접·팀 상속을 합산한 최고 유효 등급이다.
	Grade GraphGrade
}

// Validate는 계층별 필수 속성, 시간 표현과 상태 조합을 저장 방식과 무관하게 검증한다.
func (c Context) Validate() error {
	if err := validateCommon(c); err != nil {
		return err
	}
	switch c.Layer {
	case LayerSource:
		if c.Source == nil || c.Derived != nil || c.Event != nil {
			return fieldErrorf("layer", "원천은 source 속성만 가져야 한다")
		}
		return c.Source.validate()
	case LayerDerived:
		if c.Derived == nil || c.Source != nil || c.Event != nil {
			return fieldErrorf("layer", "파생은 derived 속성만 가져야 한다")
		}
		return c.Derived.validate()
	case LayerEvent:
		if c.Event == nil || c.Source != nil || c.Derived != nil {
			return fieldErrorf("layer", "사건은 event 속성만 가져야 한다")
		}
		return c.Event.validate()
	default:
		return fieldErrorf("layer", "알 수 없는 컨텍스트 계층 %q", c.Layer)
	}
}

// ValidateUpdate는 기존 컨텍스트에서 다음 판으로 바꿀 때 계층별 불변 속성을 지킨다.
func ValidateUpdate(previous, next Context) error {
	if err := previous.Validate(); err != nil {
		return fmt.Errorf("기존 컨텍스트: %w", err)
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("다음 컨텍스트: %w", err)
	}
	if previous.ID != next.ID || previous.GraphID != next.GraphID || previous.Layer != next.Layer || previous.RecordedAt != next.RecordedAt || previous.CreatedBy != next.CreatedBy || previous.CreatedByAgent != next.CreatedByAgent {
		return fieldErrorf("context_id", "공통 식별자와 생성 메타데이터는 바꿀 수 없다")
	}
	switch previous.Layer {
	case LayerSource:
		if previous.Body != next.Body || *previous.Source != *next.Source || next.Version != 1 {
			return fieldErrorf("layer", "원천은 deleted_at 표시 외에 바꿀 수 없고 판 번호는 1이다")
		}
	case LayerDerived:
		if previous.Derived.Kind != next.Derived.Kind || previous.Derived.SummaryScope != next.Derived.SummaryScope || previous.Derived.EvidenceState != next.Derived.EvidenceState || previous.Derived.EvidenceInvalidated != next.Derived.EvidenceInvalidated {
			return fieldErrorf("derivation_kind", "파생의 종류, 요약 범위와 근거 상태는 바꿀 수 없다")
		}
		if next.Version != previous.Version+1 {
			return fieldErrorf("expected_version", "파생 갱신은 판 번호를 하나 증가시켜야 한다")
		}
	case LayerEvent:
		if next.Version != previous.Version+1 {
			return fieldErrorf("expected_version", "사건 갱신은 판 번호를 하나 증가시켜야 한다")
		}
	}
	return nil
}

// ValidateEvidenceInvalidation은 시스템이 근거 폐기 또는 대체 뒤 파생의 무효 표시만 켜는지 확인한다.
func ValidateEvidenceInvalidation(previous, next Context) error {
	if err := previous.Validate(); err != nil {
		return fmt.Errorf("기존 컨텍스트: %w", err)
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("다음 컨텍스트: %w", err)
	}
	if previous.Layer != LayerDerived || next.Layer != LayerDerived {
		return fieldErrorf("layer", "근거 무효 표시는 파생 컨텍스트에만 적용한다")
	}
	if previous.ID != next.ID || previous.GraphID != next.GraphID || previous.RecordedAt != next.RecordedAt || previous.CreatedBy != next.CreatedBy || previous.CreatedByAgent != next.CreatedByAgent || previous.Body != next.Body || !sameOptionalTime(previous.DeletedAt, next.DeletedAt) {
		return fieldErrorf("context_id", "근거 무효 표시는 공통 속성을 바꿀 수 없다")
	}
	if previous.Derived.Kind != next.Derived.Kind || previous.Derived.SummaryScope != next.Derived.SummaryScope || previous.Derived.EvidenceState != next.Derived.EvidenceState || previous.Derived.ConfidenceState != next.Derived.ConfidenceState || !sameOptionalTime(previous.Derived.ValidFrom, next.Derived.ValidFrom) || !sameOptionalTime(previous.Derived.ValidTo, next.Derived.ValidTo) {
		return fieldErrorf("evidence_invalidated", "근거 무효 표시는 파생의 다른 속성을 바꿀 수 없다")
	}
	if previous.Derived.EvidenceInvalidated || !next.Derived.EvidenceInvalidated {
		return fieldErrorf("evidence_invalidated", "근거 무효 표시는 false에서 true로만 바꿀 수 있다")
	}
	if next.Version != previous.Version+1 {
		return fieldErrorf("version", "근거 무효 표시는 판 번호를 하나 증가시켜야 한다")
	}
	return nil
}

// ValidateDerivedReferences는 파생이 UUIDv7 근거를 하나 이상 갖는지 확인한다.
//
// 상한은 보지 않는다. 「입력 검증」이 개수 상한을 연산 표면의 파라미터 검증으로 두어
// `mcp`에 맡겼고, 여기에서는 `SRS.md`가 필수 속성으로 둔 "1개 이상"만 강제한다.
func ValidateDerivedReferences(referenceIDs []ID) error {
	if len(referenceIDs) == 0 {
		return fieldErrorf("derived_from", "파생 근거는 1개 이상이어야 한다")
	}
	for _, referenceID := range referenceIDs {
		if !referenceID.IsV7() {
			return fieldErrorf("derived_from", "파생 근거 식별자는 UUIDv7이어야 한다")
		}
	}
	return nil
}

// ValidateEventMembers는 사건 구성원이 같은 그래프의 원천 또는 파생이고 원천 발생 시각이 사건 범위 안인지 확인한다.
func ValidateEventMembers(event Context, members []Context) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if event.Layer != LayerEvent || event.Event == nil {
		return fieldErrorf("layer", "사건 구성원 검증에는 사건 컨텍스트가 필요하다")
	}
	if len(members) != len(event.Event.MemberIDs) {
		return fieldErrorf("member_refs", "사건 구성원과 member_refs 개수가 다르다")
	}
	for index, member := range members {
		if err := member.Validate(); err != nil {
			return fmt.Errorf("사건 구성원 검증: %w", err)
		}
		if member.GraphID != event.GraphID || member.ID != event.Event.MemberIDs[index] || (member.Layer != LayerSource && member.Layer != LayerDerived) {
			return fieldErrorf("member_refs", "사건 구성원은 member_refs와 같은 원천 또는 파생이어야 한다")
		}
		if member.Layer == LayerSource && !contains(*event.Event, member.Source.OccurredAt) {
			return fieldErrorf("member_refs", "원천 구성원 발생 시각이 사건 시간 범위 밖이다")
		}
	}
	return nil
}

// Validate는 그래프 식별자, 시간과 판 번호의 기본 제약을 확인한다.
func (g Graph) Validate() error {
	if !g.ID.IsV7() || !g.CreatedBy.IsV7() {
		return fieldErrorf("graph_id", "그래프와 생성 계정은 UUIDv7이어야 한다")
	}
	if g.Name == "" {
		return fieldErrorf("name", "그래프 이름이 비어 있다")
	}
	if !isUTC(g.CreatedAt) || !isUTC(g.LastActivityAt) {
		return fieldErrorf("created_at", "그래프 시각은 UTC여야 한다")
	}
	if g.StoredChars < 0 {
		return fieldErrorf("stored_chars", "그래프 저장 문자 수는 음수일 수 없다")
	}
	if !optionalUTC(g.DeletedAt) || !optionalUTC(g.GraceStartedAt) {
		return fieldErrorf("deleted_at", "그래프 선택 시각은 UTC여야 한다")
	}
	if g.Version < 1 {
		return fieldErrorf("expected_version", "그래프 판 번호는 1 이상이어야 한다")
	}
	return nil
}

// validateCommon은 모든 계층이 공유하는 필수 속성과 불변 원천의 초기 판 번호를 확인한다.
func validateCommon(context Context) error {
	if !context.ID.IsV7() || !context.GraphID.IsV7() || !context.CreatedBy.IsV7() || !context.CreatedByAgent.IsV7() {
		return fieldErrorf("context_id", "컨텍스트, 그래프, 계정과 에이전트 식별자는 UUIDv7이어야 한다")
	}
	if !isUTC(context.RecordedAt) {
		return fieldErrorf("recorded_at", "기록 시각은 UTC여야 한다")
	}
	if context.Version < 1 {
		return fieldErrorf("version", "컨텍스트 판 번호는 1 이상이어야 한다")
	}
	if context.Layer == LayerSource && context.Version != 1 {
		return fieldErrorf("version", "원천 컨텍스트 판 번호는 1로 고정된다")
	}
	if !optionalUTC(context.DeletedAt) {
		return fieldErrorf("deleted_at", "컨텍스트 폐기 시각은 UTC여야 한다")
	}
	return nil
}

// validate는 원천의 출처 참조, 발생 시각과 출처 구분을 확인한다.
func (attributes SourceAttributes) validate() error {
	if !slices.Contains([]SourceChannel{SourceChannelConversation, SourceChannelFile, SourceChannelWeb, SourceChannelAPI, SourceChannelOther}, attributes.Reference.Channel) {
		return fieldErrorf("source_channel", "원천 수집 경로 %q가 허용된 값이 아니다", attributes.Reference.Channel)
	}
	locator, err := url.Parse(attributes.Reference.Locator)
	if err != nil || locator.Scheme == "" {
		return fieldErrorf("locator", "원천 위치가 scheme을 가진 URI가 아니다")
	}
	if !isUTC(attributes.OccurredAt) {
		return fieldErrorf("occurred_at", "원천 발생 시각은 UTC여야 한다")
	}
	if !slices.Contains([]OriginKind{OriginKindUserUtterance, OriginKindAgentOutput, OriginKindExternalContent}, attributes.OriginKind) {
		return fieldErrorf("origin_kind", "원천 출처 구분 %q가 허용된 값이 아니다", attributes.OriginKind)
	}
	return nil
}

// validate는 파생의 정체성, 의견의 신뢰 상태와 유효 시간 범위를 확인한다.
func (attributes DerivedAttributes) validate() error {
	if !slices.Contains([]DerivationKind{DerivationKindProposition, DerivationKindSummary, DerivationKindReflection}, attributes.Kind) {
		return fieldErrorf("derivation_kind", "파생 종류 %q가 허용된 값이 아니다", attributes.Kind)
	}
	if attributes.Kind == DerivationKindSummary {
		if !slices.Contains([]SummaryScope{SummaryScopeLocal, SummaryScopeGlobal}, attributes.SummaryScope) {
			return fieldErrorf("summary_scope", "요약 파생에는 local 또는 global summary_scope가 필요하다")
		}
	} else if attributes.SummaryScope != "" {
		return fieldErrorf("summary_scope", "요약이 아닌 파생에는 summary_scope를 둘 수 없다")
	}
	if !slices.Contains([]EvidenceState{EvidenceStateObservation, EvidenceStateExperience, EvidenceStateOpinion}, attributes.EvidenceState) {
		return fieldErrorf("evidence_state", "근거 상태 %q가 허용된 값이 아니다", attributes.EvidenceState)
	}
	if attributes.EvidenceState == EvidenceStateOpinion {
		if !slices.Contains([]ConfidenceState{ConfidenceStateSupported, ConfidenceStateUncertain, ConfidenceStateDisputed}, attributes.ConfidenceState) {
			return fieldErrorf("confidence_state", "의견 파생에는 유효한 신뢰 상태가 필요하다")
		}
	} else if attributes.ConfidenceState != "" {
		return fieldErrorf("confidence_state", "의견이 아닌 파생에는 신뢰 상태를 둘 수 없다")
	}
	if attributes.ValidFrom != nil && !isUTC(*attributes.ValidFrom) || attributes.ValidTo != nil && !isUTC(*attributes.ValidTo) {
		return fieldErrorf("valid_from", "파생 유효 시각은 UTC여야 한다")
	}
	if attributes.ValidFrom != nil && attributes.ValidTo != nil && attributes.ValidTo.Before(*attributes.ValidFrom) {
		return fieldErrorf("valid_to", "파생 유효 종료 시각이 시작 시각보다 빠르다")
	}
	return nil
}

// validate는 사건의 구성원 존재와 UTC 시간 범위를 확인한다.
//
// 구성원 수의 상한은 「입력 검증」이 `mcp`에 맡겼으므로 여기에서는 보지 않는다.
func (attributes EventAttributes) validate() error {
	if len(attributes.MemberIDs) == 0 {
		return fieldErrorf("member_refs", "사건 구성원은 1개 이상이어야 한다")
	}
	for _, memberID := range attributes.MemberIDs {
		if !memberID.IsV7() {
			return fieldErrorf("member_refs", "사건 구성원 식별자는 UUIDv7이어야 한다")
		}
	}
	if !isUTC(attributes.Start) || attributes.End != nil && !isUTC(*attributes.End) {
		return fieldErrorf("start", "사건 시간 범위는 UTC여야 한다")
	}
	if attributes.End != nil && attributes.End.Before(attributes.Start) {
		return fieldErrorf("end", "사건 종료 시각이 시작 시각보다 빠르다")
	}
	return nil
}

// isUTC는 비어 있지 않은 UTC 시각인지 확인한다.
func isUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

// optionalUTC는 비어 있거나 UTC인 선택 시각인지 확인한다.
func optionalUTC(value *time.Time) bool {
	return value == nil || isUTC(*value)
}

// sameOptionalTime은 선택 시각 두 값이 함께 비었거나 같은 순간인지 확인한다.
func sameOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

// contains는 사건 시간 범위가 지정한 UTC 시각을 포함하는지 확인한다.
func contains(event EventAttributes, value time.Time) bool {
	if value.Before(event.Start) {
		return false
	}
	return event.End == nil || !value.After(*event.End)
}
