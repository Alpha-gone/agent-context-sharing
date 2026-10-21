package diagnostic

import "strings"

// Fields는 종류별 허용 목록이다. 확장 map·인터페이스·원문 문자열·포인터를
// 받지 않으며 문자열은 고정 목록·확인한 UUIDv7·사전 등록한 공개 client 이름으로 제한한다.
type Fields struct {
	Identity   IdentityFields   `json:"identity,omitzero"`
	Request    RequestFields    `json:"request,omitzero"`
	Auth       AuthFields       `json:"auth,omitzero"`
	Search     SearchFields     `json:"search,omitzero"`
	Index      IndexFields      `json:"index,omitzero"`
	Management ManagementFields `json:"management,omitzero"`
	System     SystemFields     `json:"system,omitzero"`
}

type IdentityFields struct {
	AccountID  string `json:"account_id,omitempty"`
	GraphID    string `json:"graph_id,omitempty"`
	ContextID  string `json:"context_id,omitempty"`
	RelationID string `json:"relation_id,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	GrantID    string `json:"grant_id,omitempty"`
}

type RequestFields struct {
	Route           string `json:"route,omitempty"`
	Method          string `json:"method,omitempty"`
	Tool            string `json:"tool,omitempty"`
	Status          uint16 `json:"status,omitzero"`
	RPCCode         int32  `json:"rpc_code,omitzero"`
	LatencyMS       uint64 `json:"latency_ms,omitzero"`
	ValidationField string `json:"validation_field,omitempty"`
}
type AuthFields struct {
	ValidationField string `json:"validation_field,omitempty"`
}
type SearchFields struct {
	Channel         string `json:"channel,omitempty"`
	Path            string `json:"path,omitempty"`
	SelectionReason string `json:"selection_reason,omitempty"`
	Candidates      uint64 `json:"candidates,omitzero"`
	Nodes           uint64 `json:"nodes,omitzero"`
	Edges           uint64 `json:"edges,omitzero"`
	Characters      uint64 `json:"characters,omitzero"`
	LatencyMS       uint64 `json:"latency_ms,omitzero"`
	Truncated       bool   `json:"truncated,omitzero"`
}
type IndexFields struct {
	TaskID         string `json:"task_id,omitempty"`
	Generation     uint64 `json:"generation,omitzero"`
	Attempts       uint64 `json:"attempts,omitzero"`
	LeaseResult    string `json:"lease_result,omitempty"`
	ProviderStatus uint16 `json:"provider_status,omitzero"`
	LatencyMS      uint64 `json:"latency_ms,omitzero"`
}
type ManagementFields struct {
	Operation       string `json:"operation,omitempty"`
	ValidationField string `json:"validation_field,omitempty"`
	RelationType    string `json:"relation_type,omitempty"`
	ExpectedLayer   string `json:"expected_layer,omitempty"`
	ActualLayer     string `json:"actual_layer,omitempty"`
	CommitState     string `json:"commit_state,omitempty"`
}
type SystemFields struct {
	Job                  string `json:"job,omitempty"`
	ConfigurationVersion uint64 `json:"configuration_version,omitzero"`
	MigrationNumber      uint64 `json:"migration_number,omitzero"`
	Processed            uint64 `json:"processed,omitzero"`
}

func (fields Fields) valid(kind Kind, verified bool) bool {
	id := fields.Identity
	if !verified && (id != (IdentityFields{}) || fields.Index.TaskID != "") {
		return false
	}
	for _, value := range []string{id.AccountID, id.GraphID, id.ContextID, id.RelationID, id.GrantID, fields.Index.TaskID} {
		if value != "" && !validID(value) {
			return false
		}
	}
	// client_id는 운영자가 사전 등록한 공개 이름만 전달한다. 자유 입력은 금지한다.
	if id.ClientID != "" && !validInstance(id.ClientID) {
		return false
	}
	if kind != Request && fields.Request != (RequestFields{}) || kind != Auth && fields.Auth != (AuthFields{}) ||
		kind != Search && fields.Search != (SearchFields{}) || kind != Index && fields.Index != (IndexFields{}) ||
		kind != Management && fields.Management != (ManagementFields{}) || kind != System && fields.System != (SystemFields{}) {
		return false
	}
	if kind != Auth && (id.ClientID != "" || id.GrantID != "") {
		return false
	}
	if kind != Index && kind != Management && (id.ContextID != "" || id.RelationID != "") {
		return false
	}
	if kind == Index && id.RelationID != "" || kind == System && id != (IdentityFields{}) {
		return false
	}
	r, a, s, i, m, y := fields.Request, fields.Auth, fields.Search, fields.Index, fields.Management, fields.System
	return optional(r.Route, "login register logout authorize token connections graphs graph_detail graph_access graph_deletion teams audit operator_restores mcp discovery health unknown") &&
		optional(r.Method, "GET HEAD POST PUT PATCH DELETE OPTIONS CONNECT TRACE unknown") && optional(r.Tool, tools) && status(r.Status) && rpcCode(r.RPCCode) && optional(r.ValidationField, validationFields) &&
		optional(a.ValidationField, validationFields) && optional(s.Channel, "semantic keyword time graph global_summary") && optional(s.Path, "direct local global") &&
		optional(s.SelectionReason, "no_relevant_local event_anchor cross_channel_agreement semantic_separation ambiguous_local global_summary_absent forced legacy_auto explicit_scope") &&
		optional(i.LeaseResult, "acquired stale released expired unavailable") && status(i.ProviderStatus) &&
		optional(m.Operation, operations) && optional(m.ValidationField, validationFields) && optional(m.RelationType, "precedes causes part_of relates_to") &&
		optional(m.ExpectedLayer, "source derived event") && optional(m.ActualLayer, "source derived event") && optional(m.CommitState, "committed rolled_back unknown") &&
		optional(y.Job, "startup shutdown database_ping migration diagnostic_prepare diagnostic_rotate diagnostic_cleanup diagnostic_flush index_worker grace_expiry relation_cleanup audit_cleanup idempotency_cleanup revocation_cleanup rate_window_cleanup embedding_tier oauth_cleanup")
}

func status(value uint16) bool { return value == 0 || value >= 100 && value <= 599 }
func rpcCode(value int32) bool {
	switch value {
	case 0, -32700, -32600, -32601, -32602, -32603, -32000:
		return true
	default:
		return false
	}
}
func optional(value, values string) bool { return value == "" || listed(value, values) }
func listed(value, values string) bool {
	if value == "" {
		return false
	}
	for item := range strings.SplitSeq(values, " ") {
		if value == item {
			return true
		}
	}
	return false
}

const results = "completed failed rejected skipped pending committed rolled_back unknown"
const reasons = "completed invalid_argument invalid_credential authentication_unavailable database_unavailable store_unavailable target_unavailable search_failed expired individually_revoked family_revoked inactive_expired reuse_detected binding_violation permission_denied not_found constraint_violation layer_mismatch time_invalid cycle_detected commit_unknown canceled deadline_exceeded channel_unavailable provider_unavailable retry_exhausted stale_generation lease_lost queue_full file_unavailable lock_unavailable capacity_exceeded partial_write shutdown_timeout"
const tools = "agent_register graph_list graph_create graph_get graph_update node_create node_get node_update node_discard node_restore context_flow_get relation_list relation_confirm relation_discard"
const operations = "graph_create graph_update node_create node_update node_supersede node_discard node_restore node_keep relation_confirm relation_discard grade_grant grade_revoke team_create team_update team_delete team_restore restore_request operator_restore"
const validationFields = "arguments account_id agent_id graph_id context_id relation_id from_context_id to_context_id client_id grant_id request_id username password scope work_context body layer relation_type occurred_at started_at ended_at version cursor action code redirect_uri code_verifier refresh_token"

func stages(kind Kind) string {
	switch kind {
	case Request:
		return "receive validation authentication permission dispatch response write"
	case Auth:
		return "validation registration login logout authorization code_exchange token_issue token_validation web_session_validation refresh revocation connections_list connections_revoke_grant connections_revoke_agent write"
	case Search:
		return "validation channel routing hop selection budget flow write"
	case Index:
		return "validation enqueue acquire provider apply retry lease write"
	case Management:
		return "validation argument_validation actor_binding permission endpoint_load layer_check time_check cycle_check write audit commit"
	case System:
		return "validation startup shutdown database_ping worker migration prepare rotate cleanup flush write"
	default:
		return ""
	}
}

// ValidationField는 기존 검증 오류의 필드 이름을 허용 목록으로 좁힌다. 입력값은 받지 않는다.
func ValidationField(name string) string {
	if listed(name, validationFields) {
		return name
	}
	return "arguments"
}
