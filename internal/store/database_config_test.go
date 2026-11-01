package store

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPoolConfigUsesParsedMaximumKey(t *testing.T) {
	base := "postgres://user:password@localhost/db?sslmode=disable"
	defaults, err := pgxpool.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name, raw string
		maximum   int32
	}{
		{"default", base, max(defaults.MaxConns, minPoolConns)},
		{"password_substring", "postgres://user:pool_max_conns@localhost/db?sslmode=disable", max(defaults.MaxConns, minPoolConns)},
		{"value_substring", base + "&application_name=pool_max_conns", max(defaults.MaxConns, minPoolConns)},
		{"other_key", base + "&pool_max_conns_suffix=4", max(defaults.MaxConns, minPoolConns)},
		{"explicit", base + "&pool_max_conns=4", 4},
		{"encoded_key", base + "&%70ool_max_conns=4", 4},
		{"last_encoded_key", base + "&pool_max_conns=8&%70ool_max_conns=4", 4},
		{"last_valid", base + "&pool_max_conns=invalid&%70ool_max_conns=4", 4},
		{"keyword_value", "host=localhost user=user password=pool_max_conns dbname=db sslmode=disable pool_max_conns=4", 4},
	} {
		t.Run(sample.name, func(t *testing.T) {
			cfg, err := parsePoolConfig(sample.raw)
			if err != nil || cfg.MaxConns != sample.maximum {
				t.Fatalf("풀 상한: config=%v err=%v", cfg != nil, err)
			}
			if _, leaked := cfg.ConnConfig.RuntimeParams["pool_max_conns"]; leaked {
				t.Fatal("풀 전용 키가 PostgreSQL 시작 파라미터에 남았다")
			}
		})
	}
}

func TestDatabaseURLPoolIntegration(t *testing.T) {
	isolated := newIsolatedIntegrationStore(t)
	raw := integrationDatabaseURL(t)
	separator := "?"
	if strings.Contains(raw, "?") {
		separator = "&"
	}
	// 마지막 dbname으로 시험 전용 DB를 선택하여 기존 DB에 연결하지 않는다.
	raw += separator + "dbname=" + isolated.pool.Config().ConnConfig.Database + "&%70ool_max_conns=4&application_name=acs+281"
	if err := ValidateDatabaseURL(raw); err != nil {
		t.Fatal("실제 DB URL 구성 검증 실패")
	}
	database, err := New(t.Context(), raw, isolated.graphName, nil, nil, "")
	if err != nil {
		t.Fatal("시험 전용 저장소를 준비하지 못했다")
	}
	t.Cleanup(database.Close)
	if database.pool.Stat().MaxConns() != 4 {
		t.Fatal("인코딩된 명시 풀 상한 미적용")
	}
	var actualDB, app string
	if err := database.pool.QueryRow(t.Context(), "SELECT current_database(), current_setting('application_name')").Scan(&actualDB, &app); err != nil {
		t.Fatal("시험 전용 연결 조회 실패")
	}
	if actualDB != isolated.pool.Config().ConnConfig.Database || app != "acs+281" {
		t.Fatal("실제 DB/query 해석 불일치")
	}
}
