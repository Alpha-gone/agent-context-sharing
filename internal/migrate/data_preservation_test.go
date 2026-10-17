package migrate_test

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func migrationFilesBefore(t *testing.T, version int) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= fmt.Sprintf("%03d_", version) {
			continue
		}
		data, err := fs.ReadFile(migrations.FS, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return files
}

func preservationQuery(t *testing.T, pool *pgxpool.Pool, query string) string {
	t.Helper()
	var value string
	if err := pool.QueryRow(t.Context(), query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

const preservationOriginals = `SELECT jsonb_build_object(
	'nodes',(SELECT jsonb_agg(to_jsonb(n) ORDER BY id) FROM agent_context."Context" n),
	'edges',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM agent_context._ag_label_edge e),
	'graphs',(SELECT jsonb_agg(to_jsonb(g)-'content_revision' ORDER BY graph_id) FROM public.context_graph g),
	'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM public.operation_log o),
	'accounts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY account_id) FROM public.account a),
	'grants',(SELECT jsonb_agg(to_jsonb(g) ORDER BY grant_id) FROM public.oauth_grant g),
	'codes',(SELECT jsonb_agg(to_jsonb(c) ORDER BY code_hash) FROM public.oauth_authorization_code c),
	'refresh',(SELECT jsonb_agg(to_jsonb(r) ORDER BY token_hash) FROM public.oauth_refresh_token r),
	'registrations',(SELECT jsonb_agg(to_jsonb(a) ORDER BY agent_id) FROM public.agent_registration a),
	'idempotency',(SELECT jsonb_agg(to_jsonb(i) ORDER BY actor_account_id,idempotency_key) FROM public.idempotency_record i)
	)::text`

const preservationMutable = `SELECT jsonb_build_object(
	'embeddings',(SELECT jsonb_agg(to_jsonb(e) ORDER BY context_id,storage_tier) FROM public.context_embedding e),
	'tasks',(SELECT jsonb_agg(to_jsonb(t) ORDER BY task_id) FROM public.index_task t),
	'graphs',(SELECT jsonb_agg(to_jsonb(g) ORDER BY graph_id) FROM public.context_graph g),
	'type',(SELECT format_type(atttypid,atttypmod) FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'),
	'indexes',(SELECT jsonb_agg(indexdef ORDER BY indexname) FROM pg_indexes WHERE tablename IN ('context_embedding','context_embedding_hot','context_embedding_cold'))
	)::text`

func preservationFixture(t *testing.T, typ string) (*pgxpool.Pool, *store.Store, migrate.Config) {
	t.Helper()
	pool, address := isolatedMigrationDatabase(t)
	cfg := migrate.Config{GraphName: "agent_context", VectorType: typ, VectorDim: 8, IndexTargetLayers: "all_layers"}
	if _, err := applyTarget(t.Context(), pool, migrationFilesBefore(t, 12), cfg); err != nil {
		t.Fatal(err)
	}
	database, err := store.New(t.Context(), address, cfg.GraphName, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	newID := func() model.ID {
		id, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	now := time.Now().UTC()
	actor := newID()
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.account VALUES ($1,'data09','synthetic-hash',$2)`, actor.String(), now); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		graph := newID()
		if _, err := database.CreateGraph(t.Context(), model.Graph{ID: graph, Name: "합성 보존 표본", CreatedBy: actor, CreatedAt: now, LastActivityAt: now, Version: 1}); err != nil {
			t.Fatal(err)
		}
		source := model.Context{ID: newID(), GraphID: graph, Layer: model.LayerSource, Body: "보존할 합성 원천", RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1,
			Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelWeb, Locator: "https://example.test/data09"}, OccurredAt: now, OriginKind: model.OriginKindExternalContent}}
		if _, err := database.CreateContext(t.Context(), graph, source, nil); err != nil {
			t.Fatal(err)
		}
		derived := model.Context{ID: newID(), GraphID: graph, Layer: model.LayerDerived, Body: "보존할 합성 파생", RecordedAt: now, CreatedBy: actor, CreatedByAgent: actor, Version: 1,
			Derived: &model.DerivedAttributes{Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateObservation}}
		if _, err := database.CreateContext(t.Context(), graph, derived, []model.ID{source.ID}); err != nil {
			t.Fatal(err)
		}
		// 각 그래프에 재등록해서는 안 되는 폐기 표본도 남긴다.
		deleted := derived
		deleted.ID = newID()
		if _, err := database.CreateContext(t.Context(), graph, deleted, []model.ID{source.ID}); err != nil {
			t.Fatal(err)
		}
		op := &store.OperationRecord{Kind: store.OperationDiscard, GraphID: graph, ContextID: deleted.ID, TargetVersion: 1, JudgmentInput: "합성 폐기", AccountID: actor, AgentID: actor}
		if _, err := database.DiscardContext(t.Context(), graph, deleted.ID, op, store.WriteLimits{}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO public.context_embedding
			(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
			SELECT (properties->>'context_id'::text)::uuid,$1,'[1,0,0,0,0,0,0,0]','previous',$2,$2,$3
			FROM agent_context."Context" WHERE (properties->>'graph_id'::text)::uuid=$1`, graph.String(), now, []string{"hot", "cold"}[i]); err != nil {
			t.Fatal(err)
		}
	}
	// 기존 OAuth·등록·완료 결과도 형상 변경과 독립적으로 보존한다.
	for _, sql := range []string{grantSeed, codeSeed, refreshSeed, registrationSeed,
		strings.Replace(idempotencySeed, "NULL,now()", `'{"result":"preserved"}',now()`, 1),
		`DELETE FROM public.index_task WHERE context_id IN (SELECT (properties->>'context_id'::text)::uuid FROM agent_context."Context" WHERE properties->>'deleted_at'::text IS NOT NULL)`,
		`UPDATE public.index_task SET state='failed',attempts=5,last_error='synthetic',correlation_id='019a0000-0000-7000-8000-000000000001'`} {
		if _, err := pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	return pool, database, cfg
}

func TestData09ShapeTransitionAndPreservationIntegration(t *testing.T) {
	for _, shape := range []struct {
		from, to  string
		dimension int
	}{
		{"vector", "vector", 16}, {"vector", "halfvec", 8}, {"halfvec", "vector", 16},
		{"vector", "vector", 8}, {"halfvec", "halfvec", 8},
	} {
		for _, targets := range []string{"all_layers", "without_source"} {
			t.Run(fmt.Sprintf("%s_to_%s%d/%s", shape.from, shape.to, shape.dimension, targets), func(t *testing.T) {
				pool, database, original := preservationFixture(t, shape.from)
				target := original
				target.VectorType, target.VectorDim, target.IndexTargetLayers = shape.to, shape.dimension, targets
				before := preservationQuery(t, pool, preservationOriginals)
				mutable := preservationQuery(t, pool, preservationMutable)
				historySQL := `SELECT jsonb_agg(to_jsonb(h) ORDER BY version)::text FROM public.schema_migration h WHERE version<12`
				history := preservationQuery(t, pool, historySQL)
				// DDL·벡터·큐 변경 뒤 장애가 나도 이력까지 함께 롤백한다.
				broken := migrationFilesBefore(t, 13)
				broken["012_embedding_configuration.sql"].Data = append(broken["012_embedding_configuration.sql"].Data, []byte("\nSELECT 1/0;")...)
				if _, err := applyTarget(t.Context(), pool, broken, target); err == nil {
					t.Fatal("전환 장애가 성공했다")
				}
				if preservationQuery(t, pool, preservationMutable) != mutable || preservationQuery(t, pool, preservationOriginals) != before {
					t.Fatal("실패한 전환이 기존 상태를 변경했다")
				}
				if count, err := applyTarget(t.Context(), pool, migrations.FS, target); err != nil || count != 1 {
					t.Fatalf("전환=%d: %v", count, err)
				}
				if preservationQuery(t, pool, preservationOriginals) != before || preservationQuery(t, pool, historySQL) != history {
					t.Fatal("원본·업무 상태·과거 이력이 변경됐다")
				}
				if err := database.CheckEmbeddingSchema(t.Context(), target.VectorType, target.VectorDim); err != nil {
					t.Fatal(err)
				}
				changed := original.VectorType != target.VectorType || original.VectorDim != target.VectorDim
				if !changed {
					if preservationQuery(t, pool, preservationMutable) != mutable {
						t.Fatal("같은 형상의 벡터·작업·내용 판이 변경됐다")
					}
				} else {
					want := "4"
					if targets == "without_source" {
						want = "2"
					}
					if preservationQuery(t, pool, `SELECT count(*)::text FROM public.context_embedding`) != "0" || preservationQuery(t, pool, `SELECT count(*)::text FROM public.index_task`) != want {
						t.Fatal("호환되지 않는 벡터 또는 대상 작업 수가 다르다")
					}
					if preservationQuery(t, pool, `SELECT count(*)::text FROM public.index_task WHERE state<>'pending' OR attempts<>0 OR last_error IS NOT NULL OR correlation_id IS NOT NULL OR enqueued_at<>next_attempt_at`) != "0" {
						t.Fatal("요청 없는 재등록의 세대·예산·상관 ID 초기화 실패")
					}
					// 적용된 012를 재실행하지 않고 새 번호로 원래 형상에 복귀한다.
					files := migrationFilesBefore(t, 13)
					data, err := migrations.FS.ReadFile("012_embedding_configuration.sql")
					if err != nil {
						t.Fatal(err)
					}
					files["013_data09_return.sql"] = &fstest.MapFile{Data: data}
					if count, err := applyTarget(t.Context(), pool, files, original); err != nil || count != 1 {
						t.Fatalf("새 번호 복귀=%d: %v", count, err)
					}
					if err := database.CheckEmbeddingSchema(t.Context(), original.VectorType, original.VectorDim); err != nil {
						t.Fatal(err)
					}
					if preservationQuery(t, pool, preservationOriginals) != before {
						t.Fatal("복귀가 원본을 변경했다")
					}
				}
				if count, err := applyTarget(t.Context(), pool, migrations.FS, target); err != nil || count != 0 {
					t.Fatalf("적용 파일 반복=%d: %v", count, err)
				}
				if pool.Stat().AcquiredConns() != 0 {
					t.Fatal("마이그레이션 연결이 반환되지 않았다")
				}
			})
		}
	}
}

func TestData09LegacyConfigurationBackfillAtomicIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := targetConfig()
	if _, err := applyTarget(t.Context(), pool, migrationFilesBefore(t, 10), cfg); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE public.schema_migration SET render_config=NULL`,
		`CREATE FUNCTION public.data09_reject_config() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.version=9 THEN RAISE EXCEPTION 'synthetic metadata failure'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER data09_reject_config BEFORE UPDATE ON public.schema_migration FOR EACH ROW EXECUTE FUNCTION public.data09_reject_config()`,
	} {
		if _, err := pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	historySQL := `SELECT jsonb_agg(to_jsonb(h) ORDER BY version)::text FROM public.schema_migration h`
	before := preservationQuery(t, pool, historySQL)
	if _, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err == nil {
		t.Fatal("이력 보충 장애가 성공했다")
	}
	if preservationQuery(t, pool, historySQL) != before {
		t.Fatal("보충 실패가 일부 이력을 남겼다")
	}
	if _, err := pool.Exec(t.Context(), `DROP TRIGGER data09_reject_config ON public.schema_migration; UPDATE public.schema_migration SET checksum='tampered' WHERE version=9`); err != nil {
		t.Fatal(err)
	}
	if _, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err == nil {
		t.Fatal("다른 파일의 checksum 불일치가 허용됐다")
	}
	if preservationQuery(t, pool, `SELECT count(*)::text FROM public.schema_migration WHERE render_config IS NOT NULL`) != "0" {
		t.Fatal("전체 검증 전에 구성 일부를 기록했다")
	}
	// 일치하는 checksum을 복원한 뒤 구성 보충과 후속 DDL이 성공한다.
	loaded, err := migrate.Load(migrationFilesBefore(t, 10), ".", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE public.schema_migration SET checksum=$1 WHERE version=9`, loaded[1].Checksum); err != nil {
		t.Fatal(err)
	}
	if count, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err != nil || count != 3 {
		t.Fatalf("보충 후 적용=%d: %v", count, err)
	}
	if preservationQuery(t, pool, `SELECT count(*)::text FROM public.schema_migration WHERE render_config IS NULL`) != "0" {
		t.Fatal("구성 보충 누락")
	}
	if pool.Stat().AcquiredConns() != 0 {
		t.Fatal("이력 보충 연결 반환 실패")
	}
}

func TestData09OptionalCorrelationColumnIntegration(t *testing.T) {
	pool, _, cfg := preservationFixture(t, "vector")
	if _, err := pool.Exec(t.Context(), `ALTER TABLE public.index_task DROP COLUMN correlation_id`); err != nil {
		t.Fatal(err)
	}
	before := preservationQuery(t, pool, `SELECT jsonb_agg(to_jsonb(t) ORDER BY task_id)::text FROM public.index_task t`)
	if _, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err != nil {
		t.Fatal(err)
	}
	if preservationQuery(t, pool, `SELECT jsonb_agg(to_jsonb(t)-'correlation_id' ORDER BY task_id)::text FROM public.index_task t`) != before {
		t.Fatal("선택 열 추가가 기존 작업을 변경했다")
	}
	if preservationQuery(t, pool, `SELECT count(*)::text FROM public.index_task WHERE correlation_id IS NOT NULL`) != "0" {
		t.Fatal("기존 작업의 상관 ID가 NULL이 아니다")
	}
}
