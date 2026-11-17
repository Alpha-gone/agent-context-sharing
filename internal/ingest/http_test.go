package ingest

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

type fakeBackend struct {
	account, agent, graph model.ID
	calls                 int
	err                   error
}

func (f *fakeBackend) ValidateIngestCredential(_ context.Context, token string) (model.ID, model.ID, error) {
	if token != "valid" {
		return model.ID{}, model.ID{}, model.ErrNotFound
	}
	return f.account, f.agent, nil
}
func (f *fakeBackend) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	return model.GraphGradeEditor, true, nil
}
func (f *fakeBackend) Graph(context.Context, model.ID) (model.Graph, error) {
	return model.Graph{ID: f.graph}, nil
}
func (f *fakeBackend) CreateIngestSource(_ context.Context, _ model.ID, value model.Context, _ store.WriteLimits, _ time.Duration, _ string) (model.Context, bool, error) {
	f.calls++
	return value, false, f.err
}
func TestHTTPBoundaries(t *testing.T) {
	account, _ := model.NewID()
	agent, _ := model.NewID()
	graph, _ := model.NewID()
	body := fmt.Sprintf(`{"graph_id":%q,"conversation_id":"a","turns":[{"turn_index":0,"speaker":"user","occurred_at":"2026-10-20T12:00:00Z","body":"text"}]}`, graph.String())
	cases := []struct {
		name, method, auth, origin, body string
		status                           int
		writes                           int
	}{
		{"valid", "POST", "Bearer valid", "", body, 200, 1},
		{"invalid token", "POST", "Bearer other", "", body, 401, 0},
		{"authentication before body", "POST", "Bearer other", "", "{", 401, 0},
		{"method", "GET", "Bearer valid", "", body, 405, 0},
		{"origin", "POST", "Bearer valid", "https://example.test", body, 400, 0},
		{"unknown field", "POST", "Bearer valid", "", strings.Replace(body, `"graph_id"`, `"unknown":1,"graph_id"`, 1), 400, 0},
		{"duplicate field", "POST", "Bearer valid", "", strings.Replace(body, `"conversation_id":"a"`, `"conversation_id":"a","conversation_id":"b"`, 1), 400, 0},
		{"duplicate turn", "POST", "Bearer valid", "", strings.Replace(body, `}]`, `},{"turn_index":0,"speaker":"agent","occurred_at":"2026-10-20T12:00:00Z","body":"other"}]`, 1), 400, 0},
		{"nonutc", "POST", "Bearer valid", "", strings.Replace(body, "12:00:00Z", "12:00:00+01:00", 1), 400, 0},
		{"too large", "POST", "Bearer valid", "", strings.Repeat("x", 4<<20+1), 413, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{account: account, agent: agent, graph: graph}
			h := Handler{Backend: f, Gap: 30 * time.Minute}
			req := httptest.NewRequest(tc.method, "/ingest/conversation", strings.NewReader(tc.body))
			req.Header.Set("Authorization", tc.auth)
			req.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.status || f.calls != tc.writes {
				t.Fatalf("status=%d calls=%d", w.Code, f.calls)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("no-store 누락")
			}
		})
	}
}

func TestHTTPWriteLimits(t *testing.T) {
	account, _ := model.NewID()
	agent, _ := model.NewID()
	graph, _ := model.NewID()
	body := fmt.Sprintf(`{"graph_id":%q,"conversation_id":"a","turns":[{"turn_index":0,"speaker":"user","occurred_at":"2026-10-20T12:00:00Z","body":"text"}]}`, graph.String())
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"rate", plan.LimitError{Name: "writes_per_minute"}, 429, ""},
		{"storage", plan.LimitError{Name: "stored_characters_per_graph"}, 200, "limit_exceeded"},
		{"invalid", model.FieldError{Field: "body"}, 200, "invalid_argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBackend{account: account, agent: agent, graph: graph, err: tc.err}
			req := httptest.NewRequest("POST", "/ingest/conversation", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer valid")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			Handler{Backend: f, Gap: 30 * time.Minute}.ServeHTTP(w, req)
			if w.Code != tc.status || tc.reason != "" && !strings.Contains(w.Body.String(), tc.reason) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
