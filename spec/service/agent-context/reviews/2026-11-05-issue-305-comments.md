# #305 추가 코멘트의 절감안·스키마·호스트 영향 재검토

## 범위와 결론

사용자가 추가한 #305 코멘트 3개를 현재 브랜치 89d0afa와 대조했다. 대상은 [잔여 비용·지침 링크](https://github.com/Alpha-gone/agent-context-sharing/issues/305#issuecomment-6646620394), [node_create 구조 개선](https://github.com/Alpha-gone/agent-context-sharing/issues/305#issuecomment-6647262904), [나머지 13종·공통 안내](https://github.com/Alpha-gone/agent-context-sharing/issues/305#issuecomment-6648303552)다. 최초 분석 코멘트도 함께 확인했다.

추가 절감 여지는 있다. 다만 제안의 수치는 후속 압축 전 기준이고, 하위 객체 예시는 계층 선택·조건부 필수를 완전히 강제하지 않는다. 공개 힌트 제거·입력 구조 변경·승인 정책에 영향을 줄 수 있는 annotations 변경을 모두 계약 무변경으로 취급할 수 없다. 이 분석 단계에서는 제품 코드·정본 계약·스냅샷·GitHub 내용을 변경하지 않았으며, 사용자 채택 후 구현은 말미의 적용 절에 구분한다.

## 측정 기준과 수치 정정

현재 tools는 16,965바이트, instructions는 638바이트다. 코멘트의 기준 16,850·1,311바이트와 다르다. graph_get의 대표 요약 안내와 node_update의 원천 정정·파생 대체 안내가 추가됐고 공통 instructions가 축소됐다. 기존 1,311→1,061바이트 제안은 현재 안내보다 423바이트 커진다. 기존 측정을 현재 추가 절감률로 그대로 사용할 수 없다.

node_create는 여전히 4,378바이트·24개 필드다. description을 모두 지운 객체는 2,089바이트로 코멘트의 바이트 계산과 일치한다. 다만 차이 2,289바이트에는 description 키·구두점도 포함되며, 설명 문자열 자체의 합은 1,830바이트다. 동일 o200k_base로 측정하면 현행 964토큰·설명 전부 제거 시 551토큰이다. 코멘트의 약 1.4k·0.8k 수치는 같은 인코딩의 실측이 아니며 Claude의 실제 사용량으로 확인되지 않았다. 2,089바이트도 현 스키마에서 설명만 제거한 값이지 모든 설계의 절대 하한은 아니다.

다음은 제품을 수정하지 않고 공개 JSON 복사본에서 요소를 제거한 비교다. 직렬화는 compact tools 배열 + 줄바꿈 + instructions이며, instructions 638바이트는 모든 후보에서 고정했다. 설명 삭제 후보는 호출 정확도를 검증한 적용안이 아니다.

| 독립 비교 또는 조합 | tools 바이트 | 전체 바이트 | o200k_base | 현재 대비 토큰 감소 |
|---|---:|---:|---:|---:|
| 현재 89d0afa | 16,965 | 17,604 | 4,019 | — |
| 빈 allOf만 제거 | 16,913 | 17,552 | 3,999 | 0.5% |
| 0인 minLength/minItems만 제거 | 16,910 | 17,549 | 3,999 | 0.5% |
| 위 두 정리 | 16,858 | 17,497 | 3,979 | 1.0% |
| 공통 5개 필드 설명만 생략 | 15,935 | 16,574 | 3,809 | 5.2% |
| 의미상 빈 제약 + 공통 설명 생략 | 15,828 | 16,467 | 3,769 | 6.2% |
| 위 조합 + uuid-v7/base64url format 생략 | 15,178 | 15,817 | 3,565 | 11.3% |

