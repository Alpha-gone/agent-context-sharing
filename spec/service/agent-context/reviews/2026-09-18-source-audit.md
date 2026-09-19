# 전체 소스 결함 검사

## 문서 정보

- 문서 성격: 특정 시점의 구현 검사 기록이며 명세가 아니다. 계약은 `SRS.md`와 `SDD.md`가 소유한다.
- 검사일: 2026-09-17 ~ 2026-09-18
- 검사 대상: `dev` 브랜치 `d07509d` (9단계 병합 직후)
- 검사 범위: `cmd/`, `internal/`, `migrations/`의 테스트가 아닌 코드 약 11,000줄 전체. 번들된 Cytoscape.js는 제외했다.
- 2차 검사: 같은 대상에 대해 2026-09-18에 성능과 자원 사용 관점으로 한 번 더 읽었다. 결과는 40번, 41번과 「최적화 가능 지점」에 있다.
- 기준 문서: [SRS](../SRS.md), [SDD](../SDD.md), [개발 계획](../DEVELOPMENT_PLAN.md)

## 검사 방법

코드를 네 영역으로 나눠 병렬로 검사하고, 심각도 상 항목은 코드를 다시 읽어 교차 확인했다.

| 영역 | 대상 |
|------|------|
| 그래프 저장 핵심 | `internal/store`의 `context.go`, `relation.go`, `hop.go`, `agtype.go`, `cursor.go`, `graph.go`, `operation.go` |
| 저장소 나머지와 마이그레이션 | `internal/store`의 `auth.go`, `rate.go`, `web.go`, `permission.go`, `index.go`, `periodic.go`, `store.go`, `migrations/`, `internal/migrate` |
| MCP와 검색 | `internal/mcp`, `internal/search`, `internal/model`, `internal/plan`, `internal/perm` |
| 인가와 웹 서버 | `internal/authz`, `internal/web`, `cmd/server`, `internal/config`, `internal/index` |

| 방법 | 내용 |
|------|------|
| 코드 판독 | 명세의 확정 값과 구현을 하나씩 대조했다 |
| 명세 역확인 | 의심 항목을 먼저 `SRS.md`·`SDD.md`에서 확인해 확정된 계약을 결함으로 적지 않았다 |
| 실 데이터베이스 실행 | 임시 통합 테스트로 동시 요청, 권한 경계, 질의 계획을 실제 AGE에서 재현했다. 임시 파일은 검사 뒤 모두 지웠다 |

근거 열의 `재현`은 실행으로 확인한 것이고, `판독`은 코드와 명세 대조로만 확인한 것이다.

2차 검사는 같은 코드를 왕복 횟수, 질의 계획, 연결 점유, 응답 크기 관점으로 다시 읽었다. 실 데이터베이스는 쓰지 않았고, 의존 라이브러리의 기본 동작이 판단 근거인 항목은 해당 버전의 원본을 직접 확인했다. 따라서 2차 검사에서 나온 항목의 근거는 모두 `판독`이다.

## 확정된 계약이라 결함이 아닌 것

| 의심한 것 | 확정 근거 |
|-----------|-----------|
| 순위 역수 합에 k 상수와 가중치가 없다 | `SRS.md`가 "순위 역수, 가중치 없음"으로 확정했다 |
| 예산을 넘는 컨텍스트에서 멈추고 뒤 순위로 채우지 않는다 | 「예산 적용과 절단」의 확정 동작이다 |
| JSON-RPC 배치 요청을 -32600으로 거부한다 | 이 protocol revision에서 배치를 쓰지 않는다 |
| 웹 POST 폼에 CSRF 토큰이 없다 | `SRS.md` 「웹 세션」이 `SameSite=Lax`로 확정했다 |
| 운영자 복구 화면이 세션만 검사한다 | `FR-AGENT_CONTEXT-135`가 확정했다 |
| 루프백 `redirect_uri`의 포트를 무시한다 | RFC 8252를 따른 동작이며 3단계 검사에서 확인했다 |
| 요청 빈도 고정 창 경계에서 최대 두 배까지 통과한다 | `SDD.md` 「계정 플랜 값」이 허용했다 |
| 색인 작업에 처리 중 상태가 없고 `failed`를 upsert가 되살린다 | `SDD.md` 「색인 작업 큐」의 확정 사항이다 |
| 원천 중복을 `graph_id`와 locator로만 판정한다 | `SDD.md`의 확정 사항이다 |

다음은 의심했으나 실행이나 판독으로 정상을 확인한 항목이다.

