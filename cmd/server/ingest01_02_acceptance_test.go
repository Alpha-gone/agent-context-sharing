package main

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent_context_sharing/internal/ingest"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

func TestINGEST02PermissionAndLimitsIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	limits, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"stored_characters_per_graph":1}}`, f.principal.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := newMCPWriteHandlers(f.database, limits)
	if err != nil {
		t.Fatal(err)
	}
	args := f.args("node_create")
	args["layer"], args["body"], args["origin_kind"], args["occurred_at"] = "source", "합성 제한 표본", "user_utterance", time.Now().UTC().Format(time.RFC3339)
	args["source_ref"] = map[string]any{"channel": "file", "locator": "file:///ingest02-limit"}
	result, err := callMCPWrite(t.Context(), handlers, f.principal, "node_create", args)
	value := writeContent(t, result, err, "limit_exceeded")
	if value["limit"] != "stored_characters_per_graph" {
		t.Fatal("MCP 한도 분류 불일치")
	}
	ownerToken, err := f.database.IssueIngestCredential(t.Context(), f.principal.AccountID, "합성")
	if err != nil {
		t.Fatal(err)
	}
	viewer, _ := model.NewID()
	if err := f.database.CreateAccount(t.Context(), store.Account{ID: viewer, LoginID: "ingest02_viewer", PasswordHash: "synthetic", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := f.database.GrantGraph(t.Context(), f.graph, f.principal.AccountID, viewer, store.GrantSubjectAccount, model.GraphGradeViewer); err != nil {
		t.Fatal(err)
	}
	viewerToken, err := f.database.IssueIngestCredential(t.Context(), viewer, "합성")
	if err != nil {
		t.Fatal(err)
	}
	viewerAgent, err := f.database.RegisterAgent(t.Context(), viewer, "viewer-client", "viewer_key_00001")
	if err != nil {
		t.Fatal(err)
	}
	viewerArgs := map[string]any{"graph_id": f.graph.String(), "agent_id": viewerAgent.String(), "judgment_input": "synthetic", "layer": "source", "body": "합성 제한 표본", "origin_kind": "user_utterance", "occurred_at": time.Now().UTC().Format(time.RFC3339), "source_ref": map[string]any{"channel": "file", "locator": "file:///ingest02-viewer"}}
	result, err = callMCPWrite(t.Context(), f.handlers, mcp.Principal{AccountID: viewer, ClientID: "viewer-client"}, "node_create", viewerArgs)
	writeContent(t, result, err, "permission_denied")
	handler := ingest.Handler{Backend: f.database, Plans: limits, Gap: time.Minute}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	payload, err := json.Marshal(map[string]any{"graph_id": f.graph.String(), "conversation_id": "ingest02-limit", "turns": []map[string]any{{"turn_index": 0, "speaker": "user", "occurred_at": time.Now().UTC().Format(time.RFC3339), "body": "합성 제한 표본"}}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(token string) (int, []byte) {
		t.Helper()
		req, e := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/ingest/conversation", bytes.NewReader(payload))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		body, e := io.ReadAll(res.Body)
		if e != nil {
			t.Fatal(e)
		}
		return res.StatusCode, body
	}
	status, body := call(ownerToken)
	if status != 200 || !bytes.Contains(body, []byte(`"reason":"limit_exceeded"`)) {
		t.Fatalf("수집 저장 한도: %d %s", status, body)
	}
	status, _ = call(viewerToken)
	if status != http.StatusNotFound {
		t.Fatalf("열람자 수집 거부: %d", status)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment_task WHERE graph_id=$1 AND conversation_id='ingest02-limit'`, f.graph.String()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("거부 요청 작업 잔존: %d %v", count, err)
	}
}
