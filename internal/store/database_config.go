package store

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ValidateDatabaseURL는 DB에 연결하지 않고 실제 풀과 같은 해석기로 URI를 검사한다.
// pgx의 환경 기본값·서비스/비밀번호/TLS 파일 해석을 포함한다. 파서 오류는 접속 원문을
// 보존할 수 있어 외부 구성 경계에 전달하지 않는다.
func ValidateDatabaseURL(raw string) error {
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		if _, err := pgxpool.ParseConfig(raw); err == nil {
			return nil
		}
	}
	return errors.New("PostgreSQL 접속 URL을 해석할 수 없다")
}

func parsePoolConfig(raw string) (*pgxpool.Config, error) {
	// pgxpool은 풀 옵션을 RuntimeParams에서 제거한다. 키의 출처는 삭제 전 pgx 해석
	// 결과로 확인하여 비밀번호·다른 값의 부분 문자열이나 인코딩된 키에 좌우되지 않게 한다.
	connection, err := pgx.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	_, explicitMaximum := connection.RuntimeParams["pool_max_conns"]
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	if !explicitMaximum {
		cfg.MaxConns = max(cfg.MaxConns, minPoolConns)
	}
	return cfg, nil
}
