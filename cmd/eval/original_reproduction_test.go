package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var originalCapturedBody = []byte("private-before|retained-original|private-after")

func originalFixture(t *testing.T, address string) (originalInput, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "capture.bin"), originalCapturedBody, 0600); err != nil {
		t.Fatal(err)
	}
	input := originalInput{Version: "original-v1", CollectorVersion: "https-byte-v1", Provenance: "synthetic", TimeoutMS: 1000, MaxResponseBytes: 1024, RequestLimit: len(originalScenarios) * 10,
		Origins: []originalOrigin{{Code: "origin1", URL: address}}}
	start, end := len("private-before|"), len("private-before|retained-original")
	for _, scenario := range originalScenarios {
		sample := originalSample{Code: scenario, Independent: "resource1", Scenario: scenario, Origin: "origin1",
			Locator:  fmt.Sprintf("%s/%s?revision=rev1#bytes=%d-%d", address, scenario, start, end),
			Revision: &originalRevision{Binding: "query", QueryKey: "revision", Value: "rev1", ResponseHeader: "X-Source-Revision", ResponseValue: "rev1"},
			Captured: &originalCapture{File: "capture.bin", SHA256: fmt.Sprintf("%x", sha256.Sum256(originalCapturedBody))}}
		if scenario == "unversioned" {
			sample.Revision = nil
			sample.Locator = fmt.Sprintf("%s/unversioned#bytes=%d-%d", address, start, end)
		}
		input.Samples = append(input.Samples, sample)
	}
	return input, filepath.Join(dir, "input.json")
}

func writeOriginalFixture(t *testing.T, input originalInput, path string) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOriginalInputContracts(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*originalInput)
	}{
		{"empty_version", func(i *originalInput) { i.Version = "" }},
		{"timeout", func(i *originalInput) { i.TimeoutMS = 0 }},
		{"response_cap", func(i *originalInput) { i.MaxResponseBytes = maxOriginalBytes + 1 }},
		{"request_limit", func(i *originalInput) { i.RequestLimit-- }},
		{"duplicate_sample", func(i *originalInput) { i.Samples[1].Code = i.Samples[0].Code }},
		{"no_independent_unit", func(i *originalInput) { i.Samples[0].Independent = "" }},
		{"unknown_scenario", func(i *originalInput) { i.Samples[0].Scenario = "unknown" }},
		{"http", func(i *originalInput) { i.Origins[0].URL = "http://127.0.0.1:9" }},
		{"userinfo", func(i *originalInput) {
			i.Samples[0].Locator = "https://private-user:private-token@127.0.0.1:9/fixed_version"
		}},
		{"unapproved_origin", func(i *originalInput) { i.Samples[0].Locator = "https://unapproved.invalid/fixed_version" }},
		{"origin_path", func(i *originalInput) { i.Origins[0].URL += "/path" }},
		{"duplicate_origin", func(i *originalInput) {
			i.Origins = append(i.Origins, originalOrigin{Code: "origin2", URL: i.Origins[0].URL})
		}},
		{"scope_syntax", func(i *originalInput) {
			i.Samples[0].Locator = "https://127.0.0.1:9/fixed_version?revision=rev1#bytes=1-1"
		}},
		{"scope_over_cap", func(i *originalInput) {
			i.Samples[0].Locator = "https://127.0.0.1:9/fixed_version?revision=rev1#bytes=1-1025"
		}},
		{"scope_outside_capture", func(i *originalInput) {
			i.Samples[0].Locator = "https://127.0.0.1:9/fixed_version?revision=rev1#bytes=1-100"
		}},
		{"revision_mismatch", func(i *originalInput) { i.Samples[0].Revision.Value = "rev2" }},
		{"duplicate_revision_query", func(i *originalInput) {
			i.Samples[0].Locator = "https://127.0.0.1:9/fixed_version?revision=rev1&revision=rev1#bytes=1-2"
		}},
		{"weak_etag", func(i *originalInput) {
			r := i.Samples[0].Revision
			r.ResponseHeader = "ETag"
			r.ResponseValue = `W/"rev1"`
		}},
		{"single_quote_etag", func(i *originalInput) { r := i.Samples[0].Revision; r.ResponseHeader = "ETag"; r.ResponseValue = `"` }},
		{"inner_quote_etag", func(i *originalInput) {
			r := i.Samples[0].Revision
			r.ResponseHeader = "ETag"
			r.ResponseValue = `"rev"1"`
		}},
		{"control_etag", func(i *originalInput) {
			r := i.Samples[0].Revision
			r.ResponseHeader = "ETag"
			r.ResponseValue = "\"rev\t1\""
		}},
		{"unsafe_revision_header", func(i *originalInput) { i.Samples[0].Revision.ResponseHeader = "Authorization" }},
		{"stale_capture", func(i *originalInput) { i.Samples[0].Captured.SHA256 = strings.Repeat("a", 64) }},
		{"missing_capture", func(i *originalInput) { i.Samples[0].Captured.File = "private-missing.bin" }},
		{"escape_capture", func(i *originalInput) { i.Samples[0].Captured.File = "../private.bin" }},
		{"absolute_capture", func(i *originalInput) { i.Samples[0].Captured.File = "/private/private.bin" }},
		{"missing_credential", func(i *originalInput) { i.Origins[0].TokenEnv = "ACS_EVAL10_MISSING_TOKEN" }},
		{"bad_credential_name", func(i *originalInput) { i.Origins[0].TokenEnv = "private-token" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ACS_EVAL10_MISSING_TOKEN", "")
			input, path := originalFixture(t, "https://127.0.0.1:9")
			test.change(&input)
			writeOriginalFixture(t, input, path)
			_, _, err := loadOriginalInput(path)
			if err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "https:") {
				t.Fatalf("사전 검증·비공개 값: %v", err)
			}
		})
	}
	t.Run("symlink_escape", func(t *testing.T) {
		input, path := originalFixture(t, "https://127.0.0.1:9")
		outside := filepath.Join(t.TempDir(), "outside.bin")
		if err := os.WriteFile(outside, originalCapturedBody, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(filepath.Dir(path), "escape.bin")); err != nil {
			t.Fatal(err)
		}
		input.Samples[0].Captured.File = "escape.bin"
		writeOriginalFixture(t, input, path)
		if _, _, err := loadOriginalInput(path); err == nil {
			t.Fatal("snapshot root 밖 symlink 허용")
		}
	})
	t.Run("unknown_json", func(t *testing.T) {
		input, path := originalFixture(t, "https://127.0.0.1:9")
		writeOriginalFixture(t, input, path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw = append([]byte(`{"private-unknown":"private-value",`), raw[1:]...)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadOriginalInput(path); err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatalf("엄격한 JSON: %v", err)
		}
	})
}

