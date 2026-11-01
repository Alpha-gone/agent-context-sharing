package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatabaseURLMatchesPGX(t *testing.T) {
	for _, sample := range []struct{ name, raw string }{
		{"raw_at", "postgres://user:synthetic-secret@part@localhost/db?sslmode=disable"},
		{"escaped_at", "postgres://user:synthetic-secret%40part@localhost/db?sslmode=disable"},
		{"nul_query", "postgres://user:synthetic-secret@localhost/db?application_name=%00&sslmode=disable"},
		{"nul_password", "postgres://user:synthetic-secret%00@localhost/db?sslmode=disable"},
		{"repeated_last_valid", "postgres://user:synthetic-secret@localhost/db?pool_max_conns=invalid&pool_max_conns=4&sslmode=disable"},
		{"repeated_last_invalid", "postgres://user:synthetic-secret@localhost/db?pool_max_conns=4&pool_max_conns=synthetic-secret&sslmode=disable"},
		{"literal_plus", "postgres://user:synthetic-secret@localhost/db?pool_max_conns=+4&sslmode=disable"},
		{"encoded_plus", "postgres://user:synthetic-secret@localhost/db?pool_max_conns=%2B4&sslmode=disable"},
		{"runtime_plus", "postgres://user:synthetic-secret@localhost/db?application_name=a+b&sslmode=disable"},
		{"default_host", "postgres:///db?user=user&password=synthetic-secret&sslmode=disable"},
		{"multiple_hosts", "postgres://user:synthetic-secret@localhost:5432,127.0.0.1:5433/db?sslmode=disable"},
		{"literal_hash", "postgres://user:synthetic-secret@localhost/db#name?sslmode=disable"},
		{"bad_escape", "postgres://user:synthetic-secret%@localhost/db?sslmode=disable"},
		{"bad_pool_duration", "postgres://user:synthetic-secret@localhost/db?pool_max_conn_lifetime=synthetic-secret&sslmode=disable"},
		{"bad_tls_mode", "postgres://user:synthetic-secret@localhost/db?sslmode=synthetic-secret"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			expected, parseErr := pgxpool.ParseConfig(sample.raw)
			values := validValues()
			values["DATABASE_URL"] = sample.raw
			actual, err := loadValues(values)
			if (err == nil) != (parseErr == nil) {
				t.Fatalf("pgx/config 수락 불일치: pgx_accept=%v config_accept=%v", parseErr == nil, err == nil)
			}
			if err != nil {
				field, ok := errors.AsType[*Error](err)
				if !ok || field.Key != "DATABASE_URL" || errors.Unwrap(field) != nil || strings.Contains(fmt.Sprintf("%+v", err), "synthetic-secret") {
					t.Fatal("DATABASE_URL 오류 경계 위반")
				}
				return
			}
			if actual.DatabaseURL != sample.raw {
				t.Fatal("접속 원문 변경")
			}
			parsed, err := pgxpool.ParseConfig(actual.DatabaseURL)
			if err != nil || parsed.ConnConfig.Host != expected.ConnConfig.Host || parsed.ConnConfig.Password != expected.ConnConfig.Password || parsed.MaxConns != expected.MaxConns || parsed.ConnConfig.RuntimeParams["application_name"] != expected.ConnConfig.RuntimeParams["application_name"] {
				t.Fatal("실제 접속 해석 변경")
			}
			if sample.name == "raw_at" && parsed.ConnConfig.Host != "part@localhost" {
				t.Fatal("첫 @ 의미 소실")
			}
			if sample.name == "escaped_at" && parsed.ConnConfig.Password != "synthetic-secret@part" {
				t.Fatal("인코딩된 비밀번호 해석")
			}
			if sample.name == "runtime_plus" && parsed.ConnConfig.RuntimeParams["application_name"] != "a+b" {
				t.Fatal("+ 문자 소실")
			}
			if strings.Contains(sample.name, "last_valid") && parsed.MaxConns != 4 {
				t.Fatal("마지막 값 미적용")
			}
		})
	}
}