공통 5개 필드는 graph_id·context_id·agent_id·request_id·expected_version이다. judgment_input은 의미가 자명하지 않아 이 조합에서 제외했다. 각 독립 행의 절감량을 조합 결과와 다시 더하지 않는다. 코멘트의 tools 12,197바이트 후보는 완성된 전체 JSON이 제공되지 않아 동일 결과를 재현했다고 판정하지 않는다. 추가 문구 압축으로 더 줄일 가능성과 그 문구의 의미 보존 여부는 별도 확인 대상이다.

## 제안별 판정

| 코멘트 항목 | 재검토 결과 |
|---|---|
| 빈 allOf 정리 | 실제로 4곳 존재한다. graph_create.name·graph_update.name·node_create.conversation_id·context_flow_get.work_context다. allOf 안의 빈 참 스키마를 제거하는 것은 가능하다. 일반적인 빈 객체 재귀 삭제로 구현하면 const·examples 같은 데이터나 다른 조합식의 의미를 손상할 수 있어 기존 스키마 경로 순회를 유지해야 한다. |
| 0인 기본 하한 정리 | minLength 0이 3곳, minItems 0이 1곳이다. 해당 키를 생략해도 같은 문자열·배열을 허용한다. 두 구조 정리의 합은 40 BPE 토큰으로 효과는 작다. |
| 비표준 format | 전체 uuid-v7 32회·base64url 2회는 확인됐다. node_create의 uuid-v7은 9회가 아니라 7회다. 제거하면 힌트를 줄일 수 있지만 내부 형식 검증은 반드시 유지해야 한다. 불투명 커서가 base64url 형식인 것은 모순이 아니다. 모델에게 해독·합성을 요구하지 않는다는 의미다. |
| 공통 설명 | 식별자·재시도·판의 반복 설명은 줄일 후보가 맞다. 다만 필드별 설명을 없애도 현재 instructions·도구 안내가 같은 행동 규칙을 전달하는지 확인해야 한다. 특히 판단 입력까지 일괄 삭제하는 것은 별도다. |
| judgment_input | 현재 설명은 생략 시 인자 기록만 설명하고 여러 쓰기에 같은 판단을 남기는 용도를 드러내지 않는다. SDD와 실제 write_handlers.go가 그 용도를 확인시켜 준다. 명확한 짧은 설명을 보강할 가치가 있다. 공통 instructions로 옮기면 현재 700바이트 예산과 상시 적재 비용도 다시 계산해야 한다. |
| enum·배열 설명 | 중복 허용 문구는 압축 후보다. 그러나 traversal_filter의 생략/빈 배열=전체 탐색, direction의 저장 간선 방향, source_ref.channel과 origin_kind의 다른 역할은 enum만으로 전달되지 않는다. 설명 전체를 일괄 삭제할 근거는 부족하다. |
| graph_list·relation_list | 커서 재전달·첫 페이지 규칙을 짧게 유지할 수 있다. 필터 기본값과 페이지 크기의 기본/상한을 전부 min 1로 대체하면 의미가 빠진다. 플랜 상한을 선택할 수 없어도 초과 시 실패한다는 정보는 호출에 유용하다. |
| graph_create·graph_get·graph_update | 반복 이름·ID 설명을 줄일 수 있다. 갱신의 생략=유지·빈 문자열=비우기·null=대표 해제는 서로 다른 계약이므로 통합 설명에도 구분을 남겨야 한다. graph_get은 후속 커밋에서 대표 요약 안내가 이미 추가됐다. |
| node_get·context_flow_get | 현재 instructions에는 절단·부분 결과 안내가 없다. 코멘트처럼 도구 설명을 지우면 유일한 안내가 없어지는 부분이 있다. 특히 budget이 토큰이 아닌 본문 문자 수라는 단위를 유지해야 한다. |
| relation_confirm·relation_discard | 같은 그래프·재확정 설명의 중복은 줄일 수 있다. 활성 사건만 허용, 방향·시간 제약, 원천/파생의 연결 도구, 확정 실패가 앞선 노드 생성을 롤백하지 않는다는 행동 정보는 남겨야 한다. |
| node_discard·node_restore 통합 | 입력 스키마와 현재 annotations가 같다는 관찰은 맞다. 연산은 재폐기·재복구 거부, 복구 채널 등 의미가 다르다. 통합은 도구명·개수·입력·재시도 정체성·호스트 승인 설정을 바꾸는 계약 변경이다. 현재 압축 작업의 단순 정리로 취급하지 않는다. |
| HTTP 지침 링크·MCP Resource/guide 도구 | 상세 설명을 늦게 읽는 대안으로 타당하지만 자동 조회나 무료 압축을 보장하지 않는다. 외부 URL은 별도 권한·인증 전달이 필요할 수 있고 MCP Resource도 호스트의 모델 조회 수단을 확인해야 한다. 조회가 추가되면 왕복과 결과 이력이 생기므로 실제 읽기/쓰기 작업량으로 비교해야 한다. |

