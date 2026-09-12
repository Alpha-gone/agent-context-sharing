package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

// Operations는 MCP 처리기가 필요한 접근 계층의 좁은 계약이다.
type Operations interface {
	perm.GradeStore
	ListGraphs(context.Context, model.ID, model.GraphListFilter, string, int) ([]model.GraphListItem, string, error)
	CreateGraphWithOwner(context.Context, model.Graph) (model.Graph, error)
	Graph(context.Context, model.ID) (model.Graph, error)
	UpdateGraph(context.Context, model.ID, int64, string, string) (model.Graph, error)
	Context(context.Context, model.ID, model.ID) (model.Context, error)
	HopContexts(context.Context, model.ID, model.ID, int, string, []string, int) (store.HopResult, error)
	CreateContextWithOperation(context.Context, model.ID, model.Context, []model.ID, *store.OperationRecord) (model.Context, error)
	UpdateContextWithOperation(context.Context, model.ID, int64, model.Context, *store.OperationRecord) (model.Context, error)
	DiscardContext(context.Context, model.ID, model.ID, *store.OperationRecord) (model.Context, error)
	RestoreContext(context.Context, model.ID, model.ID, *store.OperationRecord) (model.Context, error)
	HasAppliedDiscard(context.Context, model.ID, model.ID) (bool, error)
	RecordRejectedOperation(context.Context, store.OperationRecord, string) error
	OwnedGraphCount(context.Context, model.ID) (int, error)
	TryIncrementRequestRate(context.Context, model.ID, time.Time, int) (int, bool, error)
}

// NewHandler는 전송 계층이 검증한 tools/call을 핵심 그래프·노드 연산으로 분배한다.
//
// logger는 거부 기록 실패처럼 연산을 되돌리지는 않지만 감사 근거에 구멍을 내는 사건을
// 남기는 데만 쓴다. nil이면 기본 로거를 쓴다.
func NewHandler(operations Operations, accountPlans plan.AccountPlans, logger *slog.Logger) CallFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, accountID model.ID, name string, arguments map[string]any) (ToolResult, error) {
		if operations == nil {
			return ToolResult{}, fmt.Errorf("MCP 처리 접근 계층이 없다")
		}
		handler := handler{operations: operations, limits: accountPlans.For(accountID), logger: logger}
		return handler.call(ctx, accountID, name, arguments)
	}
}

type handler struct {
	operations Operations
	limits     plan.Limits
	logger     *slog.Logger
}

func (h handler) call(ctx context.Context, accountID model.ID, name string, arguments map[string]any) (ToolResult, error) {
	switch name {
	case "graph_list":
		return h.listGraphs(ctx, accountID, arguments)
	case "graph_create":
		return h.createGraph(ctx, accountID, arguments)
	case "graph_get":
		return h.getGraph(ctx, accountID, arguments)
	case "graph_update":
		return h.updateGraph(ctx, accountID, arguments)
	case "node_create":
		return h.createNode(ctx, accountID, arguments)
	case "node_get":
		return h.getNode(ctx, accountID, arguments)
	case "node_update":
		return h.updateNode(ctx, accountID, arguments)
	case "node_discard":
		return h.discardNode(ctx, accountID, arguments)
	case "node_restore":
		return h.restoreNode(ctx, accountID, arguments)
	default:
		return ToolResult{}, &Error{Code: "not_supported"}
	}
}

func (h handler) listGraphs(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	pageSize := h.limits.GraphPage.Default
	if value, ok := arguments["page_size"]; ok {
		pageSize = int(value.(float64))
	}
	if err := plan.CheckRequest("graph_page", int64(pageSize), int64(h.limits.GraphPage.Maximum)); err != nil {
		return ToolResult{}, limitError(err)
	}
	filter := model.GraphListFilter{Name: optionalString(arguments, "name_filter"), Grades: graphGrades(arguments["grade_filter"])}
	graphs, cursor, err := h.operations.ListGraphs(ctx, accountID, filter, optionalString(arguments, "cursor"), pageSize)
	if err != nil {
		return ToolResult{}, mapError(err)
	}
	return result(map[string]any{"graphs": graphListItems(graphs), "next_cursor": cursor}), nil
}

