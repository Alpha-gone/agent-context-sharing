package index

import (
	"context"
	"crypto/tls"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func contractVectorJSON() string {
	return `{"embedding":{"values":[1` + strings.Repeat(",0", 127) + `]}}`
}

func TestGeminiVectorMetadataContract(t *testing.T) {
	for _, test := range []struct {
		kind      string
		dimension int
		valid     bool
	}{
		{"vector", 127, false}, {"vector", 128, true}, {"vector", 768, true}, {"vector", 2000, true}, {"vector", 2001, false},
		{"halfvec", 128, true}, {"halfvec", 3072, true}, {"halfvec", 3073, false}, {"bit", 768, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", test.kind, test.dimension), func(t *testing.T) {
			address, _ := url.Parse("https://generativelanguage.googleapis.com")
			cfg := Config{Provider: "gemini", BaseURL: address, Model: "gemini-embedding-2", VectorType: test.kind, Dimension: test.dimension}
			if err := cfg.ValidateMetadata(); (err == nil) != test.valid {
				t.Fatalf("키 없는 메타데이터 검증=%v", err)
			}
			if cfg.Validate() == nil {
				t.Fatal("호출용 키 누락을 허용했다")
			}
			cfg.APIKey = testGeminiKey
			if err := cfg.Validate(); (err == nil) != test.valid {
				t.Fatalf("호출용 구성 검증=%v", err)
			}
			if test.valid && cfg.ModelID() != fmt.Sprintf("gemini:gemini-embedding-2:retrieval-v1:%s:%d", test.kind, test.dimension) {
				t.Fatal("색인 식별자 불일치")
			}
		})
	}
}

func TestGeminiResponseContract(t *testing.T) {
	valid := contractVectorJSON()
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"single", valid, true},
		{"metadata", strings.TrimSuffix(valid, "}") + `,"usageMetadata":{"promptTokenCount":1}}`, true},
		{"negative", strings.Replace(valid, "[1", "[-1", 1), true},
		{"null_element", strings.Replace(valid, "1,0", "1,null", 1), false},
		{"truncated_json", strings.TrimSuffix(valid, "}"), false},
		{"missing_vector", `{}`, false},
		{"null_vector", `{"embedding":null}`, false},
		{"null_values", `{"embedding":{"values":null}}`, false},
		{"batch", `{"embedding":[{"values":[1]}]}`, false},
		{"two_responses", valid + valid, false},
		{"duplicate_embedding", strings.TrimSuffix(valid, "}") + `,"embedding":{"values":[1]}}`, false},
		{"string_value", strings.Replace(valid, "[1", `["synthetic-provider-key"`, 1), false},
		{"boolean_value", strings.Replace(valid, "[1", `[true`, 1), false},
		{"nonfinite", strings.Replace(valid, "[1", `[1e400`, 1), false},
		{"nan", strings.Replace(valid, "[1", `["NaN"`, 1), false},
		{"short", `{"embedding":{"values":[1]}}`, false},
		{"long", strings.Replace(valid, "[1", "[1,0", 1), false},
		{"zero", strings.Replace(valid, "[1", "[0", 1), false},
		{"invalid_utf8", valid + string([]byte{0xff}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, server := geminiWorker(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, test.body) })
			vector, err := worker.EmbedDocument(t.Context(), "synthetic-private-body")
			if (err == nil) != test.valid {
				t.Fatalf("응답 수용=%v, want=%t", err, test.valid)
			}
			if test.valid {
				if len(vector) != 128 || vector[0] == 0 {
					t.Fatal("단일 벡터를 반환하지 않았다")
				}
			} else if len(vector) != 0 || retryableEmbeddingError(err) {
				t.Fatalf("응답 계약 위반의 영구 실패=%v", err)
			}
			if err != nil {
				for _, secret := range []string{testGeminiKey, "synthetic-private-body", server.URL} {
					if strings.Contains(fmt.Sprintf("%+v", err), secret) {
						t.Fatal("오류에 요청·응답 원문이 노출됐다")
					}
				}
			}
		})
	}
}

