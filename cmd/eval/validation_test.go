package main

import (
	json "encoding/json/v2"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestConsistencyCandidateLimitRejectsInvalidPremise(t *testing.T) {
	for _, limit := range []int{0, 1, consistencyActiveContexts - 1, consistencyActiveContexts} {
		if err := validateConsistencyCandidateLimit(limit); err == nil {
			t.Fatalf("후보 상한 %d를 허용했다", limit)
		}
		if _, err := runConsistency(t.Context(), nil, nil, loadedGraph{}, contextSet{}, querySet{}, settings{candidateLimit: limit}, consistencyConditions{}); err == nil {
			t.Fatal("일관성 실행 전에 전제를 확인하지 않았다")
		}
	}
	if err := validateConsistencyCandidateLimit(consistencyActiveContexts + 1); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessMetricsAcceptMeasuredZeroAndRejectNullObject(t *testing.T) {
	var value businessMetrics
	if err := json.Unmarshal([]byte(`{"completion_time_ms":1,"rework_count":0,"resume_interactions":0,"collaboration_usage":0}`), &value); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"null", "{}", `{"completion_time_ms":1,"rework_count":0,"resume_interactions":0,"collaboration_usage":0,"body":"금지"}`} {
		if err := json.Unmarshal([]byte(raw), &value); err == nil {
			t.Fatalf("잘못된 지표 객체를 허용했다: %s", raw)
		}
	}
}

func TestBusinessMetricsRequireEveryMeasurement(t *testing.T) {
	for _, condition := range []string{"without_context", "with_context"} {
		for _, field := range []string{"completion_time_ms", "rework_count", "resume_interactions", "collaboration_usage"} {
			for _, missing := range []bool{false, true} {
				t.Run(condition+"/"+field+"/"+map[bool]string{false: "null", true: "missing"}[missing], func(t *testing.T) {
					metrics := map[string]any{"completion_time_ms": 100, "rework_count": 0, "resume_interactions": 0, "collaboration_usage": 0}
					if missing {
						delete(metrics, field)
					} else {
						metrics[field] = nil
					}
					valid := map[string]any{"completion_time_ms": 100, "rework_count": 0, "resume_interactions": 0, "collaboration_usage": 0}
					sample := map[string]any{"without_context": valid, "with_context": valid}
					sample[condition] = metrics
					body, err := json.Marshal(map[string]any{"version": "required", "conditions": businessConditions{AgentVersion: "agent", Model: "model"}, "samples": []any{sample, sample, sample}})
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(t.TempDir(), "business.json")
					if err := os.WriteFile(path, body, 0o600); err != nil {
						t.Fatal(err)
					}
					if _, err := loadBusinessSet(path); err == nil {
						t.Fatal("미측정 지표를 허용했다")
					}
				})
			}
		}
	}
}

func TestAttackSuccessCriteriaRejectDuplicateQuery(t *testing.T) {
	contexts, queries, attacks := adversarialFixture(t)
	index := slices.IndexFunc(attacks.Samples, func(sample attackSample) bool { return sample.AttackType == attackMutual })
	sample := &attacks.Samples[index]
	weak := sample.SuccessCriteria[0]
	weak.MustReturn = weak.MustReturn[:1]
	sample.SuccessCriteria = append([]successCriteria{weak}, sample.SuccessCriteria...)
	if err := validateAttackSet(attacks, contexts, queries); err == nil {
		t.Fatal("중복 성공 조건을 허용했다")
	}
}

func TestAdversarialConvertRejectsFilterBeforeWriting(t *testing.T) {
	for _, useCase := range useCases {
		t.Run(useCase, func(t *testing.T) {
			dir := t.TempDir()
			paths := []string{filepath.Join(dir, "contexts.json"), filepath.Join(dir, "queries.json"), filepath.Join(dir, "attacks.json")}
			if err := runConvert(convertOwnAdversarial, "", "filtered", 12, useCase, paths[0], paths[1], paths[2]); err == nil {
				t.Fatal("공격 표본의 사용 사례 필터를 허용했다")
			}
			for _, path := range paths {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("거부한 변환의 파일이 남았다: %s, %v", path, err)
				}
			}
		})
	}
}

func TestEvaluationModesRejectConflictsBeforeIO(t *testing.T) {
	modes := []string{"-convert=own", "-business=missing.json", "-continual", "-adaptive", "-consistency", "-adversarial"}
	for i, first := range modes {
		for _, second := range modes[i+1:] {
			t.Run(first+"/"+second, func(t *testing.T) {
				originalFlags, originalArgs := flag.CommandLine, os.Args
				flag.CommandLine = flag.NewFlagSet("eval", flag.ContinueOnError)
				os.Args = []string{"eval", first, second}
				t.Cleanup(func() { flag.CommandLine, os.Args = originalFlags, originalArgs })
				if err := run(); err == nil || !strings.Contains(err.Error(), "모드") {
					t.Fatalf("모드 충돌이 입력 전에 거부되지 않았다: %v", err)
				}
			})
		}
	}
}

func TestEmptyLatencyIsUnmeasuredNotZero(t *testing.T) {
	got := summarize(nil)
	if got.Mean != undefinedMetric || got.P50 != undefinedMetric || got.P95 != undefinedMetric || got.Max != undefinedMetric {
		t.Fatalf("미측정 지연 = %+v", got)
	}
	if got := summarize([]float64{0}); got != (latencySummary{}) {
		t.Fatalf("측정한 0이 바뀌었다: %+v", got)
	}
}