- Cypher 문자열 이스케이프: `"})`, `'`, `\`, `$$`가 든 locator와 본문의 왕복이 일치했다(재현).
- `graph_id` 격리: 모든 질의가 정점과 간선의 `graph_id`를 거르고 해석 단계에서 한 번 더 검사한다.
- JWT 검증: ES256 고정, `kid` 조회, `iss`·`aud`·`exp`·`iat`·`jti`·`sub` 확인, 폐기 목록 확인이 모두 있다.
- PKCE: S256 대조, 코드 해시 저장, 1회 조건부 소비, 재사용 시 토큰 폐기가 있다.
- 로그인 타이밍: 없는 계정도 같은 비용의 더미 해시로 대조한다.
- TLS 판정: 신뢰 대역을 상대 주소로 판정하고 `X-Forwarded-Proto`는 값이 하나일 때만 읽는다.
- XSS: 시각화 JSON은 `EscapeForHTML` 뒤에만 `template.JS`로 넘기고 노드 상세는 `textContent`로 넣는다.
- 전송 검증 순서: Origin, POST, 헤더, 불일치, 버전, 메서드 순서가 명세 표와 같다.
- 마이그레이션 멱등성과 자문 잠금 키: 001~004가 모두 재적용에 안전하고 마이그레이션 키와 주기 작업 키가 겹치지 않는다.

## 발견 사항

처리 상태를 갱신할 수 있도록 체크박스로 둔다. 심각도순이며, 같은 심각도 안에서는 영향 범위순이다.

### 상

#### - [x] 1. 인가 서버 경로와 토큰 자동 갱신이 HTTP 계층에 연결되지 않았다

`cmd/server/server.go:68`~`:85`, `internal/authz/authz.go:301`

`SDD.md` 「HTTP 진입점」의 `/authorize`, `/token`, `/jwks.json`이 어느 mux에도 등록되지 않았다. MCP 클라이언트는 `/.well-known/oauth-authorization-server`에서 이 경로를 안내받지만 실제 요청은 404가 된다. 접근 토큰을 발급받을 수 없으므로 `/mcp` 전체를 쓸 수 없고, 리소스 서버를 분리해 JWKS로 검증하는 배치도 성립하지 않는다.

`authz.Authorize`, `Exchange`, `JWKS`, `RotateSigningKey`, `Renew`를 테스트 밖에서 호출하는 곳이 없다. 그래서 「토큰 갱신」의 "만료 전 10초 안이면 새 토큰을 응답 헤더에 담는다"도 동작하지 않는다. 개발 계획 3단계에는 두 항목이 완료로 표시되어 있다.

- 근거: 재현. TLS 요청으로 세 경로 모두 404였다.

#### - [x] 2. 소유자 등급을 낮추는 부여가 마지막 소유자 판정을 지나지 않는다

`internal/store/web.go:16`~`:46`

`GrantGraph`는 upsert로 기존 등급을 덮어쓰지만 `requireOwner`를 호출하지 않는다. 유일한 소유자가 권한 화면에서 자기 계정에 editor를 부여하면 소유자가 0명인 그래프가 된다. `FR-AGENT_CONTEXT-042`와 `SDD.md` 「판정의 직렬화」의 "소유권 이전도 방아쇠"를 위반한다.

- 근거: 재현. `err=nil`, 부여 뒤 소유자 수 0.

#### - [x] 3. 유예 만료로 자동 삭제된 그래프가 웹 복구로 되살아난다

`internal/store/web.go:480`

`SetGraphDeleted`의 복구 조건이 `deleted_at IS NOT NULL`뿐이고 `grace_started_at`을 보지 않는다. 구성원 없는 팀이 소유한 그래프가 유예 만료로 삭제된 뒤 팀 관리자가 구성원을 추가하면, 그 구성원이 소유자 등급을 얻어 웹에서 복구할 수 있다. 팀 복구(`SetTeamDeleted`)로도 같은 결과가 난다. `FR-AGENT_CONTEXT-108`이 정한 "요청과 운영자 경로만"을 우회하며 `EffectiveGrade`의 "자동 삭제 그래프에는 접근 가능한 계정이 없다"는 전제도 깨진다.

- 근거: 재현. `ExpireGrace` → `AddTeamMember` → 등급 owner → 복구 `err=nil`.

#### - [x] 4. 같은 정체성의 관계가 중복 생성된다

`internal/store/relation.go:86`~`:109`, `:422`~`:456`

관계 확정이 "정체성 조회 후 없으면 생성"을 Read Committed에서 잠금이나 유일 제약 없이 수행한다. AGE 간선에는 유일 인덱스가 없다. 같은 (유형, from, to)의 `relation_confirm`이 동시에 오거나 자동 후보 제안과 겹치면 간선이 여러 개 생긴다. 이후 `relationByIdentity`는 임의의 한 행만 보므로 하나를 폐기해도 나머지 `confirmed` 간선이 탐색에 남는다. `SRS.md`의 "정체성 재확정은 멱등"에 어긋난다.

같은 원인으로 A→B와 B→A를 동시에 확정하면 둘 다 순환 검사를 통과해 순환이 확정된다.

- 근거: 재현. 동시 확정 4건을 10회 반복해 10회 모두 중복 간선이 생겼다.

#### - [x] 5. 이미 있는 관계를 재확정할 때 순환 검사를 건너뛴다

`internal/store/relation.go:111`~`:122`

proposed 또는 discarded 상태의 기존 관계를 confirmed로 바꾸는 분기가 `ValidateRelationCycle`을 부르지 않는다. A→B를 확정했다가 폐기하고 B→A를 확정한 뒤 A→B를 다시 확정하면 precedes 순환이 확정된다. `SRS.md`는 순환 확정을 `invalid_argument`로 거부한다.

- 근거: 재현. 재확정은 수락됐고 같은 형태의 새 간선은 순환으로 거부됐다.

#### - [x] 6. 폐기와 복구가 판 번호 조건 없이 노드 전체를 덮어쓴다

`internal/store/context.go:186`~`:260`

`changeContextDeletion`이 트랜잭션 초반에 읽은 스냅샷 전체를 `SET node = …`로 쓰며, 쓰기 조건에 판 번호나 `deleted_at`이 없다. 같은 노드에 폐기 두 건이 동시에 오면 둘 다 성공해 `stored_chars`가 두 번 차감되고 적용 기록이 두 줄 남는다. 폐기가 읽은 뒤 `node_update`가 커밋되면 폐기의 쓰기가 옛 본문으로 덮어써 수정이 사라지고 색인 작업도 다시 등록되지 않는다.

- 근거: 이중 적용은 재현(동시 폐기 8건 중 2건 성공, `stored_chars` 11 → 1). 수정 유실은 같은 메커니즘의 판독.

#### - [x] 7. AGE 동시 갱신 오류가 `version_conflict`가 아닌 `internal`로 나간다

`internal/store/context.go:100`~`:113`, `:233`~`:238`, `internal/store/relation.go:537`

같은 판 번호로 수정이 동시에 오면 사전 판 번호 검사를 통과한 쪽이 AGE의 `Entity failed to be updated`(XX000)를 받는다. 이 오류를 `VersionConflictError`로 바꾸지 않아 `mapError`가 `internal`을 돌려준다. `SRS.md`의 충돌 응답과 `SDD.md`의 재시도 계약에 어긋나며 폐기, 근거 무효 전파, 관계 갱신도 같다.

- 근거: 재현. 4요청 20라운드에서 성공 20, 충돌 3, XX000 57.

### 중

#### - [x] 8. 소유자 수를 셀 때 쓸 수 없는 팀 등급까지 센다

`internal/store/permission.go:254`~`:263`

`requireOwner`가 삭제된 팀이나 구성원 없는 팀의 owner 부여도 소유자로 센다. 계정 소유자 A와 삭제된 팀 T(owner)가 있을 때 A를 회수하면 통과해, 소유자 계정이 없는 그래프가 되고 유예가 시작된다. `FR-AGENT_CONTEXT-042`의 "소유자 등급의 계정을 최소 하나 유지"를 위반한다.

- 근거: 재현.

#### - [x] 9. 색인 작업자가 작업 행 잠금을 쥔 채 임베딩 제공자를 호출한다

`internal/store/index.go:65`~`:126`

작업 확보 트랜잭션 안에서 최대 30초짜리 제공자 호출이 일어난다. 그동안 같은 컨텍스트의 본문을 수정하면 저장 트랜잭션의 `enqueueIndexTask` upsert가 제공자 응답까지 막히고 정점 갱신 잠금도 유지된다. `SDD.md`의 "임베딩 생성을 트랜잭션 밖으로 뺐다"와 "대역이 응답을 늦춰도 저장 지연이 늘지 않아야 한다"에 어긋난다.

- 근거: 재현. 제공자 3초 지연 시 같은 컨텍스트 등록이 2.997초 대기.

#### - [x] 10. 팀 영향 그래프 목록을 잠금 전에 읽고 다시 읽지 않는다

`internal/store/permission.go:105`~`:112`, `internal/store/web.go:198`~`:206`, `:236`~`:244`

유예 중인 그래프에 팀 등급 부여와 그 팀의 구성원 제거가 겹치면, 제거 쪽은 그 그래프를 모르고 부여 쪽은 제거 전 구성원을 보고 유예를 취소한다. 결과는 접근 가능 계정 0명에 유예 없음이라 영구히 만료되지 않는다. 현재 웹에는 팀 부여 경로가 없어 저장소 API 계약상의 결함이다.

- 근거: 재현. 행 잠금으로 순서를 강제해 확인.

#### - [x] 11. 컨텍스트 웹 삭제·복구의 감사 기록이 별도 트랜잭션이다

`internal/store/web.go:500`~`:521`

`SetContextDeleted`가 상태 변경을 커밋한 뒤 별도 트랜잭션에서 웹 감사 기록을 남긴다. 그 사이 요청이 취소되거나 DB 오류가 나면 변경은 적용되고 기록은 사라진다(`FR-AGENT_CONTEXT-109`). 함수 주석은 "같은 트랜잭션"이라고 적고 있다.

- 근거: 판독.

#### - [x] 12. 인가 코드의 만료를 소비 여부보다 먼저 판정한다

`internal/store/auth.go:132`~`:137`, `:170`~`:175`

소비된 코드를 만료 뒤, 정리 작업 전에 다시 제시하면 `CodeUsedError`가 아닌 `ErrNotFound`가 나와 발급 토큰이 폐기되지 않는다. `SDD.md`는 재사용 감지를 위해 소비 행을 만료 뒤 정리 때까지 남긴다고 했다.

- 근거: 판독. 명세가 만료 뒤 재사용의 폐기 여부를 직접 쓰지는 않아 확인이 필요했다.
- 판단: 결함으로 처리했다. 「인가 코드 흐름」이 소비 행을 남기는 이유를 "재사용과 없는 코드를 구분하고 폐기할 토큰을 찾기 위해서"로 못박았는데, 코드 수명 60초와 접근 토큰 수명 1시간의 차이 때문에 현재 순서로는 그 판정이 정작 위험한 구간에서 동작하지 않는다. 단위 대역이 이미 소비를 먼저 보고 있던 것도 의도한 순서가 무엇인지를 보여 준다.

#### - [x] 13. 폐기된 사건을 끝으로 하는 관계 확정이 허용된다

`internal/store/relation.go:74`~`:83`

`s.context`는 폐기된 정점도 돌려주고 `ValidateRelation`과 처리기 모두 `DeletedAt`을 보지 않는다. `SDD.md`와 `FR-AGENT_CONTEXT-076`은 폐기된 사건을 끝으로 갖는 관계가 남지 않게 하라고 정했다.

- 근거: 판독.

#### - [x] 14. 커밋 뒤 재조회 실패를 연산 실패로 응답한다

`internal/store/context.go:130`~`:142`, `:250`~`:258`, `:380`~`:388`

커밋이 끝난 뒤 재조회가 실패하면 처리기가 `internal`을 응답하고 `recordRejected`로 거부 기록까지 남긴다. 적용 기록과 거부 기록이 모순되고, 호출자가 재시도하면 판 번호 충돌이 난다.

- 근거: 판독.

#### - [x] 15. 누적 한도를 저장 트랜잭션 밖에서 판정한다

`internal/mcp/handler.go:220`, `:308`, `:473`~`:485`, `internal/store/context.go:609`~`:621`

`stored_characters_per_graph`와 `graphs_per_account`를 트랜잭션 밖에서 읽은 값으로 판정하고 저장 트랜잭션에서는 다시 보지 않는다. 한도 직전 그래프에 생성이 동시에 오면 둘 다 통과해 한도를 넘는다. 6번으로 `stored_chars`가 어긋난 뒤 `stored_chars + $2 >= 0` 조건이 실패하면 `not_found`로 잘못 사상되기도 한다.

- 근거: 판독.

#### - [x] 16. `relation_list`가 관계 목록 페이지 크기 대신 그래프 목록 값을 쓴다

`internal/mcp/handler.go:334`~`:341`

플랜의 `RelationPage` 대신 `GraphPage`를 쓴다. `relation_page`를 `{default:5, maximum:10}`으로 덮어써도 기본 50이 쓰이고 150도 거부되지 않으며, 초과 시 한도 이름이 `graph_page`로 나간다. `RelationPage`를 읽는 곳이 `plan` 패키지 밖에 없다.

- 근거: 재현.

#### - [x] 17. 잘못된 목록 커서가 `invalid_argument`가 아닌 `internal`로 나간다

`internal/mcp/schema.go:65`, `:147`, `internal/mcp/handler.go:147`, `:341`

`mcp`가 커서 형식을 검증하지 않고, 저장소의 해독 오류는 표지 없는 오류라 `mapError`가 `internal`로 바꾼다. `SRS.md`는 유효하지 않은 커서를 `invalid_argument`로, `SDD.md` 「입력 검증」은 커서 형식 검증을 `mcp` 책임으로 둔다.

- 근거: 재현. `cursor:"!!"` → `internal`.

#### - [x] 18. 공백만 있는 그래프 이름을 받는다

`internal/mcp/schema.go:69`, `:76`

`graph_create`와 `graph_update`가 `"   "`을 통과시킨다. `model.Graph.Validate`와 `store.UpdateGraph`도 빈 문자열만 거부한다. `SRS.md`는 공백만으로 된 이름을 `invalid_argument`로 둔다.

- 근거: 재현.

#### - [x] 19. `max_hops`가 0(제한 없음)이면 흐름 검색의 그래프 확장이 일어나지 않는다

`internal/search/search.go:350`, `internal/mcp/handler.go:128`

플랜의 `MaxHops`를 탐색 깊이로 그대로 넘긴다. 0이면 시작 노드만 돌아오고 거리 0은 후보에서 빠지므로 graph 채널이 실패 표시 없이 항상 0건이다. 같은 함수가 예산과 `MaxHopNodes`의 0은 제한 없음으로 해석한다.

- 근거: 판독.

#### - [x] 20. 홉 탐색으로 가져온 파생에 근거 목록이 비어 있다

`internal/search/search.go:507`~`:518`, `internal/store/hop.go`

`hopNeighbors`가 `DerivedFrom`과 `MemberRefs`를 채우지 않는다. 채우는 곳은 `store/context.go:512`, `:749`뿐이다. 그래서 graph 채널로만 들어온 파생의 접기 키가 "종류|근거상태|"로 뭉개져 근거가 다른 파생끼리 접힌다. `SRS.md` 「예산 적용과 절단」은 근거가 다르면 접지 않는다. 흐름 응답의 확장 노드에서도 `derived_from`과 `member_refs`가 빈 목록으로 나간다.

- 근거: 판독.

#### - [x] 21. 그래프 목록에서 "모든 등급"으로 필터를 적용하면 400이 난다

`internal/web/web.go:639`~`:643`

기본 선택지의 값이 빈 문자열이라 `grade=`가 전송되고 `GraphGrade("").Valid()`가 거짓이다. 이름 필터만 쓰는 것도 불가능하다.

- 근거: 재현. `GET /graphs?name=&grade=` → 400.

#### - [x] 22. 그래프 목록의 "더 보기" 링크가 필터를 빠뜨린다

`internal/web/web.go:781`

다음 페이지 링크가 `cursor`만 싣는다. 이름이나 등급으로 거른 목록의 2쪽부터 필터 없는 전체 목록이 나온다.

- 근거: 판독.

#### - [x] 23. 삭제·복구 화면의 컨텍스트 목록이 50개에서 잘리고 다음 페이지가 없다

`internal/web/web.go:531`~`:553`, `internal/store/web.go:753`~`:755`

목록이 `GraphPage.Default`개에서 `context_id` 오름차순(오래된 순)으로 잘린다. 컨텍스트가 51개 이상이면 최신 컨텍스트가 목록에 없고, `?context_id=`로 직접 접근해도 "활성 컨텍스트를 찾을 수 없습니다"(404)가 되어 화면에서 삭제·복구할 수 없다.

처리 범위: 목록을 최신순으로 바꾸고, 상한에 걸리면 화면에 안내를 띄우며, `?context_id=`는 목록과 무관하게 직접 조회하도록 고쳤다. 커서 기반 다음 페이지는 저장소에 컨텍스트 목록 커서 API가 없어 두지 않았다. 필요하면 별도 항목으로 다룬다.

- 근거: 판독.

#### - [x] 24. HTTP 서버에 읽기·헤더·유휴 타임아웃이 없다

`cmd/server/main.go:108`

`ReadHeaderTimeout`, `ReadTimeout`, `IdleTimeout`이 없어 헤더를 느리게 보내는 연결을 대량으로 열면 고루틴과 파일 디스크립터가 고갈된다. TLS 판정 이전이라 인증 없이 가능하다. 명세에 값이 정해져 있지 않다.

- 근거: 판독.

#### - [x] 40. 재색인 등록이 열린 결과 행 위에서 연결을 하나 더 빌린다

`internal/store/index.go:158`~`:180`

`ReindexOutdatedEmbeddings`가 `s.pool.Query`로 연 행을 순회하는 도중 `ReindexGraph`를 부르고, 그 안에서 다시 `s.pool.Query`와 `s.pool.Begin`으로 연결을 추가로 빌린다. 같은 파일의 `searchCandidateIDs`(`:404`)와 `ProposeSimilarEventRelations`(`:463`)가 "행을 연 채 부르면 같은 풀에서 연결을 하나 더 잡아 동시 요청이 서로를 기다린다"고 적어 둔 것과 같은 형태이며, 이 경로만 그 규칙을 지키지 않았다.

색인 작업자가 기동 직후 가장 먼저 부르는 함수이고(`internal/index/index.go:105`), 풀 크기는 어디에서도 설정하지 않아 pgx 기본값을 쓴다. 옛 모델의 그래프가 많은 배포에서 기동과 요청이 겹치면 대기가 길어진다.

- 근거: 판독.

### 하

#### - [x] 25. 관계 확정·폐기도 쓰기 빈도 한도를 소비한다

`internal/mcp/handler.go:357`, `:381`

`SRS.md`와 `TBD-AGENT_CONTEXT-062`는 적용 대상을 컨텍스트를 만들거나 고치는 연산으로 한정한다. 명세 해석 확인이 필요하다.

- 근거: 판독.
- 판단: 결함으로 처리했다. `TBD-AGENT_CONTEXT-062`가 적용 대상을 그 둘로 한정했고 관계 확정·폐기는 컨텍스트를 만들지도 고치지도 않는다. 같은 근거로 상태를 바꾸지 않고 판단만 기록하는 `management_action: keep`도 함께 제외했다. 「계정 플랜 값」이 이 한도의 목적을 외부 임베딩 호출의 폭주를 막는 것으로 적은 것과도 맞는다.

#### - [x] 26. 요청 빈도 카운터를 저장 트랜잭션 밖에서 먼저 올린다

`internal/mcp/handler.go:217`~`:227`, `:285`, `:305`, `:357`

판 번호 충돌, 저장량 한도, 잘못된 `supersedes_context_id`로 거부된 요청과 원천 중복 멱등 재요청도 한도를 소비한다. `SDD.md` 「계정 플랜 값」은 카운터 갱신이 이미 열린 저장 트랜잭션 안에서 일어난다고 정했다.

- 근거: 판독.

#### - [x] 27. 계층에 맞지 않는 인자를 오류 없이 버린다

`internal/mcp/handler.go:834`~`:859`, `:880`~`:915`

원천에 붙인 `derived_from`·`member_refs`·`start`, 파생에 붙인 `source_channel`·`occurred_at`, 파생 수정의 `member_refs`가 무시된 채 성공한다. 명세는 `confidence_state` 같은 부적용 속성을 명시적으로 거부한다.

- 근거: 판독.

#### - [x] 28. JSON-RPC 입력과 정수 인자를 느슨하게 검증한다

`internal/mcp/transport.go:102`~`:110`, `internal/mcp/schema.go:240`~`:243`, `internal/model/id.go:44`

- `id`가 없는 요청에도 200과 `"id":null` 결과를 보내고, 객체 `id`도 받는다(재현).
- `integer()`가 2^63을 허용한다. 제한 없음 플랜에서 `page_size`=2^63이면 저장소의 `limit+1`이 넘쳐 `internal`로 끝나고, amd64에서는 음수 홉이 될 수 있다(판독).
- `ParseID`가 하이픈을 위치와 무관하게 지워 비정규 UUID 표기를 받는다(판독).

#### - [x] 29. 토큰 검증이 요청마다 서명 키와 폐기 목록을 DB에서 읽는다

`internal/authz/authz.go:281`, `:293`

`SDD.md` 「토큰 검증」의 "둘 다 요청마다 가져오지 않는다"와 "JWKS를 메모리에 두고 모르는 kid를 만나면 다시 읽는다"와 다르다.

- 근거: 판독.

#### - [x] 30. `redirect_uri` 허용 목록이 클라이언트별이 아니다

`internal/authz/authz.go:184`~`:193`, `internal/config/config.go:178`~`:187`

`SDD.md` 「인가 코드 흐름」 2단계는 "그 클라이언트의 허용 목록"이다. 전역 목록이 의도된 단순화인지 확인이 필요하다.

- 근거: 판독.
- 보류 사유: 고치려면 배포 구성 형식을 바꿔야 한다. `OAUTH_CLIENT_IDS`와 `OAUTH_REDIRECT_URIS`가 서로 독립한 목록이라 둘의 교차곱이 허용 집합이 되므로, 클라이언트별 목록을 두려면 `OAUTH_REDIRECT_URIS`를 클라이언트를 키로 하는 객체로 받아야 하고 「배포 구성」 표를 함께 고쳐야 한다. 등록 클라이언트가 하나인 배포에서는 전역 목록이 그 클라이언트의 목록과 같아 증상이 없고, 둘 이상일 때만 A의 코드가 B의 콜백으로 갈 수 있다. 구성 호환성을 깨는 변경이라 사용자 판단을 받는다.
- 처리: `OAUTH_CLIENT_IDS`와 `OAUTH_REDIRECT_URIS`를 폐지하고 클라이언트를 키로 하는 `OAUTH_CLIENTS` JSON 객체 하나로 통합했다. `ValidateRedirectTarget`이 요청 `client_id`의 목록만 대조하므로 교차곱 허용 집합이 구조적으로 사라졌다. 클라이언트 간 redirect_uri 격리 회귀 테스트와 구성 거부 사례를 함께 뒀다.

#### - [x] 31. 색인 작업자의 제공자 응답 처리에 상한과 원인 기록이 없다

`internal/index/index.go:152`~`:156`, `:198`

- 응답 본문 크기에 상한이 없어 잘못 설정된 제공자가 큰 응답을 보내면 작업자와 검색 경로의 메모리가 급증한다.
- HTTP 상태, 차원 불일치 값, 연결 오류를 로그나 작업 행에 남기지 않고 고정 문구만 기록한다. `SDD.md` 「색인 재시도」가 운영자 개입이 필요하다고 한 차원 불일치를 구분할 수 없다.

- 근거: 판독.

#### - [x] 32. 색인 작업자의 `Close` 뒤 `Start`가 panic을 낸다

`internal/index/index.go:88`~`:95`

`done`을 두 번 닫는다. 주기 작업자는 `startOnce`를 먼저 소비해 막지만 색인 작업자에는 같은 방어가 없다. 현재 `main` 순서에서는 발생하지 않는다.

- 처리 한계: 주기 작업자와 같은 방어를 넣고 Close→Start→Close 순서가 통과하는 회귀 테스트를 뒀다. 다만 옛 동작의 panic은 취소 뒤 고루틴에서 일어나므로 테스트가 결정적으로 재현하지는 못한다.

- 근거: 판독.

#### - [x] 33. 요청 종료 대기가 초과되면 작업자를 닫지 않고 풀을 닫는다

`cmd/server/main.go:134`~`:136`, `:51`

30초를 넘으면 곧바로 반환해 주기 작업자와 색인 작업자의 `Close`를 건너뛰고, defer된 `database.Close()`가 빌려 간 연결 반환을 기다리며 블록한다. `SDD.md`의 "프로세스 종료에 맡긴다"가 늦어질 수 있다.

- 근거: 판독.

#### - [x] 34. 직접 TLS 종단에서 상태 확인 경로가 평문으로 응답하지 못한다

`cmd/server/main.go:150`~`:154`

`TLS_TERMINATION=direct`면 수신기가 TLS 전용이라 평문 프로브에 400이 난다. `SDD.md` 「기동과 종료」의 "두 경로는 평문으로도 응답한다"를 직접 종단에도 적용하는지 확인이 필요하다.

- 판단: 코드가 아니라 명세를 고쳤다. 평문 프로브까지 받으려면 수신 주소를 하나 더 열어야 하는데 「배포 구성」에 없는 값을 늘릴 수 없다. 「기동과 종료」의 전송 보안 예외를 애플리케이션 TLS 판정에만 걸리는 것으로 정확히 하고, 직접 종단에서는 프로브도 TLS로 보낸다는 점을 적었다.

- 근거: 판독.

#### - [x] 35. 소유권 이전 감사 분류가 명세와 반대다

`internal/store/web.go:42`~`:45`

`SRS.md`와 `SDD.md`는 다른 계정에 소유자 등급을 부여하는 것을 이전으로 본다. 구현은 owner 부여를 `grant`로, owner에서 강등하는 것을 `transfer`로 기록한다.

- 근거: 판독.

#### - [x] 36. 컨텍스트 `graph_id` 인덱스를 저장소 질의가 쓰지 못한다

`migrations/001_init.sql:279`~`:280`

인덱스가 `properties -> 'graph_id'`(agtype)로 만들어졌고 질의는 `->>`로 비교한다. 키워드, 시간, 재색인, 삭제 영향 질의가 매번 label 전체를 훑는다.

- 근거: 재현. `enable_seqscan=off`에서도 Seq Scan.

#### - [x] 37. `EMBEDDING_VECTOR_TYPE=bit`로 배포하면 마이그레이션이 실패한다

`internal/migrate/migrate.go:37`, `migrations/003_context_embedding_hnsw.sql:7`

설정 검증이 `bit`를 허용하지만 pgvector에 `bit_cosine_ops`가 없다. 벡터 문자열 형식도 bit 열에 맞지 않는다.

- 근거: 재현. `pg_opclass`에 `bit_hamming_ops`, `bit_jaccard_ops`만 있다.

#### - [ ] 38. 그래프 목록 커서의 기준 값이 페이지 사이에 바뀐다

`internal/store/graph.go:140`~`:143`

1차 키 `last_activity_at`이 쓰기로 바뀌어 페이지를 넘기는 사이 행이 이동하면 중복이나 누락이 생긴다. `SDD.md`는 동일 시각 문제만 다룬다. 설계상 감수한 것인지 확인이 필요하다.

- 근거: 판독.
- 추가 확인: 실제 증상은 누락이다. 활동 시각은 늘기만 하므로 이미 본 행이 뒤로 가지 않아 중복은 나지 않고, 아직 보지 않은 행이 활동해 앞으로 이동하면 커서 조건 `last_activity_at < 커서`에 걸려 영영 보이지 않는다.
- 보류 사유: 커서에 첫 페이지 시각을 담아 그 뒤의 활동을 거르는 방법을 만들어 시험했으나 누락을 막지 못했다. 앞으로 이동한 행은 스냅샷 조건에서도 똑같이 걸러진다. 정렬 키가 변하는 keyset 페이지 처리에서 누락을 없애려면 정렬 키를 불변 값으로 바꾸거나 반복 읽기 트랜잭션으로 목록 전체를 한 번에 읽어야 하는데, 앞은 「그래프 목록 화면」이 확정한 최근 활동순을 바꾸는 요구사항 변경이고 뒤는 페이지 처리를 두는 이유를 없앤다. 사용자 판단을 받는다.

#### - [x] 39. `cypherString`이 JSON 변환 오류를 무시한다

`internal/store/context.go:663`~`:666`

잘못된 UTF-8이면 빈 문자열을 돌려 문법 오류 질의가 되고 `invalid_argument`가 아닌 `internal`로 끝난다. 주입은 아니며 MCP JSON 경로로는 도달하기 어렵다.

- 근거: 판독.

#### - [x] 41. HTTP 처리 경로에 panic 차단막이 없다

`cmd/server/server.go`, `internal/mcp/transport.go`

저장소 전체에 `recover()`가 한 곳도 없다. MCP 처리기는 `arguments["hops"].(float64)`(`internal/mcp/handler.go:246`, `:250`)처럼 검사 없는 타입 단언을 여러 곳에서 쓰고, 지금은 `schema.go:104`의 `required(nonNegativeInteger)`가 앞에서 막는다. 즉 스키마와 처리기가 어긋나는 순간 `internal`이 아니라 연결이 끊기고 구조화 로그에도 남지 않는다. 28번이 지적한 입력 검증의 느슨함과 같은 자리에 걸려 있다.

- 근거: 판독.

## 최적화 가능 지점

결함이 아니라 같은 동작을 더 적은 왕복과 자원으로 할 수 있는 자리다. 계약을 바꾸지 않으므로 발견 사항과 나누고, 번호는 이어서 붙인다. 효과가 큰 순서다.

### - [ ] 42. 모든 AGE 질의가 요청마다 서버 측 PREPARE를 유발한다

`internal/store/store.go:100`~`:113`, `internal/store/context.go:634`~`:636`

`cypherSQL`이 식별자와 본문을 SQL 문자열에 직접 박아 만들므로 질의 텍스트가 매번 유일하다. 그런데 풀 설정이 `DefaultQueryExecMode`를 지정하지 않아 pgx 기본값인 `QueryExecModeCacheStatement`가 쓰인다. 이 모드는 캐시에 없는 질의마다 `Prepare`를 먼저 보내고, 연결당 512개인 LRU가 차면 `DEALLOCATE`까지 낸다.

결과적으로 AGE 질의 하나당 왕복이 두 배가 되고 PostgreSQL의 계획 캐시는 적중률 0으로 채워졌다 버려진다. 컨텍스트 조회, 홉 탐색, 검색 후보 조립이 모두 이 경로를 지난다.

`public.*` 테이블 질의는 이미 파라미터를 쓰므로 캐시가 유효하다. 전역 설정을 바꾸면 그쪽 이득을 잃으므로 AGE 호출 지점만 좁혀 바꾸는 편이 낫다. pgx는 인자 목록에 실은 실행 방식을 그 호출에만 적용한다.

```go
rows, err := s.pool.Query(ctx, s.cypherSQL(query, "node agtype"), pgx.QueryExecModeExec)
```

openCypher 파라미터로 질의 텍스트 자체를 고정하는 방법도 있으며, 그 경우 39번의 이스케이프 문제도 함께 사라진다.

- 근거: 판독. 기본 실행 방식과 캐시 용량은 `github.com/jackc/pgx/v5@v5.10.0`의 `conn.go:191`, `:288`, `:898`에서 확인했다.

### - [ ] 43. `operation_log`와 `web_audit_log`에 `graph_id` 인덱스가 없다

`migrations/001_init.sql:133`, `:160`, `:261`~`:266`

`operation_log`의 인덱스는 `(context_id, applied_at)`과 `(relation_id, applied_at)` 둘뿐이고 `web_audit_log`에는 기본 키 말고 인덱스가 없다. 그런데 실제 질의는 대부분 `graph_id`로 거른다.

| 질의 | 위치 | 현재 |
|------|------|------|
| 감사 기록 화면 | `internal/store/web.go:785`~`:797` | 두 테이블 전체 스캔 |
| 대기 복구 요청 | `internal/store/web.go:616`~`:626` | 전체 스캔에 상관 `NOT EXISTS` |
| 기록 보존 정리 | `internal/store/periodic.go:166`~`:180` | 그래프마다 3회, 매회 전체 스캔 |
| 팀 감사 기록 정리 | `internal/store/periodic.go:107`~`:111` | 전체 스캔 |

보존 정리는 그래프 수와 테이블 크기의 곱으로 늘어난다. 인덱스를 더하고, 그래프별 반복 대신 `context_graph`와 조인한 한 문장으로 줄일 수 있다.

```sql
CREATE INDEX operation_log_graph_applied_idx ON public.operation_log (graph_id, applied_at);
CREATE INDEX web_audit_log_graph_occurred_idx ON public.web_audit_log (graph_id, occurred_at DESC);
CREATE INDEX web_audit_log_team_actor_idx ON public.web_audit_log (actor_account_id, occurred_at) WHERE graph_id IS NULL;
```

- 근거: 판독.

### - [ ] 44. 근거·구성원 검증과 참조 간선 생성이 대상 수만큼 왕복한다

`internal/store/context.go:424`~`:446`, `:449`~`:471`

`validateReferences`와 `validateEventMembers`가 대상마다 `s.context()`를 부르고, 그 `s.context()`는 다시 정점 조회와 근거 조회로 한두 번을 더 쓴다. `createReferenceEdges`도 대상마다 `CREATE`를 한 번씩 낸다. 근거 20개짜리 파생 하나를 저장하면 검증에 40여 회, 생성에 20회가 든다.

같은 파일의 `ContextsByIDs`(`:686`)가 이미 묶어 읽는 방식을 구현해 두었으므로 검증은 그것으로 대체할 수 있고, 간선 생성은 `UNWIND`로 한 문장에 묶을 수 있다.

- 근거: 판독.

### - [ ] 45. 근거·구성원 식별자만 필요한데 정점 전체를 받아 해석한다

`internal/store/context.go:527`~`:534`, `:551`~`:558`

`derivedFromIDs`와 `eventMemberIDs`가 `RETURN evidence`, `RETURN member`로 본문을 포함한 정점 전체를 받아 `parseContext`로 조립한 뒤 `.ID`만 쓰고 버린다. 같은 파일의 `edgeTargetsBySource`(`:764`)는 이미 식별자만 받는다. `RETURN evidence.context_id`로 맞추면 전송량과 해석 비용이 근거 수에 비례해 줄어든다.

- 근거: 판독.

### - [ ] 46. 홉 탐색이 깊이마다 label과 방향으로 질의를 쪼갠다

`internal/store/hop.go:84`~`:113`

깊이, label, 방향의 삼중 반복이 `hopNeighbors`를 부른다. 필터를 비우는 시각화 경로는 label 7종에 방향 2가지라 깊이마다 14회 왕복이고, 결과 상한에 걸려 더 담지 않기로 정한 뒤에도 남은 label과 방향의 질의를 계속 낸다.

openCypher의 label 교대와 무방향 패턴으로 묶으면 깊이마다 한두 번으로 줄고, 상한에 닿으면 반복을 빠져나가는 조건만 더해도 낭비가 사라진다. 20번을 고칠 때 이 함수를 같이 손대게 되므로 함께 처리하는 편이 낫다.

- 근거: 판독. AGE 배포 버전이 label 교대를 지원하는지 먼저 확인해야 한다.

### - [ ] 47. 색인 작업자가 유휴 상태에서도 초당 한 번씩 데이터베이스를 친다

`internal/index/index.go:109`, `:122`~`:133`, `internal/store/index.go:64`~`:78`

1초 주기로 `RunOnce`를 돌리고 회차마다 트랜잭션을 열어 `FOR UPDATE SKIP LOCKED` 질의를 낸다. 대기 작업이 없어도 영구히 초당 한 트랜잭션이다. 빈 회차에 물러나는 간격을 두거나 등록 시점에 알림을 보내고 작업자가 기다리게 하면 유휴 부하가 없어진다.

작업을 한 번에 하나씩만 처리하는 것도 처리량 상한이 된다. 제공자 호출이 최대 30초이므로 색인 속도가 제공자 왕복 한 번으로 묶인다. 9번을 고쳐 잠금 밖으로 호출을 빼면 여러 작업을 한 요청으로 묶는 여지도 생긴다.

- 근거: 판독.

### - [ ] 48. 시각화 자산이 캐시 헤더 없이 매번 다시 내려간다

`internal/web/web.go:111`~`:112`

`http.ServeFileFS`만 부른다. `embed.FS`의 파일은 수정 시각이 제로 값이라 `Last-Modified`가 실리지 않고 `ETag`도 붙지 않는다. 조건부 요청이 성립할 수 없으므로 그래프 상세 화면을 열 때마다 435,503바이트를 전부 다시 받는다.

내용이 빌드에 고정되므로 오래 유지되는 `Cache-Control`과 내용 해시로 만든 `ETag`를 붙이면 된다.

- 근거: 판독.

### - [ ] 49. 의미 관계 후보 질의가 임베딩 자기 조인으로 전체를 훑는다

`internal/store/index.go:485`~`:498`

`context_embedding`을 자기 자신과 조인하고 `ORDER BY target.embedding <=> candidate.embedding`으로 정렬한다. 정렬 기준이 열이 아니라 두 열의 연산이라 `context_embedding_embedding_hnsw_idx`를 쓰지 못하고 같은 그래프의 모든 임베딩에 대해 거리를 계산한다. 대상 벡터를 먼저 한 행으로 읽어 파라미터로 넘기면 인덱스를 쓰는 형태가 된다.

유사도 하한 판정도 Go에서 한다(`:511`). 상한까지 뽑은 후보가 임계값 미달로 모두 걸러지면 기준을 넘는 후보가 있어도 놓친다. 임계값을 질의의 조건으로 내리는 편이 정확하고 빠르다.

- 근거: 판독.

### - [ ] 50. 의미 유사도 검색이 벡터 인덱스의 후처리 필터로 결과를 잃을 수 있다

`internal/store/index.go:362`~`:377`

`ORDER BY candidate.embedding <=> $3::vector LIMIT $5`에 `graph_id`, `model_id`와 폐기·유효 기간 조건이 함께 붙는다. HNSW는 전역 근접 이웃을 먼저 꺼낸 뒤 조건을 적용하므로, 그래프가 여럿이고 각 그래프가 작으면 상한을 채우지 못하거나 0건이 될 수 있다. 논리 격리로 여러 그래프를 한 테이블에 두는 이 배포에서는 실제로 걸릴 수 있는 형태다.

탐색 폭을 상한보다 크게 두거나, 그래프로 거른 부분 인덱스를 두거나, 넉넉히 뽑아 조건 적용 뒤 자르는 방법 중 하나가 필요하다.

- 근거: 판독. pgvector 배포 버전의 반복 탐색 지원 여부를 먼저 확인해야 한다.

### - [ ] 51. 작은 낭비

- `internal/index/index.go:180`: `strings.NewReader(string(body))`가 요청 본문을 한 번 더 복사한다. `bytes.NewReader(body)`면 된다.
- `internal/store/index.go:521`~`:527`: `vectorText`가 차원 수만큼 `fmt.Sprintf("%g")`를 돈다. 검색 한 번과 색인 한 번마다 차원 수만큼이다. `strconv.AppendFloat`에 버퍼를 미리 잡으면 할당이 사실상 사라진다.
- `internal/store/store.go:103`~`:113`: 풀의 연결 수와 수명을 설정하지 않는다. 접속 문자열로 덮을 수는 있으나 웹, MCP, 색인 작업자, 주기 작업자가 같은 풀을 나눠 쓰고 40번처럼 연결을 겹쳐 잡는 경로가 있으므로 기본값을 코드에서 정하는 편이 안전하다.
- `internal/authz/authz.go:281`~`:287`: 29번이 지적한 요청마다의 조회와 같은 자리에서 JWK 해석과 공개 키 복원도 요청마다 반복한다. `kid` 기준 캐시를 만들 때 해석 결과까지 함께 담으면 된다.

## 남은 확인 한계

| 한계 | 내용 |
|------|------|
| 판독 항목 | 판독으로만 확인한 발견 사항은 수정 전에 재현 테스트를 먼저 작성해 확인해야 한다 |
| 명세 확인 필요 | 30번과 38번은 명세가 해당 경우를 직접 정하지 않아 결함 여부의 판단이 필요하다. 12번, 25번, 34번은 처리하면서 판단했고 근거를 각 항목에 적었다 |
| 2차 검사 미측정 | 40번, 41번과 42번부터 51번까지는 실 데이터베이스와 부하 없이 판독으로만 확인했다. 효과 순서는 추정이므로 고치기 전에 질의 계획과 측정으로 확인해야 한다 |
| 배포 버전 확인 필요 | 46번은 AGE의 label 교대 지원 여부, 50번은 pgvector의 반복 탐색 지원 여부에 따라 방법이 달라진다 |
| 불안정한 테스트 | `TestWorkerProposesSimilarEventRelationsAfterIndexIntegration`이 단독 실행 5회 중 1회 "의미 관계 후보가 만들어지지 않았다"로 실패했다. 실패한 실행은 5~8초, 통과한 실행은 0.09초였다. 공유 DB에 남은 다른 색인 작업이 루프를 소모하는 격리 문제로 추정하나 원인을 확정하지 못했다. `go test ./...`로 패키지를 함께 돌리면 "대기 중인 그래프 색인 작업을 찾지 못했다"로도 실패하며, 4번과 5번 수정 전후 각각 5회를 돌려 양쪽 모두 1회씩 실패했다. 이 수정과 무관한 기존 문제다 |
| 개발 DB 부작용 | 9번 재현 테스트가 개발 DB에 있던 pending 색인 작업 2건(context_id `01a09ad3-9f0f-75d1-9a28-bca16034eb9e`, `01a0a053-10a5-729c-a195-753f194d5f3f`)을 잡아 `last_error='slow'`와 시도 횟수 증가를 남겼다. 되돌리지 않았다 |
| 색인 작업 테스트 격리 | `internal/index`와 `internal/store`를 `go test ./...`로 함께 돌리면 두 프로세스가 같은 개발 DB의 `index_task`를 서로 집어 간다. `ProcessNextIndexTask`가 그래프를 가리지 않고 대기 행 하나를 집기 때문이다. `TestWorkerProposesSimilarEventRelationsAfterIndexIntegration`, `TestIndexRetryAndImmediateFailureIntegration`, `TestIndexTaskClaimConcurrency`가 번갈아 실패하며, 검사 대상 커밋 `d07509d`에서도 3회 중 1회 재현된다. `-p 1`로 순차 실행하면 3회 모두 통과한다. 이 문서의 수정과 무관한 테스트 격리 문제이며 별도 항목으로 다룰지 판단이 필요하다 |
| 브라우저 미확인 | 웹 화면 결함은 HTTP 응답과 코드 판독으로 확인했고 실제 브라우저로는 확인하지 않았다 |

## 개발 계획 체크박스

다음 항목은 완료로 표시되어 있으나 이 검사에서 성립하지 않음을 확인했다. 3단계 절의 서술이 OAuth 2.1 Authorization Code with PKCE를 반영했다고 적었으나 발급 경로가 HTTP에 연결되지 않은 것도 1번과 같은 원인이다. 체크박스는 이 문서에서 바꾸지 않았으며 수정 계획과 함께 판단한다.

| 단계 | 항목 | 관련 발견 사항 |
|------|------|----------------|
| 3단계 | 접근 토큰의 1시간 수명과 만료 전 10초 자동 갱신을 구현한다 | 1번 |
| 3단계 | 마지막 소유자와 마지막 접근 가능 계정 판정을 그래프 행 잠금으로 직렬화한다 | 2번, 8번, 10번 |

## 종합

인증 토큰 검증, TLS 판정, `graph_id` 격리, XSS 방어처럼 경계를 판정하는 코드는 대체로 확정된 계약대로였다. 결함은 세 자리에 몰려 있다.

첫째는 조립되지 않은 코드다. 인가 서버의 발급 경로와 토큰 갱신은 구현과 단위 테스트가 모두 있지만 HTTP 계층에 연결되지 않아, 서비스의 주 진입점인 MCP가 토큰을 받을 수 없는 상태다. 단위 테스트가 함수 단위로 통과해 체크박스가 표시된 것이 이 공백을 가렸다.

둘째는 동시성이다. 관계 중복, 폐기의 이중 적용, 동시 갱신 오류의 잘못된 사상은 모두 순차 테스트로는 드러나지 않고 동시 요청으로만 재현됐다. 저장소 통합 테스트는 대부분 한 요청씩 실행하므로 같은 자리를 계속 놓친다.

셋째는 판정의 입력이다. 마지막 소유자와 자동 삭제 복구 결함은 판정 함수 자체가 아니라 판정을 부르지 않는 경로나 판정이 세는 대상에 있었다. 권한 불변식을 바꾸는 모든 경로를 한 번씩 부르는 통합 테스트가 필요하다.

수정은 1번을 먼저 하고, 불변식이 깨지는 2번부터 8번을 이어서 처리하는 것이 좋다.

2차 검사의 최적화 항목은 성격이 다르다. 개별 함수가 느린 것이 아니라 두 가지 구조에서 비용이 나온다. 하나는 AGE 질의를 문자열로 조립하는 방식이며, 질의 텍스트가 매번 달라지는 탓에 42번처럼 준비 문장 캐시가 무력해지고 44번과 45번처럼 대상 수만큼 왕복이 늘어난다. 다른 하나는 `public.*` 테이블의 인덱스가 기록 조회의 실제 조건을 따라오지 못한 것이며 43번이 그 자리다. 36번과 같은 원인이 AGE 쪽에서 나타난 것이라 함께 보는 편이 낫다.

최적화는 계약을 바꾸지 않으므로 발견 사항 수정보다 뒤에 두어도 된다. 다만 42번과 43번은 변경 범위가 좁고 효과가 전 경로에 걸치며, 46번은 20번을, 51번의 마지막 항목은 29번을 고칠 때 같은 코드를 손대게 되므로 해당 발견 사항과 함께 처리하는 것이 낫다.
