package main

import (
	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

// 소비자 계약과 실제 제공자의 결합은 업무 패키지 밖의 조립 경계에서 확인한다.
var (
	_ perm.GradeStore        = (*store.Store)(nil)
	_ search.Store           = (*store.Store)(nil)
	_ search.ConsistentStore = (*store.Store)(nil)
	_ search.Embedder        = (*index.Worker)(nil)
	_ mcp.Plans              = plan.AccountPlans{}
	_ mcp.GraphReader        = (*store.Store)(nil)
	_ mcp.NodeReader         = (*store.Store)(nil)
	_ mcp.HopReader          = (*store.Store)(nil)
	_ mcp.RelationReader     = (*store.Store)(nil)
	_ mcp.FlowSearch         = (*search.Service)(nil)
	_ mcp.Diagnostics        = (*diagnostic.Reporter)(nil)
	_ search.Diagnostics     = (*diagnostic.Reporter)(nil)
	_ store.Diagnostics      = (*diagnostic.Reporter)(nil)
	_ authz.Diagnostics      = (*diagnostic.Reporter)(nil)
)
