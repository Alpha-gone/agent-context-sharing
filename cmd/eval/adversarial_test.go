package main

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func adversarialFixture(t *testing.T) (contextSet, querySet, attackSet) {
	t.Helper()
	contexts, queries, attacks, err := convertOwnAdversarialSet("own-adversarial", 12)
	if err != nil {
		t.Fatalf("기준 세트 생성: %v", err)
	}
	return contexts, queries, attacks
}

// TestConvertOwnAdversarialSet은 생성한 여섯 표본이 공통 제약과 유형별 성립 조건을 통과하고
// 정상 기대 경로 항목을 오염 식별자로 두지 않는지 확인한다.
func TestConvertOwnAdversarialSet(t *testing.T) {
	contexts, queries, attacks := adversarialFixture(t)
	if err := validateAttackSet(attacks, contexts, queries); err != nil {
		t.Fatalf("생성한 공격 표본 검증: %v", err)
	}
	var types []string
	for _, sample := range attacks.Samples {
		types = append(types, sample.AttackType)
		for _, path := range sample.ExpectedPaths {
			for _, step := range path.Steps {
				if slices.ContainsFunc(sample.ContaminatedRefs, func(ref attackRef) bool { return sameRef(ref, step) }) {
					t.Errorf("표본 %q의 정상 기대 경로 단계가 오염 식별자에 들어갔다", sample.SampleID)
				}
			}
		}
		for _, spec := range sample.AddContexts {
			if strings.Contains(spec.Body, "가중치") {
				t.Errorf("표본 %q가 가중치 표현을 쓴다", sample.SampleID)
			}
		}
	}
	slices.Sort(types)
	want := slices.Sorted(slices.Values(attackTypes))
	if !slices.Equal(types, want) {
		t.Fatalf("공격 유형 %v, 기대 %v", types, want)
	}
}

func sameRef(left, right attackRef) bool {
	if left.Key != "" || right.Key != "" {
		return left.Key == right.Key
	}
	return left.Relation.normalized() == right.Relation.normalized()
}

// TestValidateAttackSetRejects는 공통 제약과 유형별 성립 조건을 하나씩 어긴 표본이 거부되는지
// 확인한다.
func TestValidateAttackSetRejects(t *testing.T) {
	sampleIndex := func(attacks attackSet, id string) int {
		return slices.IndexFunc(attacks.Samples, func(sample attackSample) bool { return sample.SampleID == id })
	}
	tests := []struct {
		name   string
		mutate func(*attackSet)
		want   string
	}{
		{"판 불일치", func(set *attackSet) { set.Version = "other" }, "판"},
		{"표본 중복", func(set *attackSet) { set.Samples = append(set.Samples, set.Samples[0]) }, "중복"},
		{"기준 key를 오염 식별자로 둠", func(set *attackSet) {
			index := sampleIndex(*set, "injection-01")
			set.Samples[index].ContaminatedRefs = append(set.Samples[index].ContaminatedRefs, attackRef{Key: "a01-a"})
		}, "오염 식별자"},
		{"기준에 없는 관계 폐기", func(set *attackSet) {
			index := sampleIndex(*set, "bridge-05")
			set.Samples[index].DiscardRelations[0] = relationSpec{Type: "causes", From: "a05-c", To: "a05-b"}
		}, "기준 그래프에 없다"},
		{"허브가 최대 차수가 아님", func(set *attackSet) {
			index := sampleIndex(*set, "hub-04")
			set.Samples[index].DiscardRelations[0] = relationSpec{Type: "precedes", From: "a04-c", To: "a04-a"}
			set.Samples[index].AddRelations[0] = relationSpec{Type: "precedes", From: "a03-c", To: "a04-a"}
			set.Samples[index].ContaminatedRefs = []attackRef{{Relation: &set.Samples[index].AddRelations[0]}}
			set.Samples[index].SuccessCriteria[0].MustReturn = set.Samples[index].ContaminatedRefs
		}, "차수가 최대"},
		{"브리지가 정상 경로 밖", func(set *attackSet) {
			index := sampleIndex(*set, "bridge-05")
			set.Samples[index].DiscardRelations[0] = relationSpec{Type: "precedes", From: "a05-c", To: "a05-a"}
			set.Samples[index].AddRelations[0] = relationSpec{Type: "precedes", From: "a04-c", To: "a05-a"}
			set.Samples[index].ContaminatedRefs = []attackRef{{Relation: &set.Samples[index].AddRelations[0]}}
			set.Samples[index].SuccessCriteria[0].MustReturn = set.Samples[index].ContaminatedRefs
		}, "정상 기대 경로에 있어야"},
		{"허위 연쇄가 짧음", func(set *attackSet) {
			index := sampleIndex(*set, "chain-07")
			set.Samples[index].AddRelations = set.Samples[index].AddRelations[:2]
			set.Samples[index].ContaminatedRefs = set.Samples[index].ContaminatedRefs[:5]
		}, "추가 관계 둘 이상의 경로"},
		{"상호 강화가 오염 하나만 요구", func(set *attackSet) {
			index := sampleIndex(*set, "mutual-08")
			set.Samples[index].SuccessCriteria[0].MustReturn = set.Samples[index].SuccessCriteria[0].MustReturn[:1]
		}, "둘 이상을 요구"},
		{"공유 간선이 한 질의에만 반환 조건", func(set *attackSet) {
			index := sampleIndex(*set, "shared-02")
			set.Samples[index].SuccessCriteria[1].MustReturn = []attackRef{{Key: "a03-b"}}
		}, "둘 이상의 대상 질의"},
		{"경로가 관계로 끝남", func(set *attackSet) {
			index := sampleIndex(*set, "injection-01")
			set.Samples[index].ExpectedPaths[0].Steps = set.Samples[index].ExpectedPaths[0].Steps[:2]
		}, "번갈아"},
		{"대상 질의 성공 조건 누락", func(set *attackSet) {
			index := sampleIndex(*set, "shared-02")
			set.Samples[index].SuccessCriteria = set.Samples[index].SuccessCriteria[:1]
			set.Samples[index].SuccessCriteria[0].MustReturn = append(set.Samples[index].SuccessCriteria[0].MustReturn, attackRef{Key: "a03-b"})
		}, "성공 조건이 하나씩"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contexts, queries, attacks := adversarialFixture(t)
			test.mutate(&attacks)
			err := validateAttackSet(attacks, contexts, queries)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("오류 %v, 기대 문구 %q", err, test.want)
			}
		})
	}
}

