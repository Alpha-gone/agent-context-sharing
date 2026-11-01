package authz

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/store"
)

type measuredRevocationStore struct {
	*store.Store
	single, bulk atomic.Int64
}

func (s *measuredRevocationStore) IsTokenRevoked(ctx context.Context, id string, now time.Time) (bool, error) {
	s.single.Add(1)
	return s.Store.IsTokenRevoked(ctx, id, now)
}
func (s *measuredRevocationStore) RevokedTokenIDs(ctx context.Context, now time.Time) ([]string, error) {
	s.bulk.Add(1)
	return s.Store.RevokedTokenIDs(ctx, now)
}

func TestWebRevocationMillionRowsIntegration(t *testing.T) {
	firstStore, _, pool := newAuthenticationContractDatabase(t)
	backend := &measuredRevocationStore{Store: firstStore}
	first := testService(t, backend)
	now := time.Now().UTC().Truncate(time.Second)
	first.now = func() time.Time { return now }
	account, err := first.Register(t.Context(), "revocation_scale", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	token, err := first.WebSession(t.Context(), account, "web")
	if err != nil {
		t.Fatal(err)
	}
	revokedToken, err := first.WebSession(t.Context(), account, "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{0, 1000000} {
		if count > 0 {
			_, err := pool.Exec(t.Context(), `INSERT INTO public.revoked_token(token_id,expires_at) SELECT CASE WHEN i=1 THEN $3 ELSE 'synthetic-revoked-'||i::text END,$1 FROM generate_series(1,$2::integer) i`, now.Add(12*time.Hour), count, revokedToken.ID)
			if err != nil {
				t.Fatal("합성 폐기 행 준비 실패", err)
			}
			if _, err := pool.Exec(t.Context(), "ANALYZE public.revoked_token"); err != nil {
				t.Fatal(err)
			}
		}
		verifier := testService(t, backend)
		verifier.now = first.now
		beforeSingle, beforeBulk := backend.single.Load(), backend.bulk.Load()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		start := time.Now()
		actual, _, err := verifier.Verify(ctx, token.Raw, "web")
		elapsed := time.Since(start)
		cancel()
		t.Logf("rows=%d verify=%v cached_ids=%d single_reads=%d bulk_reads=%d", count, elapsed, len(verifier.cache.revoked), backend.single.Load()-beforeSingle, backend.bulk.Load()-beforeBulk)
		if err != nil || actual != account || elapsed >= 5*time.Second {
			t.Fatalf("대량 폐기 행의 정상 세션 판정: %v", err)
		}
		if len(verifier.cache.revoked) > 4096 || backend.bulk.Load() != beforeBulk || backend.single.Load()-beforeSingle != 1 {
			t.Error("폐기 전체 목록 적재 또는 캐시 상한 위반")
		}
		if count == 1000000 {
			start := time.Now()
			_, _, err := verifier.Verify(t.Context(), revokedToken.Raw, "web")
			elapsed := time.Since(start)
			t.Logf("revoked_rows=%d rejection=%v cached_ids=%d", count, elapsed, len(verifier.cache.revoked))
			if !errors.Is(err, ErrInvalidCredential) || elapsed >= 5*time.Second || len(verifier.cache.revoked) > 4096 {
				t.Fatal("대량 폐기 토큰의 거부/캐시 상한 위반", err)
			}
			rows, err := pool.Query(t.Context(), `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF) SELECT EXISTS (SELECT 1 FROM public.revoked_token WHERE token_id=$1 AND expires_at>$2)`, revokedToken.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			var plan strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				plan.WriteString(line)
				plan.WriteByte('\n')
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			t.Log(plan.String())
			if !strings.Contains(plan.String(), "revoked_token_pkey") || strings.Contains(plan.String(), "Seq Scan") {
				t.Error("폐기 PK 단건 조회 미사용")
			}
		}
	}
}
