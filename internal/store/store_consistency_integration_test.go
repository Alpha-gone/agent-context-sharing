package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestIdempotencyCleanupUsesDatabaseClockIntegration은 보관 만료를 DB 시계로 판정해 만료된
// 기록만 지우는지 확인한다.
func TestIdempotencyCleanupUsesDatabaseClockIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	expiredKey, currentKey := "expired-"+accountID.String(), "current-"+accountID.String()
	if _, err := database.pool.Exec(t.Context(), `
		INSERT INTO public.idempotency_record(actor_account_id,idempotency_key,tool_name,request_fingerprint,tool_result,received_at,expires_at)
		VALUES ($1,$2,'node_update',$4,'{}'::jsonb,clock_timestamp()-interval '25 hours',clock_timestamp()-interval '1 second'),
		       ($1,$3,'node_update',$4,'{}'::jsonb,clock_timestamp(),clock_timestamp()+interval '1 hour')`,
		accountID.String(), expiredKey, currentKey, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if deleted, err := database.CleanupExpiredIdempotencyRecords(t.Context()); err != nil || deleted < 1 {
		t.Fatalf("멱등성 결과 정리 = %d, %v", deleted, err)
	}
	var expired, current bool
	if err := database.pool.QueryRow(t.Context(), `SELECT
		EXISTS (SELECT 1 FROM public.idempotency_record WHERE actor_account_id=$1 AND idempotency_key=$2),
		EXISTS (SELECT 1 FROM public.idempotency_record WHERE actor_account_id=$1 AND idempotency_key=$3)`,
		accountID.String(), expiredKey, currentKey).Scan(&expired, &current); err != nil || expired || !current {
		t.Fatalf("만료 기록 정리·보관 중 기록 보존 = expired:%t current:%t err:%v", expired, current, err)
	}
}

// TestGraphActivityDriftIsInvariantViolationIntegration은 저장 문자 수가 실제 본문과 어긋나
// 갱신이 걸리지 않으면 not_found가 아니라 불변식 위반 내부 장애로 거부하고 되돌리는지 확인한다.
func TestGraphActivityDriftIsInvariantViolationIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, actor, source := boundaryGraph(t, database)
	derived := lifeDerived(t, database, graph, actor, source.ID)
	if _, err := database.pool.Exec(t.Context(), `UPDATE public.context_graph SET stored_chars = 0 WHERE graph_id = $1`, graph.String()); err != nil {
		t.Fatal(err)
	}
	next := derived
	next.Body, next.Version = "짧게", derived.Version+1
	_, err := database.UpdateContext(t.Context(), graph, derived.Version, next)
	if _, internal := errors.AsType[*StorageError](err); !internal || !errors.Is(err, ErrInvariantViolation) || errors.Is(err, ErrNotFound) {
		t.Fatalf("저장량 불일치 갱신 = %v", err)
	}
	stored, err := database.Context(t.Context(), graph, derived.ID)
	if err != nil || stored.Version != derived.Version || stored.Body != derived.Body {
		t.Fatalf("거부한 갱신이 남았다 = %#v, %v", stored, err)
	}
}

