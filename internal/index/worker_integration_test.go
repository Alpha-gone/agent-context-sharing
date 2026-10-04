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
	dimension := embeddingDimension(t, databaseURL)
	server := embeddingServer(t, dimension)
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("임베딩 제공자 주소 해석: %v", err)
	}
	worker, err := New(database, Config{BaseURL: baseURL, Model: "relation-test", VectorType: "vector", Dimension: dimension}, server.Client(), nil)
	if err != nil {
		t.Fatalf("색인 작업자 생성: %v", err)
	}
	graphID, firstEventID, secondEventID, err := createSimilarEventGraph(t, database, actorID)
	if err != nil {
		t.Fatalf("의미 관계 그래프 생성: %v", err)
	}
	focusIndexQueue(t, databaseURL, graphID)
	for range 100 {
		processed, err := worker.RunOnceInGraph(t.Context(), graphID)
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

// focusIndexQueue는 이 테스트가 만든 대기 작업을 확보 가능한 상태로 두고 뒤에 지운다.
//
// 예전에는 다른 그래프의 대기 작업을 한 시간 미루고 자기 행만 지난 시각으로 당겨 확보
// 순서를 고정했다. 색인 큐는 `graph_id`로 나뉘지 않는 유일한 공유 자원이라 그 UPDATE가
// 다른 테스트의 행까지 밀어 「테스트 사이의 격리」를 깼고, 당겨 둔 자기 행은 반대로 다른
// 패키지의 작업자가 가장 먼저 집어 갔다. 확보를 그래프로 좁히면 두 방향 모두 사라지므로
// 남의 행을 건드리지 않는다.
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

	const ready = `UPDATE public.index_task SET next_attempt_at = now() WHERE graph_id = $1`
	if _, err := connection.Exec(t.Context(), ready, graphID.String()); err != nil {
		t.Fatalf("색인 작업 확보 가능 상태 설정: %v", err)
	}
}

// embeddingDimension은 시험 DB의 벡터 열 차원을 읽어 대역 응답을 현재 마이그레이션 구성에 맞춘다.
func embeddingDimension(t *testing.T, databaseURL string) int {
	t.Helper()
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("임베딩 차원 조회 연결: %v", err)
	}
	defer connection.Close(context.WithoutCancel(t.Context()))
	var dimension int
	if err := connection.QueryRow(t.Context(), `
		SELECT COALESCE(substring(format_type(atttypid, atttypmod) FROM '[0-9]+')::int, 0)
		FROM pg_attribute
		WHERE attrelid = 'public.context_embedding'::regclass AND attname = 'embedding'`).Scan(&dimension); err != nil || dimension <= 0 {
		t.Fatalf("임베딩 차원 조회 = %d, %v", dimension, err)
	}
	return dimension
}

func embeddingServer(t *testing.T, dimension int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		embedding := make([]float64, dimension)
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