func (h handler) createGraph(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	if err := h.checkGraphCount(ctx, accountID); err != nil {
		return ToolResult{}, err
	}
	graphID, err := model.NewID()
	if err != nil {
		return ToolResult{}, fmt.Errorf("그래프 식별자 생성: %w", err)
	}
	now := time.Now().UTC()
	graph, err := h.operations.CreateGraphWithOwner(ctx, model.Graph{
		ID: graphID, Name: arguments["name"].(string), Description: optionalString(arguments, "description"), CreatedBy: accountID,
		CreatedAt: now, LastActivityAt: now, Version: 1,
	})
	if err != nil {
		return ToolResult{}, mapError(err)
	}
	return result(graphValue(graph)), nil
}

func (h handler) getGraph(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	graphID := argumentID(arguments, "graph_id")
	graph, err := h.requireActiveGraph(ctx, graphID, accountID, model.GraphGradeViewer)
	if err != nil {
		return ToolResult{}, err
	}
	return result(graphValue(graph)), nil
}

func (h handler) updateGraph(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	graphID := argumentID(arguments, "graph_id")
	if _, err := h.requireActiveGraph(ctx, graphID, accountID, model.GraphGradeEditor); err != nil {
		return ToolResult{}, err
	}
	graph, err := h.operations.UpdateGraph(ctx, graphID, int64(arguments["expected_version"].(float64)), arguments["name"].(string), optionalString(arguments, "description"))
	if err != nil {
		return ToolResult{}, mapError(err)
	}
	return result(graphValue(graph)), nil
}

func (h handler) createNode(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	graphID := argumentID(arguments, "graph_id")
	graph, err := h.requireActiveGraph(ctx, graphID, accountID, model.GraphGradeEditor)
	if err != nil {
		return ToolResult{}, err
	}
	contextID, err := model.NewID()
	if err != nil {
		return ToolResult{}, fmt.Errorf("컨텍스트 식별자 생성: %w", err)
	}
	agentID := argumentID(arguments, "created_by_agent")
	value, references, err := newContext(graphID, contextID, accountID, agentID, arguments)
	if err != nil {
		return ToolResult{}, invalidArgument(err)
	}
	if err := value.Validate(); err != nil {
		return ToolResult{}, invalidArgument(err)
	}
	if value.Layer == model.LayerDerived {
		if err := model.ValidateDerivedReferences(references); err != nil {
			return ToolResult{}, invalidArgument(err)
		}
	}
	if err := h.checkWrite(ctx, accountID); err != nil {
		return ToolResult{}, err
	}
	if err := h.checkStoredCharacters(graph, int64(len([]rune(value.Body)))); err != nil {
		return ToolResult{}, err
	}
	operation := operationRecord(store.OperationAdd, graphID, contextID, accountID, agentID, arguments)
	stored, err := h.operations.CreateContextWithOperation(ctx, graphID, value, references, &operation)
	if err != nil {
		h.recordRejected(ctx, operation, err)
		return ToolResult{}, mapError(err)
	}
	return result(contextValue(stored)), nil
}

