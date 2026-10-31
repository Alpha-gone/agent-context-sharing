package index

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func TestWorkerWhitespaceDocumentPublicationIntegration(t *testing.T) {
	database, pool := newIndexContractDatabase(t)
	for _, test := range []struct{ name, body string }{
		{"space", "   "}, {"newline", "\t\n\r"}, {"unicode_space", "\u00a0\u2003\u3000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor, graph := integrationID(t), integrationID(t)
			now := time.Now().UTC().Truncate(time.Second)
			if err := database.CreateAccount(t.Context(), store.Account{ID: actor, LoginID: strings.ReplaceAll(actor.String(), "-", ""), PasswordHash: "synthetic-password-hash", CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if _, err := database.CreateGraph(t.Context(), model.Graph{ID: graph, Name: "공백 본문 색인 회귀", CreatedBy: actor, CreatedAt: now, LastActivityAt: now, Version: 1}); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			inputs := make(chan string, 3)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Content geminiContent `json:"content"`
				}
				if err := json.UnmarshalRead(r.Body, &payload); err != nil || len(payload.Content.Parts) != 1 {
					t.Errorf("문서 입력 해석: %+v, %v", payload, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				inputs <- payload.Content.Parts[0].Text
				vector := make([]float64, 128)
				vector[0] = float64(calls.Add(1))
				if err := json.MarshalWrite(w, map[string]any{"embedding": map[string]any{"values": vector}}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := New(database, Config{Provider: "gemini", APIKey: testGeminiKey, BaseURL: base, Model: "gemini-embedding-2", VectorType: "vector", Dimension: 128}, server.Client(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := worker.Close(context.WithoutCancel(t.Context())); err != nil {
					t.Error(err)
				}
			})
			sink := &obs05ProviderEvents{}
			source, err := diagnostic.NewSource("whitespace")
			if err != nil {
				t.Fatal(err)
			}
			reporter, err := diagnostic.NewReporter(diagnostic.Index, sink, source)
			if err != nil {
				t.Fatal(err)
			}
			worker.SetDiagnostics(reporter)
			published := int64(0)
			publish := func(value model.Context) {
				t.Helper()
				stored, err := database.Context(t.Context(), graph, value.ID)
				if err != nil || stored.Body != value.Body {
					t.Fatalf("저장 본문=%q: %v", stored.Body, err)
				}
				var taskID, state string
				var attempts int
				if err := pool.QueryRow(t.Context(), `SELECT task_id::text,state,attempts FROM public.index_task WHERE graph_id=$1 AND context_id=$2`, graph.String(), value.ID.String()).Scan(&taskID, &state, &attempts); err != nil || state != "pending" || attempts != 0 {
					t.Fatalf("공백 본문 등록 state=%s attempts=%d: %v", state, attempts, err)
				}
				if processed, err := worker.RunOnceInGraph(t.Context(), graph); err != nil || !processed {
					t.Fatalf("공백 본문 처리=%t: %v", processed, err)
				}
				published++
				select {
				case input := <-inputs:
					if input != "title: none | text: "+value.Body {
						t.Fatalf("문서 원문 전송=%q, want body=%q", input, value.Body)
					}
				default:
					t.Fatal("공백 본문이 제공자에 전달되지 않았다")
				}
				var remaining int
				var vector, modelID string
				if err := pool.QueryRow(t.Context(), `SELECT embedding::text,model_id,
					(SELECT count(*) FROM public.index_task WHERE graph_id=$1 AND context_id=$2)
					FROM public.context_embedding WHERE graph_id=$1 AND context_id=$2`, graph.String(), value.ID.String()).Scan(&vector, &modelID, &remaining); err != nil || remaining != 0 || modelID != worker.ModelID() || vector != fmt.Sprintf("[%d%s]", published, strings.Repeat(",0", 127)) {
					t.Fatalf("벡터 공개 vector=%s model=%s tasks=%d: %v", vector, modelID, remaining, err)
				}
				if len(sink.events) != int(published) || calls.Load() != published {
					t.Fatalf("제공자 이벤트/호출 수=%d/%d, want=%d", len(sink.events), calls.Load(), published)
				}
				event := sink.events[len(sink.events)-1]
				if event.Validate() != nil || event.Stage != "provider" || event.Result != "completed" || event.ReasonCode != "completed" || event.Level != "INFO" || event.Fields.Index.TaskID != taskID || event.Fields.Identity.ContextID != value.ID.String() {
					t.Fatalf("공백 본문 제공자 진단=%+v", event)
				}
			}
			sourceValue := model.Context{ID: integrationID(t), GraphID: graph, Layer: model.LayerSource, Body: test.body, RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1,
				Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelAPI, Locator: "api://whitespace/" + graph.String()}, OccurredAt: now, OriginKind: model.OriginKindExternalContent}}
			sourceValue, err = database.CreateContext(t.Context(), graph, sourceValue, nil)
			if err != nil {
				t.Fatal(err)
			}
			publish(sourceValue)
			event := model.Context{ID: integrationID(t), GraphID: graph, Layer: model.LayerEvent, Body: test.body, RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1,
				Event: &model.EventAttributes{MemberIDs: []model.ID{sourceValue.ID}, Start: now.Add(-time.Minute)}}
			event, err = database.CreateContext(t.Context(), graph, event, nil)
			if err != nil {
				t.Fatal(err)
			}
			publish(event)
			event.Body, event.Version = test.body+test.body, event.Version+1
			event, err = database.UpdateContext(t.Context(), graph, event.Version-1, event)
			if err != nil {
				t.Fatal(err)
			}
			publish(event)
		})
	}
}
