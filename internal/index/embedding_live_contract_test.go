package index

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

type liveProviderTransport struct {
	next  http.RoundTripper
	calls atomic.Int64
}

func (transport *liveProviderTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.calls.Add(1) > 2 {
		return nil, fmt.Errorf("실연결 제공자 시험의 최대 두 번 호출을 넘었다")
	}
	return transport.next.RoundTrip(request)
}

func TestLiveProviderTransportBudget(t *testing.T) {
	var received atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { received.Add(1); w.WriteHeader(503) }))
	t.Cleanup(server.Close)
	transport := &liveProviderTransport{next: http.DefaultTransport}
	for i := range 3 {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := transport.RoundTrip(request)
		if i == 2 {
			if err == nil || response != nil {
				t.Fatal("세 번째 호출을 차단하지 않았다")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if received.Load() != 2 {
		t.Fatalf("실제 전송=%d, 기대=2", received.Load())
	}
}

func TestSyntheticLiveEmbeddingContract(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("x-goog-api-key") != testGeminiKey {
			t.Error("합성 키 전달이 다르다")
		}
		vector := make([]float64, 768)
		vector[0] = 1
		raw, err := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	exerciseLiveEmbeddingContract(t, testGeminiKey, address, server.Client().Transport)
	if calls.Load() != 2 {
		t.Fatalf("합성 TLS 전송=%d, 기대=2", calls.Load())
	}
}

func exerciseLiveEmbeddingContract(t *testing.T, key string, address *url.URL, next http.RoundTripper) {
	t.Helper()
	if closer, ok := next.(interface{ CloseIdleConnections() }); ok {
		t.Cleanup(closer.CloseIdleConnections)
	}
	transport := &liveProviderTransport{next: next}
	worker, err := New(testStore(t), Config{
		Provider: "gemini", APIKey: key, BaseURL: address,
		Model: "gemini-embedding-2", VectorType: "vector", Dimension: 768,
	}, &http.Client{Transport: transport}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := worker.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 65*time.Second)
	defer cancel()
	for _, call := range []struct {
		name  string
		input string
		embed func(context.Context, string) ([]float64, error)
	}{
		{"document", "합성 시험 문서입니다. 파란색 상자는 선반 위에 있습니다.", worker.EmbedDocument},
		{"query", "합성 시험의 파란색 상자는 어디에 있습니까?", worker.Embed},
	} {
		started := time.Now()
		vector, err := call.embed(ctx, call.input)
		if err != nil {
			t.Fatalf("%s 실연결 실패: %v", call.name, err)
		}
		if len(vector) != 768 {
			t.Fatalf("%s 벡터 차원 = %d, 기대값 768", call.name, len(vector))
		}
		nonzero := false
		for _, value := range vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				t.Fatalf("%s 벡터가 유한하지 않다", call.name)
			}
			nonzero = nonzero || value != 0
		}
		if !nonzero {
			t.Fatalf("%s 응답이 영벡터다", call.name)
		}
		t.Logf("%s: dimension=%d elapsed_ms=%d", call.name, len(vector), time.Since(started).Milliseconds())
	}
	if transport.calls.Load() != 2 {
		t.Fatalf("provider calls=%d, want=2", transport.calls.Load())
	}
}