type contractRoundTripper func(*http.Request) (*http.Response, error)

func (call contractRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return call(request)
}

type contractBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (body *contractBody) Read(p []byte) (int, error) {
	n, err := body.reader.Read(p)
	body.read += n
	return n, err
}
func (body *contractBody) Close() error { body.closed = true; return nil }

func TestGeminiResponseReadBoundary(t *testing.T) {
	for _, size := range []int{maxEmbeddingResponseBytes - 1, maxEmbeddingResponseBytes, maxEmbeddingResponseBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			valid := contractVectorJSON()
			body := &contractBody{reader: strings.NewReader(valid + strings.Repeat(" ", size-len(valid)))}
			worker := testWorker(t, nil)
			worker.httpClient.Transport = contractRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
			})
			_, err := worker.Embed(t.Context(), "synthetic")
			if (err == nil) != (size <= maxEmbeddingResponseBytes) || !body.closed || body.read > maxEmbeddingResponseBytes+1 {
				t.Fatalf("응답 상한·닫기=%d, %t: %v", body.read, body.closed, err)
			}
			if err != nil && retryableEmbeddingError(err) {
				t.Fatal("응답 상한 위반을 재시도한다")
			}
		})
	}
}

type contractReadError struct{ err error }

func (reader contractReadError) Read([]byte) (int, error) { return 0, reader.err }

func TestGeminiResponseTransportFailure(t *testing.T) {
	for _, failure := range []error{io.ErrUnexpectedEOF, errors.New("synthetic-private-body " + testGeminiKey)} {
		body := &contractBody{reader: io.MultiReader(strings.NewReader(contractVectorJSON()), contractReadError{failure})}
		worker := testWorker(t, nil)
		worker.httpClient.Transport = contractRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
		})
		vector, err := worker.Embed(t.Context(), "synthetic-private-body")
		if err == nil || len(vector) != 0 || !body.closed || !retryableEmbeddingError(err) || strings.Contains(err.Error(), testGeminiKey) || strings.Contains(err.Error(), "synthetic-private-body") {
			t.Fatalf("전송 중단 분류·비밀 보호=%v", err)
		}
	}
}

func TestGeminiResponseBodyCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			entered := make(chan struct{})
			worker, _ := geminiWorker(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				close(entered)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := error(context.Canceled)
			if deadline {
				worker.httpClient.Timeout = 100 * time.Millisecond
				want = context.DeadlineExceeded
			}
			done := make(chan error, 1)
			go func() { _, err := worker.Embed(ctx, "synthetic-private-body"); done <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("응답 헤더 수신 실패")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) || !retryableEmbeddingError(err) {
					t.Fatalf("본문 대기 중 취소·시간 초과=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("본문 대기가 끝나지 않았다")
			}
		})
	}
}

func TestGeminiClientSecurityAndOwnership(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); writeGeminiVector(w, 128) }))
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	cfg := Config{Provider: "gemini", APIKey: testGeminiKey, BaseURL: address, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 128}
	for _, timeout := range []time.Duration{-time.Second, 0, time.Millisecond, 31 * time.Second} {
		client := server.Client()
		client.Timeout = timeout
		transport := client.Transport.(*http.Transport)
		worker, err := New(testStore(t), cfg, client, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := timeout
		if want <= 0 || want > 30*time.Second {
			want = 30 * time.Second
		}
		if worker.httpClient.Timeout != want || client.Timeout != timeout || client.CheckRedirect != nil || client.Transport != transport || worker.httpClient.Transport == transport {
			t.Fatal("주입 client 보존·유한 timeout·transport 분리 실패")
		}
		worker.httpClient.CloseIdleConnections()
	}
	unsafe := *server.Client()
	unsafe.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	if _, err := New(testStore(t), cfg, &unsafe, nil); err == nil {
		t.Fatal("TLS 인증서 검증 해제를 허용했다")
	}
	worker, err := New(testStore(t), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Embed(t.Context(), "synthetic"); err == nil || calls.Load() != 0 {
		t.Fatal("신뢰하지 않는 TLS 인증서를 허용했다")
	}
	trusted := server.Client()
	worker, err = New(testStore(t), cfg, trusted, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.httpClient.CloseIdleConnections)
	// 구성 원본을 바꿔도 생성한 제공자의 주소와 TLS 설정은 유지된다.
	address.Host = "invalid.test"
	trusted.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
	if _, err := worker.Embed(t.Context(), "synthetic"); err != nil || calls.Load() != 1 {
		t.Fatalf("구성 원본 변경의 격리=%v", err)
	}
	if worker.httpClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS 설정을 원본과 공유했다")
	}
}

