package index

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

type relationProposerFunc func(context.Context, model.ID, model.ID, string) error

func (propose relationProposerFunc) ProposeSimilarEventRelations(ctx context.Context, graph, event model.ID, modelID string) error {
	return propose(ctx, graph, event, modelID)
}

// 실제 TLS 제공자·작업 큐·공개 경로 뒤에 조립한 신호의 순서와 진단 필드를 확인한다.
func TestWorkerPublicationSignalIntegration(t *testing.T) {
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("필수 시험 DB 주소가 없다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 작업자 공개 시험을 건너뛴다")
	}
	for _, mode := range []string{"source", "event", "provider_failure", "provider_unauthorized", "provider_contract_failure", "provider_dimension_failure", "stale", "proposal_failure"} {
		t.Run(mode, func(t *testing.T) {
			database, err := store.New(t.Context(), address, "agent_context", nil, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			conn, err := pgx.Connect(t.Context(), address)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(context.WithoutCancel(t.Context()))
			graph, target, _, err := createSimilarEventGraph(t, database, integrationID(t))
			if err != nil {
				t.Fatal(err)
			}
			focusIndexQueue(t, address, graph)
			if mode == "source" {
				event, err := database.Context(t.Context(), graph, target)
				if err != nil || event.Event == nil || len(event.Event.MemberIDs) != 1 {
					t.Fatalf("원천 근거 조회: %+v, %v", event, err)
				}
				target = event.Event.MemberIDs[0]
			}
			correlation, newer := integrationID(t), integrationID(t)
			if _, err := conn.Exec(t.Context(), `UPDATE public.index_task SET next_attempt_at=statement_timestamp()+interval '1 hour' WHERE graph_id=$1`, graph.String()); err != nil {
				t.Fatal(err)
			}
			var jobID string
			if err := conn.QueryRow(t.Context(), `UPDATE public.index_task SET correlation_id=$2,next_attempt_at=statement_timestamp()-interval '1 second'
				WHERE context_id=$1 RETURNING task_id::text`, target.String(), correlation.String()).Scan(&jobID); err != nil {
				t.Fatal(err)
			}
			dimension := embeddingDimension(t, address)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "provider_failure", "provider_unauthorized":
					status := http.StatusServiceUnavailable
					if mode == "provider_unauthorized" {
						status = http.StatusUnauthorized
					}
					w.WriteHeader(status)
					fmt.Fprint(w, "private response marker")
					return
				case "provider_contract_failure":
					fmt.Fprint(w, `{"embedding":{"values":["private response marker"]}}`)
					return
				case "provider_dimension_failure":
					fmt.Fprint(w, `{"embedding":{"values":[1]}}`)
					return
				}
				if mode == "stale" {
					if _, err := conn.Exec(r.Context(), `UPDATE public.index_task SET enqueued_at=enqueued_at+interval '1 microsecond',
						next_attempt_at=statement_timestamp(),correlation_id=$2 WHERE task_id=$1`, jobID, newer.String()); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				vector := make([]float64, dimension)
				vector[0] = 1
				body, err := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(body)
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			proposer := relationProposerFunc(func(ctx context.Context, gotGraph, gotEvent model.ID, modelID string) error {
				calls++
				if gotGraph != graph || gotEvent != target || modelID == "" {
					t.Errorf("신호 대상=%s/%s/%s", gotGraph, gotEvent, modelID)
				}
				var tasks, embeddings int
				// 별도 연결에서 보이는 커밋 결과만 신호의 선행 조건으로 인정한다.
				if err := conn.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM public.index_task WHERE task_id=$1),
					(SELECT count(*) FROM public.context_embedding WHERE context_id=$2 AND model_id=$3)`, jobID, target.String(), modelID).Scan(&tasks, &embeddings); err != nil || tasks != 0 || embeddings != 1 {
					t.Errorf("신호 시점 커밋 tasks=%d embeddings=%d: %v", tasks, embeddings, err)
				}
				if mode == "proposal_failure" {
					return errors.New("private proposal marker")
				}
				return nil
			})
			var logs bytes.Buffer
			worker, err := NewWithRelationProposer(database, Config{Provider: "gemini", APIKey: "private-key-marker", BaseURL: base, Model: "gemini-embedding-2", VectorType: "vector", Dimension: dimension}, server.Client(), slog.New(slog.NewJSONHandler(&logs, nil)), proposer)
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Close(t.Context())
			sink := &obs05ProviderEvents{}
			source, _ := diagnostic.NewSource("obs05")
			reporter, _ := diagnostic.NewReporter(diagnostic.Index, sink, source)
			worker.SetDiagnostics(reporter)

			processed, err := worker.RunOnceInGraph(t.Context(), graph)
			if err != nil || !processed {
				t.Fatalf("작업자 회차=%t: %v", processed, err)
			}
			wantCalls := 0
			if mode == "event" || mode == "proposal_failure" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("관계 후보 신호=%d want=%d", calls, wantCalls)
			}
			wantEvents := 1
			if mode == "provider_failure" || mode == "provider_unauthorized" {
				wantEvents = 2
			}
			if len(sink.events) != wantEvents {
				t.Fatalf("제공자 진단 수: %d", len(sink.events))
			}
			var e diagnostic.Event
			for _, event := range sink.events {
				if event.Fields.Index.TaskID == jobID {
					e = event
				}
			}
			wantStatus := uint16(200)
			wantResult, wantReason, wantLevel := "completed", "completed", "INFO"
			switch mode {
			case "provider_failure":
				wantStatus = 503
				wantResult, wantReason, wantLevel = "failed", "provider_unavailable", "WARN"
			case "provider_unauthorized":
				wantStatus = 401
				wantResult, wantReason, wantLevel = "failed", "provider_contract_violation", "ERROR"
			case "provider_contract_failure":
				wantResult, wantReason, wantLevel = "failed", "provider_contract_violation", "ERROR"
			case "provider_dimension_failure":
				wantResult, wantReason, wantLevel = "failed", "dimension_mismatch", "ERROR"
			}
			if e.Validate() != nil || e.CorrelationID != correlation.String() || e.JobID == "" || e.Fields.Index.TaskID != jobID || e.Fields.Index.Generation == 0 || e.Fields.Index.ProviderStatus != wantStatus || e.Result != wantResult || e.ReasonCode != wantReason || e.Level != wantLevel {
				t.Fatalf("실제 제공자 계측: %+v", e)
			}
			eventBytes, _ := json.Marshal(sink.events)
			output := logs.String() + string(eventBytes)
			for _, secret := range []string{"private proposal marker", "private response marker", "private-key-marker", "같은 의미의 사건", server.URL} {
				if strings.Contains(output, secret) {
					t.Errorf("진단에 비허용 원문 노출: %q", secret)
				}
			}
			for _, field := range []string{`"job_id":"` + jobID + `"`, `"correlation_id":"` + correlation.String() + `"`} {
				if !strings.Contains(output, field) {
					t.Errorf("진단 필드 누락: %s", field)
				}
			}
			if mode == "stale" {
				var current string
				if err := conn.QueryRow(t.Context(), `SELECT correlation_id FROM public.index_task WHERE task_id=$1`, jobID).Scan(&current); err != nil || current != newer.String() || strings.Contains(output, newer.String()) {
					t.Fatalf("무효 세대의 진단 상관 ID 혼합: %s, %v", current, err)
				}
			}
			if mode == "proposal_failure" && !strings.Contains(output, `"reason_code":"proposal_failed"`) {
				t.Fatal("관계 후보 실패 진단 누락")
			}
		})
	}
}

type obs05ProviderEvents struct{ events []diagnostic.Event }

func (s *obs05ProviderEvents) Emit(_ context.Context, e diagnostic.Event) bool {
	s.events = append(s.events, e)
	return false
}
