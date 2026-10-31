package main

import (
	"context"
	"errors"
	"fmt"

	"encoding/json/v2"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/diagnostic"
)

type diagnostic199Sink struct{ events []diagnostic.Event }

func (s *diagnostic199Sink) Emit(_ context.Context, e diagnostic.Event) bool {
	s.events = append(s.events, e)
	return true
}

func TestHTTPDiagnosticFailureReasons(t *testing.T) {
	for _, sample := range []struct {
		status                int
		reason, level, result string
	}{
		{200, "completed", "INFO", "completed"}, {400, "invalid_argument", "WARN", "rejected"},
		{401, "invalid_credential", "WARN", "rejected"}, {403, "permission_denied", "WARN", "rejected"},
		{404, "not_found", "WARN", "rejected"}, {405, "method_not_allowed", "WARN", "rejected"},
		{413, "payload_too_large", "WARN", "rejected"}, {429, "capacity_exceeded", "WARN", "rejected"},
		{500, "internal_error", "ERROR", "failed"}, {503, "target_unavailable", "ERROR", "failed"},
		{504, "deadline_exceeded", "INFO", "failed"},
	} {
		t.Run(sample.reason, func(t *testing.T) {
			app := newApplication(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
			sink := &diagnostic199Sink{}
			source, _ := diagnostic.NewSource("reasons199")
			app.diagnostics, _ = diagnostic.NewReporter(diagnostic.Request, sink, source)
			handler := app.logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(sample.status) }))
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://service.test/graphs", nil))
			assert199Response(t, sink, sample.result, sample.reason, sample.level)
		})
	}
}

func assert199Response(t *testing.T, sink *diagnostic199Sink, result, reason, level string) {
	t.Helper()
	if len(sink.events) != 2 {
		t.Fatalf("events=%d", len(sink.events))
	}
	e := sink.events[1]
	if e.Validate() != nil || e.Stage != "response" || e.Result != result || e.ReasonCode != reason || e.Level != level {
		t.Fatalf("response=%+v", e)
	}
}

func TestHTTPDiagnosticMiddlewareReasons(t *testing.T) {
	for _, kind := range []string{"panic", "partial_panic", "tls", "budget", "canceled", "read_deadline_failure"} {
		t.Run(kind, func(t *testing.T) {
			app := newApplication(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), proxyTransport())
			sink := &diagnostic199Sink{}
			source, _ := diagnostic.NewSource("middleware199")
			app.diagnostics, _ = diagnostic.NewReporter(diagnostic.Request, sink, source)
			reason, level, result := "handler_panic", "ERROR", "failed"
			var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "partial_panic" {
					w.WriteHeader(200)
				}
				panic("private-panic-marker")
			})
			request := httptest.NewRequest("GET", "http://service.test/graphs", nil)
			switch kind {
			case "tls":
				handler = app.requireTLS(handler)
				reason, level, result = "https_required", "WARN", "rejected"
			case "budget":
				handler = withRequestBudget(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					<-r.Context().Done()
					writeStatus(w, 500, "internal_error")
				}), time.Millisecond, httpBudgetFailure)
				reason, level = "deadline_exceeded", "INFO"
			case "read_deadline_failure":
				handler = withRequestBudget(handler, time.Second, httpBudgetFailure)
				reason, level = "internal_error", "ERROR"
			case "canceled":
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
				handler = withRequestBudget(handler, time.Second, httpBudgetFailure)
				reason, level = "canceled", "INFO"
			}
			var writer http.ResponseWriter = httptest.NewRecorder()
			if kind == "read_deadline_failure" {
				writer = failing199DeadlineWriter{httptest.NewRecorder()}
			}
			app.logRequests(app.recoverPanics(handler)).ServeHTTP(writer, request)
			assert199Response(t, sink, result, reason, level)
		})
	}
}

func TestStartupDiagnosticOutcomesIntegration(t *testing.T) {
	_, _, address := newServerContractDatabase(t)
	for _, kind := range []string{"completed", "configuration", "schema", "authentication", "database"} {
		t.Run(kind, func(t *testing.T) {
			cfg := arch06Configuration(t, address, "https://provider.invalid")
			cfg.IndexWorkerPlacement = config.PlacementExternal
			reason, result := "completed", "completed"
			switch kind {
			case "configuration":
				cfg.GeminiAPIKey = ""
				reason, result = "configuration_invalid", "failed"
			case "schema":
				cfg.EmbeddingDimension = 128
				reason, result = "embedding_schema_mismatch", "failed"
			case "authentication":
				cfg.BcryptCost = 0
				reason, result = "authentication_unavailable", "failed"
			case "database":
				cfg.DatabaseURL = ":invalid"
				reason, result = "database_unavailable", "failed"
			}
			assembly, err := assembleApplication(t.Context(), cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if result == "completed" {
				if err != nil {
					t.Fatal(err)
				}
				if err := assembly.close(nil, true); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				assembly.close(nil, true)
				t.Fatal("assembly unexpectedly succeeded")
			}
			raw, err := os.ReadFile(filepath.Join(cfg.Diagnostic.Directory, cfg.Diagnostic.InstanceID, "system.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			shutdownComplete := false
			for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
				var e diagnostic.Event
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatal(err)
				}
				if e.Stage == "shutdown" && e.Result == "completed" {
					shutdownComplete = true
				}
				if e.Stage == "startup" && e.Result == result {
					wantLevel := "ERROR"
					if result == "completed" {
						wantLevel = "INFO"
					}
					if e.Validate() != nil || e.ReasonCode != reason || e.Level != wantLevel {
						t.Fatalf("startup=%+v", e)
					}
					found = true
				}
			}
			if !shutdownComplete {
				t.Fatal("successful cleanup was reported as shutdown failure")
			}
			if !found {
				t.Fatalf("missing startup %s/%s", result, reason)
			}
		})
	}
}

func TestSystemDiagnosticFailureReasons(t *testing.T) {
	for _, sample := range []struct {
		err                     error
		fallback, reason, level string
	}{
		{fmt.Errorf("private cause: %w", context.Canceled), "configuration_invalid", "canceled", "INFO"},
		{fmt.Errorf("private cause: %w", context.DeadlineExceeded), "database_unavailable", "deadline_exceeded", "INFO"},
		{fmt.Errorf("private cause: %w", diagnostic199DatabaseError{}), "embedding_schema_mismatch", "database_unavailable", "ERROR"},
		{errors.New("private configuration cause"), "configuration_invalid", "configuration_invalid", "ERROR"},
		{&net.OpError{Op: "listen", Net: "tcp", Err: errors.New("private address")}, "target_unavailable", "target_unavailable", "ERROR"},
		{&net.OpError{Op: "read", Net: "tcp", Err: errors.New("private database address")}, "embedding_schema_mismatch", "database_unavailable", "ERROR"},
	} {
		level, reason := systemDiagnosticResult(sample.err, sample.fallback)
		if level != sample.level || reason != sample.reason {
			t.Fatalf("got %s/%s", level, reason)
		}
	}
}

type diagnostic199DatabaseError struct{}

func (diagnostic199DatabaseError) Error() string    { return "private database cause" }
func (diagnostic199DatabaseError) SQLState() string { return "08006" }

type failing199DeadlineWriter struct{ *httptest.ResponseRecorder }

func (failing199DeadlineWriter) SetReadDeadline(time.Time) error {
	return errors.New("private controller failure")
}
