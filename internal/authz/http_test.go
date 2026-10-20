package authz

import (
	"agent_context_sharing/internal/model"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJWKSPublishesPublicKeysOnly(t *testing.T) {
	service := testService(t, newMemoryStore())
	// 첫 발급이 활성 서명 키를 만든다.
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.WebSession(t.Context(), id, "web"); err != nil {
		t.Fatalf("세션 발급: %v", err)
	}
	handler, err := NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.JWKS(recorder, httptest.NewRequest(http.MethodGet, "/jwks.json", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("응답 상태 = %d, want %d", recorder.Code, http.StatusOK)
	}
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &set); err != nil {
		t.Fatalf("JWKS 해석: %v", err)
	}
	if len(set.Keys) == 0 {
		t.Fatal("공개 키가 비어 있다")
	}
	for _, key := range set.Keys {
		// `d`는 EC 개인 키 성분이다. 공개 목록에 나가면 안 된다.
		if _, found := key["d"]; found {
			t.Fatal("JWKS에 개인 키 성분이 실렸다")
		}
	}
}
