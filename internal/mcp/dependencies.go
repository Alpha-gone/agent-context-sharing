// Package mcp는 표준 MCP 전송·도구 입력·도메인 결과 사상의 경계를 소유한다.
// ARCH-03은 주입 계약을 제공하며 SDK 전송과 도구 등록은 MCP 개발 항목에서 구현한다.
package mcp

import (
	"context"
	"fmt"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
)

// Plans는 인증된 계정의 플랜만 조회한다. 구성 원문을 처리기에 전달하지 않는다.
type Plans interface {
	For(model.ID) plan.Limits
}

type GraphReader interface {
	Graph(context.Context, model.ID) (model.Graph, error)
	ListGraphs(context.Context, model.ID, model.GraphListFilter, string, int) ([]model.GraphListItem, string, error)
}

type NodeReader interface {
	Context(context.Context, model.ID, model.ID) (model.Context, error)
}

type HopReader interface {
	SearchHopContextsFrom(context.Context, model.ID, []model.Context, int, string, []string, int) (model.HopResult, error)
}

type RelationReader interface {
	ListContextRelations(context.Context, model.ID, model.ID, []model.RelationState, []model.RelationType, string, int) ([]model.Relation, string, error)
}

// FlowSearch를 구현하는 검색기는 저장소와 임베딩 제공자를 자체 주입받는다.
type FlowSearch interface {
	Flow(context.Context, model.FlowInput) (model.ContextFlow, error)
}

// AgentRegistry는 검증된 계정·OAuth client에 등록을 결합한다. clientInfo를 받지 않는다.
type AgentRegistry interface {
	RegisterAgent(context.Context, model.ID, string, string) (model.ID, error)
	ValidateAgent(context.Context, model.ID, string, model.ID) error
}

// Actor는 인증·등록 결합을 마친 관리 연산의 실행 주체다.
// JudgmentInput은 업무 감사 전용이며 Diagnostics로 전달하지 않는다.
type Actor struct {
	AccountID     model.ID
	AgentID       model.ID
	JudgmentInput string
}

// 쓰기 구현은 등급·활성 상태·누적 한도·감사·색인 등록을 같은 저장 트랜잭션에서 검사한다.
// 처리기에는 트랜잭션이나 DB 실행기를 내보내지 않는다.
type GraphWriter interface {
	CreateGraph(context.Context, model.Graph, plan.Limits) (model.Graph, error)
	UpdateGraph(context.Context, model.Graph, int64, model.ID, plan.Limits) (model.Graph, error)
}

type NodeWriter interface {
	CreateNode(context.Context, model.ID, model.Context, Actor, plan.Limits) (model.Context, error)
	UpdateNode(context.Context, model.ID, model.Context, int64, model.OperationKind, Actor, plan.Limits) (model.Context, error)
	DiscardNode(context.Context, model.ID, model.ID, Actor, plan.Limits) (model.Context, error)
	RestoreNode(context.Context, model.ID, model.ID, Actor, plan.Limits) (model.Context, error)
}

// RejectedOperations는 도메인 저장점 롤백 뒤 같은 재생 트랜잭션에 거부를 기록한다.
type RejectedOperations interface {
	RecordRejectedOperation(context.Context, model.ManagementOperation) error
}

type RelationWriter interface {
	ConfirmRelation(context.Context, model.ID, model.Relation, Actor, plan.Limits) (model.Relation, error)
	DiscardRelation(context.Context, model.ID, model.ID, Actor, plan.Limits) (model.Relation, error)
}

// WriteExecutor는 계정 범위의 멱등성 예약과 도메인 쓰기를 한 커밋에 묶는다.
// normalizedInput에는 request_id와 OAuth 자격·표준 결과 외피를 포함하지 않는다.
// check는 잠금을 얻은 뒤 재생 전에 현재 인증·등급·행위자를 읽기 전용으로 검사한다.
// apply는 성공·결정적 거부를 도메인 JSON 객체로 반환한다. error는 미완료 실패로
// 예약·쓰기를 롤백하며 표준 SDK 외피와 내부 일시 장애는 보관하지 않는다.
type WriteExecutor interface {
	ExecuteWrite(ctx context.Context, accountID model.ID, requestID, tool string, normalizedInput []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error)
}

// Diagnostics는 고정 코드만 전달한다. 본문·URL·error·헤더·자격을 받지 않는다.
type Diagnostics interface {
	RecordDiagnostic(ctx context.Context, level, stage, result, reasonCode string) bool
}

// Dependencies는 조립 지점이 연결할 기능이다. 구현 패키지는 처리기가 import하지 않는다.
type Dependencies struct {
	Grades         perm.GradeStore
	Plans          Plans
	Graphs         GraphReader
	Nodes          NodeReader
	Hops           HopReader
	Relations      RelationReader
	Flow           FlowSearch
	Agents         AgentRegistry
	GraphWrites    GraphWriter
	NodeWrites     NodeWriter
	RelationWrites RelationWriter
	Writes         WriteExecutor
	Rejections     RejectedOperations
	Diagnostics    Diagnostics
}

// Validate는 외부 부작용 전에 조립 누락을 판정하며 의존 구현을 호출하지 않는다.
func (d Dependencies) Validate() error {
	for _, dependency := range []struct {
		name  string
		value any
	}{
		{"grades", d.Grades}, {"plans", d.Plans}, {"graphs", d.Graphs},
		{"nodes", d.Nodes}, {"hops", d.Hops}, {"relations", d.Relations},
		{"flow", d.Flow}, {"agents", d.Agents}, {"graph_writes", d.GraphWrites},
		{"node_writes", d.NodeWrites}, {"relation_writes", d.RelationWrites},
		{"writes", d.Writes}, {"rejections", d.Rejections}, {"diagnostics", d.Diagnostics},
	} {
		if dependency.value == nil {
			return fmt.Errorf("MCP 조립에 %s가 필요하다", dependency.name)
		}
	}
	return nil
}
