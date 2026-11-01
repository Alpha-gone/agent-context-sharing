package search

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"agent_context_sharing/internal/model"
)

func longFoldBody() string {
	body := make([]rune, 8000)
	for i := range body {
		body[i] = '가' + rune(i)
	}
	return string(body)
}

// 준비 비용은 측정에서 제외하며 본문 길이·후보 수·접기 키만 통제한다.
func foldAllocationCandidates(tb testing.TB, mode, body string) []combinedCandidate {
	tb.Helper()
	candidates := make([]combinedCandidate, 64)
	for i := range candidates {
		id, err := model.ParseID(fmt.Sprintf("0199aabb-ccdd-7000-8000-%012x", i+1))
		if err != nil {
			tb.Fatal(err)
		}
		reference := id
		if mode != "unique_keys" {
			reference = candidates[0].value.ID
			if i == 0 {
				reference = id
			}
		}
		candidateBody := body
		if mode == "similar_bodies" && i > 0 {
			runes := []rune(body)
			runes[len(runes)-1] = '。'
			candidateBody = string(runes)
		}
		candidates[i] = combinedCandidate{value: model.Context{ID: id, Layer: model.LayerDerived, Body: candidateBody, Derived: &model.DerivedAttributes{Kind: model.DerivationKindProposition, EvidenceState: model.EvidenceStateObservation, DerivedFrom: []model.ID{reference}}}}
	}
	return candidates
}

func BenchmarkFoldSimilarDerived(b *testing.B) {
	for _, mode := range []string{"unique_keys", "identical_body", "similar_bodies"} {
		b.Run(mode, func(b *testing.B) {
			candidates := foldAllocationCandidates(b, mode, longFoldBody())
			b.ReportAllocs()
			for b.Loop() {
				result := foldSimilarDerived(candidates, 0.9)
				want := 1
				if mode == "unique_keys" {
					want = len(candidates)
				}
				if len(result) != want {
					b.Fatal("접기 결과 개수", len(result))
				}
			}
		})
	}
}

func TestFoldAvoidsUnusedTrigramAllocations(t *testing.T) {
	for _, mode := range []string{"unique_keys", "identical_body"} {
		t.Run(mode, func(t *testing.T) {
			short := foldAllocationCandidates(t, mode, "가")
			long := foldAllocationCandidates(t, mode, longFoldBody())
			measure := func(candidates []combinedCandidate) float64 {
				return testing.AllocsPerRun(5, func() {
					result := foldSimilarDerived(candidates, 0.9)
					want := 1
					if mode == "unique_keys" {
						want = len(candidates)
					}
					if len(result) != want {
						t.Fatal("접기 결과 개수", len(result))
					}
				})
			}
			shortAllocs, longAllocs := measure(short), measure(long)
			if longAllocs > shortAllocs {
				t.Fatalf("비교가 필요 없는 본문에서 길이에 따라 할당 증가: short=%g long=%g", shortAllocs, longAllocs)
			}
		})
	}
}

func TestFoldLazyRepresentativesPreserveRankAndCounts(t *testing.T) {
	candidates := foldAllocationCandidates(t, "identical_body", "배포 파이프라인의 캐시 키 충돌 원인을 확인했다")[:6]
	bodies := []string{candidates[0].value.Body, "스테이징 데이터베이스 마이그레이션 지연을 확인했다", "스테이징 데이터베이스 마이그레이션 지연을 확인했다.", candidates[0].value.Body, "스테이징 데이터베이스 마이그레이션 지연을 확인했다", "독립적인 세 번째 판단을 작성했다"}
	for i := range candidates {
		candidates[i].value.Body = bodies[i]
	}
	before := slices.Clone(candidates)
	result := foldSimilarDerived(candidates, 0.9)
	if len(result) != 3 || result[0].value.ID != candidates[0].value.ID || result[1].value.ID != candidates[1].value.ID || result[2].value.ID != candidates[5].value.ID || result[0].folded != 1 || result[1].folded != 2 || result[2].folded != 0 {
		t.Fatalf("대표·순서·접힌 수 변경: %+v", result)
	}
	if !reflect.DeepEqual(candidates, before) {
		t.Fatal("입력 후보 변경")
	}
	// 완전 일치보다 앞선 대표가 임계값을 충족하면 기존 순위의 대표로 접어야 한다.
	earlier := slices.Clone(candidates[:3])
	earlier[0].value.Body = "가나다라"
	earlier[1].value.Body = "가나다마"
	earlier[2].value.Body = "가나다마"
	got := foldSimilarDerived(earlier, 1.0/3)
	if len(got) != 1 || got[0].value.ID != earlier[0].value.ID || got[0].folded != 2 {
		t.Fatalf("처음 대표 우선순위 변경: %+v", got)
	}
}
