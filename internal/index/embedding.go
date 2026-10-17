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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Validate는 제공자 호출 전에 주소·인증과 벡터 계약을 검증한다.
func (cfg Config) Validate() error {
	if err := cfg.ValidateMetadata(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.APIKey) == "" || strings.ContainsFunc(cfg.APIKey, unicode.IsControl) {
		return fmt.Errorf("Gemini 임베딩의 API 키 구성이 올바르지 않다")
	}
	return nil
}

// ValidateMetadata는 읽기 전용 감사에서도 제공자·주소·모델·벡터 계약을 검증한다.
// 외부 호출을 하지 않는 감사에 API 키를 요구하지 않도록 호출용 인증과 구분한다.
func (cfg Config) ValidateMetadata() error {
	if cfg.BaseURL == nil || (cfg.BaseURL.Scheme != "http" && cfg.BaseURL.Scheme != "https") || cfg.BaseURL.Host == "" || cfg.BaseURL.User != nil || cfg.BaseURL.RawQuery != "" || cfg.BaseURL.ForceQuery || cfg.BaseURL.Fragment != "" || cfg.BaseURL.Opaque != "" {
		return fmt.Errorf("임베딩 제공자 주소가 올바르지 않다")
	}
	if strings.TrimSpace(cfg.Model) == "" || (cfg.VectorType != "vector" && cfg.VectorType != "halfvec") || cfg.Dimension < 1 || (cfg.VectorType == "vector" && cfg.Dimension > 2000) || (cfg.VectorType == "halfvec" && cfg.Dimension > 4000) {
		return fmt.Errorf("임베딩 모델·저장 타입·차원이 올바르지 않다")
	}
	if cfg.BaseURL.Hostname() == "" || strings.HasSuffix(cfg.BaseURL.Host, ":") {
		return fmt.Errorf("임베딩 제공자 host·포트가 올바르지 않다")
	}
	if port := cfg.BaseURL.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return fmt.Errorf("임베딩 제공자 포트가 올바르지 않다")
		}
	}
	if cfg.Provider != "gemini" {
		return fmt.Errorf("임베딩 제공자는 gemini로 명시해야 한다")
	}
	if cfg.BaseURL.Scheme != "https" || cfg.Model != "gemini-embedding-2" || cfg.Dimension < 128 || cfg.Dimension > 3072 {
		return fmt.Errorf("Gemini 임베딩의 HTTPS·모델·차원 구성이 올바르지 않다")
	}
	return nil
}

// ModelID는 제공자·모델·입력 형식과 벡터 저장 구성을 구분한다.
func (cfg Config) ModelID() string {
	return "gemini:" + cfg.Model + ":retrieval-v1:" + cfg.VectorType + ":" + fmt.Sprint(cfg.Dimension)
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
	prefix := "task: search result | query: "
	if document {
		prefix = "title: none | text: "
	}
	payload := geminiRequest(worker.config.Model, prefix+input, worker.config.Dimension)
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1beta/models/" + worker.config.Model + ":embedContent"
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, ProviderError{Reason: "임베딩 요청 직렬화 실패"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ProviderError{Reason: "임베딩 요청 생성 실패"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", worker.config.APIKey)
	response, err := worker.httpClient.Do(request)
	if err != nil {
		return nil, safeTransportError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ProviderError{Reason: fmt.Sprintf("임베딩 제공자 상태: %d", response.StatusCode), Retryable: response.StatusCode == 408 || response.StatusCode == 429 || (response.StatusCode >= 500 && response.StatusCode < 600)}
	}
	// 전송 실패와 완전히 수신한 JSON의 계약 위반을 분리한다.
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxEmbeddingResponseBytes+1))
	if len(raw) > maxEmbeddingResponseBytes {
		return nil, ProviderError{Reason: "임베딩 응답이 크기 상한을 넘었다"}
	}
	if err != nil {
		return nil, safeTransportError(ctx, err)
	}
	var decoded struct {
		Embedding struct {
			Values []*float64 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, ProviderError{Reason: "임베딩 응답 해석 실패"}
	}
	if len(decoded.Embedding.Values) != worker.config.Dimension {
		return nil, DimensionError{Got: len(decoded.Embedding.Values), Want: worker.config.Dimension}
	}
	embedding := make([]float64, worker.config.Dimension)
	nonzero := false
	for i, number := range decoded.Embedding.Values {
		if number == nil {
			return nil, ProviderError{Reason: "임베딩 응답에 숫자가 아닌 값이 있다"}
		}
		value := *number
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, ProviderError{Reason: "임베딩 응답에 유한하지 않은 값이 있다"}
		}
		nonzero = nonzero || value != 0
		embedding[i] = value
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
