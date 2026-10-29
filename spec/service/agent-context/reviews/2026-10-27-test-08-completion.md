# TEST-08 실제 만료·OAuth 거부·취소·현재 권한 인수

## 현재 상태와 범위

이 검토는 [OAuth 후속 인수](2026-10-27-test-08-oauth.md)를 이어서 기록한다. 기존 14개 파일은 `9499a3d`로 커밋했다. macOS 26.6.2/arm64·Go 1.27.1·SDK v1.8.0·MCP 2026-07-28·Codex CLI 0.160.1 exec·Chrome 156.0.8078.48과 파일 자격 저장 모드가 대상이다. features.mcp_2026_07_28=true·사전 client·고정 loopback callback·별도 CA를 유지한다.

실제 접근 자격은 21:10:38 KST 발급·22:10:38 KST 만료였고, 22:10:40 KST에 기존 자격의 401·단일 갱신·새 프로세스 재사용을 확인했다. TEST-08을 완료하고 TBD-064를 선정한 CLI exec·기능 활성·파일 저장 구성에서 해소했다. 전체 117개 완료·2개 진행·5개 미착수이며 진행 중인 항목은 TEST-06과 OPS-09다.

## 새 시험과 관찰한 결과

| 항목 | 실제 관찰 |
|---|---|
| 미등록 callback | 실제 CLI 인가 요청을 시험 경계에서 변조했다. 브라우저의 authorize 400, 코드/계열/갱신 자격 0개·token 요청 0회를 확인했다. 대기 중인 CLI 로그인은 시험이 취소·정리한다. |
| 불일치/누락 issuer | 실제 브라우저 callback 응답의 iss를 바꾸거나 제거했다. CLI가 실패로 종료하고 코드 교환 0회·소비/계열/갱신 자격 0개·자격 파일 미생성을 확인했다. |
| verifier/resource 위반 | 실제 CLI token 요청을 시험 경계에서 변조했다. 교환 거부와 미소비 코드·계열/갱신 자격 0개·자격 파일 미생성을 확인했다. |
| scope 403·내부 500 | 호스트 반응만 통제 응답으로 검사했다. 403에는 실제 계약과 같은 resource_metadata·insufficient_scope 도전을 사용했다. 유한 종료·자격 파일 바이트 보존·추가 token 요청 없음, 오류 해제 뒤 기존 자격의 실제 보호 요청 성공을 확인했다. 서버 오류 분류 자체의 근거는 TEST-07과 OPS-09 실제 DB 중단 인수를 구분해 참조한다. |
| 실제 쓰기 취소 | 그래프 행 잠금을 잡고 CLI의 graph_update가 실제 DB 잠금 대기에 들어간 것을 관찰한 뒤 SIGINT를 보냈다. HTTP 요청 취소·처리 종료, graph version=1·갱신 재생 기록 0개, 시험 잠금 연결을 제외한 active/transactional 연결 0개를 확인했다. 풀의 private AcquiredConns 값을 직접 측정한 시험으로 확대하지 않는다. |
| 현재 쓰기 권한 | 다른 소유자를 실제 서비스로 추가하고 기존 계정을 viewer로 낮춘 뒤 같은 키를 재생했다. permission_denied와 graph version=2·최초 갱신 재생 기록 1개를 유지했다. |
| 다른 지문·오래된 판 | 같은 키의 다른 인자는 invalid_argument, 다른 키의 오래된 expected_version은 version_conflict·현재 판 2를 반환했다. 그래프 변경은 한 번이고 결정적 오류의 새 키 결과는 별도 기록했다. |
| 중복 쓰기 요청 | 로컬 모델 대역이 같은 인자의 두 function_call을 한 응답에 넣어 실제 CLI로 실행했다. 두 호출의 성공 결과와 graph version=2·갱신 재생 기록 1개를 확인했다. 호스트 내부의 실제 병렬 스케줄링을 직접 계측한 것은 아니다. 서버의 동시 재생 계약은 별도 TEST-07/MCP-09에 대조한다. |

모델 대역은 고정 응답이며 실제 에이전트의 과업 성공률이나 유료 추론을 측정하지 않는다. 최초 자격·PKCE·코드 교환은 실제 CLI/브라우저가 수행한다. 오류 표본의 변조는 합성 시험 경계에만 적용하며 제품 계약·사용자 설정·revision을 바꾸지 않는다.

## 실제 시간 경과를 기다리는 시험

