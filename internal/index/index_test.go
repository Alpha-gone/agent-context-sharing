package index

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	worker, err := New(testStore(t), Config{Provider: "ollama", BaseURL: baseURL, Model: "test", VectorType: "vector", Dimension: 2}, server.Client(), nil)
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
	worker, err := New(testStore(t), Config{Provider: "ollama", BaseURL: baseURL, Model: "test", VectorType: "vector", Dimension: 2}, server.Client(), nil)
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

// TestCloseBeforeStartDoesNotPanic은 시작하지 않은 작업자를 닫은 뒤 Start를 불러도
// done이 두 번 닫히지 않는지 확인한다. 주기 작업자가 이미 쓰는 방어를 여기에도 둔다.
func TestCloseBeforeStartDoesNotPanic(t *testing.T) {
	worker := testWorker(t, nil)
	if err := worker.Close(t.Context()); err != nil {
		t.Fatalf("시작 전 종료: %v", err)
	}
	// 두 번째 Close와 이후 Start가 모두 아무것도 하지 않아야 한다.
	if err := worker.Close(t.Context()); err != nil {
		t.Fatalf("두 번째 종료: %v", err)
	}
	worker.Start(t.Context())
	if err := worker.Close(t.Context()); err != nil {
		t.Fatalf("Start 뒤 종료: %v", err)
	}
}

// TestEmbedRejectsOversizedResponse는 제공자 응답이 상한을 넘으면 거부하는지 확인한다.
// 상한이 없으면 잘못 설정된 제공자가 작업자와 검색 경로의 메모리를 그대로 늘린다.
func TestEmbedRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"embeddings":[[`))
		// 상한을 넘을 때까지 값을 흘려보낸다.
		chunk := strings.Repeat("1,", 4096)
		for sent := 0; sent <= maxEmbeddingResponseBytes; sent += len(chunk) {
			if _, err := writer.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	worker := testWorker(t, server)
	if _, err := worker.Embed(t.Context(), "본문"); err == nil {
		t.Fatal("상한을 넘는 응답이 통과했다")
	} else if !strings.Contains(err.Error(), "상한") {
		t.Fatalf("오류 = %v; 상한 초과를 알려야 한다", err)
	}
}

// testWorker는 지정한 제공자를 보는 색인 작업자를 만든다. server가 nil이면 호출하지 않는
// 경로만 검사하는 것이므로 주소만 형식을 맞춘다.
func testWorker(t *testing.T, server *httptest.Server) *Worker {
	t.Helper()
	address := "http://127.0.0.1:1"
	var client *http.Client
	if server != nil {
		address, client = server.URL, server.Client()
	}
	baseURL, err := url.Parse(address)
	if err != nil {
		t.Fatalf("테스트 제공자 URL 해석: %v", err)
	}
	worker, err := New(testStore(t), Config{Provider: "ollama", BaseURL: baseURL, Model: "test", VectorType: "vector", Dimension: 2}, client, nil)
	if err != nil {
		t.Fatalf("색인 작업자 생성: %v", err)
	}
	return worker
}
