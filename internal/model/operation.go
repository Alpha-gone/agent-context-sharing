package model

import "time"

// OperationKind는 확정된 컨텍스트 관리 연산의 종류다.
type OperationKind string

const (
	OperationAdd       OperationKind = "add"
	OperationUpdate    OperationKind = "update"
	OperationSupersede OperationKind = "supersede"
	OperationDiscard   OperationKind = "discard"
	OperationKeep      OperationKind = "keep"
)

// OperationResult는 관리 판단의 적용 또는 거부 결과다.
type OperationResult string

const (
	OperationApplied  OperationResult = "applied"
	OperationRejected OperationResult = "rejected"
)

// ManagementOperation은 저장 방식과 무관한 관리 연산의 실행 기록이다.
// 거부 기록은 요청 대상이 없을 수 있고 사건 관계 대상에는 판 번호를 두지 않는다.
type ManagementOperation struct {
	Kind            OperationKind
	GraphID         ID
	ContextID       ID
	RelationID      ID
	TargetVersion   int64
	JudgmentInput   string
	AccountID       ID
	AgentID         ID
	AppliedAt       time.Time
	Result          OperationResult
	RejectionReason string
}

// Validate는 대상의 구분·주체·UTC 시각·결과와 판단 입력을 확인한다.
func (operation ManagementOperation) Validate() error {
	switch operation.Kind {
	case OperationAdd, OperationUpdate, OperationSupersede, OperationDiscard, OperationKeep:
	default:
		return fieldErrorf("operation_kind", "허용된 관리 연산이 아니다")
	}
	if operation.Result != OperationApplied && operation.Result != OperationRejected {
		return fieldErrorf("result", "관리 연산 결과는 적용 또는 거부여야 한다")
	}
	for _, item := range []struct {
		field string
		id    ID
	}{{"graph_id", operation.GraphID}, {"account_id", operation.AccountID}, {"agent_id", operation.AgentID}} {
		if err := validateID(item.field, item.id); err != nil {
			return err
		}
	}
	if !operation.ContextID.IsZero() {
		if err := validateID("context_id", operation.ContextID); err != nil {
			return err
		}
	}
	if !operation.RelationID.IsZero() {
		if err := validateID("relation_id", operation.RelationID); err != nil {
			return err
		}
	}
	switch {
	case !operation.ContextID.IsZero() && !operation.RelationID.IsZero():
		return fieldErrorf("context_id", "관리 연산은 컨텍스트와 관계를 동시에 대상으로 삼을 수 없다")
	case !operation.RelationID.IsZero():
		if operation.Kind != OperationAdd && operation.Kind != OperationDiscard || operation.TargetVersion != 0 {
			return fieldErrorf("relation_id", "관계 기록은 판 없이 추가 또는 폐기만 할 수 있다")
		}
	case !operation.ContextID.IsZero():
		if operation.TargetVersion < 1 {
			return fieldErrorf("version", "컨텍스트 대상에는 양수 판 번호가 필요하다")
		}
	default:
		if operation.Result != OperationRejected || operation.TargetVersion != 0 {
			return fieldErrorf("context_id", "대상 없는 기록은 판 없는 거부 결과만 허용한다")
		}
	}
	if err := validateText("judgment_input", operation.JudgmentInput); err != nil {
		return err
	}
	if operation.JudgmentInput == "" {
		return fieldErrorf("judgment_input", "관리 연산 판단 입력이 비어 있다")
	}
	if !isUTC(operation.AppliedAt) {
		return fieldErrorf("applied_at", "관리 연산 적용 시각은 UTC여야 한다")
	}
	if err := validateText("rejection_reason", operation.RejectionReason); err != nil {
		return err
	}
	if operation.Result == OperationRejected && operation.RejectionReason == "" {
		return fieldErrorf("rejection_reason", "거부한 관리 연산에는 사유가 필요하다")
	}
	if operation.Result == OperationApplied && operation.RejectionReason != "" {
		return fieldErrorf("rejection_reason", "적용한 관리 연산에는 거부 사유를 둘 수 없다")
	}
	return nil
}