`cmd/server/codex_expiry_live_test.go`의 codex_expiry_live 태그는 최초 발급 JWT의 exp-iat=3600을 확인하고 클라이언트 자격 파일의 바이트를 보존한 채 exp+2초까지 기다린다. 서버/호스트 시계·DB 시각·토큰·expires_at을 수정하지 않는다. 시험 전용 인증서는 2시간, 시험 context는 70분, 외부 go test 제한은 75분이다. 태그 없는 일반 회귀에 1시간 대기를 넣지 않는다.

기존 만료 자격의 실제 TLS 401·invalid_token, CLI의 단일 갱신·새 자격 0600 저장, 또 다른 CLI 프로세스의 추가 갱신 없는 재사용을 각각 판정한다. CLI가 만료를 인식해 선제 갱신하는 것도 정상이다. 실제 CLI 자신이 반드시 401을 받은 뒤 갱신해야 한다는 이전 표현은 너무 좁아 기존 만료 자격의 직접 거부와 호스트 갱신을 분리했다.

```sh
TEST_DATABASE_URL='<새 합성 DB>' TEST_DATABASE_REQUIRED=1 \
TEST_CODEX_BINARY='<기존 캐시의 macOS CLI 0.160.1 바이너리>' \
TEST_CODEX_REQUIRED=1 GOCACHE=/private/tmp/acs-eval12-go-cache \
/Users/alphagone/sdk/go1.27.1/bin/go test -tags codex_expiry_live \
  -race -count=1 -json -run '^TestTEST08CodexCLINaturalAccessExpiryIntegration$' \
  -timeout=75m ./cmd/server
```

일반 최종 회귀는 기존 선택 범위에 새 TEST-08 3종을 포함한 **267건**(최상위 **22건**)이며 필수 DB·race에서 실패·건너뜀 없이 통과했다. OPS-09가 공용 DB 정리 도우미를 보완한 뒤 같은 범위를 다시 실행했으며 최종 인증 192건·4.686초, 서버 75건·83.708초가 실패·건너뜀 없이 통과했다. 실제 만료 1건도 race에서 실패·건너뜀 없이 통과했고 시험 3603.120초·패키지 3604.698초를 기록했다. 일반 회귀와 이름이 겹치지 않는 총 268건(최상위 23건)의 근거이며 중복 예비 실행을 더하지 않았다. 원시 경로는 /private/tmp/acs-test08-complete-regression.json, /private/tmp/acs-test08-natural-final.json이다. 예비 실행을 최종 수에 더하지 않는다.

## 남은 범위

TEST-06의 Safari·VoiceOver와 OPS-09의 나머지 운영 장애 인수는 별도로 유지한다. keyring·TUI·앱·IDE·실제 운영 공개 지원으로 확대하지 않는다. 사용자 Codex 설정·자격과 기존 개발 DB·이전 OAuth 시험 컨테이너는 유지한다. 새 합성 자원은 최종 결과와 잔여 연결을 확인한 뒤 해당 ID·소유 라벨·익명 볼륨만 정리한다.


실제 만료 실행은 최초 준비 구성으로 시작했고 대기 중 추가한 오류 표본·DB 정리 도우미 보완은 최종 일반 회귀로 따로 확인했다. 제품의 접근 수명·키 정책·SDK/호스트 판·운영 환경은 바꾸지 않았다. DOX pass에서 지속 경계·소유권이 유지되어 기존 DOX를 유지하고 요구사항/설계의 현재 검증 상태만 동기화했다.


실제 만료 시험과 일반 회귀의 개별 DB가 모두 제거되어 최종 합성 클러스터에는 postgres/test08만 남고 잔여 클라이언트 연결 0개인 것을 확인했다. f2dce0a67cd0857c53011291c1b2925e72108c14657330c76b73010aa684ed71의 소유 라벨·익명 볼륨을 대조한 뒤 정상 정지·제거했다. 기존 개발 DB·WAL 정리·이전 OAuth 시험 컨테이너만 유지되었고 장시간 시험의 작업 디렉터리도 자동 정리되었다.

최종 변경 Markdown 15개·로컬 링크/앵커 941개, 개발 작업 124개·완료 117개·미완료 7개와 요구사항/설계 추적 184건의 집합을 대조했다. 오류·누락·중복이 없고 git diff --check·Node 구문 검사·전체 Go 정적 검사·두 선택 태그의 빌드도 통과했다. 새 변경은 미커밋이며 개인 IDE 파일 두 개를 보존했다.
