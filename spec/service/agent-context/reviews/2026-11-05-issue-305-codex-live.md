# #305 공통 안내 추가 압축과 실제 Codex 검증

## 범위와 게시 상태

사용자는 영문 전환 결과를 먼저 PR로 게시하고, 분리 CLI 표시값도 줄이며 실제 모델 검증에는 Codex를 사용하도록 요청했다. 영문 전환까지의 a78252d를 dev 대상 Draft [PR #307](https://github.com/Alpha-gone/agent-context-sharing/pull/307)로 게시했다. 분류는 performance이며 자동 이슈 종료 대신 Refs #305를 사용한다. 해당 커밋의 Go static and vulnerability checks 실행 199는 성공했다. 아래 후속 변경은 같은 브랜치와 PR의 범위다.

앞선 원인 분석·스키마 투영·호스트 호환성·영문 전환 결과는 [11월 4일 기록](2026-11-04-issue-305.md)에 보존한다. 이번 기록은 그 이후의 측정과 검증만 소유한다.

## 항상 적재하는 공통 안내 축소

instructions에는 등록·그래프 선택·기대 판·재시도·권한 경계만 남겼다. 대표 요약은 graph_get, 원천 정정·파생 대체는 node_update에 안내하고, 참조·대화·복구·부분 결과는 기존 도구·필드 설명에 유지한다. 도구를 찾을 때 필요한 설명을 함께 읽도록 배치한 변경이다. 입력 필드·제약·annotations·응답·서버 검증은 바꾸지 않았다.

동일 compact JSON과 tools + 줄바꿈 + instructions 직렬화를 비교했다.

| 측정 | 영문 a78252d | 후속 압축 |
|---|---:|---:|
| tools UTF-8 바이트 | 16,850 | 16,965 |
| instructions UTF-8 바이트 | 1,311 | 638 |
| instructions o200k_base | 255 | 139 |
| instructions cl100k_base | 256 | 139 |
| tools+instructions o200k_base | 4,113 | 4,019 |
| tools+instructions cl100k_base | 3,947 | 3,853 |

공통 안내 바이트는 51.3%, 비교 BPE 합계는 o200k_base 2.3%·cl100k_base 2.4% 감소했다. 최초 f601e50의 o200k_base 11,969 대비 합계는 66.4% 감소다. instructions 회귀 상한은 1,500→700바이트로 강화하고 tools 전체 17,500·도구당 5,000바이트 상한은 유지했다. 새 상한은 변경 전 1,311바이트에서 실패하고 변경 후 통과했다.

같은 분리 Claude Code 2.1.318·일반 모드·Tool Search 조건에서 14종이 모두 loaded on-demand이며 도구 0, 서버 안내 대체 추정치는 440→216으로 50.9% 감소했다. 합성 API 키와 로컬 차단 서버를 사용했으며 모델 생성 요청은 수행하지 않았다. 토큰 계산 API 실패 시의 호스트 추정치이므로 실제 Claude 입력·과금 토큰 절감으로 해석하지 않는다.

## 실제 Codex 모델 호출

Codex CLI 0.165.0과 gpt-6-luna를 사용해 실제 모델이 영문 도구 정의를 읽고 인자를 생성하도록 했다. 기존 ChatGPT 로그인은 CLI가 사용했으며 자격 파일을 읽거나 복사하지 않았다. 사용자 설정은 무시하고 임시 작업 디렉터리·읽기 전용 샌드박스·로컬 MCP 하나를 사용했다. 셸·파일·웹·다른 에이전트 기능은 비활성화했다. 모델 응답이나 업무 입력을 대역으로 재생하지 않았다.

시험은 명시적인 codex_live 빌드 태그와 TEST_CODEX_LIVE=1을 함께 요구한다. 작업 전용 합성 PostgreSQL 컨테이너 안에서 시험별 새 DB를 만들고 실제 서버·인증·TLS·MCP·저장 경로를 사용한다. 임베딩 제공자는 로컬 실패 응답으로 격리했다. 서버 인증 토큰은 시험 계정의 토큰이며 환경 변수로 자식 CLI에만 전달한다. 사용자 운영 서버·기존 그래프와는 연결하지 않았다.

실행은 83.85초에 통과했다. MCP 호출은 20건이며 다음 실제 저장 결과를 모델의 최종 답변과 별도로 대조했다.

- 대화 원천 2개, 관찰 명제 1개, 사건 2개, 국소 요약 1개: 총 6개 활성 노드.
- 한국어 원문·대화 식별자·턴, 요약의 두 원천 근거와 두 사건 안내 참조, 그래프 대표 요약.
- 둘째 사건 생성 시 previous_event_id 지정과 첫째→둘째의 confirmed precedes 관계. 생성 요청 기록과 저장된 관계를 각각 확인했다.
- 같은 명제의 본문 수정→폐기→복구 성공 응답, 판 2→3→4와 활성→폐기→활성 상태. 최종 저장 판·본문·활성 상태도 확인했다.

첫 원천 생성에서는 모델이 공개 스키마에 없는 최상위 channel을 추가해 invalid_argument(field=channel) 1건이 발생했다. 모델은 해당 필드를 제거한 다음 생성에 성공했다. 오류 없는 첫 호출 성공률이나 모든 모델의 동작을 보장하는 결과는 아니다. 성공 경로는 agent_register 1·graph_create 1·graph_get 2·graph_update 1·node_create 6·node_get 5·node_update/discard/restore 각 1건이다.

CLI가 보고한 단일 실행 usage는 input_tokens=525081, cached_input_tokens=449920, output_tokens=2813, reasoning_output_tokens=52, cache_write_input_tokens=0이다. 여러 모델 요청을 합산한 한 작업의 관측값이며 대조 실행이 없다. 위 메타데이터 BPE나 Claude 표시값과 비교하여 실제 모델 토큰 감소율을 산출하지 않는다.

## 재현과 최종 검사

실행 파일과 모델은 환경에 맞게 명시한다. 아래 DB는 직접 만든 합성 DB여야 한다. 실제 모델 사용량을 소비하므로 일반 회귀나 CI에는 포함하지 않는다.

```sh
TEST_CODEX_LIVE=1 \
TEST_CODEX_BINARY=/absolute/path/to/codex \
TEST_CODEX_LIVE_MODEL=gpt-6-luna \
TEST_CODEX_LIVE_ARTIFACT_DIR=/private/tmp/issue305-live \
TEST_DATABASE_URL='postgres://user:password@127.0.0.1:port/synthetic?sslmode=disable' \
TEST_DATABASE_REQUIRED=1 \
go test -tags codex_live ./cmd/server \
  -run '^TestIssue305CodexLiveMCP$' -count=1 -v -timeout=4m
```

- 실제 실행 후 독립 리뷰에서 실패 호출까지 수명주기 존재 검사에 포함되는 약점을 발견했다. 성공한 동일 그래프·명제의 호출 순서·응답 ID·본문·판·폐기 상태를 대조하도록 보강했다. 실제 캡처를 이 검사에 재생해 통과했고, 실패·다른 대상·순서 역전·활성인 폐기 응답·잘못된 판의 5개 변형을 모두 거부했다. 모델 호출은 반복하지 않았으며 보강 후 검증은 캡처 재생으로 구분한다. 후속 리뷰에 남은 blocker는 없다.
- 전체 internal/mcp 시험, 갱신 환경 변수 없는 실제 DB의 MCP-09 통과. MCP-09의 도구 호출·오류 응답은 a78252d와 동일하고 discover·tools_list의 안내만 변경됐다. 설명 외 공개 스키마·annotations는 동일하며 14종을 유지한다.
- Go 1.27.3의 go build ./..., go vet ./..., go vet -tags codex_live ./cmd/server, gofmt -l . 통과. 최초 제한 환경에서는 로컬 수신·DB 연결이 거부되어 실행 권한을 조정한 뒤 실제 회귀를 통과했다.
- 시험 결과 원본은 비공개 임시 디렉터리에 저장하며 커밋하지 않는다. 캡처에는 합성 노드만 기록하고 인증 헤더는 기록하지 않는다. 작업 전용 DB 컨테이너와 익명 볼륨은 검증 후 정리한다.

도구 안내와 검증 실행 경계는 MCP·시험 SDD에 반영했다. 새 소유 경계나 배포 구성이 없어 DOX·운영 매뉴얼을 유지한다. 실제 Claude 앱의 동일 모델·세션 전후 usage, 원격 OAuth 전체 경로와 운영 배포는 이번 인수 범위에 포함하지 않는다.
