package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// test-transition.sh의 전용 클러스터만 사용한다. 단계 사이에 물리 백업과 이미지 교체가 있다.
func TestOPS08Transition(t *testing.T) {
	phase := os.Getenv("OPS08_PHASE")
	if phase == "" {
		t.Skip("test-transition.sh 전용 시험")
	}
	address := os.Getenv("TEST_DATABASE_URL")
	if address != "postgres://ops08:ops08@localhost/ops08" || (phase != "seed" && phase != "verify" && phase != "write") {
		t.Fatal("격리 DB 주소·시험 단계가 올바르지 않다")
	}
	pool, err := migrate.NewPool(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	database, err := store.New(t.Context(), address, "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	service, err := authz.New(database, authz.Config{Issuer: "https://agent-context.example.com", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	if phase == "seed" {
		account, err := service.Register(t.Context(), "ops08_account", "synthetic password")
		if err != nil {
			t.Fatal(err)
		}
		agent, err := database.RegisterAgent(t.Context(), account, "ops08_client", "ops08_registration")
		if err != nil {
			t.Fatal(err)
		}
		graphID, sourceID := ops08ID(t), ops08ID(t)
		now := time.Now().UTC()
		_, err = database.CreateGraphWithOwner(t.Context(), model.Graph{ID: graphID, Name: "전환 전 합성 그래프", CreatedBy: account, CreatedAt: now, LastActivityAt: now, Version: 1}, store.WriteLimits{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = database.CreateContext(t.Context(), graphID, model.Context{
			ID: sourceID, GraphID: graphID, Layer: model.LayerSource, Body: "백업에 보존할 합성 원천", RecordedAt: now, CreatedBy: account, CreatedByAgent: agent, Version: 1,
			Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelFile, Locator: "file:///ops08/source"}, OccurredAt: now, OriginKind: model.OriginKindExternalContent},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		token, err := service.WebSession(t.Context(), account, "web")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), "CREATE TABLE public.ops08_expected (graph_id uuid, source_id uuid, account_id uuid, agent_id uuid, token text, fingerprint text)"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), "INSERT INTO public.ops08_expected VALUES ($1,$2,$3,$4,$5,$6)", graphID.String(), sourceID.String(), account.String(), agent.String(), token.Raw, ops08Fingerprint(t, pool)); err != nil {
			t.Fatal(err)
		}
	}
	var graphText, sourceText, accountText, agentText, token, fingerprint string
	if err := pool.QueryRow(t.Context(), "SELECT graph_id::text,source_id::text,account_id::text,agent_id::text,token,fingerprint FROM public.ops08_expected").Scan(&graphText, &sourceText, &accountText, &agentText, &token, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if ops08Fingerprint(t, pool) != fingerprint {
		t.Fatal("원본·권한·등록·서명 키·대기 작업이 달라졌다")
	}
	account, _, err := service.Verify(t.Context(), token, "web")
	if err != nil || account.String() != accountText {
		t.Fatal("이전 세션을 검증하지 못했다", err)
	}
	if phase == "write" {
		graphID, _ := model.ParseID(graphText)
		sourceID, _ := model.ParseID(sourceText)
		agentID, _ := model.ParseID(agentText)
		now := time.Now().UTC()
		_, err := database.CreateContext(t.Context(), graphID, model.Context{
			ID: ops08ID(t), GraphID: graphID, Layer: model.LayerDerived, Body: "새 스키마를 사용한 합성 파생", RecordedAt: now, CreatedBy: account, CreatedByAgent: agentID, Version: 1,
			Derived: &model.DerivedAttributes{Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateObservation},
		}, []model.ID{sourceID})
		if err != nil {
			t.Fatal(err)
		}
		var revisions int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM public.context_revision").Scan(&revisions); err != nil || revisions != 1 {
			t.Fatal("013을 사용한 새 파생 판이 없다", err)
		}
	}
}

func ops08ID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ops08Fingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	hash := sha256.New()
	for _, table := range []pgx.Identifier{
		{"public", "account"}, {"public", "context_graph"}, {"public", "graph_grant"}, {"public", "agent_registration"},
		{"public", "signing_key"}, {"public", "index_task"}, {"public", "context_embedding"}, {"public", "operation_log"},
		{"public", "oauth_grant"}, {"public", "oauth_authorization_code"}, {"public", "oauth_refresh_token"}, {"public", "idempotency_record"},
		{"agent_context", "Context"},
	} {
		var snapshot string
		if err := pool.QueryRow(t.Context(), "SELECT coalesce(jsonb_agg(row ORDER BY row)::text,'[]') FROM (SELECT to_jsonb(value) AS row FROM "+table.Sanitize()+" value) s").Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(hash, table, snapshot)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
