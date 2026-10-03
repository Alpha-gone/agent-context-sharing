# 클라이언트 2단계 구현 검토

- 검토일: 2026-10-03
- 범위: 도구 계약·공개 정책, 발견·목록 캐시, 오류 분류와 개발 계획의 완료 기준
- 기준: [SRS](../SRS.md), [SDD](../SDD.md), [개발 계획](../DEVELOPMENT_PLAN.md)
- 이 기록은 해당 시점의 검토 결과이며 동작 계약의 정본은 아니다.

## 발견 사항과 처리

### DEF-001: 호스트 경계 완료 기준의 잘못된 완료 표시

- 심각도: 중간
- 상태: 계획 수정 완료, 호스트 경계 구현·검증은 미완료
- 근거: `host.Tools` 패키지 시험은 있으나 현재 `serve`는 이를 연결하지 않는다. 계약 검증 오류의 `CallToolResult.structuredContent.client_error` 중계도 구현되지 않았다.
- 처리: 2단계를 진행 중으로 바꾸고 `client_protocol` 전달·실제 목록과 호출의 정책 일치 완료 기준을 미완료로 유지한다. 5단계 연결·직렬화 검증 뒤 다시 판정한다.

### DEF-002: 중복 정책 검증과 잘못된 구성 오류 분류

- 심각도: 낮음
- 상태: 패키지 수정·검증 완료, 호스트 연결은 5단계
- 근거: 기존 `config.knownTools`와 `contract.NewPolicy`가 정책을 따로 검증하고, 정책 오류를 `ErrProtocol`로 반환했다.
- 처리: `config.Load`가 `contract.NewPolicy`를 재사용하며 `PublicationPolicy()`로 검증한 정책을 제공한다. `Classify`와 `InjectsAgent`는 단일 분류표를 사용하고 공유 manifest와의 이름 일치를 검사한다. 정책 오류는 `ErrConfiguration`으로 분리했으며 호스트 코드는 `client_configuration`·`retryable=false`다.
- 회귀 시험: `TestLoadedPolicyMatchesContractClassification`, `TestInvalidPolicyIsConfigurationError`와 계약 정책 시험.

### DEF-003: `created_by_agent` 우회 입력의 오류 코드 모호성

- 심각도: 낮음
- 상태: 설계 명확화 완료, 호스트 직렬화는 5단계
- 근거: SDD의 기존 “입력 오류” 표현에는 일곱 클라이언트 오류 코드 중 어떤 코드를 쓰는지 명시되지 않았다.
- 처리: 기존 패키지 구현의 `ErrProtocol`과 일치하도록 `client_protocol`·`retryable=false`를 확정했다. 다른 호스트 입력 스키마 위반도 같으며 새 입력 코드를 만들거나 서버 도메인 오류로 바꾸지 않는다.
- 검증: 기존 여섯 도구의 우회 입력·스키마 선검증 시험을 포함한 클라이언트 전체 시험 통과.

### DEF-004: 조회 중 정상 계정 전환을 프로토콜 위반으로 처리

- 심각도: 낮음
- 상태: 계약·패키지 수정·검증 완료. 후속 4단계에서 실제 인증 조정자·로컬 TLS 시험 서버 연결을 검증했으며 호스트 오류 직렬화는 5단계다.
- 근거: 발견·목록 조회 중 인증 주체 변경을 `ErrProtocol`로 반환했다.
- 처리: SRS·SDD에서 `client_authorization`·`retryable=true`로 정하고 `ErrIdentityChanged`를 반환한다. 다른 주체의 응답은 캐시·공개·최초 지문 변경 판정에 쓰지 않는다. 취소가 우선하며 이 오류 자체로 자동 재인가·도구 호출·재시도를 시작하지 않는다. 호출자는 계정을 확인하고 다시 인가한 뒤 새 요청을 보낸다.
- 회귀 시험: `TestIdentityChangeDiscardsResponseAndAllowsNewRequest`, `TestCancellationPrecedesIdentityChange`와 기존 조회 중 주체 변경 시험.

## 호환성 참고

`tools/list.nextCursor`의 생략·빈 문자열은 허용하고 `null`·비문자열·비어 있지 않은 값은 거부한다. 현재 서버는 이 필드를 생략하므로 호환된다. 이 엄격한 경계를 SDD에 명시했으며 `null` 허용이나 페이지 조회 기능은 추가하지 않았다.

## 검증 결과와 한계

- Go `1.27.1`에서 클라이언트 `go test ./...`, `go test -race ./...` 통과.
- 서버·클라이언트 각각 `go build ./...`, `go vet ./...` 통과.
- 서버 `TestToolManifestMatchesClient` 통과. 서버 manifest는 변경하지 않았다.
- 저장소 전체 `gofmt -l .`, `git diff --check`와 변경 명세의 상대 링크 검사 통과. 한국어 표현과 오류 코드·단계 상태의 일관성을 검토했다.
- DOX의 클라이언트 구현·명세 로컬 계약을 보완했다. 상위 문서는 구조·소유권·Child DOX Index가 바뀌지 않아 유지했다.
- 실제 인증·HTTP·호스트 중계·실제 서버 종단 간 시험은 수행하지 않았다. 오류 코드 직렬화와 실제 정책 적용을 검증하기 전까지 2단계의 호스트 완료 기준을 닫지 않는다.
