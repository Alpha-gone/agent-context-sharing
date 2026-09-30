package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

type forbiddenInput struct{}

func (forbiddenInput) Read([]byte) (int, error) {
	panic("구성 검증 전에 stdin에 접근했습니다")
}
func (forbiddenInput) Close() error { return nil }

func validEnvironment() map[string]string {
	return map[string]string{
		"AGENT_CONTEXT_CLIENT_REMOTE_URL":      "https://example.com/mcp",
		"AGENT_CONTEXT_CLIENT_ID":              "registered-client",
		"AGENT_CONTEXT_CLIENT_AGENT_ID":        "0198e7c0-0000-7000-8000-000000000001",
		"AGENT_CONTEXT_CLIENT_AUTH_TIMEOUT":    "5m",
		"AGENT_CONTEXT_CLIENT_REQUEST_TIMEOUT": "30s",
		"AGENT_CONTEXT_CLIENT_TOOL_POLICY":     "all",
		"AGENT_CONTEXT_CLIENT_TOOL_ALLOWLIST":  "",
	}
}

func TestCommandValidationDoesNotReadInput(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"serve", "extra"}, {"doctor", "--bad"}} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), args, func(string) string { panic("명령 검증 전에 환경 변수를 읽었습니다") },
			forbiddenInput{}, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "사용법") {
			t.Fatalf("args=%v: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestServeRejectsConfigurationBeforeInput(t *testing.T) {
	env := validEnvironment()
	env["AGENT_CONTEXT_CLIENT_REMOTE_URL"] = "https://private.example/mcp?secret=private"
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"serve"}, func(key string) string { return env[key] },
		forbiddenInput{}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "serve_failed") ||
		strings.Contains(stderr.String(), "private") {
		t.Fatalf("구성 실패가 출력 경계를 지키지 못했습니다: code=%d stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
}

func TestDoctorRemainsSeparateAndDoesNotClaimSuccess(t *testing.T) {
	env := validEnvironment()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"doctor", "--json"}, func(key string) string { return env[key] },
		forbiddenInput{}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stdout.String(), `"status":"skipped"`) || stderr.Len() != 0 {
		t.Fatalf("미구현 진단이 성공으로 보였습니다: code=%d stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
}

func TestServeEOFHasNoNonMCPStdout(t *testing.T) {
	env := validEnvironment()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"serve"}, func(key string) string { return env[key] },
		io.NopCloser(strings.NewReader("")), &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"event":"host_stop"`) {
		t.Fatalf("EOF 종료가 출력 경계를 지키지 못했습니다: code=%d stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
}

func TestServeDiscoverImmediatelyBeforeEOF(t *testing.T) {
	const request = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"test-host","version":"1"}}}}`
	env := validEnvironment()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"serve"}, func(key string) string { return env[key] },
		io.NopCloser(strings.NewReader(request+"\n")), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("요청 직후 EOF 종료 코드 = %d, stderr=%q", code, stderr.String())
	}
	var response struct {
		ID     int64 `json:"id"`
		Result struct {
			SupportedVersions []string `json:"supportedVersions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &response); err != nil {
		t.Fatalf("요청 직후 EOF 응답 = %q: %v", stdout.String(), err)
	}
	if response.ID != 1 || len(response.Result.SupportedVersions) != 1 || response.Result.SupportedVersions[0] != "2026-07-28" {
		t.Fatalf("요청 직후 EOF discover 응답 = %+v", response)
	}
}

func TestBuildVersionUsesInjectedRelease(t *testing.T) {
	previous := releaseVersion
	releaseVersion = "v1.2.3"
	t.Cleanup(func() { releaseVersion = previous })
	if got := buildVersion(); got != "v1.2.3" {
		t.Fatalf("빌드 판 = %q", got)
	}
}

func TestServeTerminatesOnSignal(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal os.Signal
	}{
		{"SIGINT", os.Interrupt},
		{"SIGTERM", syscall.SIGTERM},
	} {
		t.Run(test.name, func(t *testing.T) {
			testServeSignal(t, test.signal)
		})
	}
}

func testServeSignal(t *testing.T, termination os.Signal) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stdin, keepOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer keepOpen.Close()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeSignalHelper$")
	cmd.Stdin = stdin
	cmd.Env = append(os.Environ(), "CLIENT_SIGNAL_HELPER=1")
	for key, value := range validEnvironment() {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), `"event":"host_start"`) {
				ready <- nil
				return
			}
		}
		if err := scanner.Err(); err != nil {
			ready <- err
			return
		}
		ready <- io.EOF
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("호스트 프로세스 시작 시간 초과")
	}
	if err := cmd.Process.Signal(termination); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil || stdout.Len() != 0 {
		t.Fatalf("종료 신호 처리: err=%v stdout=%q", err, stdout.String())
	}
}

func TestServeSignalHelper(t *testing.T) {
	if os.Getenv("CLIENT_SIGNAL_HELPER") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, []string{"serve"}, os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
