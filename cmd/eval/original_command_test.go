package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOriginalCommandIntegration(t *testing.T) {
	var calls, redirects atomic.Int64
	var lock sync.Mutex
	var order []string
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirects.Add(1); _, _ = w.Write(originalCapturedBody) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer private-eval10-token" || r.URL.Fragment != "" {
			t.Error("승인 자격 또는 locator 요청 범위가 다르다")
		}
		scenario := strings.TrimPrefix(r.URL.Path, "/")
		lock.Lock()
		order = append(order, scenario)
		lock.Unlock()
		if scenario != "unversioned" && r.URL.Query().Get("revision") != "rev1" {
			t.Error("URI의 수집 판 누락")
		}
		w.Header().Set("X-Source-Revision", "rev1")
		switch scenario {
		case "content_changed":
			w.Header().Set("X-Source-Revision", "rev2")
			_, _ = w.Write([]byte("private-before|changed-original|private-after"))
		case "moved":
			http.Redirect(w, r, target.URL+"/private-moved", http.StatusFound)
		case "deleted":
			w.WriteHeader(http.StatusNotFound)
		case "forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "transient_failure":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = w.Write(originalCapturedBody)
		}
	}))
	defer server.Close()
	trustEvalEmbeddingServer(t, server)
	t.Setenv("ACS_EVAL10_TOKEN", "private-eval10-token")
	t.Setenv("EVAL_TEST_COMMAND_PROCESS", "1")
	t.Setenv("DATABASE_URL", "postgres://127.0.0.1:1/no-db")
	t.Setenv("EMBEDDING_PROVIDER", "")
	input, path := originalFixture(t, server.URL)
	input.Origins[0].TokenEnv = "ACS_EVAL10_TOKEN"
	writeOriginalFixture(t, input, path)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(output string, extra ...string) ([]byte, error) {
		args := []string{"-test.run=^TestEvalCommandProcess$", "--", "-original-reproduction=" + path, "-out=" + output}
		return exec.CommandContext(t.Context(), binary, append(args, extra...)...).CombinedOutput()
	}
	output := filepath.Join(t.TempDir(), "result.json")
	logs, err := invoke(output)
	if err != nil {
		t.Fatalf("원본 CLI: %v\n%s", err, logs)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-", server.URL, target.URL, "capture.bin", "https://"} {
		if bytes.Contains(body, []byte(private)) || bytes.Contains(logs, []byte(private)) {
			t.Fatal("원본·URI·자격이 결과나 로그에 포함됐다")
		}
	}
	var result originalReport
	if err := json.Unmarshal(body, &result, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	s := result.Overall
	if s.Samples != 7 || s.Identifiable != 6 || s.IdentifiableRate != 6.0/7 || s.Attempts != 70 || s.Reachable != 30 || s.Compared != 30 || s.Matched != 20 || s.Changed != 10 || s.Unmeasured != 40 || s.MatchRate != 2.0/7 || s.Independent.Samples != 1 || s.Independent.Interval != -1 || len(result.Scenarios) != 7 || len(result.Attempts) != 70 || result.StartedAt.IsZero() {
		t.Fatalf("분모·미측정: %+v", s)
	}
	if calls.Load() != 70 || redirects.Load() != 0 {
		t.Fatalf("GET/redirect 횟수: %d %d", calls.Load(), redirects.Load())
	}
	lock.Lock()
	gotOrder := append([]string(nil), order...)
	lock.Unlock()
	for n, scenario := range gotOrder {
		repeat, offset := n/7, n%7
		if scenario != originalScenarios[(repeat+offset)%7] {
			t.Fatal("표본 순환 순서가 다르다")
		}
	}
	for _, attempt := range result.Attempts {
		if attempt.Scenario == "unversioned" && (attempt.VersionKnown || !attempt.RangeKnown || attempt.ContentMatch != 1) {
			t.Fatal("고정 판 식별과 실제 내용 일치 혼동")
		}
		if attempt.Scenario == "content_changed" && (attempt.ContentMatch != 0 || attempt.VersionMatch != 0 || attempt.Status != "version_mismatch") {
			t.Fatal("변경된 최신 내용을 원본 일치로 처리")
		}
	}
	captured, err := os.ReadFile(filepath.Join(filepath.Dir(path), "capture.bin"))
	if err != nil || !bytes.Equal(captured, originalCapturedBody) {
		t.Fatal("원본 snapshot 변경")
	}
	for _, extra := range [][]string{{"-adaptive"}, {"-budget=1"}, {"-contexts=missing"}, {"-repeats=9"}} {
		t.Run("preflight/"+extra[0], func(t *testing.T) {
			before := calls.Load()
			destination := filepath.Join(t.TempDir(), "failed.json")
			if _, err := invoke(destination, extra...); err == nil {
				t.Fatal("모드/전용 옵션 충돌 허용")
			}
			if calls.Load() != before {
				t.Fatal("사전 거부 전에 접속")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("거부 뒤 정상 보고서 생성")
			}
		})
	}
	t.Run("last_snapshot_invalid_before_first_get", func(t *testing.T) {
		input.Samples[len(input.Samples)-1].Captured.SHA256 = strings.Repeat("a", 64)
		writeOriginalFixture(t, input, path)
		before := calls.Load()
		destination := filepath.Join(t.TempDir(), "failed.json")
		if _, err := invoke(destination); err == nil {
			t.Fatal("stale snapshot 허용")
		}
		if calls.Load() != before {
			t.Fatal("전체 snapshot 준비 전 접속")
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatal("준비 실패 보고서 생성")
		}
	})
}