## 하위 객체 예시의 실제 검증 결과

코멘트의 create·update JSON 블록을 그대로 추출해 적용 판 jsonschema-go v0.4.3의 Resolve·Validate로 검사했다. 계층 선택·조건을 위반한 7개 입력과 생략 의미가 바뀌는 update 1개를 대조했다. 다음 8개 표본이 모두 제안 스키마를 통과했다. 실제 서버가 이 입력을 수락했다는 뜻은 아니다.

- create: source/derived/event를 모두 생략.
- create: 세 블록을 동시에 전달.
- create: summary인데 summary_scope 생략.
- create: opinion인데 confidence_state 생략.
- create: source.conversation_id를 넣고 turn_index 생략.
- create: previous_event_id를 넣고 conversation_id 생략.
- update: derived/event 모두 생략.
- update: derived/event 동시 전달.

하위 required는 그 객체가 존재할 때만 적용되고, 선택 객체 여러 개의 상호 배타성을 보장하지 않는다. summary/opinion·대화 쌍·이전 사건의 조건은 예시에서 여전히 서술에 의존한다. 허용 객체와 모드 선택을 구조로 완결하려면 추가 제약·서버 검증과 호스트 인수가 필요하며 그 크기도 측정에 포함해야 한다. [JSON Schema 객체](https://json-schema.org/understanding-json-schema/reference/object)·[조합식](https://json-schema.org/understanding-json-schema/reference/combining)의 의미와 일치한다. [Claude 공식 문서](https://code.claude.com/docs/en/mcp#tool-input-schemas-with-a-root-level-combinator)는 properties 내부 조합식을 허용한다고 설명하지만 모든 호스트의 수용을 보장하지는 않는다.

update의 모두 생략=keep 제안은 특히 의미 변경이다. 현재 기본은 update이고 patchNode는 가변 필드가 없어도 판을 증가시킨다. keep은 명시적 management_action으로 판을 검사하고 상태 전이 없이 관리 기록을 남긴다. 생략을 keep으로 바꾸는 것은 기존 호출과 같은 동작이 아니다. 저장 노드의 계층과 입력 블록이 다른 경우도 여전히 조회 후 검사해야 한다. 현재도 patchNode가 교차 계층 가변 필드를 거부하므로 이를 서버 검증 누락으로 해석하지 않는다.

구조 변경에는 도구 정의·핸들러·명세·스냅샷 외에도 WriteRunner.normalizeIDs를 포함해야 한다. 현재 UUID 정규화는 최상위 스칼라와 배열만 처리한다. 중첩하면 내부 ID까지 정규화하도록 바꾸거나 평면 도메인 입력으로 변환하는 경계가 필요하다. request_id 재생 비교는 정규화된 tool+arguments JSON을 사용하므로 기존 입력과 새 입력의 정체성·24시간 재생 호환성도 설계해야 한다. 새 도구명으로 분리·통합할 때도 같은 문제가 있다.

3도구 분리안은 같은 설명 수준의 단일 도구안과 비교해야 한다. 기존 평면안과 압축한 분리안을 비교한 순감소를 도구 분리 자체의 효과로 해석할 수 없다. 지연 적재에서 실제로 몇 개 계층을 쓰는지에 따라 유리한 쪽이 달라지며, 원천 생성의 자연 키 중복 반환도 감사·재생·삭제 원천 처리와 함께 확인해야 한다.

## annotations와 별도로 확인한 호출 결과 비용

MCP의 destructiveHint=false는 단지 삭제 API가 아니라는 뜻이 아니라 가산적 갱신만 한다는 힌트다. 현재 relation_confirm은 신규 생성뿐 아니라 proposed/discarded 관계를 confirmed로 바꾸고 확인 주체·시각을 갱신한다. node_restore는 폐기 시각을 지우고 파생/사건의 판과 관리 기록을 바꾼다. 이것만으로 false가 잘못이라고 단정할 수도, 삭제가 없으므로 false로 바꿔야 한다고 단정할 수도 없다. 상태 전이 전체를 기준으로 정책을 정해야 한다.

또한 true이면 Claude Code에서 매번 승인을 요구한다는 인과관계는 확인되지 않았다. [MCP](https://modelcontextprotocol.io/specification/2026-07-28/schema#toolannotations)는 이를 힌트로 정의하며, Claude 공식 문서는 [도구별 승인 규칙](https://code.claude.com/docs/en/permissions#mcp)과 [매번 승인용 requiresUserInteraction 메타데이터](https://code.claude.com/docs/en/mcp#require-approval-for-a-specific-tool)를 따로 설명한다. 이번 검토에서는 실제 승인 UI를 재현하지 않았다. [idempotentHint·readOnlyHint](https://code.claude.com/docs/en/mcp#mark-tools-that-are-safe-to-call-twice)는 연결 종료 후 재호출·병렬 호출에 사용될 수 있어 단순 토큰 절감을 위해 지우거나 바꾸면 안 된다.

기존 실제 Codex 실행 캡처에서는 20개 MCP 응답 모두 content text와 structured_content를 포함했고 두 JSON 값이 모두 같았다. 텍스트 본문 합은 13,234 UTF-8 바이트, 구조화 JSON의 compact 합은 12,926바이트다. 실제 모델에 두 사본이 모두 들어갔는지는 해당 캡처만으로 알 수 없다. [MCP는 구조화 결과의 호환 텍스트도 권고](https://modelcontextprotocol.io/specification/2026-07-28/server/tools#structured-content)하므로 서버에서 한 사본을 즉시 제거할 근거는 부족하다. 다음 실사용 비용 분석에서는 호스트의 결과 변환과 반복 조회 본문을 함께 확인할 가치가 있다.

## 판단과 검증 범위

현재 PR 범위에 추가하기 가장 작은 후보는 빈 제약과 공통 설명 정리이며 약 6.2%의 비교 BPE 감소가 확인됐다. 형식 힌트 제거는 최대치가 아닌 별도 측정 후보로 두고, 하위 객체화·도구 분리/통합·annotations 정책은 각각 호환성·호스트 행동을 검증한 뒤 판단하는 것이 적절하다. 지침 링크 분리보다 먼저 중복 제거의 이익과 잘못된 호출의 비용을 함께 보아야 한다. 이전 실제 모델 실행의 최상위 channel 오류는 입력 구조의 혼동 가능성과 일치하지만 단일 표본이므로 평면 스키마가 원인이라고 확정하지 않는다.

이 분석 단계의 검증은 공개 JSON 복사본의 크기·BPE 재측정, 코멘트 JSON의 실제 공급자 검증기 표본, 업무 코드·정본·공식 문서 대조와 기존 합성 실행 캡처 분석이다. 모델·Claude CLI·운영 서버·DB를 새로 실행하지 않았다. 제품 변경이 없어 전체 Go 회귀를 반복하지 않았다. 분석 문서·요청 기록·외부 근거만 유지하며 소유 경계가 바뀌지 않아 DOX와 운영 매뉴얼은 유지한다.

## 채택된 우선 정리의 구현과 검증

사용자가 빈 제약과 반복 설명 정리를 우선 적용해 PR에 반영하도록 요청했다. 기준은 89d0afa이며 기존 fix/305-mcp-tool-context와 [Draft PR #307](https://github.com/Alpha-gone/agent-context-sharing/pull/307)을 사용한다. 작업 전 원격 HEAD가 로컬 기준과 같음을 확인했다.

- 공개 복사본의 스키마 위치에서만 minLength/minItems 0과 allOf의 빈 참 항목을 생략한다. pattern을 제거해 남은 빈 객체뿐 아니라 공급자 직렬화의 true도 처리한다. 남은 항목이 없는 allOf는 생략한다. anyOf/oneOf/not의 빈 분기·false와 const/enum/examples/default의 리터럴 데이터는 유지한다.
- graph_id·context_id·agent_id·request_id·expected_version의 반복 설명만 생략한다. 형식·필수·길이·공통 안내는 유지하며, 판단 입력·참조·계층 조건·방향·단위·기본값·생략/null·부분 결과 안내는 유지한다. 내부 검증·annotations·도구 이름/개수·입력 구조·instructions는 바꾸지 않았다.
- 실제 공개 tools는 16,965→15,828바이트(-6.7%), instructions는 638바이트로 동일하다. 합계 직렬화는 17,604→16,467바이트(-6.5%), o200k_base는 4,019→3,769(-6.2%), cl100k_base는 3,853→3,601(-6.5%)이다. 최초 f601e50의 o200k_base 11,969 대비 감소는 68.5%다. 이는 비교 BPE이며 실제 Claude 과금량이 아니다.
- 전체 tools 상한은 17,500→16,000바이트, 도구당 상한은 5,000→4,500바이트로 강화했다. 최대 도구 node_create는 4,262바이트이며 instructions 상한은 700바이트를 유지한다. MCP·시험 SDD를 먼저 정합화했다.

변경 전 빈 조건 정리 시험과 새 예산 시험은 각각 잔여 제약과 16,965바이트 초과로 실패했다. 구현 뒤 스키마 검증기로 원본/공개 복사본의 허용·거부 9개 표본을 대조했다. 비어 있지 않은 allOf 조건, 빈 배열/문자열 허용, anyOf/oneOf/not·false의 의미, const/default 데이터 보존을 확인했다. 공급자 라이브러리가 빈 Schema를 true로 직렬화하는 경계도 이 과정에서 확인해 처리했다.

최종 전체 internal/mcp 시험과 갱신 환경 변수 없는 실제 합성 DB의 MCP-09를 통과했다. 실제 서버·인증·HTTP·저장 경로에서 원천/파생/사건 생성·관계·수정·폐기·복구와 기존 오류 응답을 검증했다. 두 스냅샷은 정식 생성 경로로 갱신했다. MCP-09는 tools_list만 변경됐고 discover·업무 호출·오류 응답은 기준과 동일하다. 공개 JSON의 독립 비교에서 허용한 5개 필드 설명과 빈 제약 외 스키마 값·형식·annotations가 보존됐다.

Go 1.27.3의 go build ./...·go vet ./...·gofmt -l .을 통과했다. 독립 리뷰에서 blocker는 없었다. 링크·Markdown·diff와 DOX pass를 수행하며 소유 경계 변경이 없어 DOX와 운영 매뉴얼을 유지한다. 이번 추가 정리에서는 실제 Codex 모델·Claude CLI를 재실행하지 않았으며 앞선 모델 인수와 이번 실제 서버 회귀를 구분한다. 검증 후 작업 전용 합성 DB 컨테이너·익명 볼륨을 정리한다.
