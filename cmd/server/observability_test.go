package main

import (
	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"bufio"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOBS05RequestActualFileAndTrace(t *testing.T) {
	base, err := os.MkdirTemp(".", ".obs05-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	root, err := filepath.Abs(filepath.Join(base, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	source, _ := diagnostic.NewSource("server_obs05")
	recorder, err := diagnostic.NewRecorder(t.Context(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close(context.Background())
	reporter, _ := diagnostic.NewReporter(diagnostic.Request, recorder, source)
	app := newApplication(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
	app.diagnostics = reporter
	var corr string
	handler := app.logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		corr = model.CorrelationID(r.Context()).String()
		if corr == "" || corr != diagnostic.TraceFromContext(r.Context()).CorrelationID() {
			t.Fatal("HTTP 상관 ID가 색인 context에 없음")
		}
		w.WriteHeader(403)
		w.WriteHeader(500)
	}))
	request := httptest.NewRequest("POST", "https://example.test/secret/path?token=private_token", strings.NewReader("private_body"))
	request.Header.Set("X-Correlation-ID", "private_client_header")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(root, "server_obs05", "request.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	count := 0
	for scan.Scan() {
		raw := scan.Text()
		for _, secret := range []string{"secret/path", "private_token", "private_body", "private_client_header"} {
			if strings.Contains(raw, secret) {
				t.Fatal("입력 원문 누출")
			}
		}
		var e diagnostic.Event
		if err := json.Unmarshal([]byte(raw), &e); err != nil || e.Validate() != nil {
			t.Fatal("잘못된 실제 JSON 행")
		}
		if e.CorrelationID != corr || e.Fields.Request.Route != "unknown" {
			t.Fatal("서버 흐름·고정 route 불일치")
		}
		if e.Stage == "response" && e.Fields.Request.Status != 403 {
			t.Fatal("중복 WriteHeader가 실제 상태를 덮음")
		}
		count++
	}
	if scan.Err() != nil || count != 2 || recorder.Stats().Dropped != 0 {
		t.Fatalf("실제 요청 파일 기록: %d %v %+v", count, scan.Err(), recorder.Stats())
	}
}
