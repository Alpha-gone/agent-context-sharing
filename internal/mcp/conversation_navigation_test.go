package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"maps"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestConversationNavigationCreateContract(t *testing.T) {
	catalog := mustCatalog(t)
	source := catalogSample("node_create")
	source["conversation_id"], source["turn_index"] = "conversation-303", 0
	event := eventCatalogSample()
	event["conversation_id"], event["previous_event_id"] = "conversation-303", catalogID
	summary := derivedCatalogSample()
	summary["derivation_kind"], summary["summary_scope"], summary["navigation_refs"] = "summary", "global", []string{catalogID}
	for _, sample := range []map[string]any{source, event, summary} {
		args := validateCatalogSample(t, catalog, "node_create", sample, "")
		node, err := newNode(args, Principal{})
		if err != nil {
			t.Fatal(err)
		}
		output := nodeObject(node)
		if _, leaked := output["previous_event_id"]; leaked {
			t.Fatal("create-only predecessor leaked")
		}
		if node.Source != nil && (node.Source.TurnIndex == nil || *node.Source.TurnIndex != 0 || output["conversation_id"] != "conversation-303") {
			t.Fatal("source order missing")
		}
		if node.Event != nil && (node.Event.PreviousEventID == nil || output["conversation_id"] != "conversation-303") {
			t.Fatal("event create arguments missing")
		}
		if node.Derived != nil && len(node.Derived.NavigationRefs) != 1 {
			t.Fatal("navigation missing")
		}
	}
	for _, sample := range []struct {
		name  string
		base  map[string]any
		patch func(map[string]any)
		field string
	}{
		{"missing turn", source, func(v map[string]any) { delete(v, "turn_index") }, "turn_index"},
		{"missing conversation", source, func(v map[string]any) { delete(v, "conversation_id") }, "conversation_id"},
		{"wrong channel", source, func(v map[string]any) { v["source_ref"] = map[string]any{"channel": "file", "locator": "file:test"} }, "layer"},
		{"blank conversation", source, func(v map[string]any) { v["conversation_id"] = "  " }, "conversation_id"},
		{"long conversation", source, func(v map[string]any) { v["conversation_id"] = strings.Repeat("a", 129) }, "conversation_id"},
		{"negative turn", source, func(v map[string]any) { v["turn_index"] = -1 }, "turn_index"},
		{"fractional turn", source, func(v map[string]any) { v["turn_index"] = 0.5 }, "turn_index"},
		{"predecessor conversation", event, func(v map[string]any) { delete(v, "conversation_id") }, "conversation_id"},
		{"event turn", event, func(v map[string]any) { v["turn_index"] = 1 }, "layer"},
		{"source navigation", source, func(v map[string]any) { v["navigation_refs"] = []string{} }, "layer"},
		{"non summary navigation", summary, func(v map[string]any) { v["derivation_kind"] = "proposition"; delete(v, "summary_scope") }, "layer"},
		{"duplicate navigation", summary, func(v map[string]any) { v["navigation_refs"] = []string{catalogID, catalogID} }, "navigation_refs"},
		{"too many navigation", summary, func(v map[string]any) { v["navigation_refs"] = strings.Fields(strings.Repeat(catalogID+" ", 101)) }, "navigation_refs"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			v := maps.Clone(sample.base)
			sample.patch(v)
			validateCatalogSample(t, catalog, "node_create", v, sample.field)
		})
	}
	summary["navigation_refs"] = []string{}
	validateCatalogSample(t, catalog, "node_create", summary, "")
	for _, field := range []string{"conversation_id", "turn_index", "navigation_refs", "previous_event_id"} {
		v := catalogSample("node_update")
		v[field] = nil
		validateCatalogSample(t, catalog, "node_update", v, field)
	}
	get := catalogSample("node_get")
	get["traversal_filter"] = []string{"navigation_refs"}
	validateCatalogSample(t, catalog, "node_get", get, "")
	id, _ := model.ParseID(catalogID)
	refs, rels, err := edgeObjects(map[model.ID]bool{id: true}, []model.HopEdge{{FromID: id, ToID: id, Kind: "navigation_refs"}})
	if err != nil || len(refs) != 1 || len(rels) != 0 || refs[0]["reference_type"] != "navigation_refs" {
		t.Fatalf("navigation response: %v %v", refs, err)
	}
}

func TestConversationNavigationWriteFingerprints(t *testing.T) {
	var fingerprints [][]byte
	executor := writeExecutorFunc(func(ctx context.Context, account model.ID, key, tool string, input []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error) {
		fingerprints = append(fingerprints, bytes.Clone(input))
		return apply(ctx)
	})
	runner, err := NewWriteRunner(executor, agentRegistryStub{})
	if err != nil {
		t.Fatal(err)
	}
	account, _ := model.NewID()
	principal := Principal{AccountID: account, ClientID: "client"}
	for _, entry := range []any{nil, catalogID, strings.ReplaceAll(strings.ToUpper(catalogID), "-", "")} {
		args := catalogSample("graph_update")
		args["entry_context_id"] = entry
		data, _ := json.Marshal(args)
		if _, err := runner.Execute(t.Context(), principal, "graph_update", data, func(context.Context) error { return nil }, func(context.Context) ([]byte, error) { return []byte(`{"ok":true}`), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Equal(fingerprints[0], fingerprints[1]) || !bytes.Equal(fingerprints[1], fingerprints[2]) {
		t.Fatal("entry null or UUID fingerprint normalization")
	}
	for _, field := range []string{"conversation_id", "turn_index", "previous_event_id", "navigation_refs"} {
		sample := catalogSample("node_create")
		switch field {
		case "conversation_id", "turn_index":
			sample["conversation_id"], sample["turn_index"] = "chat", 0
		case "previous_event_id":
			sample = eventCatalogSample()
			sample["conversation_id"], sample[field] = "chat", catalogID
		case "navigation_refs":
			sample = derivedCatalogSample()
			sample["derivation_kind"], sample["summary_scope"], sample[field] = "summary", "global", []string{catalogID}
		}
		changed := maps.Clone(sample)
		switch field {
		case "conversation_id":
			changed[field] = "other-chat"
		case "turn_index":
			changed[field] = 1
		case "previous_event_id":
			id, _ := model.NewID()
			changed[field] = id.String()
		case "navigation_refs":
			changed[field] = []string{}
		}
		for _, args := range []map[string]any{sample, changed} {
			data, _ := json.Marshal(args)
			if _, err := runner.Execute(t.Context(), principal, "node_create", data, func(context.Context) error { return nil }, func(context.Context) ([]byte, error) { return []byte(`{"ok":true}`), nil }); err != nil {
				t.Fatal(err)
			}
		}
		count := len(fingerprints)
		if bytes.Equal(fingerprints[count-2], fingerprints[count-1]) {
			t.Fatalf("fingerprint excludes %s", field)
		}
	}
}
