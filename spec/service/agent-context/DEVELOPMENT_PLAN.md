# 에이전트 컨텍스트 서비스 신규 개발 계획

## 문서 정보

- 기준일: 2026-10-21
- 문서 상태: TRACE-01 신규 구현 기준·범위 확정, ARCH-01 도구 체인·모듈·의존성 준비, ARCH-02 논리 모델, ARCH-03 패키지 경계·주입 계약, PERM-01 플랜 구성과 ARCH-05 구성 해석·기동 전 검증의 구현·모듈 시험, DATA-01 물리 스키마 대응·검토, OPS-01 개발 DB·격리 환경과 DATA-02 마이그레이션, ARCH-04 AGE 연결·예약과 DATA-03 단일 접근 계층과 DATA-04 격리·저장 상태 검증과 DATA-05 조회 인덱스·통계와 DATA-06 공통 불변식·읽기 전용 감사와 DATA-07 임베딩·HNSW 저장과 DATA-08 온라인 계층 이동·접근 기록과 IDX-01 저장 트랜잭션의 작업 등록과 IDX-02 작업 확보·리스와 IDX-03 Gemini 제공자 요청·응답 계약과 IDX-04 실패 분류·재시도 예산과 IDX-05 성공 결과 세대 검증·원자적 공개와 IDX-06 소비 루프·embedded/external 배치와 IDX-07 전체 재색인·형상 변경의 작업 전이와 IDX-08 색인 준비·실연결 시험 경계와 DATA-09 구성 변경·데이터 보존 이행과 PERM-02 현재 유효 등급 조회·검사와 PERM-05 요청·누적 한도와 PERM-06 계정별 쓰기 빈도·보존 조건과 LIFE-06 웹 삭제·보관 유예·재연결 전이와 PERM-03 등급 부여·회수·작업 인계와 PERM-04 팀 생성·구성원·삭제/복구 구현·모듈 인수와 PERM-07 권한·한도 분야 통합 인수와 LIFE-01 그래프 생성·메타데이터 갱신과 LIFE-02 계층별 노드 생성·참조 저장과 LIFE-03 파생 갱신·대체·근거 무효·유지 구현·모듈 시험 완료. LIFE-03의 재검토 세 항목을 보완·검증했고 LIFE-04 사건 갱신·재구성과 관계 상태 전이와 LIFE-05 관계 후보 신호·재제안과 LIFE-07 노드 폐기·복구·현재 채널 판정과 LIFE-08 출처 전달·관리 감사·오염 되짚기 저장 경계의 구현·모듈 시험과 LIFE-09 트랜잭션·직렬화·실패 전파의 수명주기 분야 통합 인수와 SEARCH-01 검색 입력·주입·동기화 스냅숏 경계와 SEARCH-02 세 진입 채널·독립 실패 처리와 SEARCH-03 정확한 순위 역수 합·결정적 정렬과 SEARCH-04 정확한 홉 탐색 기준과 SEARCH-05 양수 상한의 후보·본문·간선 적재 보호와 SEARCH-06 홉 일관 읽기·공통 시간 예산·자원 회복과 SEARCH-07 접기·문자 예산·평면 흐름 조립과 SEARCH-08 scope·전역 진입·질의 적응형 라우팅의 구현·모듈 시험을 완료했다. MCP-01의 최소 SDK 실제 교환과 이전 수명주기 거부·오류 형식·Base64 이름 처리의 진입 경계 보완·모듈 인수와 MCP-03의 14종 도구 스키마·설명·annotations·입력 검증과 MCP-04의 에이전트 등록·행위자 결합·소유자 폐기 API와 MCP-02의 보호 HTTP·발견·외피 검증, MCP-05의 쓰기 키·24시간 결과 재생 실행 경계와 MCP-06의 여덟 쓰기 도구 연결, MCP-07의 다섯 읽기 도구·결과 직렬화와 MCP-08의 오류 계층·시간 예산·HTTP 취소 모듈 인수를 완료했다. AUTH-01~10의 계정 입력·공유 키·자격 대조·공개 인증 자원 보호·웹 세션·OAuth 발견과 사전 등록 대조·최초 인가·코드 교환·Bearer 보호 인증·갱신 회전·본인 연결 폐기와 인증 분야 통합 인수 및 OPS-03의 TLS·신뢰 프록시·실제 상대 해석 인수도 완료했다. WEB-01의 공통 화면·내장 자산과 입력 이스케이프·자산 캐시 및 WEB-02의 출처·세션·계정 요청 경계 모듈 인수도 완료했다. OBS-01의 이벤트·흐름, OBS-02의 파일 준비·권한·잠금과 OBS-03의 유한 대기열·JSON Lines·이벤트 크기와 OBS-04의 회전·보존·총상한과 OBS-05의 처리 지점 계측과 OBS-06의 감사 조회·보존 예외·복구 대기와 OBS-07의 파일 장애·stderr·부분 쓰기·종료 모듈 인수와 OBS-08의 실제 파일·재시작·다중 instance 분야 인수와 ARCH-06의 전체 애플리케이션 조립과 ARCH-07의 HTTP 진입점·공통 요청 경계와 OPS-04의 아홉 주기 작업·실행권·보존 조건과 OPS-05의 기동·수신 실패·정상 종료 공통 정리와 OPS-06의 자동 삭제 그래프 요청·운영자 복구와 OPS-07의 WAL 보관·cold tablespace 물리 백업·시점 복구와 OPS-02의 운영 템플릿·영속 진단 배치와 OPS-08의 실행 안내·전환·복귀와 WEB-03의 그래프 목록·필터·페이지·복구 요청 진입과 WEB-04의 권한·팀 관리·마지막 소유자 안내 모듈 인수도 완료했다. 나머지 개발 작업·분야별 통합 인수는 미착수.
- 서비스 식별자: `AGENT_CONTEXT`
- 입력: 현재 [SRS](SRS.md), [분야별 SDD 진입점](SDD.md), 채택된 결정과 [용어 정리집](../../DICTIONARY.md).
- 책임: 전체 개발 순서·분야 색인·시험 시점·완료 판정. 상세 작업은 `development-plan/`이 소유한다.
- 적용 계약: [서비스 DOX](AGENTS.md), [개발 계획 DOX](development-plan/AGENTS.md).