func (h handler) getNode(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	graphID := argumentID(arguments, "graph_id")
	if _, err := h.requireActiveGraph(ctx, graphID, accountID, model.GraphGradeViewer); err != nil {
		return ToolResult{}, err
	}
	if err := plan.CheckRequest("max_hops", int64(arguments["hops"].(float64)), int64(h.limits.MaxHops)); err != nil {
		return ToolResult{}, limitError(err)
	}
	traversalFilter := stringValues(arguments["traversal_filter"])
	hops, err := h.operations.HopContexts(ctx, graphID, argumentID(arguments, "context_id"), int(arguments["hops"].(float64)), optionalString(arguments, "direction"), traversalFilter, h.limits.MaxHopNodes)
	if err != nil {
		return ToolResult{}, mapError(err)
	}
	contexts := make([]any, 0, len(hops.Contexts))
	for _, value := range hops.Contexts {
		contexts = append(contexts, contextValue(value))
	}
	references, relations := hopEdges(hops.Edges)
	response := map[string]any{"contexts": contexts, "references": references, "relations": relations}
	// 절단은 오류가 아니라 「정상 응답의 부분 상태」의 표시이므로 확정된 이름으로 싣는다.
	if hops.Truncated {
		response["result_truncated"] = map[string]any{"truncated_hop": hops.Boundary}
	}
	return result(response), nil
}

func (h handler) updateNode(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	graphID := argumentID(arguments, "graph_id")
	graph, err := h.requireActiveGraph(ctx, graphID, accountID, model.GraphGradeEditor)
	if err != nil {
		return ToolResult{}, err
	}
	contextID := argumentID(arguments, "context_id")
	previous, err := h.operations.Context(ctx, graphID, contextID)
	if err != nil {
		return ToolResult{}, mapError(err)
	}
	if previous.DeletedAt != nil {
		return ToolResult{}, &Error{Code: "not_found"}
	}
	next, err := updatedContext(previous, arguments)
	if err != nil {
		return ToolResult{}, invalidArgument(err)
	}
	if err := model.ValidateUpdate(previous, next); err != nil {
		return ToolResult{}, invalidArgument(err)
	}
	if err := h.checkWrite(ctx, accountID); err != nil {
		return ToolResult{}, err
	}
	if err := h.checkStoredCharacters(graph, int64(len([]rune(next.Body))-len([]rune(previous.Body)))); err != nil {
		return ToolResult{}, err
	}
	agentID := argumentID(arguments, "created_by_agent")
	operation := operationRecord(store.OperationUpdate, graphID, contextID, accountID, agentID, arguments)
	operation.TargetVersion = previous.Version
	stored, err := h.operations.UpdateContextWithOperation(ctx, graphID, int64(arguments["expected_version"].(float64)), next, &operation)
	if err != nil {
		h.recordRejected(ctx, operation, err)
		return ToolResult{}, mapError(err)
	}
	return result(contextValue(stored)), nil
}

func (h handler) discardNode(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	graphID, contextID := argumentID(arguments, "graph_id"), argumentID(arguments, "context_id")
	if _, err := h.requireActiveGraph(ctx, graphID, accountID, model.GraphGradeEditor); err != nil {
		return ToolResult{}, err
	}
	previous, err := h.operations.Context(ctx, graphID, contextID)
	if err != nil {
		return ToolResult{}, mapError(err)
	}
	agentID := argumentID(arguments, "created_by_agent")
	operation := operationRecord(store.OperationDiscard, graphID, contextID, accountID, agentID, arguments)
	operation.TargetVersion = previous.Version
	if previous.DeletedAt != nil {
		h.recordRejected(ctx, operation, errors.New("컨텍스트가 이미 폐기됐다"))
		return ToolResult{}, &Error{Code: "invalid_argument"}
	}
	if err := h.checkWrite(ctx, accountID); err != nil {
		return ToolResult{}, err
	}
	stored, err := h.operations.DiscardContext(ctx, graphID, contextID, &operation)
	if err != nil {
		h.recordRejected(ctx, operation, err)
		return ToolResult{}, mapError(err)
	}
	return result(contextValue(stored)), nil
}

