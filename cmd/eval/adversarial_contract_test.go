package main

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func TestAdversarialInputBoundaries(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*attackSet)
		want   string
	}{
		{"duplicate_target", func(a *attackSet) {
			a.Samples[0].TargetQueryIDs = append(a.Samples[0].TargetQueryIDs, a.Samples[0].TargetQueryIDs[0])
		}, "중복"},
		{"contradictory_criteria", func(a *attackSet) {
			a.Samples[0].SuccessCriteria[0].MustOmit = slices.Clone(a.Samples[0].SuccessCriteria[0].MustReturn)
		}, "동시에"},
		{"unknown_relation_criteria", func(a *attackSet) {
			a.Samples[0].SuccessCriteria[0].MustReturn = []attackRef{{Relation: &relationSpec{Type: "causes", From: "a01-c", To: "a01-b"}}}
		}, "기준·추가"},
		{"path_without_relation", func(a *attackSet) { a.Samples[0].ExpectedPaths[0].Steps = a.Samples[0].ExpectedPaths[0].Steps[:1] }, "번갈아"},
		{"unconnected_auxiliary_event", func(a *attackSet) {
			a.Samples[0].AddContexts = append(a.Samples[0].AddContexts, a.Samples[4].AddContexts...)
		}, "구성원·근거"},
		{"duplicate_relation_identity", func(a *attackSet) {
			r := a.Samples[1].AddRelations[0]
			r.From, r.To = r.To, r.From
			a.Samples[1].AddRelations = append(a.Samples[1].AddRelations, r)
		}, "중복"},
		{"duplicate_success", func(a *attackSet) {
			a.Samples[0].SuccessCriteria = append(a.Samples[0].SuccessCriteria, a.Samples[0].SuccessCriteria[0])
		}, "중복"},
	} {
		t.Run(test.name, func(t *testing.T) {
			contexts, queries, attacks := adversarialFixture(t)
			test.mutate(&attacks)
			err := validateAttackSet(attacks, contexts, queries)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("%v; want %s", err, test.want)
			}
		})
	}
	t.Run("mutual_relation_identifiers", func(t *testing.T) {
		contexts, queries, attacks := adversarialFixture(t)
		sample := &attacks.Samples[5]
		sample.SuccessCriteria[0].MustReturn = []attackRef{{Relation: &sample.AddRelations[0]}, {Relation: &sample.AddRelations[1]}}
		if err := validateAttackSet(attacks, contexts, queries); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("transitive_support", func(t *testing.T) {
		contexts, queries, attacks := adversarialFixture(t)
		// 사건의 구성원인 파생이 원천을 근거로 쓰는 간접 보조 구조도 허용한다.
		sample := &attacks.Samples[0]
		sample.AddContexts = append(sample.AddContexts, contextSpec{Key: "support", Layer: "derived", Body: "보조 근거", Derived: &derivedSpec{Kind: "fact", DerivedFrom: []string{"x1-s"}, EvidenceState: "valid", ConfidenceState: "supported"}})
		sample.AddContexts[1].Event.Members = []string{"support"}
		if err := validateAttackSet(attacks, contexts, queries); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAdversarialIndependentMetrics(t *testing.T) {
	run := targetRun{Success: 1, Affected: 1, Recovered: 1, Traced: 1}
	series := make([]targetRun, 10)
	for i := range series {
		series[i] = run
	}
	runs := []sampleRuns{
		{targets: map[string][]targetRun{"q1": series, "q2": series}, units: map[string]string{"q1": "same-task", "q2": "same-task"}, controlRuns: map[string][]controlRun{"c1": {{after: 1}}}},
		{targets: map[string][]targetRun{"q1": series}, units: map[string]string{"q1": "same-task"}, controlRuns: map[string][]controlRun{"c1": {{after: 1}}}},
	}
	metrics := summarizeMetrics(runs)
	if metrics.TargetQueries != 2 || metrics.TargetPairs != 3 || metrics.AffectedQueries != 2 || metrics.AttackSuccess.Units != 1 || metrics.AttackSuccess.HalfWidth != -1 || metrics.FalsePositive.Units != 1 {
		t.Fatalf("중복 집계: %+v", metrics)
	}
	t.Run("empty", func(t *testing.T) {
		m := summarizeMetrics(nil)
		if m.AttackSuccess.Value != -1 || m.Trace.HalfWidth != -1 || m.Recovery.Value != -1 {
			t.Fatalf("빈 표본: %+v", m)
		}
	})
	t.Run("no_success", func(t *testing.T) {
		m := summarizeMetrics([]sampleRuns{{targets: map[string][]targetRun{"q": {{Traced: -1}}}}})
		if m.Trace.Value != -1 || m.Trace.Units != 0 {
			t.Fatalf("추적 분모: %+v", m.Trace)
		}
	})
	t.Run("paired_units", func(t *testing.T) {
		runs[1].targets["q3"] = runs[1].targets["q1"]
		delete(runs[1].targets, "q1")
		runs[1].units["q3"] = "other-task"
		m := summarizeMetrics(runs)
		if m.AttackSuccess.Units != 2 || math.Abs(m.AttackSuccess.Value-1) > 1e-9 || m.AttackSuccess.HalfWidth != 0 {
			t.Fatalf("대응 단위: %+v", m)
		}
	})
}

func TestAdversarialRecoveryOrder(t *testing.T) {
	specs := []contextSpec{{Key: "event", Event: &eventSpec{Members: []string{"derived"}}}, {Key: "source"}, {Key: "derived", Derived: &derivedSpec{DerivedFrom: []string{"source"}}}}
	order, err := attackRecoveryOrder(specs)
	if err != nil || len(order) != 3 || order[0].Key != "event" || order[1].Key != "derived" || order[2].Key != "source" {
		t.Fatalf("참조 순서: %+v %v", order, err)
	}
	specs[1].Derived = &derivedSpec{DerivedFrom: []string{"event"}}
	if _, err := attackRecoveryOrder(specs); err == nil {
		t.Fatal("순환을 허용했다")
	}
}

func TestAdversarialBeforeGuard(t *testing.T) {
	contexts, _, attacks := adversarialFixture(t)
	sample := attacks.Samples[0]
	edge := *sample.ExpectedPaths[0].Steps[1].Relation
	baseline := testResponse([]string{"a01-a", "a01-b"}, edge)
	for _, test := range []struct {
		name   string
		mutate func(*response)
		bad    bool
	}{
		{"equal", func(*response) {}, false},
		{"rank", func(r *response) { r.Ranks["a01-a"] = 3 }, true},
		{"edge", func(r *response) { r.Edges = nil }, true},
		{"entry", func(r *response) { r.Entries = []string{"a01-b"} }, true},
		{"already_success", func(r *response) { r.Ranks["x1"] = 3; r.Contexts = append(r.Contexts, "x1") }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual := testResponse([]string{"a01-a", "a01-b"}, edge)
			test.mutate(&actual)
			err := validateAttackBefore("adv-01", actual, baseline, sample, newBaseShape(contexts))
			if (err != nil) != test.bad {
				t.Fatal(err)
			}
		})
	}
}

func TestAdversarialReferenceChangesOnlyAffectImpact(t *testing.T) {
	contexts, _, attacks := adversarialFixture(t)
	sample := attacks.Samples[0]
	path := sample.ExpectedPaths[0]
	normal := *path.Steps[1].Relation
	before := testResponse([]string{"a01-a", "a01-b"}, normal)
	for _, kind := range []string{"derived_from", "supersedes", "has_member"} {
		t.Run(kind, func(t *testing.T) {
			after := testResponse([]string{"a01-a", "a01-b"}, normal, relationSpec{Type: kind, From: "a01-a", To: "x1-s"})
			if _, warned := diagnosePath(after, path, newBaseShape(contexts)); warned {
				t.Fatal("참조에 경고했다")
			}
			if before.signature() == after.signature() {
				t.Fatal("참조 영향을 누락했다")
			}
		})
	}
}

func TestAdversarialAffectedUnion(t *testing.T) {
	runs := []sampleRuns{
		{targets: map[string][]targetRun{"same-query": {{Affected: 1}, {Affected: 0}}}},
		{targets: map[string][]targetRun{"same-query": {{Affected: 0}, {Affected: 1}}}},
	}
	metrics := summarizeMetrics(runs)
	if metrics.TargetQueries != 1 || metrics.TargetPairs != 2 || metrics.AffectedQueries != 1 || metrics.Affected.Value != 1 || metrics.Affected.Difference != 1 || metrics.Affected.Units != 1 || metrics.Affected.HalfWidth != -1 {
		t.Fatalf("표본 간 회차별 합집합: %+v", metrics)
	}
}