func TestOriginalHTTPAndContentBoundaries(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   []byte
		header http.Header
		want   string
		match  float64
	}{
		{"same_scope", 200, originalCapturedBody, http.Header{"X-Source-Revision": {"rev1"}}, "matched", 1},
		{"outside_scope_changed", 200, []byte("changed-before|retained-original|changed-after"), http.Header{"X-Source-Revision": {"rev1"}}, "matched", 1},
		{"new_latest", 200, []byte("private-before|changed-original|private-after"), http.Header{"X-Source-Revision": {"rev2"}}, "version_mismatch", 0},
		{"same_bytes_wrong_revision", 200, originalCapturedBody, http.Header{"X-Source-Revision": {"rev2"}}, "version_mismatch", 1},
		{"duplicate_revision", 200, originalCapturedBody, http.Header{"X-Source-Revision": {"rev1", "rev1"}}, "version_mismatch", 1},
		{"partial_response", 206, originalCapturedBody, nil, "unexpected_status", -1},
		{"not_modified", 304, nil, nil, "unexpected_status", -1},
		{"range_gone", 200, []byte("short"), nil, "range_missing", -1},
		{"gzip", 200, originalCapturedBody, http.Header{"Content-Encoding": {"gzip"}}, "unsupported_encoding", -1},
		{"oversize", 200, bytes.Repeat([]byte("x"), 1025), nil, "response_too_large", -1},
		{"permission", 401, nil, nil, "forbidden", -1},
		{"gone", 410, nil, nil, "deleted", -1},
		{"rate_limited", 429, nil, nil, "transient_failure", -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Fragment != "" {
					t.Error("fragment가 요청에 포함됐다")
				}
				for name, values := range test.header {
					w.Header()[name] = values
				}
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			}))
			defer server.Close()
			input, path := originalFixture(t, server.URL)
			writeOriginalFixture(t, input, path)
			_, samples, err := loadOriginalInput(path)
			if err != nil {
				t.Fatal(err)
			}
			got := fetchOriginal(t.Context(), server.Client(), input, samples[0], 1)
			if got.Status != test.want || got.ContentMatch != test.match || got.HTTPStatus != test.status {
				t.Fatalf("%+v", got)
			}
		})
	}
	t.Run("unmeasured_capture", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Source-Revision", "rev1")
			_, _ = w.Write(originalCapturedBody)
		}))
		defer server.Close()
		input, path := originalFixture(t, server.URL)
		input.Samples[0].Captured = nil
		writeOriginalFixture(t, input, path)
		_, samples, err := loadOriginalInput(path)
		if err != nil {
			t.Fatal(err)
		}
		got := fetchOriginal(t.Context(), server.Client(), input, samples[0], 1)
		if got.ContentMatch != -1 || got.Status != "unmeasured" || !got.Reachable || got.VersionMatch != 1 {
			t.Fatal(got)
		}
	})
	t.Run("transport", func(t *testing.T) {
		input, path := originalFixture(t, "https://127.0.0.1:1")
		writeOriginalFixture(t, input, path)
		_, samples, err := loadOriginalInput(path)
		if err != nil {
			t.Fatal(err)
		}
		got := fetchOriginal(t.Context(), &http.Client{}, input, samples[0], 1)
		if got.ContentMatch != -1 || got.Status != "transport_failure" {
			t.Fatal(got)
		}
	})
}