// TestEventProposalsDoNotRecordAccessIntegration은 시스템의 사건 관계 후보 제안이 대상 사건의
// 접근 시각을 갱신하거나 cold 임베딩을 hot으로 승격하지 않는지 확인한다. 사용자 조회는
// 같은 행을 갱신해야 하므로 양성 대조로 함께 확인한다.
func TestEventProposalsDoNotRecordAccessIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	graph, actor, member := lifeProposalFixture(t, database)
	now := time.Now().UTC().Truncate(time.Second)
	target := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, new(now.Add(time.Minute)))
	vector := make([]float64, embeddingDimension(t, database))
	vector[0] = 1
	accessed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.context_embedding
		(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
		VALUES ($1,$2,$3,'synthetic-model',$4,$4,'cold')`, target.ID.String(), graph.String(), vectorText(vector), accessed); err != nil {
		t.Fatal(err)
	}
	access := func() (time.Time, string) {
		t.Helper()
		var at time.Time
		var tier string
		if err := database.pool.QueryRow(t.Context(), `SELECT last_accessed_at, storage_tier FROM public.context_embedding WHERE context_id=$1`, target.ID.String()).Scan(&at, &tier); err != nil {
			t.Fatal(err)
		}
		return at, tier
	}
	database.relationProposals = lifeProposalConfig(1)
	if err := database.ProposeEventRelations(t.Context(), graph, target.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ProposeSimilarEventRelations(t.Context(), graph, target.ID, "synthetic-model"); err != nil {
		t.Fatal(err)
	}
	if at, tier := access(); !at.Equal(accessed) || tier != embeddingTierCold {
		t.Fatalf("시스템 제안의 접근 기록 = %v %s", at, tier)
	}
	if _, err := database.Context(t.Context(), graph, target.ID); err != nil {
		t.Fatal(err)
	}
	if at, tier := access(); at.Equal(accessed) || tier != embeddingTierHot {
		t.Fatalf("사용자 조회의 접근 기록 = %v %s", at, tier)
	}
}

// TestCleanupExpiredProposalsContinuesAfterGraphFailureIntegration은 그래프 하나의 관계 해석
// 오류가 회차 전체를 멈추지 않고, 뒤 순서의 그래프를 계속 정리한 뒤 오류를 함께 보고하는지 확인한다.
func TestCleanupExpiredProposalsContinuesAfterGraphFailureIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, database, accountID)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	proposal := func(locator string) (model.ID, model.Relation) {
		t.Helper()
		graphID := createTestGraph(t, database, accountID)
		source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, accountID, locator), nil)
		if err != nil {
			t.Fatal(err)
		}
		var events []model.Context
		for range 2 {
			event, err := database.CreateContext(t.Context(), graphID, testEventContext(t, graphID, accountID, source.ID), nil)
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		relation, err := database.CreateRelation(t.Context(), graphID, model.Relation{
			ID: newTestID(t), GraphID: graphID, Type: model.RelationTypeRelatesTo, FromContextID: events[0].ID, ToContextID: events[1].ID,
			State: model.RelationStateProposed, ProposedBy: model.ProposalSourceSystem, ProposedAt: now.AddDate(0, 0, -91),
		})
		if err != nil {
			t.Fatal(err)
		}
		return graphID, relation
	}
	// graph_id는 UUIDv7이므로 먼저 만든 그래프를 먼저 훑는다.
	broken, brokenRelation := proposal("https://test.invalid/broken-proposal")
	healthy, _ := proposal("https://test.invalid/healthy-proposal")
	if _, err := database.pool.Exec(t.Context(), `UPDATE "`+database.graphName+`"."RELATES_TO"
		SET properties = ag_catalog.agtype_in(replace(ag_catalog.agtype_out(properties)::text, '"proposed_by": "system"', '"proposed_by": "corrupted"')::cstring)
		WHERE properties ->> 'relation_id'::text = $1`, brokenRelation.ID.String()); err != nil {
		t.Fatal(err)
	}
	deleted, err := database.CleanupExpiredProposals(t.Context(), now, func(id model.ID) int {
		if id == accountID {
			return 90
		}
		return 0
	})
	if err == nil || deleted != 1 {
		t.Fatalf("한 그래프 해석 실패 뒤 정리 = %d, %v", deleted, err)
	}
	if relations := proposalRelations(t, database, healthy); len(relations) != 0 {
		t.Fatalf("뒤 순서 그래프의 후보가 남았다: %d", len(relations))
	}
	var remaining int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM "`+database.graphName+`"."RELATES_TO" WHERE properties ->> 'graph_id'::text = $1`, broken.String()).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("해석 실패 그래프의 후보 = %d, %v", remaining, err)
	}
	if strings.Contains(err.Error(), broken.String()) {
		t.Fatal("정리 오류에 그래프 식별자가 섞였다")
	}
}

// TestAuditUnregisteredGraphsIntegration은 context_graph에 없는 graph_id의 정점·간선·임베딩을
// graph_registration으로 보고하고, 등록된 그래프의 행은 보고하지 않는지 확인한다.
func TestAuditUnregisteredGraphsIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	registered, _, _ := boundaryGraph(t, database)
	orphan, from, to, relation := newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := database.pool.Exec(ctx, `SELECT * FROM cypher('`+database.graphName+`', $$ MATCH (n:Context) WHERE n.graph_id = '`+orphan.String()+`' DETACH DELETE n $$) AS (v agtype)`); err != nil {
			t.Error(err)
		}
		if _, err := database.pool.Exec(ctx, `DELETE FROM public.context_embedding WHERE graph_id=$1`, orphan.String()); err != nil {
			t.Error(err)
		}
	})
	if _, err := database.pool.Exec(t.Context(), `SELECT * FROM cypher('`+database.graphName+`', $$
		CREATE (:Context {graph_id: '`+orphan.String()+`', context_id: '`+from.String()+`', layer: 'event'})
			-[:RELATES_TO {graph_id: '`+orphan.String()+`', relation_id: '`+relation.String()+`', state: 'proposed'}]->
			(:Context {graph_id: '`+orphan.String()+`', context_id: '`+to.String()+`', layer: 'event'}) $$) AS (v agtype)`); err != nil {
		t.Fatal(err)
	}
	vector := make([]float64, embeddingDimension(t, database))
	vector[0] = 1
	if _, err := database.pool.Exec(t.Context(), `INSERT INTO public.context_embedding
		(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
		VALUES ($1,$2,$3,'synthetic-model',now(),now(),'hot')`, from.String(), orphan.String(), vectorText(vector)); err != nil {
		t.Fatal(err)
	}
	violations, err := database.AuditUnregisteredGraphs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, violation := range violations {
		if violation.GraphID == registered.String() {
			t.Fatalf("등록된 그래프를 미등록으로 보고했다: %+v", violation)
		}
		if violation.GraphID != orphan.String() {
			continue
		}
		if violation.Rule != RuleGraphRegistration {
			t.Fatalf("규칙 = %s", violation.Rule)
		}
		got[violation.Target+"/"+violation.Label] = append(got[violation.Target+"/"+violation.Label], strings.Join(violation.IDs, ","))
	}
	want := map[string][]string{
		TargetContext + "/":            {from.String(), to.String()},
		TargetRelation + "/relates_to": {relation.String() + "," + from.String() + "," + to.String()},
		TargetEmbedding + "/":          {from.String()},
	}
	for key, ids := range want {
		slices.Sort(ids)
		slices.Sort(got[key])
		if !slices.Equal(got[key], ids) {
			t.Fatalf("미등록 그래프 보고 %s = %v, want %v (전체 %v)", key, got[key], ids, got)
		}
	}
}
