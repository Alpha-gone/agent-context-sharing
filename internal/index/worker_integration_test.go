package index

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestWorkerProposesSimilarEventRelationsAfterIndexIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 색인 작업자 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	proposals := &store.RelationProposalConfig{AdjacencyWindow: time.Hour, SimilarityThreshold: 0.8, Limit: 10}
	database, err := store.New(t.Context(), databaseURL, graphName, proposals, nil, "")
	if err != nil {
		t.Fatalf("저장소 준비: %v", err)
	}
	defer database.Close()

	actorID := integrationID(t)
	server := embeddingServer(t)
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("임베딩 제공자 주소 해석: %v", err)
	}
	worker, err := New(database, Config{BaseURL: baseURL, Model: "relation-test", VectorType: "vector", Dimension: 1024}, server.Client(), nil)
	if err != nil {
		t.Fatalf("색인 작업자 생성: %v", err)
	}
	graphID, firstEventID, secondEventID, err := createSimilarEventGraph(t, database, actorID)
	if err != nil {
		t.Fatalf("의미 관계 그래프 생성: %v", err)
	}
	focusIndexQueue(t, databaseURL, graphID)
	for range 100 {
		processed, err := worker.RunOnce(t.Context())
		if err != nil {
			t.Fatalf("색인 작업 실행: %v", err)
		}
		if !processed {
			t.Fatal("대기 중인 그래프 색인 작업을 찾지 못했다")
		}
		relations, _, err := database.ListRelations(t.Context(), graphID, "", 10)
		if err != nil {
			t.Fatalf("관계 후보 조회: %v", err)
		}
		if len(relations) > 1 {
			t.Fatalf("의미 관계 후보가 예상보다 많다: %#v", relations)
		}
		if len(relations) != 1 {
			continue
		}
		relation := relations[0]
		if relation.Type != model.RelationTypeRelatesTo || relation.State != model.RelationStateProposed {
			t.Fatalf("의미 관계 후보 = %#v", relation)
		}
		if !((relation.FromContextID == firstEventID && relation.ToContextID == secondEventID) ||
			(relation.FromContextID == secondEventID && relation.ToContextID == firstEventID)) {
			t.Fatalf("의미 관계 후보 양 끝 = %#v", relation)
		}
		return
	}
	t.Fatal("색인 성공 뒤 의미 관계 후보가 만들어지지 않았다")
}

// focusIndexQueue는 이 테스트의 그래프 작업만 확보 대상으로 남긴다.
//
// 개발 데이터베이스를 공유하므로 다른 테스트가 남긴 대기 작업이 있고, 작업자는 그중 가장
// 오래된 것을 집는다. 그대로 두면 이 테스트가 자기 작업 대신 남의 작업을 처리하다 회차를
// 다 쓰고, 남은 행의 상태에 따라 통과와 실패가 갈린다. 나머지를 미루고 이 그래프의 작업만
// 지난 시각으로 당겨 확보 순서를 고정한다. `internal/store`의 같은 이름 도우미와 방법이
// 같으며, 미뤄 둔 행은 한 시간 뒤에 저절로 다시 대기가 된다.
func focusIndexQueue(t *testing.T, databaseURL string, graphID model.ID) {
	t.Helper()
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("색인 큐 정리 연결: %v", err)
	}
	// 연결 닫기를 먼저 등록해 마지막에 돌게 한다. Cleanup은 나중에 등록한 것부터 돈다.
	t.Cleanup(func() { connection.Close(context.WithoutCancel(t.Context())) })
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := connection.Exec(ctx, `DELETE FROM public.index_task WHERE graph_id = $1`, graphID.String()); err != nil {
			t.Errorf("색인 작업 정리: %v", err)
		}
	})

	const postpone = `UPDATE public.index_task SET next_attempt_at = now() + interval '1 hour' WHERE state = 'pending' AND graph_id <> $1`
	if _, err := connection.Exec(t.Context(), postpone, graphID.String()); err != nil {
		t.Fatalf("다른 색인 작업 미루기: %v", err)
	}
	const ready = `UPDATE public.index_task SET enqueued_at = to_timestamp(-3000000000), next_attempt_at = to_timestamp(-3000000000) WHERE graph_id = $1`
	if _, err := connection.Exec(t.Context(), ready, graphID.String()); err != nil {
		t.Fatalf("색인 작업 우선순위 설정: %v", err)
	}
}

func embeddingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		embedding := make([]float64, 1024)
		embedding[0] = 1
		body, err := json.Marshal(map[string]any{"embeddings": [][]float64{embedding}})
		if err != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(body)
	}))
}

func createSimilarEventGraph(t *testing.T, database *store.Store, actorID model.ID) (graphID, firstEventID, secondEventID model.ID, err error) {
	t.Helper()
	graphID = integrationID(t)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err = database.CreateGraph(t.Context(), model.Graph{
		ID: graphID, Name: "index relation integration", CreatedBy: actorID,
		CreatedAt: now, LastActivityAt: now, Version: 1,
	}); err != nil {
		return graphID, firstEventID, secondEventID, err
	}
	source := model.Context{
		ID: integrationID(t), GraphID: graphID, Layer: model.LayerSource, Body: "의미 관계 원천",
		RecordedAt: now, CreatedBy: actorID, CreatedByAgent: actorID, Version: 1,
		Source: &model.SourceAttributes{
			Reference:  model.SourceReference{Channel: model.SourceChannelAPI, Locator: "https://example.test/" + graphID.String()},
			OccurredAt: now, OriginKind: model.OriginKindExternalContent,
		},
	}
	source, err = database.CreateContext(t.Context(), graphID, source, nil)
	if err != nil {
		return graphID, firstEventID, secondEventID, err
	}
	firstEventID, secondEventID = integrationID(t), integrationID(t)
	end := now.Add(time.Minute)
	for _, eventID := range []model.ID{firstEventID, secondEventID} {
		value := model.Context{
			ID: eventID, GraphID: graphID, Layer: model.LayerEvent, Body: "같은 의미의 사건",
			RecordedAt: now, CreatedBy: actorID, CreatedByAgent: actorID, Version: 1,
			Event: &model.EventAttributes{MemberIDs: []model.ID{source.ID}, Start: now.Add(-time.Minute), End: &end},
		}
		if _, err = database.CreateContext(t.Context(), graphID, value, nil); err != nil {
			return graphID, firstEventID, secondEventID, err
		}
	}
	return graphID, firstEventID, secondEventID, nil
}

func integrationID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("UUIDv7 생성: %v", err)
	}
	return id
}