func testResponse(contexts []string, edges ...relationSpec) response {
	result := response{Ranks: map[string]int{}, Entries: contexts[:1]}
	for index, key := range contexts {
		result.Contexts = append(result.Contexts, key)
		result.Ranks[key] = index + 1
	}
	for _, edge := range edges {
		result.Edges = append(result.Edges, edge.normalized())
	}
	return result
}

// TestDiagnosePath는 기준 응답에서 경고가 없고, 공격 응답마다 첫 불일치 홉과 네 경고 코드가
// 재현되며, 구성원 참조 변화가 경고에 섞이지 않는지 확인한다.
func TestDiagnosePath(t *testing.T) {
	contexts, _, _ := adversarialFixture(t)
	base := newBaseShape(contexts)
	causes := relationSpec{Type: "causes", From: "a01-a", To: "a01-b"}
	path := expectedPath{PathID: "path-01", QueryID: "adv-01", Steps: []pathStep{{Key: "a01-a"}, {Relation: &causes}, {Key: "a01-b"}}}
	precedes := relationSpec{Type: "precedes", From: "a01-c", To: "a01-a"}
	tests := []struct {
		name     string
		response response
		code     string
		hop      int
		actual   []string
	}{
		{"기준 응답", testResponse([]string{"a01-a", "a01-b", "a01-c", "a01-sa"}, causes, precedes, relationSpec{Type: "has_member", From: "a01-a", To: "a01-sa"}), "", 0, nil},
		{"구성원 참조만 바뀜", testResponse([]string{"a01-a", "a01-b"}, causes, relationSpec{Type: "has_member", From: "a01-a", To: "x1-s"}), "", 0, nil},
		{"기대 사건 누락", testResponse([]string{"a01-b"}), warningMissingContext, 0, nil},
		{"기준 밖 사건 연결", testResponse([]string{"a01-a", "x1", "a01-b"}, causes, relationSpec{Type: "causes", From: "a01-a", To: "x1"}), warningUnexpectedContext, 0, []string{"x1"}},
		{"기대 관계 누락", testResponse([]string{"a01-a", "a01-b", "a02-b"}, relationSpec{Type: "causes", From: "a01-a", To: "a02-b"}), warningMissingRelation, 1, []string{"causes|a01-a|a02-b"}},
		{"기준 사건 사이 새 관계", testResponse([]string{"a01-a", "a01-b", "a02-a"}, causes, relationSpec{Type: "relates_to", From: "a02-a", To: "a01-a"}), warningUnexpectedRelation, 1, []string{"relates_to|a01-a|a02-a"}},
		{"끝 사건 누락", testResponse([]string{"a01-a"}, causes), warningMissingContext, 2, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			warning, warned := diagnosePath(test.response, path, base)
			if test.code == "" {
				if warned {
					t.Fatalf("경고 %+v가 나왔다", warning)
				}
				return
			}
			if !warned || warning.Code != test.code || warning.HopIndex != test.hop {
				t.Fatalf("경고 %+v, 기대 코드 %s 홉 %d", warning, test.code, test.hop)
			}
			var actual []string
			for _, ref := range warning.Actual {
				if ref.Key != "" {
					actual = append(actual, ref.Key)
					continue
				}
				actual = append(actual, ref.Relation.Type+"|"+ref.Relation.From+"|"+ref.Relation.To)
			}
			if !slices.Equal(actual, test.actual) {
				t.Fatalf("실제 식별자 %v, 기대 %v", actual, test.actual)
			}
		})
	}
}

