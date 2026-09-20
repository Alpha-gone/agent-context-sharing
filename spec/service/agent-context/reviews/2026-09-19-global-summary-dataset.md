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

`cmd/eval -convert own-global-auto`는 같은 컨텍스트 구조를 사용하되 질의 범위를 `auto`로 바꾼다. 원천 본문에는 비공개 작업 표식만, 질의와 전역 요약에는 별도의 전역 질의 표식만 넣어 키워드 채널이 국소 관련성을 만들지 않게 한다. 이 변형은 정상적으로 색인된 컨텍스트와 성공한 채널만 사용해 자동 전역 전환을 검증한다.

## 통제 조건

- 데이터셋 판: `own-global-50q`
- 반복: 단계별 3회
- 컨텍스트 예산: 4,000자
- 채널 후보 상한: 50
- 최대 홉 수: 4
- 최대 홉 노드 수: 200
- 임베딩 모델: `bge-m3:vector:1024`
- 색인 대상: `all_layers`

## 명시적 전역 범위 결과

| 단계 | 재현율 | 순위 역수 | 정답 1건당 문자 수 | 전역 요약 기여 | 그래프 기여 |
|------|-------:|----------:|------------------:|---------------:|------------:|
| `baseline` | 0.00 | 0.00 | 산정 불가 | 1.00 | 0.00 |
| `references` | 1.00 | 0.50 | 62.99 | 0.20 | 0.80 |
| `relations` | 1.00 | 0.50 | 62.99 | 0.20 | 0.80 |
| `global` | 1.00 | 0.50 | 62.99 | 0.20 | 0.80 |

세 반복은 모두 같았다. `references`는 기준선보다 질의별 재현율이 1.00, 순위 역수가 0.50 높았고 두 차이의 95% 신뢰구간 반폭은 0이었다. 명시적 `global` 범위에서는 전역 요약이 모든 단계의 진입점이고, `references`부터 `derived_from`을 따라가므로 이후 두 단계가 더 개선되지 않는 것은 계약과 일치한다.

## 초기 발견 사항

개선 전 `SEARCH_GRAPH_STAGE=global`이 여는 `scope=auto`의 전역 요약 되돌림은 정상 데이터에서 도달할 수 없었다.

- 시간 필터는 활성 컨텍스트를 질의와 무관하게 후보로 내므로 컨텍스트가 있는 그래프의 국소 결합 결과가 비기 어렵다.
- 의미 유사도 채널은 유사도 하한 없이 근접 이웃을 상한까지 반환하므로 색인된 컨텍스트가 있으면 역시 결과가 비지 않는다.
- 활성 전역 요약 자체도 시간 필터와 의미 유사도의 대상이어서, 전역 요약이 존재하면서 국소 결합 결과만 비는 조건을 데이터셋으로 만들 수 없다.

따라서 최초 세트는 호출자가 강제한 `scope=global` 경로와 근거 확장만 검증했다. 이 문제를 데이터셋에서 장애나 미색인 상태로 인위적으로 만들지 않았다. 그렇게 하면 전역 요약 효과가 아니라 채널 장애를 재게 된다.

## 자동 전역 전환 개선

국소 결과 유무 대신 질의 관련성을 판정하도록 검색 계약과 구현을 바꿨다.

- 전역 요약은 의미 유사도·키워드·시간 필터의 국소 채널에서 제외한다.
- `scope=auto`는 최상위 의미 후보가 유사도 하한 이상이거나 키워드 후보가 있을 때만 국소 관련성이 있다고 본다.
- 하한 미달 의미 후보와 시간 후보는 최종 결합에는 남지만 자동 전환을 막지 않는다.
- 자동 전환 뒤 전역 요약이 있으면 그 요약만 그래프 확장 시작점으로 사용한다.
- `SEARCH_SEMANTIC_SIMILARITY_THRESHOLD`를 배포 구성으로 열고 `global_fallback_triggered`와 `global_fallback_applied`를 완료 로그에 남긴다.

유사도 하한을 의미 후보 제거 조건으로 적용한 시험에서는 `0.70`이 MuSiQue 사실 재현율을 `0.929`에서 `0.268`, 연상 재현율을 `0.303`에서 `0.022`로 낮췄다. `0.50`도 연상 재현율을 `0.265`로 낮췄다. 그래서 하한은 후보를 버리지 않고 자동 전환 판정에만 사용한다. 시작값 `0.50`은 자동 전환 세트의 약한 의미 후보를 모두 하한 미달로 판정한다.

## 자동 전역 범위 결과

`own-global-auto-50q`, 유사도 하한 `0.50`에서 나머지 통제 조건을 유지해 세 번 반복했다. 모든 질의에서 의미 후보 50건을 보존하면서 `global_fallback_triggered=true`가 관측됐고, `global` 단계에서는 `global_fallback_applied=true`도 확인됐다.

| 단계 | 재현율 | 순위 역수 | 정답 1건당 문자 수 | 전역 요약 기여 | 그래프 기여 |
|------|-------:|----------:|------------------:|---------------:|------------:|
| `baseline` | 0.980 | 0.510 | 1,003.95 | 0.000 | 0.000 |
| `references` | 0.905 | 0.386 | 1,097.41 | 0.000 | 0.287 |
| `relations` | 0.905 | 0.386 | 1,097.41 | 0.000 | 0.287 |
| `global` | 1.000 | 1.000 | 985.85 | 0.011 | 0.042 |

`global`은 직전 `relations`보다 재현율이 `0.095 ± 0.04174`, 순위 역수가 `0.61378 ± 0.06612` 높았고 정답 1건당 문자 수가 `152.90 ± 71.23` 적었다. 세 차이는 모두 95% 신뢰구간이 0을 포함하지 않아 유의한 개선으로 판정됐다. 전역 요약 단계가 실제 자동 전환 경로에 도달하고, 전역 요약에서 네 근거를 확장하는 목적을 달성했다.

`references`가 자동 전역 전환을 인식하면서도 전역 요약 단계가 꺼진 상태에서 약한 국소 시작점을 확장해 기준선보다 나빠진 점은 비교 단계의 의도된 대조군이다. 후속 [검색 품질 본 측정](2026-09-20-search-quality-measurement.md)은 공개·사건 세트까지 함께 비교해, 기본 운영 구성에서 국소 그래프 확장은 끄고 자동 전역 전환만 독립적으로 켜기로 판정했다.

후속 [지속 평가 본 측정](2026-09-20-continual-evaluation.md)은 이 운영 구성을 포함한 온라인 갱신·재생·복구·망각·상충 해소와, 근거 경로가 필요한 전이 시나리오를 반복해 기준선 대비 품질이 나빠지지 않음을 확인했다.

## 재현 명령

```shell
go run ./cmd/eval -convert own-global -convert-name own-global -convert-questions 50 \
  -contexts /tmp/own-global-contexts.json -queries /tmp/own-global-queries.json

go run ./cmd/eval -contexts /tmp/own-global-contexts.json \
  -queries /tmp/own-global-queries.json -repeat 3 \
  -stages baseline,references,relations,global -out /tmp/own-global-report.json

go run ./cmd/eval -convert own-global-auto -convert-name own-global-auto -convert-questions 50 \
  -contexts /tmp/own-global-auto-contexts.json -queries /tmp/own-global-auto-queries.json

SEARCH_SEMANTIC_SIMILARITY_THRESHOLD=0.50 go run ./cmd/eval \
  -contexts /tmp/own-global-auto-contexts.json -queries /tmp/own-global-auto-queries.json \
  -repeat 3 -stages baseline,references,relations,global \
  -out /tmp/own-global-auto-report.json
```
