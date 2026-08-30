// Package migrations는 마이그레이션 SQL 파일을 실행 파일에 담는다.
//
// 파일을 실행 파일에 담는 이유는 배포한 실행 파일이 파일 시스템 배치에 기대지 않게 하기
// 위해서다. 파일 이름은 NNN_name.sql 형식을 지킨다.
package migrations

import "embed"

// FS는 이 디렉터리의 마이그레이션 파일이다.
//
//go:embed *.sql
var FS embed.FS
