package migrate_test

import (
	"testing"

	"agent_context_sharing/migrations"
)

func TestRelationInferenceMigrationPreservesLegacyIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := targetConfig()
	if count, err := applyTarget(t.Context(), pool, migrationFilesBefore(t, 18), cfg); err != nil || count != 10 {
		t.Fatalf("이전 스키마 준비=%d: %v", count, err)
	}
	_, err := pool.Exec(t.Context(), `INSERT INTO public.operation_log
		(operation_id,graph_id,operation_kind,relation_id,judgment_input,actor_account_id,actor_agent_id,applied_at,result)
		VALUES ('019a0000-0000-7000-8000-000000000001','019a0000-0000-7000-8000-000000000002',
		'add','019a0000-0000-7000-8000-000000000003','legacy',
		'019a0000-0000-7000-8000-000000000004','019a0000-0000-7000-8000-000000000005',now(),'applied')`)
	if err != nil {
		t.Fatal(err)
	}
	old := preservationQuery(t, pool, `SELECT (to_jsonb(o)-'judgment_source')::text FROM public.operation_log o`)
	for _, want := range []int{1, 0} {
		if count, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err != nil || count != want {
			t.Fatalf("018 적용=%d want=%d: %v", count, want, err)
		}
		if current := preservationQuery(t, pool, `SELECT (to_jsonb(o)-'judgment_source')::text FROM public.operation_log o`); current != old {
			t.Fatal("기존 관리 기록이 변경됐다")
		}
	}
	if got := preservationQuery(t, pool, `SELECT COALESCE(judgment_source,'agent') FROM public.operation_log`); got != "agent" {
		t.Fatalf("기존 판정 주체=%s", got)
	}
	if got := preservationQuery(t, pool, `SELECT count(*)::text FROM pg_tables WHERE schemaname='public' AND tablename IN ('relation_inference_task','relation_inference_run','relation_inference_decision')`); got != "3" {
		t.Fatalf("추론 테이블 수=%s", got)
	}
}
