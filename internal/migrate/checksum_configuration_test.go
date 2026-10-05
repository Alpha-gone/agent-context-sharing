package migrate_test

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"agent_context_sharing/internal/migrate"
)

// TestAllRecordedRenderValuesPreserveChecksums은 치환 값 변경과 파일 변조를 구분한다.
// 새 파일은 현재 구성으로 렌더링하되 이미 적용한 파일은 당시 구성으로 검증해야 한다.
func TestAllRecordedRenderValuesPreserveChecksums(t *testing.T) {
	const sql = "SELECT '{{.GraphName}} {{.VectorType}} {{.VectorDim}} {{.ColdTablespaceClause}} {{.IndexTargetLayers}}';"
	original := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 1024, IndexTargetLayers: "all_layers"}
	for name, change := range map[string]func(*migrate.Config){
		"GraphName":            func(c *migrate.Config) { c.GraphName = "other_graph" },
		"VectorType":           func(c *migrate.Config) { c.VectorType = "halfvec" },
		"VectorDim":            func(c *migrate.Config) { c.VectorDim = 768 },
		"ColdTablespaceClause": func(c *migrate.Config) { c.ColdTablespace = "cold_storage" },
		"IndexTargetLayers":    func(c *migrate.Config) { c.IndexTargetLayers = "without_source" },
	} {
		t.Run(name, func(t *testing.T) {
			files := fstest.MapFS{"001_init.sql": {Data: []byte(sql)}, "002_next.sql": {Data: []byte(sql)}}
			old, err := migrate.Load(files, ".", original)
			if err != nil {
				t.Fatal(err)
			}
			applied := map[int]migrate.Applied{1: {Version: 1, Name: old[0].Name, Checksum: old[0].Checksum, Config: new(original)}}
			target := original
			change(&target)
			loaded, err := migrate.LoadRecorded(files, ".", target, applied)
			if err != nil {
				t.Fatal(err)
			}
			if loaded[0].Checksum != old[0].Checksum || loaded[0].Rendered != old[0].Rendered || loaded[1].Checksum == old[1].Checksum || loaded[1].Config != target {
				t.Fatal("과거 구성 보존 또는 미적용 파일의 현재 구성 치환 실패")
			}
			pending, err := migrate.Pending(loaded, applied)
			if err != nil || len(pending) != 1 || pending[0].Version != 2 {
				t.Fatalf("미적용 목록: %v, %v", pending, err)
			}
			files["001_init.sql"] = &fstest.MapFile{Data: []byte(sql + " -- 원본 변경")}
			if _, err := migrate.LoadRecorded(files, ".", target, applied); err == nil {
				t.Fatal("적용된 원본의 주석 변경이 허용됐다")
			}
		})
	}
}

// TestPrepareConfigurationChangeKeepsAppliedHistoryIntegration은 실제 001의 과거 치환
// 구성과 체크섬·적용 시각을 보존하고, 미적용 007만 현재 구성으로 준비하는지 확인한다.
func TestPrepareConfigurationChangeKeepsAppliedHistoryIntegration(t *testing.T) {
	pool, _ := isolatedMigrationDatabase(t)
	files := make(fstest.MapFS)
	for _, name := range []string{"001_init.sql", "007_embedding_dimensions.sql"} {
		data, err := readMigration(name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: data}
	}
	original := migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 1024, IndexTargetLayers: "all_layers"}
	old, err := migrate.Load(files, ".", original)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(t.Context(), pool, old[0]); err != nil {
		t.Fatal(err)
	}
	var beforeChecksum, beforeConfig string
	var beforeTime time.Time
	const history = `SELECT checksum, render_config::text, applied_at FROM public.schema_migration WHERE version=1`
	if err := pool.QueryRow(t.Context(), history).Scan(&beforeChecksum, &beforeConfig, &beforeTime); err != nil {
		t.Fatal(err)
	}
	target := original
	target.VectorType, target.VectorDim, target.ColdTablespace, target.IndexTargetLayers = "halfvec", 768, "new_cold_storage", "without_source"
	// Prepare는 아직 존재하지 않는 tablespace를 만들거나 과거 파일을 재적용하지 않는다.
	loaded, applied, err := migrate.Prepare(t.Context(), pool, files, ".", target)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := migrate.Pending(loaded, applied)
	if err != nil || len(pending) != 1 || pending[0].Version != 7 || !strings.Contains(pending[0].Rendered, "halfvec(768)") {
		t.Fatalf("현재 구성 준비: %v", err)
	}
	var afterChecksum, afterConfig string
	var afterTime time.Time
	if err := pool.QueryRow(t.Context(), history).Scan(&afterChecksum, &afterConfig, &afterTime); err != nil {
		t.Fatal(err)
	}
	if beforeChecksum != afterChecksum || beforeConfig != afterConfig || !beforeTime.Equal(afterTime) {
		t.Fatal("과거 이력을 다시 썼다")
	}
	files["001_init.sql"] = &fstest.MapFile{Data: append([]byte("-- 원본 변조\n"), files["001_init.sql"].Data...)}
	if _, _, err := migrate.Prepare(t.Context(), pool, files, ".", target); err == nil {
		t.Fatal("실제 DB 이력이 원본 변조를 허용했다")
	}
}
