package mcp

import (
	"strings"
	"testing"

	"agent_context_sharing/internal/perm"
)

// 어떤 의존도 호출되면 실패하는 대역으로 조립 검증의 무부작용을 확인한다.
type dependencyStub struct {
	perm.GradeStore
	Plans
	GraphReader
	NodeReader
	HopReader
	RelationReader
	FlowSearch
	AgentRegistry
	GraphWriter
	NodeWriter
	RelationWriter
	WriteExecutor
	Diagnostics
}

func TestDependenciesValidateWithoutCallingProviders(t *testing.T) {
	stub := &dependencyStub{}
	dependencies := Dependencies{
		Grades: stub, Plans: stub, Graphs: stub, Nodes: stub, Hops: stub,
		Relations: stub, Flow: stub, Agents: stub, GraphWrites: stub,
		NodeWrites: stub, RelationWrites: stub, Writes: stub, Diagnostics: stub,
	}
	if err := dependencies.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name   string
		remove func(*Dependencies)
	}{
		{"grades", func(d *Dependencies) { d.Grades = nil }},
		{"plans", func(d *Dependencies) { d.Plans = nil }},
		{"graphs", func(d *Dependencies) { d.Graphs = nil }},
		{"nodes", func(d *Dependencies) { d.Nodes = nil }},
		{"hops", func(d *Dependencies) { d.Hops = nil }},
		{"relations", func(d *Dependencies) { d.Relations = nil }},
		{"flow", func(d *Dependencies) { d.Flow = nil }},
		{"agents", func(d *Dependencies) { d.Agents = nil }},
		{"graph_writes", func(d *Dependencies) { d.GraphWrites = nil }},
		{"node_writes", func(d *Dependencies) { d.NodeWrites = nil }},
		{"relation_writes", func(d *Dependencies) { d.RelationWrites = nil }},
		{"writes", func(d *Dependencies) { d.Writes = nil }},
		{"diagnostics", func(d *Dependencies) { d.Diagnostics = nil }},
	} {
		t.Run(sample.name, func(t *testing.T) {
			missing := dependencies
			sample.remove(&missing)
			err := missing.Validate()
			if err == nil || !strings.Contains(err.Error(), sample.name) {
				t.Fatalf("미연결 의존: %v", err)
			}
		})
	}
}
