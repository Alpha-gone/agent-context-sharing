package web

import (
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type ingestStub struct {
	account, target model.ID
	calls           int
}

func (s *ingestStub) ListIngestCredentials(_ context.Context, account model.ID) ([]store.IngestCredential, error) {
	s.account = account
	return nil, nil
}
func (s *ingestStub) IssueIngestCredential(_ context.Context, account model.ID, label string) (string, error) {
	s.account = account
	s.calls++
	if label != "test" {
		return "", model.FieldError{Field: "label"}
	}
	return "acs_ing_synthetic-secret", nil
}
func (s *ingestStub) RevokeIngestCredential(_ context.Context, account, target model.ID) error {
	s.account, s.target = account, target
	s.calls++
	return model.ErrNotFound
}

func TestIngestCredentialsHTTP(t *testing.T) {
	account, target := testID(t), testID(t)
	source := &ingestStub{}
	server := newTestServer(t, account, model.ID{})
	server.config.Connections = &connectionStub{}
	server.config.IngestCredentials = source
	call := func(method string, values url.Values, origin string) *httptest.ResponseRecorder {
		request := sessionRequest(method, "/connections", values)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	response := call(http.MethodPost, url.Values{"action": {"issue_ingest"}, "label": {"test"}}, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "acs_ing_synthetic-secret") || source.account != account {
		t.Fatal("발급 응답 경계")
	}
	response = call(http.MethodGet, nil, "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "acs_ing_synthetic-secret") {
		t.Fatal("목록에 원문 노출")
	}
	response = call(http.MethodPost, url.Values{"action": {"revoke_ingest"}, "target_id": {target.String()}}, "")
	if response.Code != http.StatusNotFound || source.target != target {
		t.Fatal("폐기 격리")
	}
	before := source.calls
	response = call(http.MethodPost, url.Values{"action": {"issue_ingest"}, "label": {"test"}}, "https://other.test")
	if response.Code != http.StatusForbidden || source.calls != before {
		t.Fatal("출처 검사")
	}
	server.config.IngestCredentials = nil
	response = call(http.MethodPost, url.Values{"action": {"issue_ingest"}, "label": {"test"}}, "")
	if response.Code != http.StatusNotFound || source.calls != before {
		t.Fatal("비활성 발급 허용")
	}
}