func (h handler) restoreNode(ctx context.Context, accountID model.ID, arguments map[string]any) (ToolResult, error) {
	graphID, contextID := argumentID(arguments, "graph_id"), argumentID(arguments, "context_id")
	if _, err := h.requireActiveGraph(ctx, graphID, accountID, model.GraphGradeEditor); err != nil {
		return ToolResult{}, err
	}
	previous, err := h.operations.Context(ctx, graphID, contextID)
	if err != nil {
		return ToolResult{}, mapError(err)
	}
	agentID := argumentID(arguments, "created_by_agent")
	operation := operationRecord(store.OperationUpdate, graphID, contextID, accountID, agentID, arguments)
	operation.TargetVersion = previous.Version
	if previous.DeletedAt == nil {
		h.recordRejected(ctx, operation, errors.New("컨텍스트가 활성 상태다"))
		return ToolResult{}, &Error{Code: "invalid_argument"}
	}
	wasDiscarded, err := h.operations.HasAppliedDiscard(ctx, graphID, contextID)
	if err != nil {
		return ToolResult{}, mapError(err)
	}
	if !wasDiscarded {
		rejected := &Error{Code: "not_supported", Data: map[string]any{"alternative_channel": "web"}}
		h.recordRejected(ctx, operation, rejected)
		return ToolResult{}, rejected
	}
	if err := h.checkWrite(ctx, accountID); err != nil {
		return ToolResult{}, err
	}
	stored, err := h.operations.RestoreContext(ctx, graphID, contextID, &operation)
	if err != nil {
		h.recordRejected(ctx, operation, err)
		return ToolResult{}, mapError(err)
	}
	return result(contextValue(stored)), nil
}

// requireActiveGraph는 등급을 대조하고 소프트 삭제된 그래프를 거른 뒤 읽은 그래프 행을
// 돌려준다. 호출자가 같은 행을 다시 읽지 않게 하려는 것이다.
func (h handler) requireActiveGraph(ctx context.Context, graphID, accountID model.ID, required model.GraphGrade) (model.Graph, error) {
	if err := perm.Require(ctx, h.operations, graphID, accountID, required); err != nil {
		return model.Graph{}, mapError(err)
	}
	graph, err := h.operations.Graph(ctx, graphID)
	if err != nil {
		return model.Graph{}, mapError(err)
	}
	if graph.DeletedAt != nil {
		return model.Graph{}, &Error{Code: "not_found"}
	}
	return graph, nil
}

func (h handler) checkGraphCount(ctx context.Context, accountID model.ID) error {
	if h.limits.GraphsPerAccount == 0 {
		return nil
	}
	count, err := h.operations.OwnedGraphCount(ctx, accountID)
	if err != nil {
		return mapError(err)
	}
	if err := plan.CheckIncrease("graphs_per_account", int64(count), 1, int64(h.limits.GraphsPerAccount)); err != nil {
		return limitError(err)
	}
	return nil
}

func (h handler) checkWrite(ctx context.Context, accountID model.ID) error {
	count, allowed, err := h.operations.TryIncrementRequestRate(ctx, accountID, time.Now().UTC(), h.limits.WritesPerMinute)
	if err != nil {
		return mapError(err)
	}
	if allowed {
		return nil
	}
	return &Error{Code: "limit_exceeded", Data: map[string]any{"limit": "writes_per_minute", "current": count, "allowed": h.limits.WritesPerMinute}}
}

func (h handler) checkStoredCharacters(graph model.Graph, delta int64) error {
	if h.limits.StoredCharactersPerGraph == 0 {
		return nil
	}
	if err := plan.CheckIncrease("stored_characters_per_graph", graph.StoredChars, delta, h.limits.StoredCharactersPerGraph); err != nil {
		return limitError(err)
	}
	return nil
}

