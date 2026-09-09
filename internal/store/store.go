// Package store는 데이터베이스 연결과 이후 단계의 단일 데이터 접근 계층을 소유한다.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound는 지정한 그래프 안에서 대상을 찾지 못했음을 나타낸다.
var ErrNotFound = errors.New("대상을 찾지 못했다")

// ErrActiveSigningKeyExists는 이미 활성 서명 키가 있을 때 새 활성 키를 만들려 했음을 나타낸다.
var ErrActiveSigningKeyExists = errors.New("활성 서명 키가 이미 있다")

// VersionConflictError는 낙관적 잠금 비교에 실패했을 때 현재 판 번호를 담는다.
type VersionConflictError struct {
	// Current 필드는 저장된 대상의 현재 판 번호다.
	Current int64
}

// Error는 오류를 사람이 읽을 수 있는 형태로 만든다.
func (error VersionConflictError) Error() string {
	return fmt.Sprintf("판 번호가 일치하지 않는다: 현재 판 %d", error.Current)
}

// graphNamePattern은 AGE 그래프 이름을 SQL 식별자로 안전하게 사용할 수 있는지 확인한다.
var graphNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Store는 애플리케이션이 소유하는 단일 데이터 접근 계층이다.
type Store struct {
	// pool 필드는 패키지 밖에 노출하지 않는 pgx 연결 풀이다.
	pool *pgxpool.Pool
	// graphName 필드는 모든 AGE openCypher 호출이 공유하는 물리 그래프 이름이다.
	graphName string
}

// New는 AGE 준비를 마친 연결만 담는 애플리케이션 풀을 만든다.
func New(ctx context.Context, databaseURL, graphName string) (*Store, error) {
	if !graphNamePattern.MatchString(graphName) {
		return nil, fmt.Errorf("AGE 그래프 이름 %q가 영문 소문자, 숫자와 밑줄 형식이 아니다", graphName)
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("접속 문자열 해석: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		for _, statement := range []string{"LOAD 'age'", `SET search_path = ag_catalog, "$user", public`} {
			if _, err := conn.Exec(ctx, statement); err != nil {
				return fmt.Errorf("연결 준비 %q: %w", statement, err)
			}
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("연결 풀 생성: %w", err)
	}
	return &Store{pool: pool, graphName: graphName}, nil
}

// Ping은 데이터베이스 연결과 AGE 준비가 현재 요청을 받을 수 있는지 확인한다.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("연결 풀이 초기화되지 않았다")
	}
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("데이터베이스 준비 확인: %w", err)
	}
	return nil
}

// Close는 새 연결을 만들지 않게 하고 빌려간 연결이 끝난 뒤 풀을 닫는다.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}