func TestOriginalCommandSignalCancellation(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	trustEvalEmbeddingServer(t, server)
	t.Setenv("EVAL_TEST_COMMAND_PROCESS", "1")
	t.Setenv("DATABASE_URL", "")
	input, path := originalFixture(t, server.URL)
	input.TimeoutMS = 10000
	writeOriginalFixture(t, input, path)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "result.json")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestEvalCommandProcess$", "--", "-original-reproduction="+path, "-out="+output)
	var logs bytes.Buffer
	command.Stdout, command.Stderr = &logs, &logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("신호 전 종료: %v\n%s", err, logs.String())
	case <-ctx.Done():
		<-done
		t.Fatal(ctx.Err())
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	err = <-done
	status, ok := errors.AsType[*exec.ExitError](err)
	if !ok || status.ExitCode() != 1 || !strings.Contains(logs.String(), "context canceled") {
		t.Fatalf("신호 종료: %v\n%s", err, logs.String())
	}
	if strings.Contains(logs.String(), server.URL) || strings.Contains(logs.String(), "private-") {
		t.Fatal("취소 오류의 원본 URI 노출")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("취소 뒤 정상 보고서 생성")
	}
}

func TestOriginalTimeoutAndTruncatedBody(t *testing.T) {
	for _, phase := range []string{"response_header", "response_body", "truncated_body"} {
		t.Run(phase, func(t *testing.T) {
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "truncated_body" {
					w.Header().Set("Content-Length", fmt.Sprint(len(originalCapturedBody)+10))
					_, _ = w.Write(originalCapturedBody)
					return
				}
				if phase == "response_body" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(unblock)
			input, path := originalFixture(t, server.URL)
			input.TimeoutMS = 50
			writeOriginalFixture(t, input, path)
			_, samples, err := loadOriginalInput(path)
			if err != nil {
				t.Fatal(err)
			}
			got := fetchOriginal(t.Context(), server.Client(), input, samples[0], 1)
			want := "timeout"
			if phase == "truncated_body" {
				want = "response_read_failure"
			}
			if got.Status != want || got.ContentMatch != -1 {
				t.Fatalf("%+v", got)
			}
		})
	}
}