WEB-04의 권한·팀 관리와 마지막 소유자 안내는 [모듈 인수](reviews/2026-10-24-web-04.md)에 기록했다. 권한·팀 화면의 대상 식별자·본문 회수 불가 이유·팀별 그래프 등급과 삭제 팀 상속 무효 안내를 보완했다. 실제 DB/TLS에서 여섯 소유자 회수 표본·관리자 범위·동명 팀 선택·부여 성공과 화면 이후 권한/팀 상태 변경의 거부를 확인했다. 신규 20건을 포함한 웹 662건·race·전체 Go 정적 검사가 실패·건너뜀 없이 통과했다. 전체 88개 완료·36개 미착수이며 실제 브라우저·전체 웹 인수는 후속 범위다.

## 신규 구현 기준

현재 요구사항과 SDD의 채택된 목표를 빈 구현에서 시작해 모두 작성한다. 기존 코드·테스트·패키지·스키마의 존재, 이전 계획의 완료 표시와 과거 측정 결과는 작업 생략이나 완료 판정에 사용하지 않는다. 기존 단계별 진행 기록을 이 신규 계획의 완료 상태로 승계하지 않는다.

SRS와 SDD의 확정 계약은 구현 입력으로 유지한다. 문서의 현재 구현 설명은 목표 범위를 줄이는 근거로 쓰지 않으며 MCP·OAuth·회전형 갱신·공통 등록·새 평가·진단 파일도 신규 구현에 포함한다. 채택된 스냅숏·패키지 경계·검색 기본값은 설계를 따르고 성능·효과는 새 구현으로 다시 측정한다.