// recordRejected는 거부된 관리 연산을 되돌려진 트랜잭션 밖에 남긴다. 기록이 실패해도
// 이미 확정된 거부를 되돌리지 않지만, 조용히 지나가면 감사 근거의 구멍이 드러나지 않으므로
// 로그를 남긴다. `SDD.md`의 「관측성」이 판단 입력을 operation_log 밖으로 복제하지 못하게
// 했으므로 식별자와 분류만 남기고 본문과 판단 입력은 넣지 않는다.
func (h handler) recordRejected(ctx context.Context, operation store.OperationRecord, cause error) {
	reason := rejectReason(cause)
	if err := h.operations.RecordRejectedOperation(ctx, operation, reason); err != nil {
		h.logger.ErrorContext(ctx, "거부된 관리 연산 기록 실패",
			"graph_id", operation.GraphID.String(), "context_id", operation.ContextID.String(),
			"operation_kind", string(operation.Kind), "reject_reason", reason, "error", err.Error())
	}
}

func operationRecord(kind store.OperationKind, graphID, contextID, accountID, agentID model.ID, arguments map[string]any) store.OperationRecord {
	encoded, _ := json.Marshal(arguments)
	return store.OperationRecord{Kind: kind, GraphID: graphID, ContextID: contextID, TargetVersion: 1, JudgmentInput: string(encoded), AccountID: accountID, AgentID: agentID}
}

func rejectReason(cause error) string {
	if domain, ok := errors.AsType[*Error](cause); ok && validDomainCode(domain.Code) {
		return domain.Code
	}
	switch {
	case errors.Is(cause, store.ErrNotFound):
		return "not_found"
	default:
		if _, ok := errors.AsType[store.VersionConflictError](cause); ok {
			return "version_conflict"
		}
		return "invalid_argument"
	}
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if domain, ok := errors.AsType[*Error](err); ok {
		return domain
	}
	if errors.Is(err, store.ErrNotFound) {
		return &Error{Code: "not_found"}
	}
	if denied, ok := errors.AsType[perm.DeniedError](err); ok {
		return &Error{Code: "permission_denied", Data: map[string]any{"required_grade": denied.Required}}
	}
	if _, ok := errors.AsType[perm.NotFoundError](err); ok {
		return &Error{Code: "not_found"}
	}
	if conflict, ok := errors.AsType[store.VersionConflictError](err); ok {
		return &Error{Code: "version_conflict", Data: map[string]any{"current_version": conflict.Current}}
	}
	return &Error{Code: "internal"}
}

// invalidArgument는 `SDD.md`의 「오류 부가 정보」대로 검증이 지목한 필드를 함께 싣는다.
// 필드를 특정할 수 없는 검증만 코드만 돌려준다.
func invalidArgument(err error) error {
	if field, ok := errors.AsType[*argumentError](err); ok && field.Field != "" {
		return &Error{Code: "invalid_argument", Data: map[string]any{"field": field.Field}}
	}
	if field, ok := errors.AsType[model.FieldError](err); ok && field.Field != "" {
		return &Error{Code: "invalid_argument", Data: map[string]any{"field": field.Field}}
	}
	return &Error{Code: "invalid_argument"}
}

func limitError(err error) error {
	limit, ok := errors.AsType[plan.LimitError](err)
	if !ok {
		return &Error{Code: "internal"}
	}
	return &Error{Code: "limit_exceeded", Data: map[string]any{"limit": limit.Name, "current": limit.Current, "allowed": limit.Allowed}}
}

func result(value any) ToolResult {
	encoded, _ := json.Marshal(value)
	return ToolResult{Content: []Content{{Type: "text", Text: string(encoded)}}, StructuredContent: value}
}

func optionalString(arguments map[string]any, name string) string {
	value, _ := arguments[name].(string)
	return value
}

func argumentID(arguments map[string]any, name string) model.ID {
	id, _ := model.ParseID(arguments[name].(string))
	return id
}

func graphGrades(value any) []model.GraphGrade {
	items, _ := value.([]any)
	grades := make([]model.GraphGrade, len(items))
	for index, item := range items {
		grades[index] = model.GraphGrade(item.(string))
	}
	return grades
}

func stringValues(value any) []string {
	items, _ := value.([]any)
	values := make([]string, 0, len(items))
	for _, item := range items {
		values = append(values, item.(string))
	}
	return values
}

