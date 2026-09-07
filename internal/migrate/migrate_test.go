package migrate_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"agent_context_sharing/internal/migrate"
)

const (
	// waitLimit은 일어나야 하는 일을 기다리는 상한이다. 넉넉히 두어 느린 환경에서
	// 거짓 실패가 나지 않게 한다.
	waitLimit = 10 * time.Second
	// blockedWindow는 막혀 있어야 하는 일이 정말 막혔는지 보는 구간이다. 짧게 두어
	// 테스트가 느려지지 않게 하되, 잠금이 없으면 통과할 만큼은 준다.
	blockedWindow = 300 * time.Millisecond
)

// newTestPool은 데이터베이스가 필요한 테스트의 접속을 마련한다.
//
// SDD.md의 「데이터베이스가 필요한 테스트」가 정한 대로 TEST_DATABASE_URL이 없으면
// 건너뛴다. 문서만 고치는 작업에서도 go test ./...이 돌아야 하기 때문이다. 다만
// 건너뛰기만 두면 변수를 깜빡한 실행이 조용히 통과하므로, TEST_DATABASE_REQUIRED가
// 설정된 곳에서는 건너뛰지 않고 실패시킨다.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정되었으나 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 건너뛴다")
	}

	pool, err := migrate.NewPool(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("풀 생성: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// lockHeld는 실행기의 잠금 키가 지금 잡혀 있는지 본다.
func lockHeld(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()

	const query = `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND ((classid::bigint << 32) | objid::bigint) = $1`
	var count int
	if err := pool.QueryRow(t.Context(), query, migrate.AdvisoryLockKey).Scan(&count); err != nil {
		t.Fatalf("잠금 조회: %v", err)
	}
	return count > 0
}

// TestGraphNameBoundary는 마이그레이션이 config, store와 같은 AGE 그래프 이름 경계를
// 쓰는지 확인한다. 이 패키지의 테스트는 외부 패키지라 정규식 변수를 직접 볼 수 없으므로
// 공개 검증 함수의 수용·거부 결과로 같은 경계를 확인한다. 세 패키지가 어긋나면 한 계층만
// 통과하고 다른 계층에서 기동이 실패하므로 표본에 밑줄 시작 이름을 반드시 남긴다.
func TestGraphNameBoundary(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{name: "agent_context", valid: true},
		{name: "_agent_context", valid: true},
		{name: "1agent_context", valid: false},
		{name: "agent-context", valid: false},
		{name: "AgentContext", valid: false},
	}
	for _, test := range tests {
		cfg := migrate.Config{GraphName: test.name, VectorType: "vector", VectorDim: 1024}
		if got := cfg.Validate() == nil; got != test.valid {
			t.Errorf("그래프 이름 %q 허용 결과 = %t, want %t", test.name, got, test.valid)
		}
	}
}

// TestWithLockIsExclusive는 잠금이 실제로 배타적인지 확인한다.
//
// 하나가 잠근 동안 다른 하나가 진입하지 못하고, 앞이 끝난 뒤에 진입해 완료되며,
// 둘 다 끝나면 잠금이 남지 않는지 본다. 배타성이 성립하지 않으면 여러 인스턴스가
// 같은 마이그레이션을 함께 적용한다.
func TestWithLockIsExclusive(t *testing.T) {
	pool := newTestPool(t)

	acquired := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- migrate.WithLock(t.Context(), pool, func(context.Context) error {
			close(acquired)
			<-release
			return nil
		})
	}()

	select {
	case <-acquired:
	case err := <-holder:
		t.Fatalf("잠금을 잡기도 전에 끝났다: %v", err)
	case <-time.After(waitLimit):
		t.Fatal("잠금을 잡지 못했다")
	}

	entered := make(chan error, 1)
	go func() {
		entered <- migrate.WithLock(t.Context(), pool, func(context.Context) error {
			return nil
		})
	}()

	select {
	case err := <-entered:
		close(release)
		<-holder
		t.Fatalf("잠금이 배타적이지 않다. 두 번째 호출이 겹쳐 진입했다: %v", err)
	case <-time.After(blockedWindow):
	}

	close(release)
	if err := <-holder; err != nil {
		t.Fatalf("잠금을 잡은 호출: %v", err)
	}

	select {
	case err := <-entered:
		if err != nil {
			t.Fatalf("두 번째 호출: %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("앞의 잠금이 풀렸는데도 두 번째 호출이 진입하지 못했다")
	}

	if lockHeld(t, pool) {
		t.Fatal("두 호출이 끝났는데 잠금이 남아 있다")
	}
}

// TestWithLockReleasesOnError는 fn이 실패해도 잠금이 풀리는지 확인한다.
//
// 실패 경로에서 잠금이 남으면 그 뒤의 모든 기동이 막히므로 가장 위험한 결함이다.
func TestWithLockReleasesOnError(t *testing.T) {
	pool := newTestPool(t)

	want := errors.New("적용 실패")
	err := migrate.WithLock(t.Context(), pool, func(context.Context) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("fn의 오류가 그대로 올라와야 한다. got %v", err)
	}
	if lockHeld(t, pool) {
		t.Fatal("fn이 실패한 뒤 잠금이 남아 있다")
	}

	// 남지 않았다면 곧바로 다시 잡을 수 있어야 한다.
	if err := migrate.WithLock(t.Context(), pool, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("실패 뒤 재획득: %v", err)
	}
}