124개 개발 작업은 모두 `[ ]` 미착수에서 시작하며 새 산출물과 검증 근거를 확보한 항목만 완료 처리한다. 현재 [TRACE-01](development-plan/traceability.md#trace-01)의 기준·범위 확정, [ARCH-01](development-plan/architecture.md#arch-01)의 도구 체인·모듈·의존성 준비, [ARCH-02](development-plan/architecture.md#arch-02)의 논리 모델, [ARCH-03](development-plan/architecture.md#arch-03)의 패키지 경계·주입 계약, [PERM-01](development-plan/authorization.md#perm-01)의 플랜 구성과 [ARCH-05](development-plan/architecture.md#arch-05)의 구성 해석·기동 전 검증 구현·모듈 시험과 [DATA-01](development-plan/data.md#data-01)의 물리 스키마 대응·검토와 [OPS-01](development-plan/operations.md#ops-01)의 개발 DB·격리 환경 모듈 시험과 [DATA-02](development-plan/data.md#data-02)의 마이그레이션과 [ARCH-04](development-plan/architecture.md#arch-04)의 AGE 연결·예약과 [DATA-03](development-plan/data.md#data-03)의 단일 접근 계층과 [DATA-04](development-plan/data.md#data-04)의 격리·저장 상태 검증과 [DATA-05](development-plan/data.md#data-05)의 조회 인덱스·통계와 [DATA-06](development-plan/data.md#data-06)의 공통 불변식·읽기 전용 감사와 [DATA-07](development-plan/data.md#data-07)의 임베딩·HNSW 저장과 [DATA-08](development-plan/data.md#data-08)의 온라인 계층 이동·접근 기록과 [IDX-01](development-plan/indexing.md#idx-01)의 저장 트랜잭션 작업 등록과 [IDX-02](development-plan/indexing.md#idx-02)의 작업 확보·리스와 [IDX-03](development-plan/indexing.md#idx-03)의 Gemini 제공자 요청·응답 계약과 [IDX-04](development-plan/indexing.md#idx-04)의 실패 분류·재시도 예산과 [IDX-05](development-plan/indexing.md#idx-05)의 성공 결과 세대 검증·원자적 공개와 [IDX-06](development-plan/indexing.md#idx-06)의 소비 루프·embedded/external 배치와 [IDX-07](development-plan/indexing.md#idx-07)의 전체 재색인·형상 변경 작업 전이와 [IDX-08](development-plan/indexing.md#idx-08)의 색인 준비·실연결 시험 경계와 [DATA-09](development-plan/data.md#data-09)의 구성 변경·데이터 보존 이행과 [PERM-02](development-plan/authorization.md#perm-02)의 현재 유효 등급 조회·검사와 [PERM-05](development-plan/authorization.md#perm-05)의 요청·누적 한도와 [PERM-06](development-plan/authorization.md#perm-06)의 계정별 쓰기 빈도·보존 조건과 [LIFE-06](development-plan/context-lifecycle.md#life-06)의 웹 삭제·보관 유예·재연결 전이와 [PERM-03](development-plan/authorization.md#perm-03)의 등급 부여·회수·작업 인계와 [PERM-04](development-plan/authorization.md#perm-04)의 팀 생성·구성원·삭제/복구 구현·모듈 인수와 [PERM-07](development-plan/authorization.md#perm-07)의 권한·한도 분야 통합 인수와 [LIFE-01](development-plan/context-lifecycle.md#life-01)의 그래프 생성·메타데이터 갱신과 [LIFE-02](development-plan/context-lifecycle.md#life-02)의 계층별 노드 생성·참조 저장과 [LIFE-03](development-plan/context-lifecycle.md#life-03)의 파생 갱신·대체·근거 무효·유지 및 재검토 보완과 [LIFE-04](development-plan/context-lifecycle.md#life-04)의 사건 갱신·재구성·관계 상태 전이와 [LIFE-05](development-plan/context-lifecycle.md#life-05)의 관계 후보 신호·재제안과 [LIFE-07](development-plan/context-lifecycle.md#life-07)의 노드 폐기·복구·현재 채널 판정과 [LIFE-08](development-plan/context-lifecycle.md#life-08)의 출처 전달·관리 감사·오염 되짚기 저장 경계 모듈 시험과 [LIFE-09](development-plan/context-lifecycle.md#life-09)의 수명주기 분야 통합 인수와 [SEARCH-01](development-plan/search.md#search-01)의 검색 입력·주입·동기화 스냅숏 경계와 [SEARCH-02](development-plan/search.md#search-02)의 세 진입 채널·독립 실패 처리와 [SEARCH-03](development-plan/search.md#search-03)의 정확한 순위 역수 합·결정적 정렬과 [SEARCH-04](development-plan/search.md#search-04)의 정확한 홉 탐색 기준과 [SEARCH-05](development-plan/search.md#search-05)의 양수 상한 후보·본문·간선 적재 보호와 [SEARCH-06](development-plan/search.md#search-06)의 홉 일관 읽기·공통 시간 예산·자원 회복과 [SEARCH-07](development-plan/search.md#search-07)의 접기·문자 예산·평면 흐름 조립과 [SEARCH-08](development-plan/search.md#search-08)의 scope·전역 진입·질의 적응형 라우팅과 [SEARCH-09](development-plan/search.md#search-09)의 근거 경로 보존 선택 모듈 인수를 완료하여 현재 MCP-01의 최소 SDK 진입 경계와 MCP-03의 14종 도구 정의·표면 검증과 AUTH-01의 계정 입력·비밀번호 저장 형식·레거시 재해시와 AUTH-02의 공유 서명 키·JWKS·검증 캐시와 AUTH-03의 계정 등록·자격 대조와 OPS-03의 TLS·신뢰 프록시·실제 상대 해석과 AUTH-04의 공개 로그인·가입 자원 보호와 AUTH-05의 웹 세션과 AUTH-06의 OAuth 발견·사전 등록 대조와 AUTH-07의 최초 인가·코드 발급·토큰 교환과 AUTH-08의 Bearer 보호 인증과 AUTH-09의 갱신 회전과 MCP-04의 에이전트 등록·결합·폐기 API와 MCP-02의 보호 HTTP·발견·외피 검증과 MCP-05의 쓰기 키·24시간 결과 재생과 MCP-06의 여덟 쓰기 도구 연결과 MCP-07의 다섯 읽기 도구·결과 직렬화와 MCP-08의 오류 계층·요청 시간 예산·HTTP 취소와 AUTH-10의 본인 연결 폐기·인증 분야 통합 인수와 [WEB-01](development-plan/web.md#web-01)의 공통 화면·내장 자산 모듈 인수와 [WEB-02](development-plan/web.md#web-02)의 출처·세션·계정 요청 경계 모듈 인수와 [OBS-01](development-plan/observability.md#obs-01)의 진단 이벤트·안전 필드·흐름 계약 모듈 인수와 [OBS-02](development-plan/observability.md#obs-02)의 파일 준비·권한·잠금과 [OBS-03](development-plan/observability.md#obs-03)의 유한 대기열·JSON Lines·이벤트 크기와 [OBS-04](development-plan/observability.md#obs-04)의 회전·보존·총상한 모듈 인수와 [OBS-05](development-plan/observability.md#obs-05)의 처리 지점 계측과 [OBS-06](development-plan/observability.md#obs-06)의 감사 조회·보존 예외·복구 대기와 [OBS-07](development-plan/observability.md#obs-07)의 파일 장애·stderr·부분 쓰기·종료와 [OBS-08](development-plan/observability.md#obs-08)의 실제 파일·재시작·다중 instance 인수와 [ARCH-06](development-plan/architecture.md#arch-06)의 전체 애플리케이션 조립과 [ARCH-07](development-plan/architecture.md#arch-07)의 HTTP 진입점·공통 요청 경계와 [OPS-04](development-plan/operations.md#ops-04)의 주기 작업·실행권·보존 조건과 [OPS-05](development-plan/operations.md#ops-05)의 기동·수신 실패·정상 종료 공통 정리와 [OPS-06](development-plan/operations.md#ops-06)의 자동 삭제 그래프 요청·운영자 복구와 [OPS-07](development-plan/operations.md#ops-07)의 물리 백업·시점 복구, [OPS-02](development-plan/operations.md#ops-02)의 운영 템플릿·진단 배치와 [OPS-08](development-plan/operations.md#ops-08)의 실행 안내·전환·복귀와 [WEB-03](development-plan/web.md#web-03)의 그래프 목록·필터·페이지·복구 요청 진입과 [WEB-04](development-plan/web.md#web-04)의 권한·팀 관리·마지막 소유자 안내까지 88개 완료·36개 미착수다. 문서 작성·문서 검증, 코드 구현, 서버 계약 시험, 실제 호스트 지원, 실제 파일 저장, 정량 효과와 운영 공개를 구분한다. 일정·담당자·예상 개선폭은 입력이 없으므로 확정하지 않는다.

새 개발은 격리된 DB·합성 자료에서 시작한다. 기존 데이터·적용 이력·배포·호스트 설정을 변경하는 단계에는 대상과 보존·검증·복귀 조건을 먼저 정한다. 계획 작성은 코드 삭제·DB 재생성·운영 전환의 실행을 포함하지 않는다.

## SDD별 상세 개발 계획

| 분야 | 기준 SDD | 상세 계획 | 작업 ID | 주요 산출물 |
|---|---|---|---|---|
| 아키텍처 | [architecture](sdd/architecture.md) | [아키텍처 계획](development-plan/architecture.md) | ARCH-01~08 | 모듈·모델·열한 패키지·연결 예약·조립·HTTP |
| 데이터 | [data](sdd/data.md) | [데이터 계획](development-plan/data.md) | DATA-01~10 | 정식 스키마·접근 계층·격리·인덱스·불변식·온라인 계층 |
| MCP | [mcp](sdd/mcp.md) | [MCP 계획](development-plan/mcp.md) | MCP-01~09 | 표준 전송·14종 도구·등록·멱등성·결과/오류 |
| 인증 | [authentication](sdd/authentication.md) | [인증 계획](development-plan/authentication.md) | AUTH-01~10 | 계정·키·웹 세션·OAuth·Bearer·회전·폐기 |
| 권한·한도 | [authorization](sdd/authorization.md) | [권한 계획](development-plan/authorization.md) | PERM-01~07 | 등급·팀·플랜·요청/누적 한도·보존 조건 |
| 수명주기 | [context-lifecycle](sdd/context-lifecycle.md) | [수명주기 계획](development-plan/context-lifecycle.md) | LIFE-01~09 | 그래프/노드 쓰기·파생·사건·관계·삭제/복구·감사 |
| 검색 | [search](sdd/search.md) | [검색 계획](development-plan/search.md) | SEARCH-01~10 | 채널·스냅숏·홉 보호·순위·접기·예산·흐름 |
| 색인 | [indexing](sdd/indexing.md) | [색인 계획](development-plan/indexing.md) | IDX-01~08 | 등록·리스·세대·Gemini·재시도·공개·재색인 |
| 웹 | [web](sdd/web.md) | [웹 계획](development-plan/web.md) | WEB-01~09 | 서버 렌더링·업무/계정/연결 화면·시각화·키보드 |
| 운영 | [operations](sdd/operations.md) | [운영 계획](development-plan/operations.md) | OPS-01~09 | 고정 이미지·Compose·TLS·9종 회차·정리·복구 |
| 관측성 | [observability](sdd/observability.md) | [관측성 계획](development-plan/observability.md) | OBS-01~08 | 지표·감사 조회·6개 파일·상관·보존·상한·실패 |
| 평가 | [evaluation](sdd/evaluation.md) | [평가 계획](development-plan/evaluation.md) | EVAL-01~13 | 공개/자체 세트·준비·통계·통제 비교·업무 비용·판정 |
| 테스트 | [testing](sdd/testing.md) | [테스트 계획](development-plan/testing.md) | TEST-01~09 | 단위·DB·격리·자원·브라우저·프로토콜·실제 호스트 |
| 추적·미정 | [traceability](sdd/traceability.md) | [작업 추적 계획](development-plan/traceability.md) | TRACE-01~05 | 184건 작업 대응·미정 조건·새 증거·상태·DOX |

각 작업은 선행 작업 링크, 구체 순서, 산출물과 관찰 가능한 완료 기준을 포함한다. 작업 ID는 요구사항·미정 ID와 별개이며 제목이 바뀌어도 ID를 유지한다. 분야와 구현 패키지는 일대일 관계가 아니므로 같은 store 쓰기는 데이터·권한·수명주기·색인 계약을 함께 만족해야 한다.

## 권장 개발 단계

아래 단계는 분야 전체를 한 번에 끝내는 순서가 아니라 작업의 착수·완료 경계다. 같은 단계 안에서도 상세 계획의 선행 링크를 먼저 충족한다. 앞 단계의 준비 작업은 뒷 분야의 본 구현과 분리하여 실행한다.

| 단계 | 작업 범위 | 다음 단계로 넘어갈 조건 | 상태 |
|---|---|---|---|
| 0. 기준·모델·계약 | TRACE-01~03, ARCH-01~03/05, PERM-01, MCP-01, TEST-01, OBS-01 | 184건 범위·도구 체인·SDK 최소 적합성·논리 모델·경계·구성 계약 준비 | 진행 중: TRACE-01·ARCH-01·ARCH-02·ARCH-03·PERM-01·ARCH-05·MCP-01·OBS-01 완료 |
| 1. DB·저장 기반 | OPS-01~03, DATA-01~07, ARCH-04, OBS-02~04 | 새 DB 정식 적용·단일 접근 계층·격리·예약·인덱스·벡터·파일 기반 검증 | 완료: DATA-01·OPS-01·OPS-02·OPS-03·DATA-02·ARCH-04·DATA-03·DATA-04·DATA-05·DATA-06·DATA-07·OBS-02·OBS-03·OBS-04 완료 |
| 2. 권한·도메인 쓰기 | PERM-02~07, LIFE-01~04/06~08, IDX-01~05 | 등급·한도·CRUD·관계·채널·큐/벡터의 원자성·경합 처리 준비 | 완료: PERM-02·PERM-03·PERM-04·PERM-05·PERM-06·PERM-07·LIFE-01·LIFE-02·LIFE-03·LIFE-04·LIFE-06·LIFE-07·LIFE-08·IDX-01·IDX-02·IDX-03·IDX-04·IDX-05 완료 |
| 3. 색인·검색 | IDX-06~08, LIFE-05/09, DATA-08~09, SEARCH-01~09 | 작업자·재색인·온라인 계층·일관 읽기·홉·흐름·후보 선택의 계약 검증 | 완료: DATA-08·DATA-09·IDX-06·IDX-07·IDX-08·LIFE-05·LIFE-09·SEARCH-01·SEARCH-02·SEARCH-03·SEARCH-04·SEARCH-05·SEARCH-06·SEARCH-07·SEARCH-08·SEARCH-09 완료 |
| 4. 인증·MCP | AUTH-01~10, MCP-02~08 | 계정/웹 세션·OAuth·회전·등록·14종·재생·오류/취소 검증 | 완료: MCP-02 보호 HTTP/발견/외피·MCP-03 도구 정의·표면 검증·MCP-04 등록/결합/폐기 API·MCP-05 쓰기 키/24시간 재생 실행 경계·MCP-06 여덟 쓰기 도구 연결·MCP-07 다섯 읽기/결과 직렬화·MCP-08 오류/시간 예산/HTTP 취소와 AUTH-01~10 계정 입력·비밀번호·공유 키·검증 캐시·등록/대조·공개 인증 보호·웹 세션·OAuth 발견/사전 등록·최초 인가/코드 교환·Bearer 보호 인증·갱신 회전·본인 연결 폐기·인증 분야 통합 인수 완료 |
| 5. 조립·운영 경계 | ARCH-06~07, OBS-05~07, OPS-04~06, WEB-01~02, TEST-02~05/07 | 전체 서버 조립·기동 실패 정리·회차·복구·새 단위/DB/전송 시험 통과 | 진행 중: ARCH-06·ARCH-07·OPS-04·OPS-05·OPS-06·WEB-01·WEB-02·OBS-05·OBS-06·OBS-07 완료 |
| 6. 웹·전체 저장/배치 인수 | WEB-03~09, OBS-08, OPS-07~08, ARCH-08, DATA-10, SEARCH-10, MCP-09, TEST-06 | 화면·파일·다중 인스턴스·백업/복귀·새 성능/계약 인수 증거 확보 | 진행 중: OBS-08·OPS-07·OPS-08·WEB-03·WEB-04 완료 |
| 7. 통제 평가 | EVAL-01~13 | 공개/자체 입력·색인 준비·독립 단위·효과/비용·실패/보류 판정 확보 | 미착수 |
| 8. 실제 호스트·운영 준비 | TEST-08, OPS-09 | 정확한 Codex CLI 표면의 실제 인수와 격리 배포의 회복 검증 | 미착수 |
| 9. 최종 완료 대조 | TEST-09, TRACE-04~05 | 전체 필수 검사·184건 새 근거·문서/DOX·남은 제한 대조 | 미착수 |

서버 인증·진단·평가 입력 준비 등 의존성이 충족된 작업은 앞서 착수할 수 있다. 단계 번호는 완료 증거와 범위를 정리하기 위한 것이며 독립 작업을 순차 수행해야 한다는 제약은 아니다. 실제 개발을 병행할 때는 연결 예약·잠금 순서·공통 저장 인터페이스를 먼저 고정한다.

## 분야 간 선행 계약

- 논리 모델→플랜·구성→마이그레이션→저장/연결 관리가 모든 요청 경로의 기반이다.
- 등급·활성 상태·한도·행위자 결합이 준비된 뒤 쓰기 외피를 연결한다. 업무·적용 감사·색인 등록·멱등성 결과는 한 커밋 경계를 사용한다.
- 큐·제공자·현재 모델 저장이 준비된 뒤 의미 검색·관계 유사도 제안·측정 전 준비 검사를 연결한다.
- 스냅숏·접근 기록·완전한 홉·결정적 순위/예산이 준비된 뒤 흐름 직렬화·시각화·구조 지표를 인수한다.
- 실제 파일 준비는 DB·요청 수신보다 앞서 수행하고 기동/종료와 다중 instance 저장 계약을 함께 검증한다.
- 서버 MCP/OAuth 계약·정확한 callback·본인 연결 폐기가 준비된 뒤 실제 Codex CLI의 최초 인가·재사용·갱신·재시작을 인수한다.
- 평가 전에는 전체 pending/failed·현재 모델 임베딩·독립 그래프·정답 유출 방지를 확인하고 실제 측정·실패·정리 결과로 판정한다.

## 요구사항 추적과 미정 경계

[요구사항별 개발·인수 대응](development-plan/traceability.md#요구사항별-개발인수-작업-대응)은 FR 171건과 NFR 13건을 각 한 행으로 연결한다. 폐기된 FR 156~159는 제외한다. 상세 설계의 주 책임과 검증 의미는 [SDD 추적표](sdd/traceability.md#요구사항-추적표)를 유지한다.

TBD-048·049·052·060·061, RLS·플랜 이름/값 등 미확정 선택은 임의로 구현 범위에 추가하지 않는다. TBD-063은 확정 수명을 구현·시험할 대상이고 TBD-064는 SDK 및 실제 첫 호스트 증거로 해소할 대상이다. 새 구현의 미착수 상태로 채택된 설계를 다시 미정으로 돌리지 않는다.

근거 경로 선택·적응형·색인 대상·모델/압축·그래프 단계의 운영 기본값 변경은 새 통제 비교와 소유 문서의 채택 조건을 따른다. 새 측정을 시작하기 전에 효과·지연·비용의 개선을 보장하지 않는다.

## 테스트 작성과 실행 시점

테스트 코드의 작성·보강·실행은 다음 두 시점에만 수행한다. 개발 작업 항목 안의 파일·함수·세부 구현을 편집할 때마다 테스트를 작성하거나 실행하지 않는다. 각 분야 계획에 적힌 시험 요구와 완료 기준은 아래 시점에 모아서 수행한다.

| 시점 | 착수 조건 | 시험 범위와 결과 |
|---|---|---|
| 개발 작업 항목의 모듈 시험 | 해당 작업 ID의 기능 구현과 시험에 필요한 선행 조건이 준비되어 완료 여부를 검증할 때 | 작업이 담당하는 기능·패키지의 단위·계약·경계 시험을 작성·보강하고 해당 범위만 실행한다. 작업 ID·대상 패키지·명령·실제 결과를 완료 근거로 남긴다 |
| 분야별 통합 테스트 | 해당 분야의 연동 기능과 선행 작업이 준비되어 계획의 통합·인수 작업을 수행할 때 | 관련 모듈을 연결하여 DB·트랜잭션·격리·권한·취소·자원 회복·전송·화면 등 해당 분야의 계약을 검증한다. 분야·작업 ID·실행 환경·결과·미실행 조건을 기록한다 |

모듈 시험의 범위는 작업 항목이 담당하는 기능·패키지다. 예를 들어 [ARCH-02](development-plan/architecture.md#arch-02)의 모델 구현이 준비되면 모델 패키지를 시험하고, [WEB-09](development-plan/web.md#web-09)의 웹 통합 인수에서는 웹·서버 조립 경계를 함께 시험한다.

```sh
# ARCH-02 모듈 시험 범위 예시
go test -count=1 ./internal/model
# WEB-09 웹 분야 통합 테스트 범위 예시
go test -count=1 ./internal/web ./cmd/server
```

분야별 통합 테스트는 해당 분야 계획의 통합·인수 작업 및 [테스트 계획](development-plan/testing.md)의 대응 작업에서 수행한다. 전체 저장소 시험은 [TEST-09](development-plan/testing.md#test-09)의 최종 통합 인수에서 수행한다. 개발 단계가 바뀌었다는 이유로 전체 시험을 반복하지 않는다.

시험 실패를 수정하거나 시험 결과 이후 대상 코드·공유 계약이 바뀌면 관련 작업 항목의 완료 판정을 보류한다. 수정 구현과 선행 조건이 다시 준비된 때 같은 모듈 시험 또는 분야별 통합 테스트 시점에서 영향을 받은 범위를 재검증한다. 구현 중간에는 아직 수행하지 않은 필수 시험을 완료 근거로 사용하지 않는다. 모듈 시험 통과와 분야 통합 통과는 각각 기록한다.

## 공통 완료 기준과 실행 검증

작업 완료에는 해당 산출물과 완료 기준의 새로운 실행 증거가 필요하다. 정상 사례뿐 아니라 입력 경계·격리·권한·경합·롤백·취소·자원 회복을 해당 범위에서 확인한다. 완료 후 상세 체크박스·단계 상태·추적 결과를 함께 갱신한다.

코드 변경 시의 빌드·정적 분석·형식 검사와 문서 검증은 위 테스트 시점과 별도로 수행한다. Go 기본 검사는 저장소 루트에서 Go 1.27.1로 실행한다.

```sh
go version
go build ./...
go vet ./...
gofmt -l .
```

DB 계층이 완료 근거에 필요한 모듈 시험·분야별 통합 테스트와 그 CI 실행에는 `TEST_DATABASE_URL`과 `TEST_DATABASE_REQUIRED`를 함께 지정한다. 서명 키 시험은 별도 임시 DB와 정식 마이그레이션을 사용한다. 관련 동시성 경계의 race 검사도 해당 시험 시점에서 수행한다. 아래 전체 DB 시험은 TEST-09의 최종 통합 인수에서 연속 두 번 수행한다.

```sh
TEST_DATABASE_REQUIRED=1 go test -count=1 -p 1 ./...
```

새로 구현한 Compose·WAL 검사도 운영 개발 작업의 모듈 시험·분야별 통합 테스트에서 해당 계획의 격리 조건을 적용하여 실행한다.

```sh
./test-dev-compose.sh
./test-prod-compose.sh
./test-dev-compose.sh --runtime
./test-wal-archive.sh
```

정적 Compose·DB runtime·실제 server/Tunnel·Gemini 실연결·브라우저/보조 기술·실제 호스트·공개 평가를 각각 기록한다. 건너뛴 DB·Node·브라우저·외부 실행을 포함한 결과를 전체 통과로 보고하지 않는다. 합성 제공자 시험은 실제 품질/비용 판정을 대신하지 않는다.

## 최종 인수 산출물

- 새 코드·정식 스키마·고정 의존/이미지·환경 예시·실행 안내와 도구 스냅샷.
- 184건 요구사항의 새 개발 작업·시험·실증 결과 및 남은 제한.
- 실제 Codex CLI의 정확한 판·표면·callback·지원/실패/보류 기록.
- 여섯 진단 파일의 저장·재시작·권한·누락·상한·다중 instance 인수.
- 기준선·지속/적대적·공개·검색 단위·원본·임베딩 용도·업무 비용 비교의 통제 명세·실제 결과·판정 보류.
- 격리 백업/복구·기동 실패 정리·온라인 계층·배포 전환/복귀 인수와 DOX 정합성.

현재 산출물은 개발 계획, TRACE-01의 기준·범위 확정, ARCH-01의 도구 체인·모듈·의존성 준비, ARCH-02의 논리 모델, ARCH-03의 열한 패키지 경계·주입 계약, PERM-01의 플랜 구성, ARCH-05의 구성 해석·기동 전 검증과 각 모듈 시험, DATA-01의 물리 스키마 대응·문서 검토, OPS-01의 개발 DB·격리 환경, DATA-02의 마이그레이션, ARCH-04의 AGE 연결·예약, DATA-03의 단일 접근 계층과 DATA-04의 격리·저장 상태 검증과 DATA-05의 조회 인덱스·통계, DATA-06의 불변식·감사, DATA-07의 임베딩·HNSW 저장, DATA-08의 온라인 계층 이동과 IDX-01~08의 등록·확보·제공자·재시도·공개·소비/배치·전체 재색인·색인 준비와 실연결 시험 경계와 DATA-09의 구성 변경·데이터 보존 이행과 PERM-02의 현재 유효 등급 조회·검사와 PERM-05의 요청·누적 한도와 PERM-06의 계정별 쓰기 빈도·보존 조건과 LIFE-06의 웹 삭제·보관 유예·재연결 전이와 PERM-03의 등급 부여·회수·작업 인계와 PERM-04의 팀 생성·구성원·삭제/복구 구현·모듈 인수와 PERM-07의 권한·한도 분야 통합 인수와 LIFE-01의 그래프 생성·메타데이터 갱신과 LIFE-02의 계층별 노드 생성·참조 저장과 LIFE-03의 파생 갱신·대체·근거 무효·유지 및 재검토 세 항목 보완·검증과 LIFE-04의 사건 갱신·재구성·관계 상태 전이와 LIFE-05의 관계 후보 신호·재제안과 LIFE-07의 노드 폐기·복구·현재 채널 판정과 LIFE-08의 출처 전달·관리 감사·오염 되짚기 저장 경계 모듈 시험과 LIFE-09의 트랜잭션·직렬화·실패 전파 분야 통합 인수 근거다. 나머지 업무 구현·분야별 통합 시험·실제 API·호스트·평가·운영 배포는 아직 실행하지 않았다.

권한 통합 인수 범위 밖에서 함께 선택된 조회 인덱스 시험의 1만 건 표본 실패는 [PERM-07 검토의 후속 사항](reviews/2026-10-17-perm-07.md#별도-후속-사항)에 남긴다. 해당 분야의 최종 완료 근거로 사용하지 않으며 권한 시험 통과와 구분한다.

[MCP-01 최소 SDK 최종 인수](reviews/2026-10-18-mcp-01.md#서비스-진입-경계-보완과-최종-인수)에서 발견·독립 호출·완료 외피·캐시와 이전 수명주기 거부·오류 형식·Base64 이름 처리의 진입 경계를 확인했다. 최소 SDK 모듈 인수는 완료했으며 실제 서버·인증·업무 도구·호스트 지원은 후속 범위다. 이어 [MCP-03](reviews/2026-10-18-mcp-03.md)의 14종 도구 정의·표면 검증을 완료했다. MCP-04에 이어 [MCP-02](reviews/2026-10-20-mcp-02.md)의 보호 HTTP·발견·외피 검증과 실제 OAuth/TLS 연결 모듈 인수를 완료했다. [MCP-06](reviews/2026-10-20-mcp-06.md)에서 여덟 쓰기의 실제 도메인 연결과 OAuth/TLS 표본을 인수했다. [MCP-07](reviews/2026-10-20-mcp-07.md)에서 다섯 읽기·커서·평면 흐름과 표준 완료 결과를 인수했다. 기동 경로의 전체 조립·서버 계약은 ARCH-07·MCP-09에서 인수한다.

[AUTH-07 모듈 인수](reviews/2026-10-20-auth-07.md)에서 명시적 동의·최초 코드 교환, 실제 DB의 단일 소비·재사용 계열 폐기·원자적 실패 복구와 실제 TLS 흐름을 확인했다. 이어 [AUTH-08 모듈 인수](reviews/2026-10-20-auth-08.md)에서 Bearer·공유 폐기·보호 HTTP 경계를 검증했다. [AUTH-09 모듈 인수](reviews/2026-10-20-auth-09.md)에서 갱신 회전·30일/7일 수명·재사용 계열 폐기와 경합·실패 롤백·TLS를 확인했다. [MCP-04 모듈 인수](reviews/2026-10-20-mcp-04.md)에서 등록·현재 행위자 바인딩·소유자 폐기 API를 확인했다. 이어 [MCP-05 모듈 인수](reviews/2026-10-20-mcp-05.md)에서 정규화·문자열 키·현재 재검사·24시간 결과 재생과 실제 여덟 쓰기의 원자성·경합·실패 복구를 확인했다. [MCP-06 모듈 인수](reviews/2026-10-20-mcp-06.md)에서 실제 여덟 쓰기·현재 권한·행위자·도메인 결과/오류·재생과 실패 롤백을 확인했다. [MCP-07 모듈 인수](reviews/2026-10-20-mcp-07.md)까지 완료해 AUTH-10의 선행 조건을 충족했다. 이어 [MCP-08 모듈 인수](reviews/2026-10-20-mcp-08.md)에서 오류 계층·요청 시간 예산·HTTP 취소와 실제 자원 회복을 확인했다. [AUTH-10 분야 인수](reviews/2026-10-20-auth-10.md)에서 본인 연결 목록·계열/등록 폐기와 인증 전체 회귀를 확인하여 단계 4를 완료했다. 이어 [WEB-01 모듈 인수](reviews/2026-10-21-web-01.md)에서 공통 화면·내장 자산·입력 이스케이프·ETag/304를 확인했다. 이어 [WEB-02 모듈 인수](reviews/2026-10-21-web-02.md)에서 출처·세션·계정·등급·조회·응답 범위와 실제 쿠키·TLS를 확인했다. 이어 [OBS-01 모듈 인수](reviews/2026-10-21-obs-01.md)에서 여섯 진단 종류·안전 필드·고정 코드와 요청/인가/작업/시험 흐름 계약을 확인했다. 이어 [OBS-02 모듈 인수](reviews/2026-10-21-obs-02.md)에서 실제 파일·권한·프로세스 잠금과 재기동 보존을 확인했다. 이어 [OBS-03 모듈 인수](reviews/2026-10-21-obs-03.md)에서 유한 대기열·JSON Lines·이벤트 크기·누락 집계와 실제 파일 기록을 확인했다. 이어 [OBS-04 모듈 인수](reviews/2026-10-21-obs-04.md)에서 회전·보존·총상한과 실제 다중 프로세스 예약·혼합 행 보존을 확인했다. 이어 [OBS-05 모듈 인수](reviews/2026-10-21-obs-05.md)에서 처리 지점 계측·상관/작업/계열 연결과 실제 세대·커밋/롤백 경계를 확인했다. 이어 [OBS-06 모듈 인수](reviews/2026-10-22-obs-06.md)에서 감사 커서·소유자·상속 근거와 보존 정리 뒤 복구·대기 판정을 확인했다. 이어 [OBS-07 모듈 인수](reviews/2026-10-23-obs-07.md)에서 안전한 stderr·부분 행 복구·5초 종료와 실제 파일 장애 뒤 업무 커밋/감사 보존을 확인했다. 이어 [OBS-08 분야 인수](reviews/2026-10-23-obs-08.md)에서 macOS·Linux의 실제 여섯 파일·보존/상한·장애/누락과 별도 볼륨의 컨테이너 재생성·슬롯 교체를 확인하여 관측성 8개 작업을 완료했다. 이어 [ARCH-06 모듈 인수](reviews/2026-10-23-arch-06.md)에서 단일 저장소·제공자·플랜과 전체 14종 MCP 의존을 연결하고 조립/시작 분리와 자원 정리를 확인했다. 이어 [ARCH-07 모듈 인수](reviews/2026-10-23-arch-07.md)에서 24개 HTTP 경로·메서드·TLS 선행·상관 ID·본문 상한·공통 마감/취소를 확인했다. 이어 [OPS-04 모듈 인수](reviews/2026-10-23-ops-04.md)에서 여덟 DB 작업과 시간별 진단 정리의 실행권·보존 조건을 확인했다. 이어 [OPS-05 모듈 인수](reviews/2026-10-23-ops-05.md)에서 기동·수신 실패·정상 종료의 공통 정리와 실제 30/10/5초 예산을 확인했다. 이어 [OPS-06 모듈 인수](reviews/2026-10-23-ops-06.md)에서 요청 자격·결정적 선택·원자적 복구·독립 롤백과 실제 세션 주체 경계를 확인했다. 이어 [OPS-07 모듈 인수](reviews/2026-10-23-ops-07.md)에서 새/기존 볼륨의 WAL 회귀와 cold tablespace를 포함한 물리 백업·목표 일시 정지/승격·AGE/HNSW·서명·큐·전체 감사를 확인했다. 이어 [OPS-02 모듈 인수](reviews/2026-10-24-ops-02.md)에서 운영 템플릿·영속 진단 볼륨·안정 슬롯과 격리 서버 기동·재생성 보존을 확인했다. 이어 [OPS-08 모듈 인수](reviews/2026-10-24-ops-08.md)에서 합성 이전 형상의 전환·실패 파일 롤백·별도 물리 복원본과 보관 이미지 복귀를 확인했다. 이어 [WEB-03 모듈 인수](reviews/2026-10-24-web-03.md)에서 목록·필터·페이지·복구 요청 진입과 실제 DB/TLS 경계를 확인했다. 이어 [WEB-04 모듈 인수](reviews/2026-10-24-web-04.md)에서 권한 회수 안내·관리 팀의 구성원/그래프 등급·실제 DB/TLS의 변경 재판정을 확인했다. 다음 권장 작업은 [WEB-05](development-plan/web.md#web-05)의 삭제·복구·운영자 요청 처리 화면이며 선행 WEB-02·LIFE-07·OPS-06을 충족했다. OPS-09에는 ARCH-08·WEB-09 등의 선행이 더 필요하다. ARCH-08에는 TEST-02·TEST-04가 추가로 필요하다. MCP-09에는 TEST-07이 추가로 필요하다. 전체 화면·실제 브라우저와 호스트 인수는 후속 범위다.
