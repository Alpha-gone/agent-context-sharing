package store

import (
	"fmt"
	"testing"

	"agent_context_sharing/internal/model"
)

// BenchmarkAuditGraphIntegration은 「그래프 불변식 규칙과 감사」가 정기 주기를 정하기 전에
// 재라고 한 그래프 크기별 전체 감사 시간과, 쓰기 한 번이 더하는 영향 주변부 검사 시간을 잰다.
// 표본은 홉 탐색 비교와 같은 사슬 그래프이며 모든 컨텍스트가 임베딩을 가진다. 전체 감사의
// 데이터베이스 질의는 간선·정점·임베딩 세 번과, 다른 그래프 끝이 있을 때 한 번이다.
//
//	TEST_DATABASE_URL=... go test -run '^$' -bench AuditGraph -benchtime 10x ./internal/store
func BenchmarkAuditGraphIntegration(b *testing.B) {
	database := newIntegrationStore(b)
	for _, groups := range []int{25, 100, 400} {
		graphID, starts := createHopChain(b, database, groups)
		dimension := embeddingDimension(b, database)
		processor := scopeTestProcessor(dimension)
		for {
			result, err := database.ProcessNextIndexTaskInGraph(b.Context(), graphID, processor)
			if err != nil {
				b.Fatalf("표본 색인: %v", err)
			}
			if !result.Found {
				break
			}
		}
		expect := EmbeddingExpectation{ModelID: "scope-test", Dimension: dimension}
		b.Run(fmt.Sprintf("nodes=%d/full", groups*4), func(b *testing.B) {
			var violations []InvariantViolation
			for b.Loop() {
				var err error
				if violations, err = database.AuditGraph(b.Context(), graphID, expect); err != nil {
					b.Fatalf("전체 감사: %v", err)
				}
			}
			b.ReportMetric(float64(len(violations)), "violations/op")
		})
		b.Run(fmt.Sprintf("nodes=%d/write", groups*4), func(b *testing.B) {
			scope := []model.ID{starts[1].ID}
			for b.Loop() {
				tx, err := database.pool.Begin(b.Context())
				if err != nil {
					b.Fatalf("쓰기 검사 트랜잭션: %v", err)
				}
				if err := database.checkWriteInvariants(b.Context(), tx, graphID, scope, expect); err != nil {
					b.Fatalf("쓰기 검사: %v", err)
				}
				_ = tx.Rollback(b.Context())
			}
		})
	}
}
