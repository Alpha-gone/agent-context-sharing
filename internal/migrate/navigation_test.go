package migrate_test

import (
	"testing"

	"agent_context_sharing/migrations"
)

func TestNavigationMigrationPreservesHistoryIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := targetConfig()
	cfg.GraphName = "agent_context"
	if _, err := applyTarget(t.Context(), pool, migrationFilesBefore(t, 17), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), graphSeed); err != nil {
		t.Fatal(err)
	}
	// 016 형상의 기존 원천에는 대화 순서와 탐색 참조를 소급 채우지 않는다.
	if _, err := pool.Exec(t.Context(), `INSERT INTO agent_context."Context" (properties)
		VALUES ('{"context_id":"019a0000-0000-7000-8000-000000000010","graph_id":"019a0000-0000-7000-8000-000000000004","layer":"source","source_ref_locator":"api://navigation-migration"}'::ag_catalog.agtype)`); err != nil {
		t.Fatal(err)
	}
	historySQL := `SELECT jsonb_agg(to_jsonb(h) ORDER BY version)::text FROM public.schema_migration h WHERE version<17`
	nodesSQL := `SELECT jsonb_agg(to_jsonb(n) ORDER BY id)::text FROM agent_context."Context" n`
	graphsSQL := `SELECT jsonb_agg(to_jsonb(g)-'entry_context_id' ORDER BY graph_id)::text FROM public.context_graph g`
	history, nodes, graphs := preservationQuery(t, pool, historySQL), preservationQuery(t, pool, nodesSQL), preservationQuery(t, pool, graphsSQL)
	for _, want := range []int{1, 0} {
		if count, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err != nil || count != want {
			t.Fatalf("017 적용·반복=%d, want=%d: %v", count, want, err)
		}
		if preservationQuery(t, pool, historySQL) != history || preservationQuery(t, pool, nodesSQL) != nodes || preservationQuery(t, pool, graphsSQL) != graphs {
			t.Fatal("017이 기존 데이터 또는 checksum·render_config·적용 시각을 바꿨다")
		}
	}
	if got := preservationQuery(t, pool, `SELECT count(*)::text FROM public.context_graph WHERE entry_context_id IS NOT NULL`); got != "0" {
		t.Fatalf("기존 그래프 대표를 자동 지정했다: %s", got)
	}
	if got := preservationQuery(t, pool, `SELECT count(*)::text FROM pg_indexes WHERE schemaname='agent_context'
		AND indexname IN ('navigates_to_hop_endpoints_idx','navigates_to_graph_id_text_idx','context_source_conversation_turn_idx')`); got != "3" {
		t.Fatalf("탐색·대화 순서 인덱스=%s", got)
	}
	if got := preservationQuery(t, pool, `SELECT count(*)::text FROM information_schema.columns
		WHERE table_schema='public' AND table_name='context_graph' AND column_name='entry_context_id' AND data_type='uuid' AND is_nullable='YES'`); got != "1" {
		t.Fatalf("대표 포인터 열=%s", got)
	}
}