func hopEdges(edges []store.HopEdge) ([]any, []any) {
	references, relations := make([]any, 0), make([]any, 0)
	for _, edge := range edges {
		value := map[string]any{"from_context_id": edge.FromID.String(), "to_context_id": edge.ToID.String(), "type": edge.Kind}
		switch edge.Kind {
		case "derived_from", "supersedes", "has_member":
			references = append(references, value)
		default:
			relations = append(relations, value)
		}
	}
	return references, relations
}

func graphListItems(graphs []model.GraphListItem) []any {
	items := make([]any, 0, len(graphs))
	for _, graph := range graphs {
		items = append(items, map[string]any{"graph_id": graph.ID.String(), "name": graph.Name, "description": graph.Description, "last_activity_at": graph.LastActivityAt, "grade": graph.Grade})
	}
	return items
}

func graphValue(graph model.Graph) map[string]any {
	return map[string]any{"graph_id": graph.ID.String(), "name": graph.Name, "description": graph.Description, "created_by": graph.CreatedBy.String(), "created_at": graph.CreatedAt, "last_activity_at": graph.LastActivityAt, "stored_chars": graph.StoredChars, "version": graph.Version}
}

func contextValue(value model.Context) map[string]any {
	result := map[string]any{"context_id": value.ID.String(), "graph_id": value.GraphID.String(), "layer": string(value.Layer), "body": value.Body, "recorded_at": value.RecordedAt, "created_by": value.CreatedBy.String(), "created_by_agent": value.CreatedByAgent.String(), "version": value.Version}
	switch value.Layer {
	case model.LayerSource:
		result["source_ref"] = map[string]any{"channel": string(value.Source.Reference.Channel), "locator": value.Source.Reference.Locator}
		result["occurred_at"] = value.Source.OccurredAt
		result["origin_kind"] = string(value.Source.OriginKind)
	case model.LayerDerived:
		result["derived_from"] = idStrings(value.Derived.DerivedFrom)
		result["derivation_kind"] = string(value.Derived.Kind)
		if value.Derived.SummaryScope != "" {
			result["summary_scope"] = string(value.Derived.SummaryScope)
		}
		result["evidence_state"] = string(value.Derived.EvidenceState)
		if value.Derived.ConfidenceState != "" {
			result["confidence_state"] = string(value.Derived.ConfidenceState)
		}
		if value.Derived.ValidFrom != nil {
			result["valid_from"] = *value.Derived.ValidFrom
		}
		if value.Derived.ValidTo != nil {
			result["valid_to"] = *value.Derived.ValidTo
		}
		result["evidence_invalidated"] = value.Derived.EvidenceInvalidated
	case model.LayerEvent:
		result["member_refs"] = idStrings(value.Event.MemberIDs)
		result["event_start"] = value.Event.Start
		if value.Event.End != nil {
			result["event_end"] = *value.Event.End
		}
	}
	if value.DeletedAt != nil {
		result["deleted_at"] = *value.DeletedAt
	}
	return result
}

// idStrings는 식별자 목록을 응답 직렬화용 문자열로 바꾼다.
func idStrings(ids []model.ID) []string {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, id.String())
	}
	return values
}

