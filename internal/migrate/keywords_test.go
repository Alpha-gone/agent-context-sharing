package migrate_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
)

// TestUnquotedKeywordsMatchCatalogIntegration은 실제 PostgreSQL 카탈로그의 예약어(R)와 타입·함수
// 이름 키워드(T)만 그래프 이름·콜드 tablespace에서 거부하고, 열 이름 키워드(C)와 비예약어(U)는
// 허용하는지 확인한다. 판올림으로 키워드가 바뀌면 이 시험이 목록 갱신을 요구한다.
func TestUnquotedKeywordsMatchCatalogIntegration(t *testing.T) {
	pool := newTestPool(t)
	rows, err := pool.Query(t.Context(), `SELECT word, catcode FROM pg_get_keywords()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	identifier := regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	checked := 0
	for rows.Next() {
		var word, category string
		if err := rows.Scan(&word, &category); err != nil {
			t.Fatal(err)
		}
		if !identifier.MatchString(word) {
			continue
		}
		reject := category == "R" || category == "T"
		graph := migrate.Config{GraphName: word, VectorType: "vector", VectorDim: 8}
		tablespace := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 8, ColdTablespace: word}
		if (graph.Validate() != nil) != reject || (tablespace.Validate() != nil) != reject {
			t.Errorf("키워드 %q(%s) 거부 = 그래프 %v, tablespace %v, want %t", word, category, graph.Validate(), tablespace.Validate(), reject)
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("카탈로그 키워드를 읽지 못했다")
	}
}

// TestColumnKeywordGraphNameMigratesIntegration은 허용하는 열 이름 키워드를 그래프 이름으로 써도
// 따옴표 없이 치환하는 012를 포함한 전체 마이그레이션이 적용되는지 확인한다.
func TestColumnKeywordGraphNameMigratesIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := migrate.Config{GraphName: "between", VectorType: "vector", VectorDim: 8, IndexTargetLayers: "all_layers"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	loaded, applied, err := migrate.Prepare(t.Context(), pool, migrations.FS, ".", cfg)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := migrate.Pending(loaded, applied)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range pending {
		if err := migrate.Apply(t.Context(), pool, m); err != nil {
			t.Fatalf("%03d_%s 적용: %v", m.Version, m.Name, err)
		}
	}
	var exists bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM ag_catalog.ag_graph WHERE name = 'between')`).Scan(&exists); err != nil || !exists {
		t.Fatalf("열 이름 키워드 그래프 생성 = %t, %v", exists, err)
	}
}

// TestInspectIsReadOnlyAndLockFreeIntegration은 status용 Inspect가 이력 테이블을 만들거나 구형
// 이력을 보충하지 않고, 진행 중인 up의 자문 잠금을 기다리지 않으며 읽기 전용 트랜잭션에서
// 동작하는지 확인한다.
func TestInspectIsReadOnlyAndLockFreeIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 8, IndexTargetLayers: "all_layers"}
	// pending은 읽기 전용 트랜잭션에서 Inspect한 미적용 목록과 적용 이력 수를 돌려준다.
	pending := func() ([]migrate.Migration, int, error) {
		tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, 0, err
		}
		defer tx.Rollback(context.WithoutCancel(t.Context()))
		loaded, applied, err := migrate.Inspect(t.Context(), tx, migrations.FS, ".", cfg)
		if err != nil {
			return nil, 0, err
		}
		pending, err := migrate.Pending(loaded, applied)
		return pending, len(applied), err
	}
	all, err := migrate.Load(migrations.FS, ".", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if missing, applied, err := pending(); err != nil || len(missing) != len(all) || applied != 0 {
		t.Fatalf("빈 DB의 미적용 목록 = %d/%d, %v", len(missing), len(all), err)
	}
	var history bool
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass('public.schema_migration') IS NOT NULL`).Scan(&history); err != nil || history {
		t.Fatalf("Inspect가 이력 테이블을 만들었다: %t, %v", history, err)
	}

	loaded, applied, err := migrate.Prepare(t.Context(), pool, migrations.FS, ".", cfg)
	if err != nil {
		t.Fatal(err)
	}
	unapplied, err := migrate.Pending(loaded, applied)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range unapplied {
		if err := migrate.Apply(t.Context(), pool, m); err != nil {
			t.Fatal(err)
		}
	}
	// 구성 열이 없던 구형 이력을 흉내 낸다. Inspect는 열을 만들거나 구성을 기록하지 않아야 한다.
	if _, err := pool.Exec(t.Context(), `ALTER TABLE public.schema_migration DROP COLUMN render_config`); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_lock($1)`, migrate.AdvisoryLockKey); err != nil {
		t.Fatal(err)
	}
	defer holder.Exec(context.WithoutCancel(t.Context()), `SELECT pg_advisory_unlock($1)`, migrate.AdvisoryLockKey)
	type inspected struct {
		missing int
		err     error
	}
	done := make(chan inspected, 1)
	go func() {
		missing, _, err := pending()
		done <- inspected{len(missing), err}
	}()
	select {
	case result := <-done:
		if result.err != nil || result.missing != 0 {
			t.Fatalf("구형 이력의 미적용 목록 = %d, %v", result.missing, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Inspect가 진행 중인 up의 자문 잠금을 기다렸다")
	}
	var column bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'public.schema_migration'::regclass AND attname = 'render_config' AND NOT attisdropped)`).Scan(&column); err != nil || column {
		t.Fatalf("Inspect가 구형 이력의 구성 열을 만들었다: %t, %v", column, err)
	}
}
