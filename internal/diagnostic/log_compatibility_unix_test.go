//go:build linux || darwin

package diagnostic

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func futureLogLine(t *testing.T, files *Files, at time.Time) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(obs04Line(t, files, Request, at), &value); err != nil {
		t.Fatal(err)
	}
	value["level"], value["stage"], value["result"], value["reason_code"] = "NOTICE", "future_stage", "future_result", "future_reason"
	value["fields"] = map[string]any{"future_details": []any{"preserve-original", 42}}
	value["future_extension"] = map[string]any{"revision": 2}
	line, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]byte(" \t"), line...), '\n')
}

func TestStoredLogVersionCompatibilityStartup(t *testing.T) {
	for _, sample := range []struct {
		name        string
		own, active bool
	}{
		{"own_active", true, true}, {"own_rotated", true, false},
		{"old_slot_active", false, true}, {"old_slot_rotated", false, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			directory := obs02Directory(t)
			files := obs04Files(t, directory, "previous")
			line := futureLogLine(t, files, obs04Now)
			obs04Raw(t, files, Request, line, sample.active, obs04Now)
			if err := files.Close(); err != nil {
				t.Fatal(err)
			}
			instance := "previous"
			if !sample.own {
				instance = "replacement"
			}
			source, err := NewSource(instance)
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			output := newFailureOutput(source, &stderr, time.Now)
			recorder, err := prepareRecorderOutput(t.Context(), directory, source, obs04Policy(), output)
			if err != nil {
				t.Fatal("다른 판의 정상 JSON 때문에 기동 실패", err)
			}
			t.Cleanup(func() { obs03Close(t, recorder) })
			event := obs03Event(t, source, Request)
			event.Time = obs04Now
			if !recorder.Emit(t.Context(), event) {
				t.Fatal("판 호환 기동 뒤 새 기록 거부")
			}
			if err := obs03Close(t, recorder); err != nil {
				t.Fatal(err)
			}
			if stats := recorder.Stats(); stats.Written != 1 || stats.Dropped != 0 {
				t.Fatal(stats)
			}
			paths, err := filepath.Glob(filepath.Join(directory, "previous", "request*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var all []byte
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				all = append(all, data...)
			}
			if bytes.Count(all, line) != 1 {
				t.Fatal("기존 미지 코드·필드의 원래 바이트를 잃었다")
			}
			obs07Wait(t, output.done)
			if bytes.Contains(stderr.Bytes(), []byte("preserve-original")) {
				t.Fatal("과거 행 원문을 stderr에 노출했다")
			}
		})
	}
}

func TestStoredLogVersionCompatibilityCompactionAndPartial(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed_retention", true: "partial_tail"}[partial], func(t *testing.T) {
			directory := obs02Directory(t)
			files := obs04Files(t, directory, "previous")
			valid := futureLogLine(t, files, obs04Now)
			data := append(futureLogLine(t, files, obs04Now.Add(-8*24*time.Hour)), valid...)
			if partial {
				data = append(data, []byte(`{"incomplete":`)...)
			}
			obs04Raw(t, files, Request, data, true, obs04Now)
			if err := files.Close(); err != nil {
				t.Fatal(err)
			}
			source, _ := NewSource("previous")
			recorder, err := prepareRecorder(t.Context(), directory, source, obs04Policy())
			if err != nil {
				t.Fatal("판 호환 행의 보존·부분 행 복구 실패", err)
			}
			if err := obs03Close(t, recorder); err != nil {
				t.Fatal(err)
			}
			paths, _ := filepath.Glob(filepath.Join(directory, "previous", "request*.jsonl"))
			var all []byte
			for _, path := range paths {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				all = append(all, content...)
			}
			if !bytes.Equal(all, valid) {
				t.Fatal("만료 제거 또는 유효 행 원문 보존 실패")
			}
		})
	}
}

func TestStoredLogCorruptionStillFailsStartup(t *testing.T) {
	cases := []string{"syntax", "null", "array", "missing_time", "invalid_time", "non_utc_time", "missing_boot", "invalid_boot", "wrong_kind", "wrong_instance", "duplicate_key", "nested_duplicate", "invalid_utf8", "oversized"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			directory := obs02Directory(t)
			files := obs04Files(t, directory, "corrupt")
			line := obs04Line(t, files, Request, obs04Now)
			var value map[string]any
			if err := json.Unmarshal(line, &value); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "syntax":
				line = []byte("{\"secret\":\n")
			case "null":
				line = []byte("null\n")
			case "array":
				line = []byte("[]\n")
			case "duplicate_key":
				line = append(bytes.TrimSuffix(line, []byte("}\n")), []byte(`,"kind":"request"}`+"\n")...)
			case "nested_duplicate":
				line = append(bytes.TrimSuffix(line, []byte("}\n")), []byte(`,"future":{"same":1,"same":2}}`+"\n")...)
			case "invalid_utf8":
				line = append(bytes.TrimSuffix(line, []byte("}\n")), []byte(",\"future\":\"\xff\"}\n")...)
			case "oversized":
				value["future"] = string(bytes.Repeat([]byte{'x'}, maxEventBytes))
			case "missing_time":
				delete(value, "time")
			case "invalid_time":
				value["time"] = "not-a-time"
			case "non_utc_time":
				value["time"] = "2026-10-21T12:00:00+01:00"
			case "missing_boot":
				delete(value, "boot_id")
			case "invalid_boot":
				value["boot_id"] = "not-a-uuid"
			case "wrong_kind":
				value["kind"] = "auth"
			case "wrong_instance":
				value["instance_id"] = "another"
			}
			if name == "oversized" || name == "missing_time" || name == "invalid_time" || name == "non_utc_time" || name == "missing_boot" || name == "invalid_boot" || name == "wrong_kind" || name == "wrong_instance" {
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				line = append(encoded, '\n')
			}
			obs04Raw(t, files, Request, line, true, obs04Now)
			if err := files.Close(); err != nil {
				t.Fatal(err)
			}
			source, _ := NewSource("corrupt")
			var stderr bytes.Buffer
			output := newFailureOutput(source, &stderr, time.Now)
			recorder, err := prepareRecorderOutput(t.Context(), directory, source, obs04Policy(), output)
			if err == nil {
				obs03Close(t, recorder)
				t.Fatal("손상된 행을 기동 검사에서 수락했다")
			}
			if failureCode(err) != "log_invalid" {
				t.Fatal("손상 오류 계약 변경", err)
			}
			data, err := os.ReadFile(filepath.Join(directory, "corrupt", "request.jsonl"))
			if err != nil || !bytes.Equal(data, line) {
				t.Fatal("실패가 원본을 변경했다", err)
			}
			obs07Wait(t, output.done)
			if bytes.Contains(stderr.Bytes(), []byte("secret")) {
				t.Fatal("손상 원문을 stderr에 노출했다")
			}
		})
	}
}

func TestNewEventsStillRejectUnknownCodes(t *testing.T) {
	event := validEvent(t, Request)
	event.ReasonCode = "future_reason"
	if _, _, ok := encodeLine(event, maxEventBytes); ok {
		t.Fatal("과거 행 호환성이 새 이벤트 허용 목록을 완화했다")
	}
}