func TestGeminiKeyContract(t *testing.T) {
	for _, control := range []rune{'\x00', '\t', '\n', '\r', '\x7f', '\u0085'} {
		t.Run(fmt.Sprintf("U+%04X", control), func(t *testing.T) {
			address, _ := url.Parse("https://generativelanguage.googleapis.com")
			cfg := Config{Provider: "gemini", APIKey: testGeminiKey + string(control), BaseURL: address, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 128}
			if _, err := New(testStore(t), cfg, nil, nil); err == nil || strings.Contains(err.Error(), testGeminiKey) {
				t.Fatalf("키 제어 문자 거부·비밀 보호=%v", err)
			}
		})
	}
}

func TestGeminiCloseReleasesOwnedTransport(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeGeminiVector(w, 128) }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	worker := testWorker(t, server)
	if _, err := worker.Embed(t.Context(), "synthetic"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
		t.Fatal("호출 직후 연결이 닫혀 정리 경계를 검사할 수 없다")
	default:
	}
	if err := worker.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("종료 뒤 제공자 연결이 남았다")
	}
}

func TestGeminiInputContract(t *testing.T) {
	for _, document := range []bool{false, true} {
		for _, test := range []struct {
			name, input string
			valid       bool
		}{
			{"empty", "", false}, {"space", "\t\n ", false}, {"invalid_utf8", string([]byte{0xff}), false},
			{"ascii_limit", strings.Repeat("a", 8000), true}, {"unicode_limit", strings.Repeat("한", 8000), true},
			{"over_limit", strings.Repeat("한", 8001), false}, {"escaped", "\x00\n\"\\한😀", true},
		} {
			t.Run(fmt.Sprintf("document=%t/%s", document, test.name), func(t *testing.T) {
				var calls atomic.Int64
				worker, _ := geminiWorker(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var payload struct {
						Model   string `json:"model"`
						Content struct {
							Parts []struct {
								Text string `json:"text"`
							} `json:"parts"`
						} `json:"content"`
						Config struct {
							Dimension int   `json:"outputDimensionality"`
							Truncate  *bool `json:"autoTruncate"`
						} `json:"embedContentConfig"`
					}
					if err := json.UnmarshalRead(r.Body, &payload); err != nil {
						t.Error(err)
						return
					}
					prefix := "task: search result | query: "
					if document {
						prefix = "title: none | text: "
					}
					if len(payload.Content.Parts) != 1 || payload.Content.Parts[0].Text != prefix+test.input || payload.Model != "models/gemini-embedding-2" || payload.Config.Dimension != 128 || payload.Config.Truncate == nil || *payload.Config.Truncate {
						t.Error("단일 입력·형식·절단 방지 계약이 다르다")
					}
					writeGeminiVector(w, 128)
				})
				call := worker.Embed
				if document {
					call = worker.EmbedDocument
				}
				_, err := call(t.Context(), test.input)
				if (err == nil) != test.valid || (test.valid && calls.Load() != 1) || (!test.valid && (calls.Load() != 0 || retryableEmbeddingError(err))) {
					t.Fatalf("입력 경계·호출 수=%d: %v", calls.Load(), err)
				}
			})
		}
	}
}
