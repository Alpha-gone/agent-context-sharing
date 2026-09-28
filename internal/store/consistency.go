package store

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// 요청 단위 일관 읽기의 동기화 스냅숏 후보다. 조정 연결이 읽기 전용 REPEATABLE READ
// 트랜잭션에서 스냅숏을 내보내고, 검색이 부르는 읽기 메서드는 요청 컨텍스트에 실린 그
// 스냅숏을 자기 읽기 전용 트랜잭션에서 가져와 같은 커밋 상태를 읽는다. 표식이 없는 요청은
// 지금처럼 풀에서 바로 읽으므로 현행 검색의 동작은 바뀌지 않는다.

// ReadStats는 한 요청이 읽기 경계에서 쓴 연결 자원이다.
type ReadStats struct {
	// Connections 필드에는 조정 연결을 포함해 경계 안에서 연 트랜잭션 수를 둔다.
	Connections int
	// PeakConnections 필드에는 동시에 열려 있던 트랜잭션 수의 최댓값을 둔다.
	PeakConnections int
	// Hold 필드에는 조정 연결이 스냅숏을 붙잡고 있던 시간을 둔다.
	Hold time.Duration
}

// readScope는 요청 컨텍스트에 싣는 스냅숏 표식과 연결 사용량이다.
type readScope struct {
	snapshot string
	// reads는 경계 안에서 동시에 열 수 있는 읽기 트랜잭션의 자리다. 조정 연결과 합쳐
	// 예약한 snapshotConnections를 넘지 않게 하며, 넘는 읽기는 앞선 읽기가 끝나기를 기다린다.
	reads  chan struct{}
	mu     sync.Mutex
	opened int
	active int
	peak   int
}

func (scope *readScope) acquire() {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.opened++
	scope.active++
	scope.peak = max(scope.peak, scope.active)
}

func (scope *readScope) release() {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.active--
}

type readScopeKey struct{}

type readTxKey struct{}

// snapshotPattern은 pg_export_snapshot이 돌려주는 식별자 모양이다. SET TRANSACTION
// SNAPSHOT은 매개변수를 받지 않으므로 리터럴로 넣기 전에 모양을 확인한다.
var snapshotPattern = regexp.MustCompile(`^[0-9A-F]+-[0-9A-F]+(-[0-9]+)?$`)

// ReadSnapshot은 한 요청의 조정 트랜잭션이다. Close 전까지 스냅숏이 유효하다.
type ReadSnapshot struct {
	tx      pgx.Tx
	scope   *readScope
	started time.Time
	// release는 BeginReadSnapshot이 받은 연결 예약을 반납한다.
	release func()
}

// BeginReadSnapshot은 연결을 예약한 뒤 조정 연결에서 읽기 전용 REPEATABLE READ 트랜잭션을
// 열고 스냅숏을 내보낸다. 호출자는 모든 읽기가 끝난 뒤 Close를 불러야 한다.
//
// 조정 연결을 쥔 채 경계 안 읽기가 연결을 더 기다리므로 「데이터베이스 연결」대로 요청의
// 최대 점유를 먼저 예약한다. 예약 없이 풀 상한만큼의 요청이 조정 연결을 잡으면 모두가
// 서로의 읽기 연결을 기다려 멈춘다.
func (s *Store) BeginReadSnapshot(ctx context.Context) (*ReadSnapshot, error) {
	release, err := s.reserveConnections(ctx, snapshotConnections)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		release()
		return nil, fmt.Errorf("스냅숏 조정 트랜잭션 시작: %w", err)
	}
	var snapshot string
	if err := tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&snapshot); err != nil {
		_ = tx.Rollback(ctx)
		release()
		return nil, fmt.Errorf("스냅숏 내보내기: %w", err)
	}
	if !snapshotPattern.MatchString(snapshot) {
		_ = tx.Rollback(ctx)
		release()
		return nil, fmt.Errorf("스냅숏 식별자 %q의 모양이 예상과 다르다", snapshot)
	}
	scope := &readScope{snapshot: snapshot, reads: make(chan struct{}, snapshotConnections-1)}
	scope.acquire()
	return &ReadSnapshot{tx: tx, scope: scope, started: time.Now(), release: release}, nil
}

// Context는 이 스냅숏을 읽기 메서드에 전달하는 요청 컨텍스트를 만든다.
func (snapshot *ReadSnapshot) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, readScopeKey{}, snapshot.scope)
}

