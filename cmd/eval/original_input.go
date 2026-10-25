package main

import (
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const maxOriginalBytes = 64 << 20

var originalEnvName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
var originalScenarios = []string{"fixed_version", "content_changed", "moved", "deleted", "forbidden", "transient_failure", "unversioned"}

type originalInput struct {
	Version          string           `json:"version"`
	CollectorVersion string           `json:"collector_version"`
	Provenance       string           `json:"provenance"`
	TimeoutMS        int              `json:"timeout_ms"`
	MaxResponseBytes int64            `json:"max_response_bytes"`
	RequestLimit     int              `json:"request_limit"`
	Origins          []originalOrigin `json:"origins"`
	Samples          []originalSample `json:"samples"`
}

type originalOrigin struct {
	Code     string `json:"code"`
	URL      string `json:"url"`
	TokenEnv string `json:"bearer_token_env,omitempty"`
}

type originalSample struct {
	Code        string            `json:"code"`
	Independent string            `json:"independent_unit"`
	Scenario    string            `json:"scenario"`
	Origin      string            `json:"origin"`
	Locator     string            `json:"locator"`
	Revision    *originalRevision `json:"revision,omitzero"`
	Captured    *originalCapture  `json:"captured,omitzero"`
}

type originalRevision struct {
	Binding        string `json:"binding"`
	QueryKey       string `json:"query_key,omitempty"`
	Value          string `json:"value"`
	ResponseHeader string `json:"response_header,omitempty"`
	ResponseValue  string `json:"response_value,omitempty"`
}

type originalCapture struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type originalSpan struct{ start, end int64 }

type preparedOriginal struct {
	spec     originalSample
	url      *url.URL
	span     *originalSpan
	expected []byte
	token    string
}

func originalOriginKey(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func parseOriginalURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return nil, fmt.Errorf("원본 비교 HTTPS URI가 올바르지 않다")
	}
	if u.Port() != "" {
		n, err := strconv.Atoi(u.Port())
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("원본 비교 URI 포트가 올바르지 않다")
		}
	}
	return u, nil
}

func parseOriginalSpan(u *url.URL, limit int64) (*originalSpan, error) {
	if u.Fragment == "" {
		return nil, nil
	}
	fragment, err := url.ParseQuery(u.Fragment)
	if err != nil || len(fragment) != 1 || len(fragment["bytes"]) != 1 {
		return nil, fmt.Errorf("원본 비교 바이트 범위가 올바르지 않다")
	}
	left, right, ok := strings.Cut(fragment.Get("bytes"), "-")
	start, e1 := strconv.ParseInt(left, 10, 64)
	end, e2 := strconv.ParseInt(right, 10, 64)
	if !ok || e1 != nil || e2 != nil || start < 0 || end <= start || end > limit {
		return nil, fmt.Errorf("원본 비교 바이트 범위가 올바르지 않다")
	}
	return &originalSpan{start, end}, nil
}

func validateOriginalRevision(revision *originalRevision, u *url.URL) error {
	if revision == nil {
		return nil
	}
	invalid := func() error { return fmt.Errorf("원본 비교 판 결합이 올바르지 않다") }
	if revision.Value == "" || len(revision.Value) > 256 || strings.ContainsAny(revision.Value+revision.ResponseValue, "\r\n") {
		return invalid()
	}
	switch revision.Binding {
	case "query":
		values, err := url.ParseQuery(u.RawQuery)
		if err != nil || revision.QueryKey == "" || len(values[revision.QueryKey]) != 1 || values.Get(revision.QueryKey) != revision.Value {
			return invalid()
		}
	case "path":
		if revision.QueryKey != "" || !slices.Contains(strings.Split(u.Path, "/"), revision.Value) {
			return invalid()
		}
	default:
		return invalid()
	}
	if revision.ResponseHeader == "" {
		if revision.ResponseValue != "" {
			return invalid()
		}
		return nil
	}
	if !slices.Contains([]string{"ETag", "X-Source-Revision"}, revision.ResponseHeader) || revision.ResponseValue == "" {
		return invalid()
	}
	if revision.ResponseHeader == "ETag" && !originalStrongETag(revision.ResponseValue) {
		return invalid()
	}
	return nil
}

func originalStrongETag(value string) bool {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	for _, c := range []byte(value[1 : len(value)-1]) {
		if !(c == 0x21 || c >= 0x23 && c <= 0x7e || c >= 0x80) {
			return false
		}
	}
	return true
}

