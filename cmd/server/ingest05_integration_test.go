package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/ingest"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

type ingest05Sink struct {
	events   []diagnostic.Event
	recorder *diagnostic.Recorder
}

func (s *ingest05Sink) Emit(ctx context.Context, e diagnostic.Event) bool {
	s.events = append(s.events, e)
	return s.recorder.Emit(ctx, e)
}

func TestINGEST05ConversationFlowAndMeasurement(t *testing.T) {
	f := newMCPWriteFixture(t)
	writer, err := store.NewWithInference(t.Context(), f.pool.Config().ConnString(), "agent_context", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	source, err := diagnostic.NewSource("ingest05")
	if err != nil {
		t.Fatal(err)
	}
	owned, err := os.MkdirTemp(".", ".ingest05-log-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(owned); err != nil {
			t.Error(err)
		}
	})
	logDir, err := filepath.Abs(filepath.Join(owned, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := diagnostic.NewRecorder(t.Context(), logDir, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close(context.Background()) })
	sink := &ingest05Sink{recorder: recorder}
	index, err := diagnostic.NewReporter(diagnostic.Index, sink, source)
	if err != nil {
		t.Fatal(err)
	}
	management, err := diagnostic.NewReporter(diagnostic.Management, sink, source)
	if err != nil {
		t.Fatal(err)
	}
	writer.SetOperationDiagnostics(management, index)
	token, err := writer.IssueIngestCredential(t.Context(), f.principal.AccountID, "INGEST05_PRIVATE_LABEL")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(token))
	credentialHash := hex.EncodeToString(digest[:])
	credentialHashBase64 := base64.StdEncoding.EncodeToString(digest[:])
	_, agent, err := writer.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	turns := []struct{ speaker, body string }{{"user", "INGEST05_SYNTHETIC_USER_ONE"}, {"agent", "INGEST05_SYNTHETIC_AGENT_ONE"}, {"user", "INGEST05_SYNTHETIC_USER_TWO"}, {"agent", "INGEST05_SYNTHETIC_AGENT_TWO"}}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var baselineRequest, baselineResponse int
	var refs []string
	for i, item := range turns {
		args := f.args("node_create")
		kind := "user_utterance"
		if item.speaker == "agent" {
			kind = "agent_output"
		}
		args["layer"], args["body"], args["origin_kind"], args["occurred_at"] = "source", item.body, kind, at.Add(time.Duration(i)*time.Second).Format(time.RFC3339)
		args["source_ref"] = map[string]any{"channel": "conversation", "locator": fmt.Sprintf("conversation:///baseline05/%d", i)}
		args["conversation_id"], args["turn_index"] = "baseline05", i
		raw, err := json.Marshal(args, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		baselineRequest += len(raw)
		result, err := callMCPWrite(t.Context(), f.handlers, f.principal, "node_create", args)
		body, err2 := json.Marshal(result, json.Deterministic(true))
		if err2 != nil {
			t.Fatal(err2)
		}
		baselineResponse += len(body)
		value := writeContent(t, result, err, "")
		refs = append(refs, value["node"].(map[string]any)["context_id"].(string))
	}
	eventArgs := f.args("node_create")
	eventArgs["layer"], eventArgs["body"], eventArgs["member_refs"] = "event", turns[0].body+"\n"+turns[2].body, refs
	eventArgs["conversation_id"], eventArgs["event_start"], eventArgs["event_end"] = "baseline05", at.Format(time.RFC3339), at.Add(3*time.Second).Format(time.RFC3339)
	raw, err := json.Marshal(eventArgs, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	baselineRequest += len(raw)
	result, err := callMCPWrite(t.Context(), f.handlers, f.principal, "node_create", eventArgs)
	response, err2 := json.Marshal(result, json.Deterministic(true))
	if err2 != nil {
		t.Fatal(err2)
	}
	baselineResponse += len(response)
	writeContent(t, result, err, "")
	bodyTurns := make([]map[string]any, 0, len(turns))
	for i, item := range turns {
		bodyTurns = append(bodyTurns, map[string]any{"turn_index": i, "speaker": item.speaker, "body": item.body, "occurred_at": at.Add(time.Duration(i) * time.Second).Format(time.RFC3339)})
	}
	handler := ingest.Handler{Backend: writer, Gap: time.Minute}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	send := func(graph model.ID, credential string, payload []byte) (int, []byte) {
		t.Helper()
		data := payload
		if graph != f.graph {
			data = bytes.Replace(payload, []byte(f.graph.String()), []byte(graph.String()), 1)
		}
		req, e := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/ingest/conversation", bytes.NewReader(data))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		res, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, e := io.ReadAll(res.Body)
		if e != nil {
			t.Fatal(e)
		}
		return res.StatusCode, b
	}
	var ingestRequest, ingestResponse int
	var lastPayload []byte
	for _, turn := range bodyTurns {
		payload, err := json.Marshal(map[string]any{"graph_id": f.graph.String(), "conversation_id": "ingest05-private-session", "turns": []any{turn}}, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		lastPayload = payload
		status, response := send(f.graph, token, payload)
		if status != 200 {
			t.Fatalf("수집 HTTP 상태 %d", status)
		}
		ingestRequest += len(payload)
		ingestResponse += len(response)
		for _, forbidden := range []string{token, credentialHash, credentialHashBase64, "INGEST05_PRIVATE_LABEL", "INGEST05_SYNTHETIC", "ingest05-private-session"} {
			if bytes.Contains(response, []byte(forbidden)) {
				t.Fatal("수집 응답 금지 값")
			}
		}
	}
	otherAccount, _ := model.NewID()
	other, _ := model.NewID()
	if err := writer.CreateAccount(t.Context(), store.Account{ID: otherAccount, LoginID: "ingest05_foreign", PasswordHash: "synthetic", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.CreateGraphWithOwner(t.Context(), model.Graph{ID: other, Name: "foreign", CreatedBy: otherAccount, CreatedAt: at, LastActivityAt: at, Version: 1}, store.WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	if status, _ := send(other, token, lastPayload); status != 404 {
		t.Fatalf("다른 계정 그래프 거부: %d", status)
	}
	if status, _ := send(f.graph, "synthetic-invalid", lastPayload); status != 401 {
		t.Fatalf("잘못된 자격 거부: %d", status)
	}
	if status, reply := send(f.graph, token, lastPayload); status != 200 || !bytes.Contains(reply, []byte(`"existing"`)) {
		t.Fatalf("재전송 멱등 상태: %d", status)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp() WHERE graph_id=$1`, f.graph.String()); err != nil {
		t.Fatal(err)
	}
	worker, err := ingest.NewSegmentWorker(writer, ingest.SegmentConfig{Gap: 30 * time.Minute, MaxTurns: 4, ExcerptChars: 300, BodyChars: 2000}, plan.AccountPlans{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker.SetDiagnostics(index, management)
	if found, err := worker.RunOnce(t.Context()); err != nil || !found {
		t.Fatalf("구간 처리: %v %v", found, err)
	}
	var eventRaw string
	var queued int
	if err := f.pool.QueryRow(t.Context(), `SELECT event_context_id::text,(SELECT count(*) FROM public.relation_inference_task WHERE context_id=event_context_id) FROM public.conversation_segment WHERE graph_id=$1 AND conversation_id=$2`, f.graph.String(), "ingest05-private-session").Scan(&eventRaw, &queued); err != nil || queued != 1 {
		t.Fatalf("사건과 관계 추론 작업: %s %d %v", eventRaw, queued, err)
	}
	eventID, err := model.ParseID(eventRaw)
	if err != nil {
		t.Fatal(err)
	}
	event, err := writer.Context(t.Context(), f.graph, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if event.CreatedByAgent != agent || len(event.Event.MemberIDs) != 4 || event.Body != turns[0].body+"\n"+turns[2].body {
		t.Fatal("사건 구성 불일치")
	}
	for i, id := range event.Event.MemberIDs {
		value, err := writer.Context(t.Context(), f.graph, id)
		if err != nil || value.Body != turns[i].body || *value.Source.TurnIndex != int64(i) {
			t.Fatalf("원천 조회 %d: %v", i, err)
		}
	}
	readHandlers := newMCP07Reads(t, f, plan.AccountPlans{})
	view := readMCP07(t, readHandlers, f.principal, "node_get", map[string]any{"graph_id": f.graph.String(), "context_id": eventID.String(), "hops": 1, "direction": "out", "traversal_filter": []string{"member_refs"}}, "")
	if len(view["contexts"].([]any)) != 5 || len(view["references"].([]any)) != 4 {
		t.Fatal("MCP 사건·구성원 조회 불일치")
	}
	if len(sink.events) == 0 {
		t.Fatal("진단 이벤트 누락")
	}
	diag, err := json.Marshal(sink.events)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{token, credentialHash, credentialHashBase64, "INGEST05_PRIVATE_LABEL", "INGEST05_SYNTHETIC", "ingest05-private-session"} {
		if strings.Contains(string(diag), forbidden) {
			t.Fatal("진단 금지 값")
		}
	}
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder.Stats().Written == 0 || recorder.Stats().Dropped != 0 {
		t.Fatal("진단 파일 기록 누락")
	}
	err = filepath.WalkDir(logDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, forbidden := range []string{token, credentialHash, credentialHashBase64, "INGEST05_PRIVATE_LABEL", "INGEST05_SYNTHETIC", "ingest05-private-session"} {
			if bytes.Contains(data, []byte(forbidden)) {
				return fmt.Errorf("진단 파일에 금지 값 포함")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("INGEST05_MEASUREMENT mcp_calls=5 mcp_arguments_bytes=%d mcp_result_json_bytes=%d ingest_http_calls=4 ingest_request_body_bytes=%d ingest_response_body_bytes=%d", baselineRequest, baselineResponse, ingestRequest, ingestResponse)
}
