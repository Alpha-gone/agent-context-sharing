package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/relinfer"
)

type inferenceTransport struct {
	mu   sync.Mutex
	sent []string
}

func (p *inferenceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(io.LimitReader(request.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var response any
	if strings.HasSuffix(request.URL.Path, ":embedContent") {
		vector := make([]float64, 768)
		if bytes.Contains(raw, []byte("SECOND_EVENT_PRIVATE")) {
			vector[1] = 1
		} else {
			vector[0] = 1
		}
		response = map[string]any{"embedding": map[string]any{"values": vector}}
	} else if request.URL.Path == "/v1/responses" {
		p.mu.Lock()
		p.sent = append(p.sent, string(raw))
		p.mu.Unlock()
		var envelope struct {
			Input string `json:"input"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, err
		}
		var input struct {
			SchemaVersion string `json:"schema_version"`
			Data          struct {
				Target struct {
					Body string `json:"body"`
				} `json:"target"`
				Candidates []struct {
					Alias   string `json:"alias"`
					Body    string `json:"body"`
					Allowed []struct {
						Type       string   `json:"type"`
						Direction  string   `json:"direction"`
						Directions []string `json:"directions"`
					} `json:"allowed"`
				} `json:"candidates"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(envelope.Input), &input); err != nil {
			return nil, err
		}
		decisions := []map[string]any{}
		for _, candidate := range input.Data.Candidates {
			for _, allowed := range candidate.Allowed {
				direction := allowed.Direction
				if len(allowed.Directions) > 0 {
					direction = allowed.Directions[0]
				}
				choice := "none"
				evidence := map[string]string{"target": "", "candidate": ""}
				if allowed.Type == "relates_to" {
					choice = "relation"
					evidence["target"] = input.Data.Target.Body
					evidence["candidate"] = candidate.Body
				}
				if allowed.Type == "causes" {
					choice = "relation"
					evidence["target"] = "unverified quotation"
					evidence["candidate"] = "unverified quotation"
				}
				decisions = append(decisions, map[string]any{"candidate": candidate.Alias, "relation_type": allowed.Type, "decision": choice, "direction": direction, "evidence": evidence, "reason": ""})
			}
		}
		text, err := json.Marshal(map[string]any{"schema_version": input.SchemaVersion, "decisions": decisions})
		if err != nil {
			return nil, err
		}
		response = map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": string(text)}}}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
	} else {
		return nil, fmt.Errorf("unexpected synthetic provider path")
	}
	body, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func TestRINF05AssembledInferenceIntegration(t *testing.T) {
	for _, placement := range []config.ComponentPlacement{config.PlacementEmbedded, config.PlacementExternal} {
		t.Run(string(placement), func(t *testing.T) {
			_, pool, address := newServerContractDatabase(t)
			transport := &inferenceTransport{}
			client := &http.Client{Transport: transport}
			cfg := arch06Configuration(t, address, "https://synthetic-provider.test")
			cfg.IndexWorkerPlacement = placement
			cfg.RelationInference.Mode = "propose"
			cfg.RelationInference.Provider = "openai"
			cfg.RelationInference.Model = "gpt-6-luna"
			cfg.RelationInference.APIKey = "synthetic-inference-key"
			cfg.RelationInference.BaseURL, _ = url.Parse("https://synthetic-provider.test/v1")
			cfg.RelationInference.IndexWait = 100 * time.Millisecond
			a, err := assembleApplication(t.Context(), cfg, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := a.close(nil, true); err != nil {
					t.Error(err)
				}
			})
			service, err := authz.New(a.database, authz.Config{Issuer: cfg.AuthorizationServerURL.String(), BcryptCost: 4, OAuth: oauthTestConfiguration(t, cfg, a.database), AuthorizationCodes: a.database, AccessTokens: a.database, RefreshTokens: a.database, Connections: a.database})
			if err != nil {
				t.Fatal(err)
			}
			owner, err := service.Register(t.Context(), "rinf05_owner", "correct password")
			if err != nil {
				t.Fatal(err)
			}
			outsider, err := service.Register(t.Context(), "rinf05_outsider", "correct password")
			if err != nil {
				t.Fatal(err)
			}
			issueOwner := func(accountID string) string {
				t.Helper()
				parsed, err := model.ParseID(accountID)
				if err != nil {
					t.Fatal(err)
				}
				clientID := "codex-cli-first-test"
				callback := "http://127.0.0.1:49152/callback"
				code, err := service.IssueAuthorizationCode(t.Context(), parsed, authz.AuthorizationRequest{ClientID: clientID, RedirectURI: callback, Resource: cfg.MCPResourceURL.String(), ResponseType: "code", Scope: "agent-context", Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", ChallengeMethod: "S256"})
				if err != nil {
					t.Fatal(err)
				}
				tokens, err := service.ExchangeAuthorizationCode(t.Context(), code, clientID, callback, cfg.MCPResourceURL.String(), "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
				if err != nil {
					t.Fatal(err)
				}
				return tokens.AccessToken
			}
			token, otherToken := issueOwner(owner.String()), issueOwner(outsider.String())
			server := httptest.NewTLSServer(a.app.handler())
			defer server.Close()
			call := func(tool string, args map[string]any) map[string]any {
				return arch06MCP(t, server, cfg.MCPResourceURL.Host, token, "tools/call", tool, args)["structuredContent"].(map[string]any)
			}
			discovery := arch06MCP(t, server, cfg.MCPResourceURL.Host, token, "server/discover", "", nil)
			if !strings.Contains(discovery["instructions"].(string), "Server finds relations") {
				t.Fatal("활성 모드 안내 누락")
			}
			agent := call("agent_register", map[string]any{"registration_key": "rinf05_registration"})["agent_id"].(string)
			graph := call("graph_create", map[string]any{"name": "추론 그래프"})["graph"].(map[string]any)["graph_id"].(string)
			otherCall := func(tool string, args map[string]any) map[string]any {
				return arch06MCP(t, server, cfg.MCPResourceURL.Host, otherToken, "tools/call", tool, args)["structuredContent"].(map[string]any)
			}
			otherAgent := otherCall("agent_register", map[string]any{"registration_key": "rinf05_other_registration"})["agent_id"].(string)
			otherGraph := otherCall("graph_create", map[string]any{"name": "다른 그래프"})["graph"].(map[string]any)["graph_id"].(string)
			otherSource := otherCall("node_create", map[string]any{"graph_id": otherGraph, "agent_id": otherAgent, "judgment_input": "synthetic judgment", "layer": "source", "body": "OTHER_SOURCE_PRIVATE", "origin_kind": "user_utterance", "occurred_at": "2026-10-20T12:00:00Z", "source_ref": map[string]any{"channel": "file", "locator": "file:///rinf05/other"}})["node"].(map[string]any)["context_id"].(string)
			otherEvent := otherCall("node_create", map[string]any{"graph_id": otherGraph, "agent_id": otherAgent, "judgment_input": "synthetic judgment", "layer": "event", "body": "FOREIGN_EVENT_PRIVATE", "member_refs": []string{otherSource}, "event_start": "2026-10-20T11:00:00Z", "event_end": "2026-10-20T13:00:00Z"})["node"].(map[string]any)["context_id"].(string)
			// 다른 그래프가 별도 추론을 요청하지 않도록 이 시험이 만든 작업만 제거한다.
			if _, err := pool.Exec(t.Context(), `DELETE FROM public.relation_inference_task WHERE context_id=$1`, otherEvent); err != nil {
				t.Fatal(err)
			}
			source := call("node_create", map[string]any{"graph_id": graph, "agent_id": agent, "judgment_input": "synthetic judgment", "layer": "source", "body": "SECRET_SOURCE_BODY", "origin_kind": "user_utterance", "occurred_at": "2026-10-20T12:00:00Z", "source_ref": map[string]any{"channel": "file", "locator": "file:///rinf05/source"}})["node"].(map[string]any)["context_id"].(string)
			createEvent := func(body string) string {
				t.Helper()
				return call("node_create", map[string]any{"graph_id": graph, "agent_id": agent, "judgment_input": "synthetic judgment", "layer": "event", "body": body, "member_refs": []string{source}, "event_start": "2026-10-20T11:00:00Z", "event_end": "2026-10-20T13:00:00Z"})["node"].(map[string]any)["context_id"].(string)
			}
			first := createEvent("FIRST_EVENT_PRIVATE")
			a.startWorkers(t.Context())
			var external *relinfer.Worker
			var externalRecorder *diagnostic.Recorder
			if placement == config.PlacementExternal {
				var queued int
				if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.relation_inference_task WHERE context_id=$1`, first).Scan(&queued); err != nil || queued != 1 {
					t.Fatalf("external 서버 작업 대기 %d %v", queued, err)
				}
				external, _, err = relinfer.NewRuntime(a.database, inferenceRuntimeConfig(cfg, a.indexer.ModelID()), client, nil)
				if err != nil {
					t.Fatal(err)
				}
				source, err := diagnostic.NewSource("rinf05-external")
				if err != nil {
					t.Fatal(err)
				}
				externalRecorder, err = diagnostic.NewRecorder(t.Context(), cfg.Diagnostic.Directory, source)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
					defer cancel()
					if err := externalRecorder.Close(cleanup); err != nil {
						t.Error(err)
					}
				})
				reporter, err := diagnostic.NewReporter(diagnostic.Index, externalRecorder, source)
				if err != nil {
					t.Fatal(err)
				}
				external.SetDiagnostics(reporter)
				external.Start(t.Context())
				a.indexer.Start(t.Context())
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
					defer cancel()
					if err := external.Close(cleanup); err != nil {
						t.Error(err)
					}
				})
			}
			waitRun := func(id string) {
				t.Helper()
				deadline := time.Now().Add(45 * time.Second)
				for time.Now().Before(deadline) {
					var count int
					if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.relation_inference_run WHERE context_id=$1`, id).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count > 0 {
						return
					}
					time.Sleep(50 * time.Millisecond)
				}
				t.Fatal("추론 회차 완료 시간 초과")
			}
			waitRun(first)
			second := createEvent("SECOND_EVENT_PRIVATE")
			waitRun(second)
			result := call("relation_list", map[string]any{"graph_id": graph, "context_id": second})
			relations := result["relations"].([]any)
			found := false
			for _, raw := range relations {
				value := raw.(map[string]any)
				if value["relation_type"] == "relates_to" && value["proposed_by"] == "inference" && value["inference_decision_id"] != "" {
					found = true
				}
				if _, ok := value["evidence"]; ok {
					t.Fatal("근거 인용 노출")
				}
			}
			if !found {
				t.Fatalf("추론 관계 없음: %+v", relations)
			}
			denied := arch06MCPResult(t, server, cfg.MCPResourceURL.Host, otherToken, "tools/call", "relation_list", map[string]any{"graph_id": graph, "context_id": second})
			if denied["isError"] != true {
				t.Fatal("다른 계정에 그래프를 노출했다")
			}
			transport.mu.Lock()
			sent := strings.Join(transport.sent, "\n")
			transport.mu.Unlock()
			if !strings.Contains(sent, "SECOND_EVENT_PRIVATE") || strings.Contains(sent, "SECRET_SOURCE_BODY") || strings.Contains(sent, "FOREIGN_EVENT_PRIVATE") || strings.Contains(sent, "OTHER_SOURCE_PRIVATE") {
				t.Fatal("추론 제공자 전송 경계 위반")
			}
			server.Close()
			if external != nil {
				if err := external.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := externalRecorder.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.close(nil, true); err != nil {
				t.Fatal(err)
			}
			if stats := a.recorder.Stats(); stats.Dropped != 0 {
				t.Fatalf("서버 진단 누락: %+v", stats)
			}
			if externalRecorder != nil {
				if stats := externalRecorder.Stats(); stats.Dropped != 0 {
					t.Fatalf("분리 작업자 진단 누락: %+v", stats)
				}
			}
			var logs strings.Builder
			err = filepath.WalkDir(cfg.Diagnostic.Directory, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					logs.Write(data)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"SECRET_SOURCE_BODY", "OTHER_SOURCE_PRIVATE", "FOREIGN_EVENT_PRIVATE", "FIRST_EVENT_PRIVATE", "SECOND_EVENT_PRIVATE", "synthetic-inference-key", "synthetic judgment", "unverified quotation"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("진단에 비공개 값이 기록됐다")
				}
			}
			for _, stage := range []string{"relation_inference_enqueue", "relation_inference_acquire", "relation_inference_provider", "relation_inference_apply", "relation_inference_reject", "evidence_unverified", "relation_inference_progress", "input_tokens", "output_tokens", "inference_profile", "queue_counts_measured"} {
				if !strings.Contains(logs.String(), stage) {
					t.Fatalf("진단 단계 누락: %s", stage)
				}
			}
		})
	}
}
