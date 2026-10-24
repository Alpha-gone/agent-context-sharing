package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// test-pitr.sh가 만든 독립 클러스터에서만 실행한다. 일반 DB 시험은 건너뛴다.
// seed와 verify 사이에는 실제 물리 백업·WAL 재생이 있으므로 DB를 정리하지 않는다.
func TestOPS07PhysicalRecovery(t *testing.T) {
	phase := os.Getenv("OPS07_PHASE")
	if phase == "" {
		t.Skip("test-pitr.sh 전용 격리 백업 시험")
	}
	address := os.Getenv("TEST_DATABASE_URL")
	if address != "postgres://pitr_test:pitr_test@localhost/pitr_test" || (phase != "seed" && phase != "verify" && phase != "search") {
		t.Fatal("격리 DB 주소·시험 단계가 올바르지 않다")
	}
	pool, err := migrate.NewPool(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if phase == "seed" {
		cfg := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 128, ColdTablespace: "ops07_cold", IndexTargetLayers: "all_layers"}
		err := migrate.WithLock(t.Context(), pool, func(ctx context.Context, conn *pgxpool.Conn) error {
			loaded, applied, err := migrate.Prepare(ctx, conn, migrations.FS, ".", cfg)
			if err != nil {
				return err
			}
			pending, err := migrate.Pending(loaded, applied)
			if err != nil {
				return err
			}
			for _, item := range pending {
				if err := migrate.Apply(ctx, conn, item); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	database, err := store.New(t.Context(), address, "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	service, err := authz.New(database, authz.Config{Issuer: "https://ops07.invalid", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	if phase == "seed" {
		ops07Seed(t, pool, database, service)
	} else {
		ops07Verify(t, pool, database, service, phase == "search")
	}
}

func ops07Seed(t *testing.T, pool *pgxpool.Pool, database *store.Store, service *authz.Service) {
	t.Helper()
	account, err := service.Register(t.Context(), "ops07_account", "synthetic password")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := database.RegisterAgent(t.Context(), account, "ops07_client", "ops07_registration")
	if err != nil {
		t.Fatal(err)
	}
	graphID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	graph, err := database.CreateGraphWithOwner(t.Context(), model.Graph{ID: graphID, Name: "기본 백업", CreatedBy: account, CreatedAt: now, LastActivityAt: now, Version: 1}, store.WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []model.Context
	for i := range 3 {
		id, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		value := model.Context{
			ID: id, GraphID: graph.ID, Layer: model.LayerSource, Body: fmt.Sprintf("복구 원천 %d", i), RecordedAt: now,
			CreatedBy: account, CreatedByAgent: agent, Version: 1,
			Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelFile, Locator: fmt.Sprintf("file:///ops07/%d", i)}, OccurredAt: now, OriginKind: model.OriginKindExternalContent},
		}
		var references []model.ID
		if i == 2 {
			value.Layer, value.Source = model.LayerDerived, nil
			value.Derived = &model.DerivedAttributes{Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateObservation}
			references = []model.ID{nodes[0].ID}
		}
		node, err := database.CreateContext(t.Context(), graph.ID, value, references)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	// 두 노드는 현재 모델로 색인하고 한 노드는 pending으로 남긴다. 외부 호출은 없다.
	for i := range 2 {
		if _, err := pool.Exec(t.Context(), "UPDATE public.index_task SET next_attempt_at=$2 WHERE context_id=$1", nodes[i].ID.String(), now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph.ID, func(context.Context, store.IndexTask) store.IndexTaskResult {
			vector := make([]float64, 128)
			vector[i] = 1
			return store.IndexTaskResult{Embedding: vector, ModelID: "gemini:gemini-embedding-2:retrieval-v1:vector:128"}
		})
		if err != nil || !result.Succeeded || result.Task.ContextID != nodes[i].ID {
			t.Fatal("합성 색인 실패", err)
		}
	}
	if _, err := pool.Exec(t.Context(), "UPDATE public.context_embedding SET storage_tier='cold' WHERE context_id=$1", nodes[0].ID.String()); err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(t.Context(), account, "web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "CREATE TABLE public.ops07_expected (graph_id uuid, account_id uuid, token text, fingerprint text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO public.ops07_expected VALUES ($1,$2,$3,$4)", graph.ID.String(), account.String(), token.Raw, ops07Fingerprint(t, pool)); err != nil {
		t.Fatal(err)
	}
	ops07Verify(t, pool, database, service, false)
}

func ops07Fingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	hash := sha256.New()
	for _, table := range []string{"account", "graph_grant", "agent_registration", "signing_key", "revoked_token", "index_task", "context_embedding", "web_audit_log", "context_graph"} {
		row := "to_jsonb(value)"
		if table == "context_graph" {
			row += " - 'name'" // 목표 전후의 서로 다른 이름은 shell에서 별도 대조한다.
		}
		var snapshot string
		if err := pool.QueryRow(t.Context(), "SELECT coalesce(jsonb_agg(row ORDER BY row)::text,'[]') FROM (SELECT "+row+" AS row FROM public."+pgx.Identifier{table}.Sanitize()+" value) s").Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(hash, table, snapshot)
	}
	for _, table := range []string{"Context", "DERIVED_FROM"} {
		var snapshot string
		if err := pool.QueryRow(t.Context(), "SELECT coalesce(jsonb_agg(row ORDER BY row)::text,'[]') FROM (SELECT to_jsonb(value) AS row FROM "+pgx.Identifier{"agent_context", table}.Sanitize()+" value) s").Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(hash, table, snapshot)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func ops07Verify(t *testing.T, pool *pgxpool.Pool, database *store.Store, service *authz.Service, search bool) {
	t.Helper()
	var graphText, accountText, token, fingerprint string
	if err := pool.QueryRow(t.Context(), "SELECT graph_id::text,account_id::text,token,fingerprint FROM public.ops07_expected").Scan(&graphText, &accountText, &token, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if ops07Fingerprint(t, pool) != fingerprint {
		t.Fatal("관계형 데이터·키·큐·임베딩 상태 지문이 다르다")
	}
	account, _, err := service.Verify(t.Context(), token, "web")
	if err != nil || account.String() != accountText {
		t.Fatal("기본 백업 전 세션 서명 검증 실패", err)
	}
	var edges int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM ag_catalog.cypher('agent_context', $$ MATCH (d:Context)-[:DERIVED_FROM]->(s:Context) RETURN d.context_id, s.context_id $$) AS (derived agtype, source agtype)`).Scan(&edges); err != nil || edges != 1 {
		t.Fatal("AGE 파생·원천 관계 복원 실패", err)
	}
	var cold, pending int
	if err := pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM public.context_embedding_cold),(SELECT count(*) FROM public.index_task WHERE state='pending')").Scan(&cold, &pending); err != nil || cold != 1 || pending != 1 {
		t.Fatalf("cold=%d pending=%d: %v", cold, pending, err)
	}
	var path string
	if err := pool.QueryRow(t.Context(), "SELECT pg_tablespace_location(reltablespace) FROM pg_class WHERE oid='public.context_embedding_cold'::regclass").Scan(&path); err != nil || path != "/var/lib/postgresql/cold" {
		t.Fatal("cold tablespace 경로 복원 실패", err)
	}
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(ctx); err != nil {
			t.Error(err)
		}
	}()
	if _, err := tx.Exec(t.Context(), "SET LOCAL enable_seqscan=off; SET LOCAL enable_sort=off"); err != nil {
		t.Fatal(err)
	}
	var plans strings.Builder
	for _, tier := range []string{"hot", "cold"} {
		rows, err := tx.Query(t.Context(), "EXPLAIN (ANALYZE) SELECT context_id FROM public.context_embedding_"+tier+" ORDER BY embedding <=> $1::vector LIMIT 1", "[1,"+strings.Repeat("0,", 126)+"0]")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plans.WriteString(line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !strings.Contains(plans.String(), "context_embedding_"+tier+"_embedding_idx") {
			t.Fatal("복원된 HNSW를 사용하지 못했다", tier)
		}
	}
	ids, err := database.AuditGraphIDs(t.Context())
	if err != nil || len(ids) != 1 {
		t.Fatal("전체 감사 대상 조회 실패", err)
	}
	for _, id := range ids {
		violations, err := database.AuditGraph(t.Context(), id, store.EmbeddingExpectation{ModelID: "gemini:gemini-embedding-2:retrieval-v1:vector:128", Dimension: 128})
		if err != nil || len(violations) != 0 {
			t.Fatal("복원된 전체 그래프 불변식 위반", violations, err)
		}
	}
	if search {
		vector := make([]float64, 128)
		vector[0] = 1
		graph, err := model.ParseID(graphText)
		if err != nil {
			t.Fatal(err)
		}
		candidates, err := database.SemanticCandidates(t.Context(), graph, "gemini:gemini-embedding-2:retrieval-v1:vector:128", vector, time.Now().UTC(), 10)
		if err != nil || len(candidates) != 2 || candidates[0].Similarity != 1 {
			t.Fatal("승격 뒤 양쪽 계층 AGE·현재 모델 검색 실패", err)
		}
		processed, err := database.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, store.IndexTask) store.IndexTaskResult {
			return store.IndexTaskResult{Embedding: vector, ModelID: "gemini:gemini-embedding-2:retrieval-v1:vector:128"}
		})
		if err != nil || !processed.Succeeded {
			t.Fatal("복원된 대기 작업을 소비하지 못했다", err)
		}
		if err := database.CheckGraphIndexReady(t.Context(), graph, "gemini:gemini-embedding-2:retrieval-v1:vector:128"); err != nil {
			t.Fatal("복원 뒤 색인 준비 실패", err)
		}
	}
	t.Log("AGE·현재 모델·hot/cold HNSW·tablespace·서명·pending·전체 불변식 확인")
}
