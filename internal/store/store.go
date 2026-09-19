// Package store는 데이터베이스 연결과 이후 단계의 단일 데이터 접근 계층을 소유한다.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound는 지정한 그래프 안에서 대상을 찾지 못했음을 나타낸다.
var ErrNotFound = errors.New("대상을 찾지 못했다")

// ErrInvalidState는 현재 상태에서 허용되지 않는 전이를 요청했음을 나타낸다.
var ErrInvalidState = errors.New("현재 상태에서 허용되지 않는 연산이다")

// ErrInvalidRelation은 관계의 양 끝, 시간 또는 순환 제약을 위반했음을 나타낸다.
var ErrInvalidRelation = errors.New("관계 제약을 위반했다")

// ErrActiveSigningKeyExists는 이미 활성 서명 키가 있을 때 새 활성 키를 만들려 했음을 나타낸다.
var ErrActiveSigningKeyExists = errors.New("활성 서명 키가 이미 있다")

// ErrLastOwner는 그래프의 마지막 소유자 등급을 회수하려 했음을 나타낸다.
var ErrLastOwner = errors.New("그래프에는 소유자 등급이 최소 하나 필요하다")

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
	// relationProposals 필드는 사건 저장 뒤에 실행할 자동 후보 제안의 배포 구성이다.
	// 설정하지 않은 저장소는 후보를 만들지 않아 기존 도구·단위 테스트의 저장 의미를 보존한다.
	relationProposals *RelationProposalConfig
	// graceDays 필드는 유예를 시작할 때 생성 계정 플랜의 유예 일수를 돌려준다. nil이면
	// 만료 시각을 남기지 않아 유예가 만료되지 않는다.
	graceDays RetentionDays
}

// WriteLimits는 저장 트랜잭션 안에서 강제할 누적 한도다. 0은 한도 없음이다.
//
// 접근 계층이 트랜잭션 밖에서 읽은 값으로만 판정하면 한도 직전의 그래프에 생성이 동시에
// 와도 둘 다 통과한다. 「계정 플랜 값」의 누적 단위 한도는 값을 늘리는 쓰기와 같은
// 트랜잭션에서 판정되어야 하므로 한도를 여기까지 넘긴다.
type WriteLimits struct {
	// StoredCharsPerGraph는 그래프 하나가 담을 수 있는 최대 문자 수다.
	StoredCharsPerGraph int64
	// GraphsPerAccount는 한 계정이 소유자 등급으로 가질 수 있는 최대 활성 그래프 수다.
	GraphsPerAccount int64
	// WritesPerMinute는 1분 고정 창에서 허용할 쓰기 요청 수다.
	WritesPerMinute int64
	// ActorID는 요청 빈도를 셀 계정이다. 비어 있으면 빈도를 세지 않는다.
	ActorID model.ID
}

// RelationProposalConfig는 사건 관계 후보 제안에 쓰는 검증된 배포 구성이다.
type RelationProposalConfig struct {
	// AdjacencyWindow는 precedes 후보로 허용할 두 사건 사이 최대 간격이다.
	AdjacencyWindow time.Duration
	// SimilarityThreshold는 6단계의 relates_to 후보 제안에 넘길 코사인 유사도 하한이다.
	SimilarityThreshold float64
	// Limit은 한 신호가 한 사건에서 만들 수 있는 proposed 관계 수 상한이다.
	Limit int
}

func (config RelationProposalConfig) validate() error {
	if config.AdjacencyWindow <= 0 {
		return fmt.Errorf("관계 시간 인접 임계값은 양수여야 한다")
	}
	if config.SimilarityThreshold < -1 || config.SimilarityThreshold > 1 {
		return fmt.Errorf("관계 유사도 임계값은 -1에서 1 사이여야 한다")
	}
	if config.Limit <= 0 {
		return fmt.Errorf("관계 후보 수 상한은 양수여야 한다")
	}
	return nil
}

// New는 AGE 준비를 마친 연결만 담는 애플리케이션 풀을 만든다.
//
// relationProposals는 생략할 수 없고 nil을 명시해야 자동 후보 제안이 꺼진다. 가변 인자로
// 두면 호출부가 빠뜨려도 조용히 통과해, 제안이 꺼진 저장소로 검증이 지나간다.
// graceDays도 같은 이유로 생략할 수 없으며 nil이면 유예 만료 시각을 남기지 않는다.
func New(ctx context.Context, databaseURL, graphName string, relationProposals *RelationProposalConfig, graceDays RetentionDays) (*Store, error) {
	if !graphNamePattern.MatchString(graphName) {
		return nil, fmt.Errorf("AGE 그래프 이름 %q가 영문 소문자, 숫자와 밑줄 형식이 아니다", graphName)
	}
	var proposalConfig *RelationProposalConfig
	if relationProposals != nil {
		if err := relationProposals.validate(); err != nil {
			return nil, fmt.Errorf("관계 후보 제안 구성: %w", err)
		}
		proposal := *relationProposals
		proposalConfig = &proposal
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
	return &Store{pool: pool, graphName: graphName, relationProposals: proposalConfig, graceDays: graceDays}, nil
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