func TestOriginalLocatorAdapters(t *testing.T) {
	input, path := originalFixture(t, "https://127.0.0.1:9")
	input.Samples[0].Locator = "https://127.0.0.1:9/commits/rev1/file#bytes=0-1"
	input.Samples[0].Revision = &originalRevision{Binding: "path", Value: "rev1", ResponseHeader: "ETag", ResponseValue: `"rev1"`}
	input.Samples[1].Locator = "https://127.0.0.1:9/content_changed?revision=rev1"
	writeOriginalFixture(t, input, path)
	_, samples, err := loadOriginalInput(path)
	if err != nil {
		t.Fatal(err)
	}
	if samples[0].span == nil || samples[0].spec.Revision.Binding != "path" || samples[1].span != nil || !bytes.Equal(samples[1].expected, originalCapturedBody) {
		t.Fatal("판·범위/전체 본문 준비")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	output := filepath.Join(t.TempDir(), "result.json")
	if err := runOriginalCommand(ctx, path, output); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("취소 보고서 생성")
	}
}

func TestOriginalIndependentMetrics(t *testing.T) {
	var attempts []originalAttempt
	for sample := range 3 {
		for repeat := 1; repeat <= 10; repeat++ {
			unit, match := "unit1", float64(1)
			if sample == 2 {
				unit = "unit2"
				match = -1
			}
			attempts = append(attempts, originalAttempt{Code: fmt.Sprintf("s%d", sample), Independent: unit, Scenario: "fixed_version", Repeat: repeat, VersionKnown: true, RangeKnown: sample != 2, ContentMatch: match, Status: map[bool]string{true: "matched", false: "timeout"}[match == 1]})
		}
	}
	summary, groups, err := aggregateOriginals(attempts)
	if err != nil || summary.Samples != 3 || summary.Attempts != 30 || summary.Matched != 20 || summary.Unmeasured != 10 || summary.Identifiable != 2 || summary.Independent.Samples != 2 || summary.MatchRate != 2.0/3 || groups["fixed_version"].Attempts != 30 {
		t.Fatalf("%+v %v", summary, err)
	}
	if _, _, err := aggregateOriginals(attempts[:29]); err == nil {
		t.Fatal("누락 회차")
	}
	duplicate := slices.Clone(attempts)
	duplicate[1] = duplicate[0]
	if _, _, err := aggregateOriginals(duplicate); err == nil {
		t.Fatal("중복 회차")
	}
	duplicate = slices.Clone(attempts)
	duplicate[1].Independent = "other"
	if _, _, err := aggregateOriginals(duplicate); err == nil {
		t.Fatal("독립 단위 변경")
	}
	single, _, err := aggregateOriginals(attempts[:10])
	if err != nil || single.Independent.Interval != -1 {
		t.Fatalf("단일 표본 구간 %+v %v", single, err)
	}
	if _, _, err := aggregateOriginals(nil); err == nil {
		t.Fatal("빈 집계")
	}
}