// TestContaminationAndTrace는 오염 근거 비율의 분모와 첫 오염 항목까지의 추적을 확인한다.
func TestContaminationAndTrace(t *testing.T) {
	injected := relationSpec{Type: "causes", From: "a01-a", To: "x1"}
	contaminated := []attackRef{{Key: "x1"}, {Relation: &injected}}
	reached := testResponse([]string{"a01-a", "a01-b", "x1"}, relationSpec{Type: "causes", From: "a01-a", To: "a01-b"}, injected)
	if got := contaminationRatio(reached, contaminated); math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("오염 근거 비율 %v, 기대 0.4", got)
	}
	if !traceable(reached, contaminated) {
		t.Fatal("진입점에서 오염 사건까지 경로를 복원하지 못했다")
	}
	detached := testResponse([]string{"a01-a", "x1"})
	if traceable(detached, contaminated) {
		t.Fatal("간선 없이 반환된 오염 사건을 추적했다고 판정했다")
	}
	if got := contaminationRatio(response{Ranks: map[string]int{}}, contaminated); got != 0 {
		t.Fatalf("빈 응답의 오염 근거 비율 %v, 기대 0", got)
	}
}

// TestSummarizeMetrics는 반복값을 단위별로 먼저 평균하고, 추적 완전성을 성공한 반복만으로
// 계산하는지 확인한다.
func TestSummarizeMetrics(t *testing.T) {
	runs := sampleRuns{
		targets: map[string][]targetRun{
			"q1": {{Success: 1, Traced: 1, Affected: 1, Recovered: 1}, {Success: 1, Traced: 0, Affected: 1, Recovered: 1}, {Success: 0, Traced: undefinedMetric, Affected: 1, Recovered: 1}},
			"q2": {{Traced: undefinedMetric}, {Traced: undefinedMetric}, {Traced: undefinedMetric}},
		},
		controls: 2, controlsPre: []float64{0, 0, 0}, controlsPost: []float64{0.5, 0.5, 0.5},
	}
	metrics := summarizeMetrics([]sampleRuns{runs})
	if metrics.TargetQueries != 2 || math.Abs(metrics.AttackSuccess.Value-1.0/3) > 1e-9 {
		t.Fatalf("공격 성공률 %+v", metrics.AttackSuccess)
	}
	if metrics.Trace.Units != 1 || metrics.Trace.Value != 0.5 {
		t.Fatalf("추적 완전성 %+v", metrics.Trace)
	}
	if metrics.AffectedQueries != 1 || metrics.Recovery.Value != 0.5 {
		t.Fatalf("영향 질의 %v, 복구 %+v", metrics.AffectedQueries, metrics.Recovery)
	}
	if metrics.FalsePositive.Units != 1 || metrics.FalsePositive.Difference != 0.5 {
		t.Fatalf("오탐률 %+v", metrics.FalsePositive)
	}
}
