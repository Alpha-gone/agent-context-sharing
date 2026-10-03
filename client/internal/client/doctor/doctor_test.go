package doctor

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

func TestDiagnosticOrderFailureSkippingAndExitCodes(t *testing.T) {
	for fail := -1; fail < len(names); fail++ {
		var seen []string
		steps := make([]func(context.Context) error, len(names))
		for index, name := range names {
			steps[index] = func(context.Context) error {
				seen = append(seen, name)
				if index == fail {
					return errors.New("https://secret.example/authorize?code=private-token")
				}
				return nil
			}
		}
		report := evaluate(t.Context(), steps, func(int) time.Duration { return time.Second })
		end, code := len(names), 0
		if fail >= 0 {
			end, code = fail+1, 1
		}
		if !reflect.DeepEqual(seen, names[:end]) || report.ExitCode() != code || len(report.Checks) != len(names) {
			t.Fatalf("순서·종료 코드: %+v %v", report, seen)
		}
		for index, check := range report.Checks {
			want := "pass"
			if index == fail {
				want = "fail"
			} else if fail >= 0 && index > fail {
				want = "skipped"
			}
			if check.Name != names[index] || check.Status != want || check.DurationMS < 0 {
				t.Fatalf("검사 외피: %+v", check)
			}
		}
		for _, asJSON := range []bool{true, false} {
			var out bytes.Buffer
			if err := report.Write(&out, asJSON); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "private-token") {
				t.Fatal("내부 오류 유출")
			}
			if asJSON {
				var root map[string]any
				if err := json.Unmarshal(out.Bytes(), &root); err != nil {
					t.Fatal(err)
				}
				if len(root) != 2 || root["status"] == nil || root["checks"] == nil {
					t.Fatal("진단 외피 변경")
				}
				for _, raw := range root["checks"].([]any) {
					for key := range raw.(map[string]any) {
						if key != "name" && key != "status" && key != "duration_ms" && key != "action" {
							t.Fatal("추가 진단 필드")
						}
					}
				}
			}
		}
	}
	report := evaluate(t.Context(), []func(context.Context) error{func(context.Context) error { return nil }}, func(int) time.Duration { return time.Second })
	if report.ExitCode() != 3 || report.Status != "skipped" {
		t.Fatal("미구현·건너뜀 코드")
	}
}

func TestDiagnosticTimeoutCancellationAndConfigurationBeforeExternalAccess(t *testing.T) {
	steps := []func(context.Context) error{func(context.Context) error { return nil }, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	report := evaluate(t.Context(), steps, func(int) time.Duration { return time.Millisecond })
	if report.ExitCode() != 1 || report.Checks[1].Action != contract.ClassifyError(context.DeadlineExceeded).Message {
		t.Fatal("시간 초과 분류")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report = evaluate(ctx, []func(context.Context) error{func(context.Context) error { t.Fatal("취소 뒤 실행"); return nil }}, func(int) time.Duration { return time.Second })
	if report.Checks[0].Status != "fail" {
		t.Fatal("취소된 진단 성공")
	}
	report = Run(t.Context(), func(string) string { return "secret" }, Options{Network: func(context.Context, string) error { t.Fatal("구성 실패 뒤 네트워크 접근"); return nil }})
	if report.ExitCode() != 1 || report.Checks[0].Status != "fail" || strings.Contains(report.Checks[0].Action, "secret") {
		t.Fatal("구성 실패 비밀 유출")
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("비공개 출력 실패") }
func TestReportWriteFailure(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		if err := (Report{Status: "pass", Checks: []Check{{Name: "configuration", Status: "pass"}}}).Write(failedWriter{}, asJSON); err == nil {
			t.Fatal("출력 오류 은폐")
		}
	}
}
