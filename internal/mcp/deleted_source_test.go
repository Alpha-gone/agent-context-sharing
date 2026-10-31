package mcp

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

type duplicateSourceWriter struct {
	NodeWriter
	node model.Context
}

func (w duplicateSourceWriter) CreateNode(context.Context, model.ID, model.Context, Actor, plan.Limits) (model.Context, error) {
	return w.node, nil
}

func TestCreateDeletedSourceExplainsExistingState(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "deleted"}[deleted], func(t *testing.T) {
			args := catalogSample("node_create")
			node, err := newNode(args, Principal{AccountID: argumentID(args, "agent_id")})
			if err != nil {
				t.Fatal(err)
			}
			if deleted {
				node.DeletedAt = new(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC))
			}
			data, err := applyWrite(t.Context(), Dependencies{NodeWrites: duplicateSourceWriter{node: node}}, Principal{AccountID: node.CreatedBy}, "node_create", args, plan.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := writeResult(data)
			if err != nil || result.IsError {
				t.Fatalf("duplicate response: %v", err)
			}
			var response map[string]any
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			returned := response["node"].(map[string]any)
			if returned["context_id"] != node.ID.String() {
				t.Fatal("wrong existing source")
			}
			if deleted {
				if returned["deleted_at"] == nil || response["creation_status"] != "existing_deleted" {
					t.Fatal("missing deleted state")
				}
				warning, _ := response["warning"].(string)
				for _, required := range []string{"새 생성이나 자동 복구가 아니며", "활성 근거", "node_restore", "웹", "request_id", "node_get"} {
					if !strings.Contains(warning, required) {
						t.Fatalf("missing guidance %q", required)
					}
				}
			} else if _, exists := response["creation_status"]; exists || response["warning"] != nil {
				t.Fatal("active response changed")
			}
		})
	}
	catalog := mustCatalog(t)
	for _, definition := range catalog.definitions {
		if definition.tool.Name == "node_create" && !strings.Contains(definition.tool.Description, "existing_deleted") {
			t.Fatal("tool description missing state")
		}
	}
}