// 모든 URI·원본 파일·자격을 준비한 다음에만 요청을 보낸다. 준비 오류에는 경로,
// 응답·URI 원문이나 환경 변수의 자격 값을 복사하지 않는다.
func loadOriginalInput(path string) (originalInput, []preparedOriginal, error) {
	file, err := os.Open(path)
	if err != nil {
		return originalInput{}, nil, fmt.Errorf("원본 비교 입력을 읽을 수 없다")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil || len(raw) > maxManifestBytes {
		return originalInput{}, nil, fmt.Errorf("원본 비교 입력 읽기 또는 크기 제한 실패")
	}
	var input originalInput
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)); err != nil {
		return originalInput{}, nil, fmt.Errorf("원본 비교 JSON 형식 또는 허용 필드가 올바르지 않다")
	}
	if !manifestCode.MatchString(input.Version) || !manifestCode.MatchString(input.CollectorVersion) || !slices.Contains([]string{"synthetic", "authorized_origin"}, input.Provenance) || input.TimeoutMS < 1 || input.TimeoutMS > 30000 || input.MaxResponseBytes < 1 || input.MaxResponseBytes > maxOriginalBytes || len(input.Samples) == 0 || len(input.Samples) > 256 || len(input.Origins) == 0 || input.RequestLimit < len(input.Samples)*initialEvaluationRepeats || input.RequestLimit > 2560 {
		return originalInput{}, nil, fmt.Errorf("원본 비교 판·출처·표본·요청 상한이 올바르지 않다")
	}
	origins, addresses, tokens := map[string]string{}, map[string]bool{}, map[string]string{}
	for _, origin := range input.Origins {
		u, err := parseOriginalURL(origin.URL)
		if err != nil || !manifestCode.MatchString(origin.Code) || origins[origin.Code] != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || addresses[originalOriginKey(u)] {
			return originalInput{}, nil, fmt.Errorf("원본 비교 승인 origin이 올바르지 않다")
		}
		key := originalOriginKey(u)
		origins[origin.Code], addresses[key] = key, true
		if origin.TokenEnv != "" {
			if !originalEnvName.MatchString(origin.TokenEnv) {
				return originalInput{}, nil, fmt.Errorf("원본 비교 자격 환경 변수 이름이 올바르지 않다")
			}
			token := os.Getenv(origin.TokenEnv)
			if token == "" || strings.ContainsAny(token, " \t\r\n") {
				return originalInput{}, nil, fmt.Errorf("원본 비교 승인 출처의 자격이 준비되지 않았다")
			}
			tokens[origin.Code] = token
		}
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return originalInput{}, nil, fmt.Errorf("원본 비교 입력 디렉터리를 열 수 없다")
	}
	defer root.Close()
	var prepared []preparedOriginal
	var totalBytes int64
	seen := map[string]bool{}
	for _, sample := range input.Samples {
		if !manifestCode.MatchString(sample.Code) || !manifestCode.MatchString(sample.Independent) || seen[sample.Code] || !slices.Contains(originalScenarios, sample.Scenario) || origins[sample.Origin] == "" {
			return originalInput{}, nil, fmt.Errorf("원본 비교 표본 코드·독립 단위·통제 유형이 올바르지 않다")
		}
		seen[sample.Code] = true
		u, err := parseOriginalURL(sample.Locator)
		if err != nil || originalOriginKey(u) != origins[sample.Origin] {
			return originalInput{}, nil, fmt.Errorf("원본 비교 locator가 승인 출처와 맞지 않는다")
		}
		span, err := parseOriginalSpan(u, input.MaxResponseBytes)
		if err != nil {
			return originalInput{}, nil, err
		}
		if err := validateOriginalRevision(sample.Revision, u); err != nil {
			return originalInput{}, nil, err
		}
		item := preparedOriginal{spec: sample, url: u.Clone(), span: span, token: tokens[sample.Origin]}
		// URL fragment는 식별용이며 HTTP 요청에 전달하지 않는다.
		item.url.Fragment, item.url.RawFragment = "", ""
		if sample.Captured != nil {
			capture := sample.Captured
			if !filepath.IsLocal(capture.File) || !manifestSHA256.MatchString(capture.SHA256) {
				return originalInput{}, nil, fmt.Errorf("원본 비교 snapshot 경로·checksum이 올바르지 않다")
			}
			body, err := readOriginalCapture(root, capture.File, input.MaxResponseBytes)
			if err != nil {
				return originalInput{}, nil, err
			}
			totalBytes += int64(len(body))
			if totalBytes > maxOriginalBytes || fmt.Sprintf("%x", sha256.Sum256(body)) != capture.SHA256 {
				return originalInput{}, nil, fmt.Errorf("원본 비교 snapshot 전체 크기 또는 checksum 대조 실패")
			}
			item.expected, err = selectOriginalBytes(body, span)
			if err != nil {
				return originalInput{}, nil, fmt.Errorf("원본 비교 snapshot 범위를 찾을 수 없다")
			}
		}
		prepared = append(prepared, item)
	}
	return input, prepared, nil
}

func readOriginalCapture(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("원본 비교 snapshot을 읽을 수 없다")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("원본 비교 snapshot은 일반 파일이어야 한다")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, fmt.Errorf("원본 비교 snapshot 읽기 또는 크기 제한 실패")
	}
	return body, nil
}

func selectOriginalBytes(body []byte, span *originalSpan) ([]byte, error) {
	if span == nil {
		return body, nil
	}
	if span.end > int64(len(body)) {
		return nil, fmt.Errorf("range_missing")
	}
	return body[span.start:span.end], nil
}