func newContext(graphID, contextID, accountID, agentID model.ID, arguments map[string]any) (model.Context, []model.ID, error) {
	now := time.Now().UTC()
	value := model.Context{
		ID: contextID, GraphID: graphID, Layer: model.Layer(arguments["layer"].(string)), Body: arguments["body"].(string),
		RecordedAt: now, CreatedBy: accountID, CreatedByAgent: agentID, Version: 1,
	}
	switch value.Layer {
	case model.LayerSource:
		occurredAt, err := requiredTime(arguments, "occurred_at")
		if err != nil {
			return model.Context{}, nil, err
		}
		value.Source = &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannel(optionalString(arguments, "source_channel")), Locator: optionalString(arguments, "locator")}, OccurredAt: occurredAt, OriginKind: model.OriginKind(optionalString(arguments, "origin_kind"))}
	case model.LayerDerived:
		value.Derived = &model.DerivedAttributes{Kind: model.DerivationKind(optionalString(arguments, "derivation_kind")), SummaryScope: model.SummaryScope(optionalString(arguments, "summary_scope")), EvidenceState: model.EvidenceState(optionalString(arguments, "evidence_state")), ConfidenceState: model.ConfidenceState(optionalString(arguments, "confidence_state")), ValidFrom: optionalTime(arguments, "valid_from"), ValidTo: optionalTime(arguments, "valid_to")}
		return value, argumentIDs(arguments, "derived_from"), nil
	case model.LayerEvent:
		start, err := requiredTime(arguments, "start")
		if err != nil {
			return model.Context{}, nil, err
		}
		value.Event = &model.EventAttributes{MemberIDs: argumentIDs(arguments, "member_refs"), Start: start, End: optionalTime(arguments, "end")}
	default:
		return model.Context{}, nil, &argumentError{Field: "layer"}
	}
	return value, nil, nil
}

func updatedContext(previous model.Context, arguments map[string]any) (model.Context, error) {
	if previous.Layer == model.LayerSource {
		return model.Context{}, &argumentError{Field: "layer"}
	}
	// 계층 속성은 포인터이므로 얕은 복사만 하면 next를 고칠 때 previous도 함께 바뀌고
	// model.ValidateUpdate의 불변 검사가 같은 값을 양쪽에서 읽어 항상 통과한다.
	next := previous
	switch previous.Layer {
	case model.LayerDerived:
		derived := *previous.Derived
		next.Derived = &derived
	case model.LayerEvent:
		event := *previous.Event
		event.MemberIDs = slices.Clone(previous.Event.MemberIDs)
		next.Event = &event
	}
	next.Version++
	changed := false
	if body, ok := arguments["body"].(string); ok {
		next.Body = body
		changed = true
	}
	switch next.Layer {
	case model.LayerDerived:
		if confidence, ok := arguments["confidence_state"].(string); ok {
			next.Derived.ConfidenceState = model.ConfidenceState(confidence)
			changed = true
		}
		if _, ok := arguments["valid_from"]; ok {
			next.Derived.ValidFrom = optionalTime(arguments, "valid_from")
			changed = true
		}
		if _, ok := arguments["valid_to"]; ok {
			next.Derived.ValidTo = optionalTime(arguments, "valid_to")
			changed = true
		}
	case model.LayerEvent:
		if _, ok := arguments["member_refs"]; ok {
			next.Event.MemberIDs = argumentIDs(arguments, "member_refs")
			changed = true
		}
		if _, ok := arguments["start"]; ok {
			start, err := requiredTime(arguments, "start")
			if err != nil {
				return model.Context{}, err
			}
			next.Event.Start = start
			changed = true
		}
		if _, ok := arguments["end"]; ok {
			next.Event.End = optionalTime(arguments, "end")
			changed = true
		}
	}
	if !changed {
		return model.Context{}, fmt.Errorf("갱신할 값이 없다")
	}
	return next, nil
}

func requiredTime(arguments map[string]any, name string) (time.Time, error) {
	text, ok := arguments[name].(string)
	if !ok {
		return time.Time{}, &argumentError{Field: name}
	}
	value, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, &argumentError{Field: name}
	}
	return value.UTC(), nil
}

func optionalTime(arguments map[string]any, name string) *time.Time {
	text, ok := arguments[name].(string)
	if !ok {
		return nil
	}
	value, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return nil
	}
	value = value.UTC()
	return &value
}

func argumentIDs(arguments map[string]any, name string) []model.ID {
	items, _ := arguments[name].([]any)
	ids := make([]model.ID, 0, len(items))
	for _, item := range items {
		id, _ := model.ParseID(item.(string))
		ids = append(ids, id)
	}
	return ids
}