// Close는 조정 트랜잭션을 끝내고 요청이 쓴 연결 자원을 돌려준다.
func (snapshot *ReadSnapshot) Close(ctx context.Context) ReadStats {
	_ = snapshot.tx.Rollback(ctx)
	snapshot.release()
	snapshot.scope.release()
	snapshot.scope.mu.Lock()
	defer snapshot.scope.mu.Unlock()
	return ReadStats{Connections: snapshot.scope.opened, PeakConnections: snapshot.scope.peak, Hold: time.Since(snapshot.started)}
}

// enterReadScope는 요청 컨텍스트에 스냅숏이 있으면 읽기 전용 REPEATABLE READ 트랜잭션을
// 열고 그 스냅숏을 가져온다. 돌려준 컨텍스트로 부른 reader가 이 트랜잭션을 쓴다. 이미
// 트랜잭션 안이거나 스냅숏이 없으면 아무것도 하지 않는다. release는 항상 불러야 한다.
//
// 경계 안 읽기 자리가 모두 차 있으면 연결을 잡기 전에 앞선 읽기가 끝나기를 기다린다.
// 같은 요청의 읽기만 자리를 돌려주므로 이 대기는 다른 요청을 기다리지 않는다.
func (s *Store) enterReadScope(ctx context.Context) (context.Context, func(), error) {
	scope, _ := ctx.Value(readScopeKey{}).(*readScope)
	if scope == nil || ctx.Value(readTxKey{}) != nil {
		return ctx, func() {}, nil
	}
	select {
	case scope.reads <- struct{}{}:
	case <-ctx.Done():
		return ctx, func() {}, fmt.Errorf("스냅숏 읽기 자리 대기: %w", context.Cause(ctx))
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		<-scope.reads
		return ctx, func() {}, fmt.Errorf("스냅숏 읽기 트랜잭션 시작: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET TRANSACTION SNAPSHOT '"+scope.snapshot+"'"); err != nil {
		_ = tx.Rollback(ctx)
		<-scope.reads
		return ctx, func() {}, fmt.Errorf("스냅숏 가져오기: %w", err)
	}
	scope.acquire()
	return context.WithValue(ctx, readTxKey{}, tx), func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		scope.release()
		<-scope.reads
	}, nil
}

// reader는 읽기 경계 안이면 그 트랜잭션을, 아니면 연결 풀을 돌려준다.
func (s *Store) reader(ctx context.Context) cypherQueryer {
	if tx, ok := ctx.Value(readTxKey{}).(pgx.Tx); ok {
		return tx
	}
	return s.pool
}

// bumpContentRevision은 검색에 공개되는 상태를 바꾼 트랜잭션 안에서 내용 판을 올린다.
// 운영 검색은 이 값을 읽지 않는다. 「요청 단위 일관 읽기 구현 비교」가 내용 판 재시도를
// 채택하지 않았지만, 증가 지점은 「테이블 열 정의」의 계약이라 유지한다.
func bumpContentRevision(ctx context.Context, tx pgx.Tx, graphID model.ID) error {
	if _, err := tx.Exec(ctx, `UPDATE public.context_graph SET content_revision = content_revision + 1 WHERE graph_id = $1`, graphID.String()); err != nil {
		return fmt.Errorf("내용 판 증가: %w", err)
	}
	return nil
}

// PoolStats는 연결 풀의 누적 사용량이다. 평가 실행기가 동시 요청 부하에서 동기화 스냅숏이
// 풀을 포화시키는지 잴 때 쓴다.
type PoolStats struct {
	// MaxConns 필드에는 풀의 연결 수 상한을 둔다.
	MaxConns int32
	// EmptyAcquires 필드에는 쉬는 연결이 없어 새 연결을 기다려야 했던 획득 수를 둔다.
	EmptyAcquires int64
	// AcquireWait 필드에는 연결 획득에 걸린 누적 시간을 둔다.
	AcquireWait time.Duration
}

// PoolStats는 연결 풀의 현재 누적 사용량을 읽는다.
func (s *Store) PoolStats() PoolStats {
	stat := s.pool.Stat()
	return PoolStats{MaxConns: stat.MaxConns(), EmptyAcquires: stat.EmptyAcquireCount(), AcquireWait: stat.AcquireDuration()}
}
