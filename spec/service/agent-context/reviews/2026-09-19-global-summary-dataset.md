# 전역 요약 자체 세트 시험 측정

## 목적

공개 벤치마크에 없는 전역 요약 사용 사례를 재현 가능한 자체 세트로 만들고, 전역 요약을 진입점으로 삼아 그래프 전반의 근거를 모으는 경로를 확인한다.

## 데이터셋

`cmd/eval -convert own-global`은 다음 두 파일을 결정적으로 생성한다.

- 질의 50건: 모두 `use_case=global`, `scope=global`이다.
- 컨텍스트 250건: 질의마다 원천 넷과 `summary_scope=global`인 요약 하나를 둔다.
- 전역 요약의 `derived_from`은 해당 질의의 정답 원천 넷과 같다.
- 질의마다 겹치지 않는 요약 유효 구간과 그 안의 `as_of`를 둔다. 한 질의에서는 전역 요약 하나만 진입점이 된다.

정답을 요약 자체로 두지 않은 이유는 시작점을 찾는 것만으로 검색이 스스로 정답을 맞히지 않게 하기 위해서다. 전역 요약에서 `derived_from`을 따라 그래프 전반의 근거를 모아야 정답이 된다.

## 통제 조건

- 데이터셋 판: `own-global-50q`
- 반복: 단계별 3회
- 컨텍스트 예산: 4,000자
- 채널 후보 상한: 50
- 최대 홉 수: 4
- 최대 홉 노드 수: 200
- 임베딩 모델: `bge-m3:vector:1024`
- 색인 대상: `all_layers`

## 결과

| 단계 | 재현율 | 순위 역수 | 정답 1건당 문자 수 | 전역 요약 기여 | 그래프 기여 |
|------|-------:|----------:|------------------:|---------------:|------------:|
| `baseline` | 0.00 | 0.00 | 산정 불가 | 1.00 | 0.00 |
| `references` | 1.00 | 0.50 | 62.99 | 0.20 | 0.80 |
| `relations` | 1.00 | 0.50 | 62.99 | 0.20 | 0.80 |
| `global` | 1.00 | 0.50 | 62.99 | 0.20 | 0.80 |

세 반복은 모두 같았다. `references`는 기준선보다 질의별 재현율이 1.00, 순위 역수가 0.50 높았고 두 차이의 95% 신뢰구간 반폭은 0이었다. 명시적 `global` 범위에서는 전역 요약이 모든 단계의 진입점이고, `references`부터 `derived_from`을 따라가므로 이후 두 단계가 더 개선되지 않는 것은 계약과 일치한다.

## 발견 사항

`SEARCH_GRAPH_STAGE=global`이 여는 `scope=auto`의 전역 요약 되돌림은 현재 정상 데이터에서 도달할 수 없다.

- 시간 필터는 활성 컨텍스트를 질의와 무관하게 후보로 내므로 컨텍스트가 있는 그래프의 국소 결합 결과가 비기 어렵다.
- 의미 유사도 채널은 유사도 하한 없이 근접 이웃을 상한까지 반환하므로 색인된 컨텍스트가 있으면 역시 결과가 비지 않는다.
- 활성 전역 요약 자체도 시간 필터와 의미 유사도의 대상이어서, 전역 요약이 존재하면서 국소 결합 결과만 비는 조건을 데이터셋으로 만들 수 없다.

따라서 이번 세트는 호출자가 강제한 `scope=global` 경로와 근거 확장을 검증한다. 그래프 효과 비교의 3단계인 자동 전역 진입점은 “국소 결과 없음”의 판정 기준을 별도로 확정하고 구현하기 전에는 효과를 측정할 수 없다. 이 문제를 데이터셋에서 장애나 미색인 상태로 인위적으로 만들지 않았다. 그렇게 하면 전역 요약 효과가 아니라 채널 장애를 재게 된다.

## 재현 명령

```shell
go run ./cmd/eval -convert own-global -convert-name own-global -convert-questions 50 \
  -contexts /tmp/own-global-contexts.json -queries /tmp/own-global-queries.json

go run ./cmd/eval -contexts /tmp/own-global-contexts.json \
  -queries /tmp/own-global-queries.json -repeat 3 \
  -stages baseline,references,relations,global -out /tmp/own-global-report.json
```
