package index

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Validate는 제공자 호출 전에 주소·인증과 벡터 계약을 검증한다.
func (cfg Config) Validate() error {
	if cfg.BaseURL == nil || (cfg.BaseURL.Scheme != "http" && cfg.BaseURL.Scheme != "https") || cfg.BaseURL.Host == "" || cfg.BaseURL.User != nil || cfg.BaseURL.RawQuery != "" || cfg.BaseURL.ForceQuery || cfg.BaseURL.Fragment != "" || cfg.BaseURL.Opaque != "" {
		return fmt.Errorf("임베딩 제공자 주소가 올바르지 않다")
	}
	if strings.TrimSpace(cfg.Model) == "" || (cfg.VectorType != "vector" && cfg.VectorType != "halfvec") || cfg.Dimension < 1 || (cfg.VectorType == "vector" && cfg.Dimension > 2000) || (cfg.VectorType == "halfvec" && cfg.Dimension > 4000) {
		return fmt.Errorf("임베딩 모델·저장 타입·차원이 올바르지 않다")
	}
	switch cfg.Provider {
	case "", "ollama":
	case "gemini":
		if cfg.BaseURL.Scheme != "https" || cfg.Model != "gemini-embedding-2" || cfg.Dimension < 128 || cfg.Dimension > 3072 || strings.TrimSpace(cfg.APIKey) == "" || strings.ContainsAny(cfg.APIKey, "\r\n") {
			return fmt.Errorf("Gemini 임베딩의 HTTPS·모델·차원·API 키 구성이 올바르지 않다")
		}
	default:
		return fmt.Errorf("임베딩 제공자는 ollama 또는 gemini여야 한다")
	}
	return nil
}

// ModelID는 제공자와 입력 형식까지 구분하고 기존 Ollama 식별자를 보존한다.
func (cfg Config) ModelID() string {
	id := ModelID(cfg.Model, cfg.VectorType, cfg.Dimension)
	if cfg.Provider == "gemini" {
		return "gemini:" + cfg.Model + ":retrieval-v1:" + cfg.VectorType + ":" + fmt.Sprint(cfg.Dimension)
	}
	return id
}

// ProviderError는 원문 응답 없이 장애 분류와 안전한 사유만 보존한다.
type ProviderError struct {
	Reason    string
	Retryable bool
}

// Error는 저장·공개·로그에 남겨도 본문과 API 키가 없는 사유다.
func (err ProviderError) Error() string { return err.Reason }

func retryableEmbeddingError(err error) bool {
	if _, ok := errors.AsType[DimensionError](err); ok {
		return false
	}
	if failure, ok := errors.AsType[ProviderError](err); ok {
		return failure.Retryable
	}
	return true
}

// Embed는 검색 질의를 본문과 같은 모델·차원의 벡터로 바꾼다.
func (worker *Worker) Embed(ctx context.Context, input string) ([]float64, error) {
	return worker.embed(ctx, input, false)
}

// EmbedDocument는 색인 본문을 문서 형식으로 호출한다.
func (worker *Worker) EmbedDocument(ctx context.Context, input string) ([]float64, error) {
	return worker.embed(ctx, input, true)
}

func (worker *Worker) embed(ctx context.Context, input string, document bool) ([]float64, error) {
	if !utf8.ValidString(input) || strings.TrimSpace(input) == "" || utf8.RuneCountInString(input) > 8000 {
		return nil, ProviderError{Reason: "임베딩 입력이 비어 있거나 UTF-8·8000자 한도를 위반했다"}
	}
	endpoint := worker.config.BaseURL.Clone()
	var payload any
	if worker.config.Provider == "gemini" {
		prefix := "task: search result | query: "
		if document {
			prefix = "title: none | text: "
		}
		payload = geminiRequest(worker.config.Model, prefix+input, worker.config.Dimension)
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1beta/models/" + worker.config.Model + ":embedContent"
	} else {
		payload = struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}{worker.config.Model, input}
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/embed"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, ProviderError{Reason: "임베딩 요청 직렬화 실패"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ProviderError{Reason: "임베딩 요청 생성 실패"}
	}
	request.Header.Set("Content-Type", "application/json")
	if worker.config.Provider == "gemini" {
		request.Header.Set("x-goog-api-key", worker.config.APIKey)
	}
	response, err := worker.httpClient.Do(request)
	if err != nil {
		return nil, safeTransportError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ProviderError{Reason: fmt.Sprintf("임베딩 제공자 상태: %d", response.StatusCode), Retryable: response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500}
	}
	limited := &io.LimitedReader{R: response.Body, N: maxEmbeddingResponseBytes + 1}
	var decoded struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.UnmarshalRead(limited, &decoded); err != nil {
		if limited.N <= 0 {
			return nil, ProviderError{Reason: "임베딩 응답이 크기 상한을 넘었다"}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("임베딩 응답 취소: %w", ctx.Err())
		}
		if network, ok := errors.AsType[net.Error](err); ok && network.Timeout() {
			return nil, fmt.Errorf("임베딩 응답 시간 초과: %w", context.DeadlineExceeded)
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ProviderError{Reason: "임베딩 응답 전송이 중단됐다", Retryable: true}
		}
		return nil, ProviderError{Reason: "임베딩 응답 해석 실패"}
	}
	if limited.N <= 0 {
		return nil, ProviderError{Reason: "임베딩 응답이 크기 상한을 넘었다"}
	}
	embedding := decoded.Embedding.Values
	if worker.config.Provider != "gemini" {
		if len(decoded.Embeddings) != 1 {
			return nil, ProviderError{Reason: "임베딩 응답 벡터 개수가 올바르지 않다"}
		}
		embedding = decoded.Embeddings[0]
	}
	if len(embedding) != worker.config.Dimension {
		return nil, DimensionError{Got: len(embedding), Want: worker.config.Dimension}
	}
	nonzero := false
	for _, value := range embedding {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, ProviderError{Reason: "임베딩 응답에 유한하지 않은 값이 있다"}
		}
		nonzero = nonzero || value != 0
	}
	if !nonzero {
		return nil, ProviderError{Reason: "임베딩 응답이 영벡터다"}
	}
	return embedding, nil
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiConfig struct {
	Dimension int  `json:"outputDimensionality"`
	Truncate  bool `json:"autoTruncate"`
}

func geminiRequest(model, text string, dimension int) any {
	return struct {
		Model   string        `json:"model"`
		Content geminiContent `json:"content"`
		Config  geminiConfig  `json:"embedContentConfig"`
	}{"models/" + model, geminiContent{[]geminiPart{{Text: text}}}, geminiConfig{Dimension: dimension}}
}

func safeTransportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("임베딩 요청 취소: %w", ctx.Err())
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("임베딩 요청 취소: %w", context.Canceled)
	}
	if network, ok := errors.AsType[net.Error](err); ok && network.Timeout() {
		return fmt.Errorf("임베딩 요청 시간 초과: %w", context.DeadlineExceeded)
	}
	return ProviderError{Reason: "임베딩 전송 실패", Retryable: true}
}
