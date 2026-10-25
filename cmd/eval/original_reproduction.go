package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

type originalReport struct {
	Version          string                     `json:"version"`
	CollectorVersion string                     `json:"collector_version"`
	Provenance       string                     `json:"provenance"`
	StartedAt        time.Time                  `json:"started_at"`
	Repeats          int                        `json:"repeats"`
	TimeoutMS        int                        `json:"timeout_ms"`
	MaxResponseBytes int64                      `json:"max_response_bytes"`
	RequestLimit     int                        `json:"request_limit"`
	Attempts         []originalAttempt          `json:"attempts"`
	Overall          originalSummary            `json:"overall"`
	Scenarios        map[string]originalSummary `json:"scenarios"`
}

type originalAttempt struct {
	Code          string  `json:"sample"`
	Independent   string  `json:"independent_unit"`
	Scenario      string  `json:"scenario"`
	Repeat        int     `json:"repeat"`
	VersionKnown  bool    `json:"version_identifiable"`
	RangeKnown    bool    `json:"range_identifiable"`
	Reachable     bool    `json:"http_200"`
	HTTPStatus    int     `json:"http_status"`
	Status        string  `json:"status"`
	VersionMatch  float64 `json:"response_version_match"`
	ContentMatch  float64 `json:"content_match"`
	ComparedBytes int     `json:"compared_bytes"`
	LatencyMS     float64 `json:"latency_ms"`
}

func runOriginalCommand(ctx context.Context, path, output string) error {
	input, samples, err := loadOriginalInput(path)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy, transport.DisableCompression = nil, true
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result := originalReport{Version: input.Version, CollectorVersion: input.CollectorVersion, Provenance: input.Provenance,
		StartedAt: time.Now().UTC(), Repeats: initialEvaluationRepeats, TimeoutMS: input.TimeoutMS, MaxResponseBytes: input.MaxResponseBytes, RequestLimit: input.RequestLimit}
	for repeat := 1; repeat <= initialEvaluationRepeats; repeat++ {
		for offset := range samples {
			if err := ctx.Err(); err != nil {
				return err
			}
			sample := samples[(offset+repeat-1)%len(samples)]
			attempt := fetchOriginal(ctx, client, input, sample, repeat)
			if err := ctx.Err(); err != nil {
				return err
			}
			result.Attempts = append(result.Attempts, attempt)
		}
	}
	result.Overall, result.Scenarios, err = aggregateOriginals(result.Attempts)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeReport(output, result)
}

func fetchOriginal(ctx context.Context, client *http.Client, input originalInput, sample preparedOriginal, repeat int) (result originalAttempt) {
	started := time.Now()
	defer func() { result.LatencyMS = float64(time.Since(started).Microseconds()) / 1000 }()
	result = originalAttempt{Code: sample.spec.Code, Independent: sample.spec.Independent, Scenario: sample.spec.Scenario, Repeat: repeat,
		VersionKnown: sample.spec.Revision != nil, RangeKnown: sample.span != nil, VersionMatch: -1, ContentMatch: -1}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutMS)*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, sample.url.String(), nil)
	if err != nil {
		result.Status = "request_failure"
		return result
	}
	request.Header.Set("Accept-Encoding", "identity")
	if sample.token != "" {
		request.Header.Set("Authorization", "Bearer "+sample.token)
	}
	response, err := client.Do(request)
	if err != nil {
		result.Status = "transport_failure"
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			result.Status = "timeout"
		}
		return result
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	if response.StatusCode != http.StatusOK {
		switch {
		case response.StatusCode >= 300 && response.StatusCode <= 308 && response.StatusCode != http.StatusNotModified:
			result.Status = "moved"
		case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
			result.Status = "deleted"
		case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
			result.Status = "forbidden"
		case response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
			result.Status = "transient_failure"
		default:
			result.Status = "unexpected_status"
		}
		return result
	}
	result.Reachable = true
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		result.Status = "unsupported_encoding"
		return result
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, input.MaxResponseBytes+1))
	if err != nil {
		result.Status = "response_read_failure"
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			result.Status = "timeout"
		}
		return result
	}
	if int64(len(body)) > input.MaxResponseBytes {
		result.Status = "response_too_large"
		return result
	}
	selected, err := selectOriginalBytes(body, sample.span)
	if err != nil {
		result.Status = "range_missing"
		return result
	}
	result.Status = "unmeasured"
	if sample.spec.Captured != nil {
		result.ComparedBytes, result.ContentMatch = len(selected), 0
		result.Status = "content_changed"
		if bytes.Equal(sample.expected, selected) {
			result.ContentMatch, result.Status = 1, "matched"
		}
	}
	if revision := sample.spec.Revision; revision != nil && revision.ResponseHeader != "" {
		result.VersionMatch = 0
		if values := response.Header.Values(revision.ResponseHeader); len(values) == 1 && values[0] == revision.ResponseValue {
			result.VersionMatch = 1
		} else {
			result.Status = "version_mismatch"
		}
	}
	return result
}
