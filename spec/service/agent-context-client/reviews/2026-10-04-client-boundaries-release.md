# 클라이언트 입력·인가 종료·배포 출처 검증

## 범위

- 요청: [GitHub #61](https://github.com/Alpha-gone/agent-context-sharing/issues/61)
- 기준: `dev`의 `842fccc`, `fix/client-boundaries-release_#61`
- 변경 경계: `host/stdio`, `authorize` callback 수신기, `cmd/release`

잘린 숫자 ID를 다른 요청 ID로 회수하는 오류, 브라우저 사전 연결 때문에 인가 종료가 지연되는 오류, 저장소 내부 출력 시 실행 파일과 출처의 dirty 상태가 달라지는 오류를 수정했다. SDD를 먼저 보완하고 기존 요구사항의 경계 처리를 복원했다. 공개 도구·오류 코드·SDK 판·의존성·배포 구성은 유지했다.

## 재현과 변경

- 수정 전 실제 초과 stdio 프레임의 양수·음수·소수·지수 ID가 잘린 정수로 회수됐다. 숫자는 입력 상한 안에서 뒤쪽 JSON 공백·쉼표·닫는 중괄호를 확인해야 하며, 문자열은 닫는 따옴표까지 확인한다. 불확실한 ID는 `null`로 반환하고 다음 메시지를 정상적으로 읽는다. 상한의 마지막 바이트와 그 다음 바이트도 구분해 검사했다.
- 실제 loopback에 헤더 없는 TCP 연결을 먼저 접수한 뒤 성공·거부·마지막 대기자 취소·시간 초과·opener 실패·`Manager.Close`를 실행했다. 수정 전 여섯 경우 모두 2초 정리 제한을 넘었다. 수신 연결을 즉시 닫고 Serve 실행을 회수하며, callback 잠금으로 기존 일회성 값 접근을 끝내고 늦은 접근을 차단한다. 정상 callback은 `Content-Length: 0` 응답을 flush한 뒤 결과를 게시하여 종료 뒤에도 완전한 HTTP 응답을 읽을 수 있다. 이미 받은 callback의 opener 오류 우선순위와 종료 중 결과 게시의 비차단성도 유지했다.
- 깨끗한 임시 Git 저장소에 새 출력 디렉터리를 만들고 네 target을 실제 빌드했다. 수정 전 첫 파일은 clean이고 나머지는 dirty였지만 출처는 모두 clean이었다. 각 target 빌드 직전 Git 상태를 읽고 `vcs.modified`와 대조한다. 생성과 오프라인 검증 모두 dirty 불일치·누락·null·잘못된 타입을 거부한다. checksum을 다시 계산한 변조 자료도 출처 검증에서 거부하여 digest 오류가 문제를 가리지 않게 했다. 임시 저장소는 사용자 Git 설정·hook·서명과 분리했다.

## 검증 결과

Go 1.27.1에서 다음 검증을 통과했다.

```shell
# client/
go test -race ./internal/client/host ./internal/client/authorize ./cmd/release -count=1 -timeout=120s
go test -race ./internal/client/authorize ./internal/client/host -run 'Test(PreconnectedCallbackSocketDoesNotDelayCleanup|CallbackResponseCompletesBeforeListenerCloses|CallbackDoesNotWaitForOpenerExit|BlockedOpenerCancellationAndIdentity|OversizedFrameDoesNotRecoverTruncatedID|IDFromPrefixRequiresCompleteNumericToken|IDFromPrefixStaysWithinInputLimit)$' -count=20 -timeout=120s
go test -race -json -count=1 ./...
go build ./...
go vet ./...
# 서버 루트
go build ./...
go vet ./...
go test ./internal/mcp -run '^TestToolManifestMatchesClient$' -count=1
go test ./cmd/server -count=1
gofmt -l .
git diff --check
```

최종 클라이언트 전체 JSON 결과는 테스트 항목 407개 통과, 실패·테스트 항목 건너뛰기 없음이다. 테스트 파일이 없는 `client/internal/client`만 패키지 수준 `skip`이었다. 반복 race 검사에는 callback·opener 경쟁, 사전 연결 정리, 취소·시간 초과·Close, HTTP 응답 완결성, ID 경계와 다음 프레임 복구가 포함된다.

실제 클라이언트의 네 플랫폼 서명 없는 후보를 저장소 밖의 새 임시 경로에 생성하여 checksum과 빌드 출처 검증을 통과했다. 미커밋 작업 트리이므로 네 실행 파일과 출처 모두 dirty였다. 네 SBOM은 `reference.md`에 등록된 공식 SPDX 2.3 schema와 format 검사로 검증했다. 사용한 schema의 SHA-256은 `239208b7ac287b3cf5d9a9af23f9d69863971102a5e1587a27a398b43490b89b`이며 검증 도구는 임시 가상환경의 `jsonschema[format] 4.25.1`이다. 프로젝트 의존성과 서명 키는 변경하지 않았다. 후보 생성은 운영체제 실행 검증이나 공식 배포·서명자 신뢰 판정이 아니다.

실제 서비스·DB의 `client_live`, 서버 전체 DB 통합, 시스템 브라우저·WSL2·지원 호스트·공식 conformance·운영체제 행렬은 이번 작업에서 실행하지 않았다. 개발 DB·볼륨도 변경하지 않았다.

## DOX와 후속 상태

루트→`client/`→`host`·`authorize`·`cmd/release`와 `spec/`→`spec/service/`→`agent-context-client/` 계약을 확인했다. 세 구현 소유 AGENTS, SDD의 동작·검증 추적, 개발 계획과 요청 기록을 동기화했다. 요구사항·소유권·구조·Child DOX Index는 변하지 않아 SRS와 부모 AGENTS를 유지했다. 기존 용어와 외부 근거를 재사용하므로 용어집·`reference.md`도 유지했다. 한국어 표현·맞춤법·띄어쓰기, Markdown 구조·상대 링크를 점검했다.

코딩 표준과 Go 버전별 지침에 따라 기존 JSON v2·context·채널 경계를 유지하고, 설계·기록 스킬에 따라 정본과 재현·검증 근거를 분리했다. 로컬 수정·검증 완료 시점의 기록이며 커밋·푸시·PR 생성·이슈 종료는 아직 수행하지 않았다. 다음 단계는 사용자 요청에 따른 PR 제출이다.
