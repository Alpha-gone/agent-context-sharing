# 저장소 작업 계약

## 목적

- DOX는 저장소와 하위 영역의 `AGENTS.md`로 구성된 작업 계약이다. 가까운 DOX가 로컬 규칙을 정하며 상위 계약을 약화하지 않는다.
- 작업 산출물·자료·자산·지속 문서의 목적과 소유 정본은 해당 DOX와 상위 문서에서 찾을 수 있어야 한다.

## 소유권

- 루트 DOX는 저장소 구조·공통 작업 규칙·Go 기준·루트 구성 파일과 최상위 Child DOX Index를 소유한다.
- `check-go.sh`는 공통 Go 도구 체인·의존성 빌드·정적 검사를, `.github/workflows/go.yml`은 같은 검사의 CI 실행을 소유한다.
- 서버는 루트 `agent_context_sharing` Go 모듈이며 구현은 `cmd/`·`internal/`, SQL은 `migrations/`, Markdown 명세는 `spec/`에 둔다.
- [README.md](README.md)는 개요·개발 시작, [OPERATIONS.md](OPERATIONS.md)는 운영 구성 기반 시험 운영 절차를 소유한다. 동작의 정본은 서비스 SRS·분야별 SDD다.
- `test-wal-archive.sh`·`compose.wal-test.yaml`은 개발 WAL 회귀를, `test-pitr.sh`·`compose.pitr-test.yaml`은 별도 합성 클러스터의 물리 백업·tablespace·시점 복구 인수를 소유한다. 시험은 자신이 생성한 자원만 정리한다.

## 로컬 계약

- 구현은 명세를 따른다. 서비스 변경은 [SDD 진입점](spec/service/agent-context/SDD.md)에서 소유 문서를 찾아 확인하고, 설계와 달라져야 하면 해당 정본을 먼저 고친다.
- SDD는 분야별 상세 파일로 유지하며 진입 문서는 색인·책임·상태만 담는다.
- DOX에는 안정적인 소유권·작업 제약만 두고 동작 본문·설정값·진행 기록은 정본과 기록 문서를 참조한다.
- 작업별 코딩·설계·Git 등의 절차는 제공된 전역 스킬을 사용하며 프로젝트에 복제하지 않는다.
- [용어 정리집](spec/DICTIONARY.md)의 표기를 사용한다. 새 프로젝트 용어·정의는 `project-glossary`로 갱신한다.
- 구현에 SDK·라이브러리·모듈의 API·동작·호환성을 사용할 때는 적용 판의 공급자 공식 문서·API reference·release notes를 확인한다. 확인되지 않은 동작은 불확실성과 검증 대기를 표시한다.

## 작업 지침

1. 편집 전에 현재 세션에서 루트부터 대상까지 모든 DOX를 읽는다. Child DOX Index에서 대상 경로를 포함하는 문서를 따라 내려가 가장 가까운 계약을 적용한다.
2. 고유한 목적·소유권·작업 기준을 가진 지속 경계에만 하위 DOX를 둔다. 기본 절은 목적→소유권→로컬 계약→작업 지침→검증→Child DOX Index다.
3. 의미 있는 변경 후 DOX pass를 수행한다. 계약·구조·소유권·작업 흐름이 바뀌면 소유 문서와 영향받는 부모·자식을 갱신한다. DOX 생성·삭제·이동·이름 변경은 부모 색인도 갱신한다.
4. 오래된·모순된·중복된 내용은 제거하거나 소유 영역으로 이관한다. 정본을 참조하는 규칙은 필요한 문서를 실제로 읽어 적용한다.
5. DOX는 한국어로 간결하게 작성하고 기술 식별자는 보존한다. 사용자 응답은 전문적이고 공식적인 말투를 사용한다.

## 저장소 상태

