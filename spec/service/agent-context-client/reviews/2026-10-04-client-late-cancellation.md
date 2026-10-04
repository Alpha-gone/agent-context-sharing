# 완료된 원격 도구 결과의 늦은 취소 검증

## 원인과 수정

GitHub 이슈 [#53](https://github.com/Alpha-gone/agent-context-sharing/issues/53)은 쓰기 호출의 원격 결과를 받은 직후 취소·호출 예산 만료가 발생하면 `client_timeout`·`retryable=true`로 바뀌는 문제다. 계약 계층의 `Client.CallTool`이 `Source.CallTool`의 정상 반환 뒤 context를 다시 검사해 완료 결과를 버렸다. 호스트가 새 논리적 호출과 새 멱등성 키로 쓰기를 재시도하면 중복 적용될 수 있다.

SDD의 「논리적 호출 상태」와 「호출 변환과 응답 보존」에 완료 결과 확정 전의 취소, 이미 완료된 결과의 보존, 호스트 명시적 취소 통지에 따른 미기록 응답 생략을 먼저 구분했다. 계약 계층의 정상 전송 뒤 취소 검사만 제거하고 JSON 객체 검증·독립 복사를 유지했다. 전송 완료 전에 발생한 취소·전달 가능성에 따른 불확실 쓰기 분류, 목록 준비·호출 직전 취소 검사와 호스트 응답 생략 정책은 변경하지 않았다. 새 멱등성 키·재전송·오류 코드·공개 API·의존성을 추가하지 않았다.

## 재현과 회귀

`TestCallToolPreservesCompletedResponse`는 목록을 준비한 뒤 원격 도구 전송 대역이 결과를 확보하고 반환하는 경계에서 호스트 context를 취소하거나 실제 원격 처리 예산의 만료를 기다린다. `host.Tools`까지 연결해 읽기·쓰기 각각의 성공·도메인 오류·잘못된 결과 객체를 검사한다. 수정 전 열두 경우 모두 context 오류로 바뀌어 결과 보존·오류 분류 검사가 실패했다. 전송 전 취소 회귀는 수정 전에도 통과했다.

수정 후 다음을 확인했다.

- 늦은 취소·예산 만료에서도 성공과 도메인 오류 결과의 필드 순서·큰 정수·커서·부분 상태를 바이트 단위로 유지한다.
- 잘못된 완료 결과 객체는 `client_protocol`·재시도 불가로 거부한다.
- 각 완료 호출의 도구 전송은 한 번이며 자동 재전송하지 않는다.
- 전송 전 취소한 쓰기는 `client_timeout`·재시도 가능이며 원격 도구 호출은 없다.
- HTTP 외피 검증 뒤 마지막 갱신 적용 대역에서 호출 context를 취소한 쓰기도 실제 stdio에 원래 호스트 ID와 결과로 기록된다. HTTP 도구 호출은 한 번이며 `client_error`로 바뀌지 않는다.
- 완료 결과가 없는 전달 가능 쓰기는 기존 `ErrIndeterminate` 분류를 유지한다.
- 클라이언트 전체 회귀에서 명시적 취소 통지의 미기록 응답 생략, EOF의 접수된 결과 기록, 재시도·키·자원 상한·동시 응답 상관도 유지된다.

## 검증 결과

Go 1.27.1을 사용했다.

```shell
# client/
go test -race ./internal/client/remote -run '^(TestCallTool(PreservesCompletedResponse|CancellationBeforeSend)|TestHostCompletedWriteSurvivesLateCancellation|TestDeliveredWriteCancellationIsIndeterminate)$' -count=10
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

모두 통과했다. 클라이언트 전체 JSON 결과에 실패와 테스트 항목의 건너뛰기는 없었고 테스트 파일이 없는 `client/internal/client`만 패키지 수준 `skip`이었다. gofmt 미정리 파일과 diff 공백 오류도 없었다.

실제 서비스·DB를 쓰는 `client_live` 시험, 서버 전체 DB 통합 시험, 지원 Codex CLI·공식 conformance·시스템 브라우저·WSL2·운영체제 행렬·운영 부하 시험은 이번 수정에서 실행하지 않았다. 주입 HTTP transport와 실제 stdio 경계를 통한 회귀를 실제 서비스·지원 호스트 검증으로 보고하지 않는다.

## DOX와 범위

루트→`client/`→`client/internal/client/remote/` 및 `spec/`→`spec/service/`→`agent-context-client/` DOX 체인과 기존 용어를 확인했다. 원격 AGENTS와 SDD·개발 계획·요청 기록을 동기화하고 한국어 표현·맞춤법·띄어쓰기, Markdown 구조와 링크를 점검했다. 기존 응답 보존·불확실 쓰기·취소 요구사항의 구현을 복원하므로 SRS는 유지했다. 구조·소유권·Child DOX Index가 바뀌지 않아 부모 및 서비스 AGENTS도 유지했고 새 외부 기술 근거를 채택하지 않아 `reference.md`와 용어집은 변경하지 않았다.

DB 변경·외부 임베딩 API·운영 권한 변경·배포는 수행하지 않았다. 로컬 수정과 검증 완료 시점의 기록이며 커밋·푸시·PR 생성과 이슈 종료는 아직 수행하지 않았다.
