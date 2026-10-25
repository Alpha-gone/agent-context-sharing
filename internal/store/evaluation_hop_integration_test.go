package store

import (
	"context"
	json "encoding/json/v2"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 전용 풀 소켓이 받은 전체 PostgreSQL 프로토콜 바이트다. SQL 반환 본문만의 크기가 아니다.
type evaluationCountedConn struct {
	net.Conn
	received *atomic.Int64
}

func (connection evaluationCountedConn) Read(buffer []byte) (int, error) {
	n, err := connection.Conn.Read(buffer)
	connection.received.Add(int64(n))
	return n, err
}

type evaluationHopRun struct {
	Implementation string  `json:"implementation"`
	Repeat         int     `json:"repeat"`
	LatencyMS      float64 `json:"latency_ms"`
	SQLCommands    int     `json:"sql_commands"`
	SQLRows        int64   `json:"sql_returned_rows"`
	ReceivedBytes  int64   `json:"protocol_received_bytes"`
	IdentifierRows int     `json:"identifier_rows"`
	BodyRows       int     `json:"body_rows"`
	BodyBytes      int     `json:"body_bytes"`
	EdgeRows       int     `json:"edge_rows"`
	ReferenceRows  int     `json:"reference_rows"`
}

// SEARCH-10의 시험 전용 후보를 재사용하되 EVAL-06은 새 표본과 새 실행 증거를 만든다.
// 모든 후보의 동등성을 먼저 통과한 뒤에만 회차별 순서를 순환하며 비용을 측정한다.
func TestEVAL06HopControlledComparisonIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	config := database.pool.Config()
	database.pool.Close()
	config.ConnConfig.Tracer = search10Tracer{}
	var received atomic.Int64
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return evaluationCountedConn{Conn: connection, received: &received}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	database.pool = pool
	for _, fixture := range []string{"sparse100", "deleted-proposed-multi", "dense16"} {
		t.Run(fixture, func(t *testing.T) {
			sample := search10Fixture(t, database, fixture)
			implementations := []string{"full", "bounded", "vle"}
			baseline, err := runSearch10Hop(t.Context(), database, sample, implementations[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range implementations[1:] {
				candidate, err := runSearch10Hop(t.Context(), database, sample, name)
				if err != nil {
					t.Fatal(err)
				}
				compareHopResults(t, name, baseline, candidate)
			}
			var runs []evaluationHopRun
			for repeat := 1; repeat <= 10; repeat++ {
				for offset := range implementations {
					name := implementations[(offset+repeat-1)%len(implementations)]
					var trace search10Trace
					before, started := received.Load(), time.Now()
					result, err := runSearch10Hop(context.WithValue(t.Context(), search10TraceKey{}, &trace), database, sample, name)
					elapsed, bytes := time.Since(started), received.Load()-before
					if err != nil {
						t.Fatal(err)
					}
					compareHopResults(t, name, baseline, result)
					entry := evaluationHopRun{Implementation: name, Repeat: repeat, LatencyMS: float64(elapsed.Nanoseconds()) / 1e6,
						SQLCommands: len(trace), ReceivedBytes: bytes, IdentifierRows: result.work.identifierRows,
						BodyRows: result.work.bodyRows, BodyBytes: result.work.bodyBytes, EdgeRows: result.work.edgeRows, ReferenceRows: result.work.referenceRows}
					for _, query := range trace {
						entry.SQLRows += query.rows
					}
					if name == "vle" {
						entry.IdentifierRows, entry.BodyRows, entry.BodyBytes, entry.EdgeRows = -1, -1, -1, -1
					}
					if entry.SQLCommands == 0 || entry.SQLRows == 0 || entry.ReceivedBytes <= 0 {
						t.Fatalf("홉 계측 누락: %+v", entry)
					}
					runs = append(runs, entry)
				}
			}
			encoded, err := json.Marshal(runs, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("EVAL06_HOP %s %s", fixture, encoded)
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}