- 작업 시작 시 현재 브랜치의 upstream 상태를 확인한다. pull 가능한 뒤처짐이 있으면 경고하고 실행 여부를 확인하며 사용자 승인 없이 pull하지 않는다.
- upstream·네트워크·권한 때문에 확인할 수 없으면 이유를 알리고 안전한 작업을 계속한다.

## Go 기준

- `go.mod`의 `go 1.27.1`과 개발·CI·검증 도구 체인을 유지한다. [Effective Go](https://go.dev/doc/effective_go), Modern Go Guidelines의 대상 버전 지침과 `gofmt`를 따른다.
- 기본 PATH에 Go 1.27.1이 없으면 IntelliJ IDEA의 프로젝트 Go SDK를 선택한 실행 구성·통합 터미널을 사용한다. 셸에서 go를 찾지 못한 것만으로 검증 불가로 판단하지 않는다.

## 루트 실행 구성

- 환경 변수의 허용 목록과 검증은 [배포 구성](spec/service/agent-context/sdd/operations.md#배포-구성)·[목표 추가 구성](spec/service/agent-context/sdd/operations.md#목표-추가-구성)을 따른다. 변경 시 `.env.example`·`.env.prod.example`과 관련 실행 안내를 함께 유지한다.
- 개발은 `compose.dev.yaml`, 운영은 `compose.prod.yaml`을 `-f`로 지정한다. 공유 이미지·기동 래퍼·loopback 공개·호스트 Tunnel·좁은 프록시 신뢰·WAL 경계는 [개발·운영 환경](spec/service/agent-context/sdd/operations.md#개발운영-환경-경계)을 따른다.
- 이미지 tag/digest와 pgvector 완전 버전을 고정하고 판올림 시 검증 기대값도 갱신한다. 초기 임베딩은 [Gemini 구성](spec/service/agent-context/sdd/indexing.md#임베딩-제공자-전환)을 명시하며 개발 Compose에 별도 모델 서비스를 두지 않는다.
- Compose·WAL 검사는 정본의 격리 조건을 적용하고 자신이 만든 자원만 정리한다. 정적 검사·DB runtime·실제 서버/Tunnel 인수 범위는 [운영 설계](spec/service/agent-context/sdd/operations.md)로 구분한다. 개발 초기화 절차는 [README](README.md#개발-환경-재생성)를 따른다.

## 검증

- 코드 변경 시 루트에서 `go build ./...`·`go vet ./...`·`gofmt -l .`을 실행한다.
- 테스트 코드의 작성·보강·실행은 개발 작업 항목의 모듈 시험과 분야별 통합 테스트 시점에만 수행한다. 개별 파일·함수·코드 편집마다 테스트를 추가하거나 실행하지 않는다. 시점·범위·완료 판정은 [개발 계획](spec/service/agent-context/DEVELOPMENT_PLAN.md#테스트-작성과-실행-시점), 시험 계층·필수 검증 내용은 [테스트 설계](spec/service/agent-context/sdd/testing.md)를 따른다.
- DB 시험은 `TEST_DATABASE_URL`을 사용하고 미설정 시 건너뛴다. 필수 실행 환경은 `TEST_DATABASE_REQUIRED`를 함께 설정하여 건너뜀을 실패로 처리한다. 미실행·건너뜀을 통과로 보고하지 않는다.
- 문서는 링크·Markdown·용어·맞춤법·한국어 표현과 DOX 색인을 검증한다. 완료 시 변경·검증·남은 조건과 관련 문서를 유지한 이유를 간결하게 보고한다.

## Child DOX Index

- `cmd/AGENTS.md`: 서버·색인 조립과 마이그레이션·평가·감사 실행 계약.
- `internal/AGENTS.md`: 업무 패키지·데이터 접근·자원 관리·시험 경계.
- `migrations/AGENTS.md`: SQL 파일·치환·적용 이력·개발 통합 예외.
- `spec/AGENTS.md`: 명세 문서·용어집과 서비스별 문서 계약.
