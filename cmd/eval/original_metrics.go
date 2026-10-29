package main

import (
	"fmt"
	"maps"
	"slices"
)

type originalSummary struct {
	Samples          int            `json:"samples"`
	Identifiable     int            `json:"version_range_identifiable_samples"`
	IdentifiableRate float64        `json:"version_range_identifiable_rate"`
	Attempts         int            `json:"attempts"`
	Reachable        int            `json:"http_200_count"`
	Compared         int            `json:"content_measured_count"`
	Matched          int            `json:"content_match_count"`
	Changed          int            `json:"content_mismatch_count"`
	Unmeasured       int            `json:"content_unmeasured_count"`
	MatchRate        float64        `json:"content_match_rate_all_attempts"`
	Statuses         map[string]int `json:"status_counts"`
	Independent      estimate       `json:"independent_unit_match_rate"`
}

func aggregateOriginals(attempts []originalAttempt) (originalSummary, map[string]originalSummary, error) {
	if len(attempts) == 0 {
		return originalSummary{}, nil, fmt.Errorf("원본 비교 집계 시도가 없다")
	}
	type identity struct {
		unit, scenario string
		version, span  bool
	}
	identities, rounds := map[string]identity{}, map[string]map[int]bool{}
	groups := map[string][]originalAttempt{}
	for _, attempt := range attempts {
		id := identity{attempt.Independent, attempt.Scenario, attempt.VersionKnown, attempt.RangeKnown}
		if !manifestCode.MatchString(attempt.Code) || !manifestCode.MatchString(attempt.Independent) || !slices.Contains(originalScenarios, attempt.Scenario) || attempt.Repeat < 1 || attempt.Repeat > initialEvaluationRepeats || (attempt.ContentMatch != -1 && attempt.ContentMatch != 0 && attempt.ContentMatch != 1) {
			return originalSummary{}, nil, fmt.Errorf("원본 비교 집계 표본·지표가 올바르지 않다")
		}
		if previous, ok := identities[attempt.Code]; ok && previous != id {
			return originalSummary{}, nil, fmt.Errorf("원본 비교 집계 독립 단위·유형·식별 조건이 다르다")
		}
		identities[attempt.Code] = id
		if rounds[attempt.Code] == nil {
			rounds[attempt.Code] = map[int]bool{}
		}
		if rounds[attempt.Code][attempt.Repeat] {
			return originalSummary{}, nil, fmt.Errorf("원본 비교 집계 회차가 중복됐다")
		}
		rounds[attempt.Code][attempt.Repeat] = true
		groups[attempt.Scenario] = append(groups[attempt.Scenario], attempt)
	}
	for _, rounds := range rounds {
		if len(rounds) != initialEvaluationRepeats {
			return originalSummary{}, nil, fmt.Errorf("원본 비교 집계 10회 시도가 누락됐다")
		}
	}
	result := map[string]originalSummary{}
	for _, scenario := range slices.Sorted(maps.Keys(groups)) {
		result[scenario] = summarizeOriginals(groups[scenario])
	}
	return summarizeOriginals(attempts), result, nil
}

func summarizeOriginals(attempts []originalAttempt) originalSummary {
	result := originalSummary{Attempts: len(attempts), Statuses: map[string]int{}}
	bySample := map[string][]originalAttempt{}
	for _, attempt := range attempts {
		bySample[attempt.Code] = append(bySample[attempt.Code], attempt)
		result.Statuses[attempt.Status]++
		if attempt.Reachable {
			result.Reachable++
		}
		switch attempt.ContentMatch {
		case 1:
			result.Compared++
			result.Matched++
		case 0:
			result.Compared++
			result.Changed++
		default:
			result.Unmeasured++
		}
	}
	units := map[string][]float64{}
	for _, code := range slices.Sorted(maps.Keys(bySample)) {
		samples := bySample[code]
		first := samples[0]
		result.Samples++
		if first.VersionKnown && first.RangeKnown {
			result.Identifiable++
		}
		matched := 0
		for _, sample := range samples {
			if sample.ContentMatch == 1 {
				matched++
			}
		}
		units[first.Independent] = append(units[first.Independent], float64(matched)/float64(len(samples)))
	}
	var values []float64
	for _, unit := range slices.Sorted(maps.Keys(units)) {
		values = append(values, mean(units[unit]))
	}
	result.Independent = estimateOf(values)
	result.IdentifiableRate = float64(result.Identifiable) / float64(result.Samples)
	result.MatchRate = float64(result.Matched) / float64(result.Attempts)
	return result
}
