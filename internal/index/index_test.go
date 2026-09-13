package index

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"agent_context_sharing/internal/store"
)

func TestEmbed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/embed" {
			t.Fatalf("임베딩 요청 = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"embeddings":[[0.25,0.75]]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("테스트 제공자 URL 해석: %v", err)
	}
	worker, err := New(testStore(t), Config{BaseURL: baseURL, Model: "test", VectorType: "vector", Dimension: 2}, server.Client(), nil)
	if err != nil {
		t.Fatalf("색인 작업자 생성: %v", err)
	}
	embedding, err := worker.Embed(t.Context(), "색인 본문")
	if err != nil {
		t.Fatalf("임베딩 생성: %v", err)
	}
	if len(embedding) != 2 || embedding[0] != 0.25 || embedding[1] != 0.75 {
		t.Fatalf("임베딩 = %v", embedding)
	}
}

func TestEmbedRejectsDimensionMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"embeddings":[[0.25]]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("테스트 제공자 URL 해석: %v", err)
	}
	worker, err := New(testStore(t), Config{BaseURL: baseURL, Model: "test", VectorType: "vector", Dimension: 2}, server.Client(), nil)
	if err != nil {
		t.Fatalf("색인 작업자 생성: %v", err)
	}
	if _, err := worker.Embed(t.Context(), "색인 본문"); err == nil {
		t.Fatal("차원이 다른 응답이 허용됐다")
	} else if _, ok := err.(DimensionError); !ok {
		t.Fatalf("차원 불일치 오류 = %T %v", err, err)
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	// New는 제공자 단위 테스트에서 데이터베이스 연결을 만들지 않도록 허용하지 않으므로,
	// Embed 경로와 무관한 저장소 필드는 nil로 둔다.
	return &store.Store{}
}
