# 마이그레이션 영역

## 목적

- `migrations/`는 데이터베이스 스키마 생성·판올림 SQL을 보관한다.

## 소유권

- 현재 열·그래프·인덱스의 정본은 [데이터 설계](../spec/service/agent-context/sdd/data.md)다. 과거 SQL은 적용 순서를 설명한다.
- 이 문서는 파일 작성·치환·적용 이력을 소유하며 `cmd/migrate`·`internal/migrate`도 이 계약을 따른다.

## 로컬 계약

- 파일명은 `NNN_name.sql`이며 버전 중복을 거부한다. 적용된 SQL·주석은 불변이고 변경은 다음 번호 파일로 추가한다. 앞선 계약을 바꾸는 파일은 머리말에 변경 내용·이유를 기록한다.
- 임베딩 차원 전환·복귀도 새 번호를 추가한다. [전환·복귀 경계](../spec/service/agent-context/sdd/indexing.md#전환복귀-경계)와 [절차 표본](../internal/migrate/testdata/007_embedding_dimensions.sql)을 따르며 서비스·색인 작업자를 정지하고 원본을 보존하며 호환되지 않는 벡터만 재색인 대상으로 교체한다.
- 새 DB에서 `001_init.sql`과 같은 결과만 내는 개발 중복 파일은 제거할 수 있다. 파일이 사라진 적용 이력은 보존하고 실행기는 무시한다. 001에 통합되지 않은 효과를 담은 파일은 제거하지 않는다.
- 보존해야 할 운영 데이터가 생기기 전에는 **사용자 승인**으로 개발 파일을 001에 통합·제거할 수 있다. 적용 SQL 불변 규칙의 유일한 예외다. checksum 변경 후 이전 개발·시험 DB는 재생성하고 이력을 덮어쓰지 않는다. 운영 데이터 보존 단계부터는 새 번호만 추가한다.
- [스키마 적용 순서](../spec/service/agent-context/sdd/data.md#스키마-적용)는 확장→그래프→label→관계형 테이블→인덱스다. 관계형 테이블·인덱스는 `public`을 명시하여 `ag_catalog`에 생성되지 않게 한다.
- `text/template`의 `GraphName`·`VectorType`·`VectorDim`·`ColdTablespaceClause`·`IndexTargetLayers`는 검증 후 치환한다.
- 이력의 `render_config`는 당시 치환 구성을 보존한다. 구형 이력은 DB 열·tablespace로 복원한 구성이 기존 checksum과 일치할 때만 보충하며 checksum·적용 시각을 덮어쓰지 않는다.
- checksum은 치환 SQL의 SHA-256이다. 적용 파일은 당시 `render_config`, 미적용 파일은 현재 구성으로 검증한다. 구성 변경만으로 기존 스키마를 변경하지 않으며 원본 SQL·주석 변경은 계속 거부한다.
- 적용 전에 [기동 계약](../spec/service/agent-context/sdd/operations.md#기동과-종료)의 자문 잠금을 얻고 `IF NOT EXISTS`·존재 확인으로 반복 실행을 보호한다.

## 작업 지침

- 스키마 변경은 데이터·색인 설계를 먼저 갱신하고 SQL에 반영한다. 실행기 수정은 해당 코드 영역의 DOX도 적용한다.
- AGE의 `create_vlabel`·`create_elabel`은 `cstring` 인자이므로 `format`의 `%L`로 리터럴을 만든다. agtype property 키는 `::text`를 명시한다.
- 표현식 인덱스는 사용 질의와 같은 표현식을 쓴다. SQL은 `properties ->> 'key'::text`, openCypher는 AGE가 번역한 `ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, '"key"'::ag_catalog.agtype])`를 사용한다.
- 표현식 인덱스 생성 파일 안에서 대상 테이블을 `ANALYZE`하여 통계를 확보한다.

## 검증

- [루트 시험 시점 계약](../AGENTS.md#검증)에 따라 마이그레이션 작업 항목의 모듈 시험·데이터 분야 통합 테스트에서 빈 전용 DB의 `go run ./cmd/migrate up` 적용과 두 번째 실행의 무변경을 확인한다.
- 관계형 테이블·인덱스가 `public`에 있고 [인덱스 설계](../spec/service/agent-context/sdd/data.md#인덱스)의 대상이 모두 생성되었는지 확인한다.
- 치환 구성·checksum·구형 이력 보충·적용 잠금·미통합 효과 보존을 위 시험 시점에 검증한다. 실행기 코드 변경 시 루트 공통 Go 정적 검증을 실행한다.

## Child DOX Index

- 현재 하위 `AGENTS.md` 문서는 없다.
