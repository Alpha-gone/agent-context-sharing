# 발견 요청의 중복 ID와 EOF 종료 검증

## 원인과 수정

GitHub 이슈 [#54](https://github.com/Alpha-gone/agent-context-sharing/issues/54)의 중복 `server/discover` 요청은 어댑터의 응답 대기 수를 올리지만, 공식 Go SDK v1.8.0은 진행 중 ID를 다시 받은 요청을 응답 없이 거절한다. SDK 출력 실패로 종료 중인 요청도 응답 쓰기가 생략될 수 있다. 어댑터가 SDK 응답 쓰기만으로 대기를 해제해 EOF에서 대기가 남았다. 다른 ID의 SDK 응답도 기존 대기 수를 내리는 문제가 있었다.

SDD의 「종료」에 ID 접수·완료·출력 실패 경계를 먼저 명시하고 다음을 수정했다.

- 발견과 도구의 진행 중 ID 충돌은 접수 단계에서 JSON-RPC `-32600`으로 거절하고 대기에 추가하지 않는다.
- SDK 발견 응답은 접수한 ID와 일치할 때만 대기를 해제한다. ID는 단일 writer의 기록 시작 직전에 해제해 완료 후 재사용을 허용한다.
- SDK 응답 인코딩·출력 실패에서는 입력과 호출 수명을 끝낸다. EOF 대기도 연결 수명 종료를 관찰하여 SDK가 후속 응답을 쓰지 않아도 종료할 수 있다.

정상 EOF의 발견·도구 응답 기록과 명시적 취소 통지의 미기록 응답 생략을 유지했다. 공개 API·도구 계약·SDK 판·의존성·배포 구성은 변경하지 않았다.

## 재현과 회귀

수정 전에 SDK의 첫 발견 핸들러를 멈추고 같은 숫자·문자열 ID의 요청과 EOF를 보낸 두 경우 모두 1초 context 제한까지 반환하지 않았다. 이미 접수한 두 발견 중 첫 응답 출력이 실패하고 나머지 응답이 없는 대역도 EOF에서 제한 시간까지 기다렸다. 접수하지 않은 다른 ID의 SDK 응답이 발견 대기를 잘못 해제하는 것도 확인했다. 실제 `Run`의 128개 발견과 출력 오류·부분 쓰기 시험은 수정 전에도 통과했으며, 이것만으로 응답 없는 경로를 판정하지 않았다.

수정 후 숫자·문자열 중복 ID의 원래 발견 결과와 중복 오류를 기록하고 EOF로 종료했다. 발견·도구 간 양방향 ID 충돌, 완료 후 ID 재사용, 다른 ID 응답의 대기 보존, SDK 출력 오류·부분 쓰기 뒤 종료와 응답 없는 발견의 대기 중단을 확인했다.

혼합 ID 테스트 대역의 `jsonrpc.MakeID`에 잘못된 정수 타입을 넘겨 응답 ID가 비어 대기한 중간 실행은 중단했다. 지원하는 `float64`로 수정하고 오류를 확인한 뒤 신규 회귀와 전체 검증을 다시 실행했다. 중간 실행을 통과로 집계하지 않았다.

## 검증 결과

Go 1.27.1에서 다음 검증을 통과했다.

```shell
# client/
go test -race ./internal/client/host -run '^Test(Host(DuplicateDiscoverDrainsAfterEOF|DiscoverWriteFailureStopsAfterEOF)|DiscoverWriteFailureStopsPendingRead|UntrackedSDKResponseDoesNotDrainDiscover|DiscoverAndToolIDsShareAdmission|DiscoverIDReuseAfterResponse)$' -count=20 -timeout=60s
go test -race ./internal/client/host -count=10 -timeout=120s
go test -race -json ./...
go build ./...
go vet ./...
# 서버 루트
go test ./internal/mcp -run '^TestToolManifestMatchesClient$' -count=1
go test ./cmd/server -count=1
go build ./...
go vet ./...
gofmt -l .
git diff --check
```

클라이언트 전체 JSON 결과에 실패·테스트 항목 건너뛰기는 없었고 테스트 파일이 없는 `client/internal/client`만 패키지 수준 `skip`이었다. 정상 EOF 직전 발견·동시 ID, 요청 취소·출력 대기 취소·EOF 도구 결과와 실행 파일의 종료 신호·구성 검증도 전체 회귀에 포함했다.

실제 서비스·DB를 사용하는 `client_live`, 서버 전체 DB 통합 시험, 지원 Codex CLI·공식 conformance·시스템 브라우저·WSL2·운영체제 행렬·운영 부하 시험은 이번 변경에서 실행하지 않았다.

## DOX와 범위

루트→`client/`→`client/internal/client/host/` 및 `spec/`→`spec/service/`→`agent-context-client/` 계약을 확인하고 호스트 AGENTS와 SDD·개발 계획·요청 기록을 동기화했다. `FR-AGENT_CONTEXT_CLIENT-026`·`027`의 EOF 종료·발견 요구사항을 복원하므로 SRS는 유지했다. 구조·소유권·Child DOX Index가 바뀌지 않아 부모 및 서비스 AGENTS는 유지했다. 기존 용어와 SDK 경계를 사용하므로 용어집·`reference.md`는 변경하지 않았다. 한국어 표현·맞춤법·띄어쓰기와 Markdown 구조·상대 링크를 점검했다.

DB 변경·외부 임베딩 API·운영 권한 변경·배포는 수행하지 않았다. 로컬 수정과 검증 완료 시점의 기록이며 커밋·푸시·PR 생성과 이슈 종료는 아직 수행하지 않았다.
