// Package store는 데이터베이스 연결과 이후 단계의 단일 데이터 접근 계층을 소유한다.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store는 애플리케이션이 소유하는 데이터베이스 연결 풀이다. 그래프와 관계형 질의는
// 2단계에서 이 타입에 추가하며, 이 단계에서는 준비 확인과 종료에 필요한 경계만 연다.
type Store struct {
	pool *pgxpool.Pool
}

// New는 AGE 준비를 마친 연결만 담는 애플리케이션 풀을 만든다.
func New(ctx context.Context, databaseURL string) (*Store, error) {
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
	return &Store{pool: pool}, nil
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
