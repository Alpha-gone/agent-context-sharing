package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func targetConfig() migrate.Config {
	return migrate.Config{GraphName: "data02_test", VectorType: "vector", VectorDim: 768, IndexTargetLayers: "all_layers"}
}

func applyTarget(ctx context.Context, pool *pgxpool.Pool, files fs.FS, cfg migrate.Config) (int, error) {
	appliedCount := 0
	err := migrate.WithLock(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		loaded, applied, err := migrate.Prepare(ctx, conn, files, ".", cfg)
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(loaded, applied)
		if err != nil {
			return err
		}
		for _, m := range pending {
			if err := migrate.Apply(ctx, conn, m); err != nil {
				return err
			}
			appliedCount++
		}
		return nil
	})
	return appliedCount, err
}

func singleConnectionPool(t *testing.T, address string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "1")
	u.RawQuery = q.Encode()
	pool, err := migrate.NewPool(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestTargetSchemaFreshAndRepeatIntegration(t *testing.T) {
	for _, vectorType := range []string{"vector", "halfvec"} {
		t.Run(vectorType, func(t *testing.T) {
			_, address := isolatedMigrationDatabase(t)
			pool := singleConnectionPool(t, address)
			cfg := targetConfig()
			cfg.VectorType = vectorType
			if vectorType == "halfvec" {
				cfg.VectorDim = 3072
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			for _, want := range []int{6, 0} {
				if got, err := applyTarget(ctx, pool, migrations.FS, cfg); err != nil || got != want {
					t.Fatalf("적용=%d want=%d: %v", got, want, err)
				}
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relnamespace='public'::regnamespace AND relkind IN ('r','p') AND NOT relispartition AND relname <> 'schema_migration'`).Scan(&count); err != nil || count != 18 {
				t.Fatalf("업무 테이블=%d: %v", count, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM ag_catalog.ag_label l JOIN ag_catalog.ag_graph g ON g.graphid=l.graph WHERE g.name=$1 AND l.name IN ('Context','DERIVED_FROM','SUPERSEDES','HAS_MEMBER','PRECEDES','CAUSES','PART_OF','RELATES_TO')`, cfg.GraphName).Scan(&count); err != nil || count != 8 {
				t.Fatalf("labels=%d: %v", count, err)
			}
			for _, column := range [][3]string{{"context_graph", "version", "bigint"}, {"operation_log", "target_version", "bigint"}, {"request_rate", "count", "bigint"}, {"idempotency_record", "idempotency_key", "text"}} {
				var typ string
				if err := pool.QueryRow(ctx, `SELECT data_type FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2`, column[0], column[1]).Scan(&typ); err != nil || typ != column[2] {
					t.Fatalf("%v: %s %v", column, typ, err)
				}
			}
			var typ string
			if err := pool.QueryRow(ctx, `SELECT format_type(atttypid,atttypmod) FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&typ); err != nil || typ != fmt.Sprintf("%s(%d)", vectorType, cfg.VectorDim) {
				t.Fatalf("embedding=%s: %v", typ, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname IN ('oauth_grant_account_client_idx','oauth_grant_retain_until_idx','oauth_authorization_code_grant_idx','oauth_authorization_code_unconsumed_expires_idx','oauth_refresh_token_grant_idx','agent_registration_account_client_idx','agent_registration_active_account_idx')`).Scan(&count); err != nil || count != 7 {
				t.Fatalf("새 인덱스=%d: %v", count, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am a ON a.oid=c.relam WHERE a.amname='hnsw' AND c.relnamespace='public'::regnamespace AND c.relkind='i'`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("HNSW=%d: %v", count, err)
			}
			loaded, err := migrate.Load(migrations.FS, ".", cfg)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(ctx, loaded[2].Rendered); err != nil {
				t.Fatalf("010 SQL 재실행: %v", err)
			}
		})
	}
}

const grantSeed = `INSERT INTO public.oauth_grant VALUES ('019a0000-0000-7000-8000-000000000001','019a0000-0000-7000-8000-000000000002','client','https://example.test/mcp','scope',now(),now(),now()+interval '30 days',now()+interval '7 days',now()+interval '30 days',NULL,NULL)`
const codeSeed = `INSERT INTO public.oauth_authorization_code (code_hash,account_id,client_id,redirect_uri,resource,scope,code_challenge,issued_at,expires_at) VALUES (decode(repeat('ab',32),'hex'),'019a0000-0000-7000-8000-000000000002','client','http://127.0.0.1:49152/callback','https://example.test/mcp','scope','challenge',now(),now()+interval '60 seconds')`
const refreshSeed = `INSERT INTO public.oauth_refresh_token (token_hash,grant_id,issued_at,expires_at) VALUES (decode(repeat('cd',32),'hex'),'019a0000-0000-7000-8000-000000000001',now(),now()+interval '7 days')`
const registrationSeed = `INSERT INTO public.agent_registration VALUES ('019a0000-0000-7000-8000-000000000003','019a0000-0000-7000-8000-000000000002','client','registration_key_1',now(),now())`
const idempotencySeed = `INSERT INTO public.idempotency_record VALUES ('019a0000-0000-7000-8000-000000000002','request_key_00001','node_create',decode(repeat('ef',32),'hex'),NULL,now(),now()+interval '24 hours')`
const graphSeed = `INSERT INTO public.context_graph (graph_id,name,created_by,created_at,last_activity_at) VALUES ('019a0000-0000-7000-8000-000000000004','test','019a0000-0000-7000-8000-000000000002',now(),now())`

func TestTargetSchemaConstraintsIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	if _, err := applyTarget(t.Context(), pool, migrations.FS, targetConfig()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, query, state string }{
		{"code_hash_length", `UPDATE public.oauth_authorization_code SET code_hash=decode(repeat('aa',31),'hex')`, "23514"},
		{"code_consumed_pair", `UPDATE public.oauth_authorization_code SET consumed_at=now()`, "23514"},
		{"code_grant_fk", `UPDATE public.oauth_authorization_code SET consumed_at=now(),grant_id='019a0000-0000-7000-8000-000000000099'`, "23503"},
		{"refresh_hash_length", `UPDATE public.oauth_refresh_token SET token_hash=decode(repeat('aa',31),'hex')`, "23514"},
		{"refresh_consumed_pair", `UPDATE public.oauth_refresh_token SET consumed_at=now()`, "23514"},
		{"successor_hash_length", `UPDATE public.oauth_refresh_token SET consumed_at=now(),successor_token_hash=decode(repeat('aa',31),'hex')`, "23514"},
		{"grant_delete_no_cascade", `DELETE FROM public.oauth_grant`, "23503"},
		{"grant_revocation_pair", `UPDATE public.oauth_grant SET revoked_at=now()`, "23514"},
		{"revoked_registration_unique", `INSERT INTO public.agent_registration SELECT '019a0000-0000-7000-8000-000000000005',account_id,client_id,registration_key,now(),NULL FROM public.agent_registration`, "23505"},
		{"registration_key", `UPDATE public.agent_registration SET registration_key='bad key'`, "23514"},
		{"request_key_short", `UPDATE public.idempotency_record SET idempotency_key=repeat('a',15)`, "23514"},
		{"request_key_long", `UPDATE public.idempotency_record SET idempotency_key=repeat('a',129)`, "23514"},
		{"request_key_non_ascii", `UPDATE public.idempotency_record SET idempotency_key=repeat('é',16)`, "23514"},
		{"fingerprint_length", `UPDATE public.idempotency_record SET request_fingerprint=decode(repeat('aa',31),'hex')`, "23514"},
		{"tool_name", `UPDATE public.idempotency_record SET tool_name='unknown'`, "23514"},
		{"null_result_reservation", `UPDATE public.idempotency_record SET tool_result=NULL`, ""},
		{"request_key_boundary", `UPDATE public.idempotency_record SET idempotency_key=repeat('Z',128)`, ""},
		{"account_scoped_key", `INSERT INTO public.idempotency_record SELECT '019a0000-0000-7000-8000-000000000006',idempotency_key,tool_name,request_fingerprint,tool_result,received_at,expires_at FROM public.idempotency_record`, ""},
		{"graph_version", `UPDATE public.context_graph SET version=0`, "23514"},
		{"content_revision", `UPDATE public.context_graph SET content_revision=0`, "23514"},
		{"stored_chars", `UPDATE public.context_graph SET stored_chars=-1`, "23514"},
		{"bigint_version", `UPDATE public.context_graph SET version=3000000000,content_revision=3000000000,stored_chars=3000000000`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			for _, q := range []string{grantSeed, codeSeed, refreshSeed, registrationSeed, idempotencySeed, graphSeed} {
				if _, err := tx.Exec(t.Context(), q); err != nil {
					t.Fatal(err)
				}
			}
			_, err = tx.Exec(t.Context(), tc.query)
			if tc.state == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			pgErr, ok := errors.AsType[*pgconn.PgError](err)
			if !ok || pgErr.Code != tc.state {
				t.Fatalf("got %v want SQLSTATE %s", err, tc.state)
			}
		})
	}
}

func TestTargetSchemaUpgradePreservesHistoryIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := targetConfig()
	old := fstest.MapFS{}
	for _, name := range []string{"001_init.sql", "009_remove_legacy_connection.sql"} {
		b, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		old[name] = &fstest.MapFile{Data: b}
	}
	if _, err := applyTarget(t.Context(), pool, old, cfg); err != nil {
		t.Fatal(err)
	}
	const seed = `INSERT INTO public.idempotency_record VALUES ('019a0000-0000-7000-8000-000000000002','019a0000-0000-7000-8000-000000000007','node_create',decode(repeat('ef',32),'hex'),'{"ok":true}','2026-10-15T00:00:00Z','2026-10-16T00:00:00Z')`
	if _, err := pool.Exec(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	var before, after string
	const history = `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM public.schema_migration s WHERE version IN (1,9)`
	if err := pool.QueryRow(t.Context(), history).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if got, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err != nil || got != 4 {
		t.Fatalf("upgrade=%d: %v", got, err)
	}
	if err := pool.QueryRow(t.Context(), history).Scan(&after); err != nil || before != after {
		t.Fatalf("이력 변경: %v", err)
	}
	var key, result, received, expires string
	if err := pool.QueryRow(t.Context(), `SELECT idempotency_key,tool_result::text,to_char(received_at,'YYYY-MM-DD HH24:MI:SS'),to_char(expires_at,'YYYY-MM-DD HH24:MI:SS') FROM public.idempotency_record`).Scan(&key, &result, &received, &expires); err != nil {
		t.Fatal(err)
	}
	if key != "019a0000-0000-7000-8000-000000000007" || result != `{"ok": true}` || received != "2026-10-15 00:00:00" || expires != "2026-10-16 00:00:00" {
		t.Fatalf("기존 결과 변경: %s %s %s %s", key, result, received, expires)
	}
}

func TestMigrationRollbackAndInvalidPreparationIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := targetConfig()
	cfg.GraphName = "bad';DROP TABLE x"
	if _, _, err := migrate.Prepare(t.Context(), pool, migrations.FS, ".", cfg); err == nil {
		t.Fatal("잘못된 치환 허용")
	}
	var exists bool
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass('public.schema_migration') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("잘못된 입력이 이력을 생성: %v", err)
	}
	cfg = targetConfig()
	files := fstest.MapFS{"001_fail.sql": {Data: []byte("CREATE TABLE public.data02_partial (id int); SELECT 1/0;")}}
	if _, err := applyTarget(t.Context(), pool, files, cfg); err == nil {
		t.Fatal("부분 실패가 성공")
	}
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass('public.data02_partial') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("부분 DDL 잔류: %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.schema_migration`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("실패 이력 잔류: %v", err)
	}
	if got, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err != nil || got != 6 {
		t.Fatalf("실패 후 재실행=%d: %v", got, err)
	}
	// 원본 checksum 불일치가 후속 DDL을 실행하지 못하게 한다.
	if _, err := pool.Exec(t.Context(), `UPDATE public.schema_migration SET checksum='tampered' WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err == nil {
		t.Fatal("checksum 변조 허용")
	}
}

func TestConcurrentMigrationSingleConnectionIntegration(t *testing.T) {
	_, address := isolatedMigrationDatabase(t)
	pools := []*pgxpool.Pool{singleConnectionPool(t, address), singleConnectionPool(t, address)}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	type result struct {
		count int
		err   error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, pool := range pools {
		wg.Go(func() { n, err := applyTarget(ctx, pool, migrations.FS, targetConfig()); results <- result{n, err} })
	}
	wg.Wait()
	close(results)
	var total int
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		total += r.count
	}
	if total != 6 {
		t.Fatalf("동시 적용 총수=%d", total)
	}
	for _, pool := range pools {
		if pool.Stat().AcquiredConns() != 0 {
			t.Fatal("연결 잔류")
		}
	}
	// 독립 세션의 실제 잠금 대기 취소 뒤에도 다시 획득할 수 있어야 한다.
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- migrate.WithLock(ctx, pools[0], func(context.Context, *pgxpool.Conn) error { close(entered); <-release; return nil })
	}()
	<-entered
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	err := migrate.WithLock(short, pools[1], func(context.Context, *pgxpool.Conn) error { return errors.New("진입하면 안 된다") })
	stop()
	close(release)
	if holderErr := <-done; holderErr != nil {
		t.Fatal(holderErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("대기 취소: %v", err)
	}
	if err := migrate.WithLock(ctx, pools[1], func(ctx context.Context, conn *pgxpool.Conn) error { _, err := conn.Exec(ctx, "SELECT 1"); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationInputAndFilenameBoundaries(t *testing.T) {
	for _, names := range [][]string{{"000_zero.sql"}, {"01_short.sql"}, {"001_first.sql", "001_other.sql"}} {
		files := fstest.MapFS{}
		for _, name := range names {
			files[name] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		}
		if _, err := migrate.Load(files, ".", targetConfig()); err == nil {
			t.Fatalf("파일명 허용: %v", names)
		}
	}
	for _, change := range []func(*migrate.Config){
		func(c *migrate.Config) { c.GraphName = strings.Repeat("a", 64) },
		func(c *migrate.Config) { c.ColdTablespace = strings.Repeat("a", 64) },
		func(c *migrate.Config) { c.VectorType = "bit" },
		func(c *migrate.Config) { c.VectorDim = 2001 },
		func(c *migrate.Config) { c.ColdTablespace = "cold;DROP TABLE x" },
	} {
		cfg := targetConfig()
		change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("허용하면 안 되는 구성: %+v", cfg)
		}
	}
	loaded, err := migrate.Load(fstest.MapFS{"001_init.sql": {Data: []byte("SELECT 1;")}}, ".", targetConfig())
	if err != nil {
		t.Fatal(err)
	}
	applied := map[int]migrate.Applied{1: {Version: 1, Name: "renamed", Checksum: loaded[0].Checksum}}
	if _, err := migrate.Pending(loaded, applied); err == nil {
		t.Fatal("적용 파일명 변경 허용")
	}
}

func TestTargetSchemaInvalidLegacyRowRollsBackIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	cfg := targetConfig()
	old := fstest.MapFS{}
	for _, name := range []string{"001_init.sql", "009_remove_legacy_connection.sql"} {
		b, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		old[name] = &fstest.MapFile{Data: b}
	}
	if _, err := applyTarget(t.Context(), pool, old, cfg); err != nil {
		t.Fatal(err)
	}
	// 001이 허용했던 잘못된 길이의 지문을 임의로 삭제·고치지 않아야 한다.
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.idempotency_record VALUES ('019a0000-0000-7000-8000-000000000002','019a0000-0000-7000-8000-000000000007','node_create',decode('ab','hex'),'{}',now(),now()+interval '24 hours')`); err != nil {
		t.Fatal(err)
	}
	if _, err := applyTarget(t.Context(), pool, migrations.FS, cfg); err == nil {
		t.Fatal("잘못된 과거 행을 허용")
	}
	var typ string
	if err := pool.QueryRow(t.Context(), `SELECT data_type FROM information_schema.columns WHERE table_schema='public' AND table_name='idempotency_record' AND column_name='idempotency_key'`).Scan(&typ); err != nil || typ != "uuid" {
		t.Fatalf("형변환이 롤백되지 않음: %s %v", typ, err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.schema_migration WHERE version=10`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("실패 이력 잔류: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT octet_length(request_fingerprint) FROM public.idempotency_record`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("과거 행 변경: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass('public.oauth_grant') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("후속 DDL 실행: %v", err)
	}
}

func TestMigrationCancelledApplyReleasesConnectionIntegration(t *testing.T) {
	_, address := isolatedMigrationDatabase(t)
	pool := singleConnectionPool(t, address)
	cfg := targetConfig()
	files := fstest.MapFS{"001_wait.sql": {Data: []byte("CREATE TABLE public.data02_cancelled (id int); SELECT pg_sleep(10);")}}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	_, err := applyTarget(ctx, pool, files, cfg)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("취소 원인: %v", err)
	}
	retry, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if _, err := applyTarget(retry, pool, migrations.FS, cfg); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(retry, `SELECT to_regclass('public.data02_cancelled') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("취소 DDL 잔류: %v", err)
	}
	if pool.Stat().AcquiredConns() != 0 {
		t.Fatal("취소 후 연결 잔류")
	}
}

func TestRetiredMigrationNumberCannotBeReused(t *testing.T) {
	files := fstest.MapFS{"002_reused.sql": {Data: []byte("SELECT 1;")}}
	loaded, err := migrate.Load(files, ".", targetConfig())
	if err != nil {
		t.Fatal(err)
	}
	applied := map[int]migrate.Applied{8: {Version: 8, Name: "retired", Checksum: "retired"}}
	if _, err := migrate.Pending(loaded, applied); err == nil {
		t.Fatal("과거 번호 재사용 허용")
	}
}
