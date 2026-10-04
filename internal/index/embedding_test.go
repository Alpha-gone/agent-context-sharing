package index

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testGeminiKey = "synthetic-provider-key"

func geminiWorker(t *testing.T, handler http.HandlerFunc) (*Worker, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := New(testStore(t), Config{Provider: "gemini", APIKey: testGeminiKey, BaseURL: address, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 128}, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return worker, server
}

func writeGeminiVector(writer http.ResponseWriter, dimension int) {
	values := make([]float64, dimension)
	values[0] = 1
	body, _ := json.Marshal(struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
	}{Embedding: struct {
		Values []float64 `json:"values"`
	}{Values: values}})
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(body)
}

func TestGeminiDocumentQueryContractAndConcurrentCalls(t *testing.T) {
	var documents, queries atomic.Int64
	worker, _ := geminiWorker(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != "POST" || request.URL.Path != "/v1beta/models/gemini-embedding-2:embedContent" || request.URL.RawQuery != "" || request.Header.Get("x-goog-api-key") != testGeminiKey {
			t.Error("Gemini endpoint·인증 계약이 다르다")
		}
		var payload struct {
			Model   string        `json:"model"`
			Content geminiContent `json:"content"`
			Config  struct {
				Dimension int   `json:"outputDimensionality"`
				Truncate  *bool `json:"autoTruncate"`
			} `json:"embedContentConfig"`
			TaskType string `json:"taskType"`
		}
		if err := json.UnmarshalRead(request.Body, &payload); err != nil {
			t.Error(err)
			return
		}
		if payload.Model != "models/gemini-embedding-2" || payload.Config.Dimension != 128 || payload.Config.Truncate == nil || *payload.Config.Truncate || payload.TaskType != "" || len(payload.Content.Parts) != 1 {
			t.Error("Gemini 차원·절단 방지·단일 입력 계약이 다르다")
			return
		}
		switch payload.Content.Parts[0].Text {
		case "title: none | text: 문서":
			documents.Add(1)
		case "task: search result | query: 질의":
			queries.Add(1)
		default:
			t.Error("문서·질의 입력 형식이 다르다")
		}
		writeGeminiVector(writer, 128)
	})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for _, call := range []struct {
				input string
				embed func(context.Context, string) ([]float64, error)
			}{{"문서", worker.EmbedDocument}, {"질의", worker.Embed}} {
				vector, err := call.embed(t.Context(), call.input)
				if err != nil || len(vector) != 128 || vector[0] != 1 {
					t.Errorf("Gemini 벡터: 길이 %d, 오류 %v", len(vector), err)
				}
			}
		})
	}
	wg.Wait()
	if documents.Load() != 16 || queries.Load() != 16 {
		t.Fatalf("호출 수: 문서 %d, 질의 %d", documents.Load(), queries.Load())
	}
}

func TestGeminiFailureClassificationAndNoSecretDisclosure(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		retryable  bool
	}{
		{"입력 한도", testGeminiKey + " original-body", 400, false},
		{"인증", testGeminiKey, 401, false},
		{"권한", testGeminiKey, 403, false},
		{"모델", testGeminiKey, 404, false},
		{"408", testGeminiKey, 408, true},
		{"429", testGeminiKey, 429, true},
		{"서버", testGeminiKey, 503, true},
		{"redirect", testGeminiKey, 302, false},
		{"해석", `{"embedding":{"values":["` + testGeminiKey + `"]}}`, 200, false},
		{"차원", `{"embedding":{"values":[1]}}`, 200, false},
		{"영벡터", `{"embedding":{"values":[` + strings.TrimSuffix(strings.Repeat("0,", 128), ",") + `]}}`, 200, false},
		{"수치 범위", `{"embedding":{"values":[1e400]}}`, 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, _ := geminiWorker(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			})
			_, err := worker.EmbedDocument(t.Context(), "original-body")
			if err == nil || retryableEmbeddingError(err) != test.retryable {
				t.Fatalf("오류 분류: %v", err)
			}
			if strings.Contains(err.Error(), testGeminiKey) || strings.Contains(err.Error(), "original-body") {
				t.Fatal("제공자 오류에 키·본문이 노출됐다")
			}
		})
	}
}

func TestGeminiRedirectDoesNotForwardCredentials(t *testing.T) {
	var redirected atomic.Int64
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	worker, server := geminiWorker(t, func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusTemporaryRedirect)
	})
	_, err := worker.Embed(t.Context(), "질의")
	if err == nil || redirected.Load() != 0 || server.Client().CheckRedirect != nil {
		t.Fatalf("redirect 차단 또는 원래 client 보존 실패: %v", err)
	}
}

func TestGeminiInputBoundsAndCancellation(t *testing.T) {
	var calls atomic.Int64
	worker, _ := geminiWorker(t, func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeGeminiVector(writer, 128)
	})
	for _, input := range []string{"", " ", strings.Repeat("한", 8001), string([]byte{0xff})} {
		if _, err := worker.Embed(t.Context(), input); err == nil || retryableEmbeddingError(err) {
			t.Fatalf("입력 경계 분류: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("잘못된 입력이 제공자에 전달됐다")
	}
	if _, err := worker.EmbedDocument(t.Context(), strings.Repeat("한", 8000)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := worker.Embed(ctx, "취소"); !errors.Is(err, context.Canceled) {
		t.Fatalf("취소 식별: %v", err)
	}
	slow, _ := geminiWorker(t, func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		writeGeminiVector(writer, 128)
	})
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer stop()
	if _, err := slow.Embed(ctx, "지연"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout 식별: %v", err)
	}
}

func TestProviderConfigAndModelID(t *testing.T) {
	address, _ := url.Parse("https://generativelanguage.googleapis.com")
	valid := Config{Provider: "gemini", BaseURL: address, APIKey: testGeminiKey, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 768}
	if err := valid.Validate(); err != nil || valid.ModelID() != "gemini:gemini-embedding-2:retrieval-v1:vector:768" {
		t.Fatalf("Gemini 구성·식별자: %v", err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Provider = "unknown" },
		func(c *Config) { c.APIKey = "" },
		func(c *Config) { c.APIKey += "\n" },
		func(c *Config) { c.Model = "other" },
		func(c *Config) { c.Dimension = 127 },
		func(c *Config) { c.Dimension = 3072 },
		func(c *Config) { c.BaseURL, _ = url.Parse("http://service.test") },
		func(c *Config) { c.BaseURL, _ = url.Parse("https://user:secret@service.test") },
		func(c *Config) { c.BaseURL, _ = url.Parse("https://service.test?key=secret") },
	} {
		cfg := valid
		change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatal("잘못된 제공자 구성이 허용됐다")
		}
	}
	valid.VectorType, valid.Dimension = "halfvec", 3072
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"", "ollama"} {
		cfg := Config{Provider: provider, Model: "bge-m3", VectorType: "vector", Dimension: 1024}
		if cfg.ModelID() != "bge-m3:vector:1024" {
			t.Fatal("기존 Ollama 식별자가 달라졌다")
		}
	}
	err := safeTransportError(t.Context(), fmt.Errorf("%s original-body", testGeminiKey))
	if strings.Contains(err.Error(), testGeminiKey) || strings.Contains(err.Error(), "original-body") || !retryableEmbeddingError(err) {
		t.Fatal("전송 오류 비밀·재시도 계약이 다르다")
	}
}
