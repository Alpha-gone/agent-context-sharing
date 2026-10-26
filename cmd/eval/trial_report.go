package main

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
)

type trialDecision struct {
	Condition string `json:"condition"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
}
type trialSummary struct {
	Trial             string          `json:"trial_id"`
	Mode              string          `json:"mode"`
	Status            string          `json:"execution_status"`
	Provenance        string          `json:"provenance"`
	Source            string          `json:"measurement_source"`
	Result            any             `json:"result"`
	Decisions         []trialDecision `json:"decisions"`
	DefaultActivation bool            `json:"default_activation"`
}
type trialCost struct {
	PricingVersion string                    `json:"pricing_version"`
	Currency       string                    `json:"currency"`
	AsOf           string                    `json:"as_of"`
	Allocation     string                    `json:"allocation_version"`
	Items          []manifestCostItem        `json:"items"`
	Total          businessCostTotal         `json:"total"`
	Pricing        *businessPricing          `json:"pricing,omitzero"`
	Details        []businessCostObservation `json:"observations,omitempty"`
}

func (t *manifestTrial) prepareBundle() error {
	progress := t.Progress.snapshot()
	if progress.ActiveGraphs != 0 || progress.CleanupFailures != 0 {
		return failTrial("cleanup_failed", nil)
	}
	value, err := t.readResult()
	if err != nil {
		return failTrial("result_invalid", err)
	}
	cost := t.resultCost(value)
	if err := t.checkCost(cost); err != nil {
		return failTrial("cost_mismatch", err)
	}
	summary := trialSummary{Trial: t.Manifest.TrialID, Mode: t.Manifest.Mode, Status: "completed", Provenance: t.Manifest.Provenance, Source: "live_evaluation", Result: value}
	if summary.Provenance == "" {
		summary.Provenance = "unverified"
	}
	if t.Manifest.Mode == "business" || t.Manifest.Mode == "business_tasks" {
		summary.Source = "imported_aggregate_trace"
	}
	summary.Decisions = t.decisions(value, cost)
	t.Preparation.Progress = progress
	t.prepareResources(value, cost)
	for _, file := range []struct {
		name  string
		value any
	}{{"manifest.json", t.Manifest}, {"preparation.json", t.Preparation}, {"summary.json", summary}, {"cost.json", cost}} {
		if err := writeTrialJSON(t.Bundle, file.name, file.value); err != nil {
			return failTrial("publication_failed", err)
		}
	}
	return nil
}

func (t *manifestTrial) prepareResources(value any, cost trialCost) {
	p := &t.Preparation
	p.Readiness, p.Pending, p.Failed = "not_applicable", -1, -1
	if p.Progress.Graphs > 0 {
		p.Readiness, p.Pending, p.Failed = "ready", 0, 0
	}
	p.DocumentCalls, p.VectorRows, p.VectorBytes = -1, -1, -1
	var measured []preparationMetrics
	switch r := value.(type) {
	case *report:
		for _, run := range r.Runs {
			if run.Preparation != nil {
				measured = append(measured, *run.Preparation)
			}
		}
	case *unitReport:
		for _, run := range r.Runs {
			if run.Metrics.Preparation != nil {
				measured = append(measured, *run.Metrics.Preparation)
			}
		}
	case *embeddingReport:
		for _, run := range r.Runs {
			measured = append(measured, run.Preparation)
		}
	}
	if len(measured) > 0 {
		p.DocumentCalls, p.VectorRows, p.VectorBytes = 0, 0, 0
		for _, m := range measured {
			p.DocumentCalls += m.EmbeddingCalls
			p.VectorRows += m.VectorRows
			p.VectorBytes += m.VectorBytes
		}
	}
	p.Costs = map[string]businessCostTotal{}
	for _, phase := range businessPhases {
		total := businessCostTotal{Status: "unmeasured", Total: -1}
		if len(cost.Details) > 0 {
			total.Status = "measured"
			for _, observation := range cost.Details {
				v := observation.Phases[phase]
				total.KnownSubtotal += v.KnownSubtotal
				if v.Status != "measured" {
					total.Status = "unmeasured"
				}
			}
			if total.Status == "measured" {
				total.Total = total.KnownSubtotal
			}
		}
		p.Costs[phase] = total
	}
}

func (t *manifestTrial) readResult() (any, error) {
	f, err := os.Open(t.Private + "/result.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (128<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 128<<20 {
		return nil, fmt.Errorf("결과는 128 MiB 이하여야 한다")
	}
	var target any
	switch t.Manifest.Mode {
	case "basic":
		target = &report{}
	case "business":
		target = &businessReport{}
	case "business_tasks":
		target = &businessTasksReport{}
	case "units":
		target = &unitReport{}
	case "embeddings":
		target = &embeddingReport{}
	case "originals":
		target = &originalReport{}
	case "continual":
		target = &continualReport{}
	case "adaptive":
		target = &adaptiveReport{}
	case "consistency":
		target = &consistencyReport{}
	case "adversarial":
		target = &adversarialReport{}
	default:
		return nil, fmt.Errorf("보고서 모드 오류")
	}
	if err := json.Unmarshal(raw, target, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	// 구문만 유효한 {}나 null 숫자를 완전한 결과로 받아들이지 않는다.
	complete, err := json.Marshal(target, json.Deterministic(true), json.FormatNilSliceAsNull(false))
	if err != nil {
		return nil, err
	}
	a, b := jsontext.Value(raw).Clone(), jsontext.Value(complete).Clone()
	if err := a.Canonicalize(); err != nil {
		return nil, err
	}
	if err := b.Canonicalize(); err != nil {
		return nil, err
	}
	if !bytes.Equal(a, b) {
		return nil, fmt.Errorf("결과의 필수 필드가 누락되었거나 null이다")
	}
	if err := t.validateResult(target); err != nil {
		return nil, err
	}
	if r, ok := target.(*businessTasksReport); ok {
		input, err := loadBusinessTasks(t.Inputs["business_tasks"])
		if err != nil {
			return nil, err
		}
		expected := summarizeBusinessTasks(input)
		expected.StartedAt = r.StartedAt
		expectedRaw, err := json.Marshal(&expected, json.Deterministic(true), json.FormatNilSliceAsNull(false))
		if err != nil {
			return nil, err
		}
		expectedCanonical := jsontext.Value(expectedRaw)
		if err := expectedCanonical.Canonicalize(); err != nil {
			return nil, err
		}
		if r.StartedAt.IsZero() || !bytes.Equal(b, expectedCanonical) {
			return nil, fmt.Errorf("수입한 과업 trace와 실제 결과 불일치")
		}
		if len(r.Costs) != len(input.Tasks)*len(input.Conditions)*initialEvaluationRepeats {
			return nil, fmt.Errorf("업무 결과 회차 누락")
		}
		for _, c := range r.Costs {
			if len(c.Items) != 15 || len(c.Amounts) != len(c.Items) {
				return nil, fmt.Errorf("비용 항목 누락")
			}
		}
	}
	if result, ok := target.(*report); ok && result.InputPreparation != nil && result.InputPreparation.Pin != nil {
		pin := *result.InputPreparation.Pin
		pin.URL, pin.License, pin.Attribution = "", "", ""
		result.InputPreparation.Pin = &pin
	}
	return target, nil
}

func (t *manifestTrial) resultCost(value any) trialCost {
	m := t.Manifest.Cost
	result := trialCost{PricingVersion: m.PricingVersion, Currency: m.Currency, AsOf: m.AsOf, Allocation: m.Allocation, Total: businessCostTotal{Status: "unmeasured", Total: -1}}
	for _, component := range businessComponents {
		result.Items = append(result.Items, manifestCostItem{Code: component, Status: "unmeasured"})
	}
	input, ok := value.(*businessTasksReport)
	if !ok {
		return result
	}
	result.Pricing = &input.Pricing
	result.Details = input.Costs
	result.Total.Status = "measured"
	for i, component := range businessComponents {
		status, sum := "not_applicable", 0.0
		for _, observation := range input.Costs {
			for j, item := range observation.Items {
				if item.Component != component {
					continue
				}
				if observation.Amounts[j] < 0 {
					status = "unmeasured"
				} else {
					sum += observation.Amounts[j]
					if status != "unmeasured" && item.Status == "measured" {
						status = "measured"
					}
				}
			}
		}
		result.Items[i].Status = status
		result.Total.KnownSubtotal += sum
		if status == "measured" {
			result.Items[i].Value = new(sum)
		}
		if status == "unmeasured" {
			result.Total.Status = "unmeasured"
		}
	}
	result.Total.Total = result.Total.KnownSubtotal
	if result.Total.Status == "unmeasured" {
		result.Total.Total = -1
	}
	return result
}

func (t *manifestTrial) checkCost(cost trialCost) error {
	for _, declared := range t.Manifest.Cost.Items {
		actual := cost.Items[slices.IndexFunc(cost.Items, func(i manifestCostItem) bool { return i.Code == declared.Code })]
		if declared.Status != actual.Status {
			return fmt.Errorf("비용 측정 상태 불일치")
		}
		if declared.Value != nil && (actual.Value == nil || math.Abs(*declared.Value-*actual.Value) > 1e-9*math.Max(1, math.Abs(*actual.Value))) {
			return fmt.Errorf("실제 사용량/단가 산정 금액 불일치")
		}
	}
	return nil
}

func (t *manifestTrial) decision(condition, kind, status, reason string, cost trialCost) trialDecision {
	value := trialDecision{condition, kind, "deferred", reason}
	if status == "rejected" {
		value.Status = "not_accepted"
		return value
	}
	if !slices.Contains([]string{"observed", "agent_trace"}, t.Manifest.Provenance) {
		value.Reason = "synthetic_or_unverified_evidence"
		return value
	}
	if t.Preparation.Independent < 2 {
		value.Reason = "insufficient_independent_units"
		return value
	}
	if status != "candidate" {
		return value
	}
	if cost.Total.Status != "measured" {
		value.Reason = "total_cost_unmeasured"
		return value
	}
	value.Status, value.Reason = "accepted", "significant_improvement_with_complete_cost"
	return value
}

func (t *manifestTrial) decisions(value any, cost trialCost) []trialDecision {
	var result []trialDecision
	add := func(condition, kind, status, reason string) {
		result = append(result, t.decision(condition, kind, status, reason, cost))
	}
	switch r := value.(type) {
	case *businessTasksReport:
		for _, entry := range r.Comparisons {
			add(entry.Condition, entry.Type, entry.Decision, entry.Reason)
		}
	case *businessReport:
		status := "deferred"
		if !r.Passed {
			status = "rejected"
		}
		add("with_context", "legacy_business", status, "legacy_four_metrics_only")
	case *report:
		for _, entry := range r.Assessments {
			status := entry.Status
			if entry.Reason == "improvement_observed_cost_and_public_acceptance_pending" {
				status = "candidate"
			}
			add(entry.Condition, entry.UseCase, status, entry.Reason)
		}
	case *unitReport:
		for _, entry := range r.Cohorts {
			add("semantic", entry.Cohort, entry.Status, entry.Reason)
		}
	case *embeddingReport:
		add("candidate", "embedding", r.Status, r.Reason)
	case *continualReport:
		for _, entry := range r.Judgements {
			status := entry.Status
			if !entry.StatePassed || !entry.RecordsPassed {
				status = "rejected"
			}
			add(entry.Scenario, "continual", status, entry.Reason)
		}
	case *adaptiveReport:
		for _, entry := range r.Judgements {
			for _, kind := range useCases {
				metrics, ok := entry.Comparisons[kind]
				if !ok {
					continue
				}
				status, reason := "deferred", "routing_effect_and_cost_acceptance_pending"
				for _, metric := range metrics {
					if metric.Significant && !metric.Improved {
						status, reason = "rejected", "significant_regression"
					}
				}
				for _, run := range r.Runs {
					if run.Stage != entry.Stage {
						continue
					}
					m := run.UseCases[kind]
					if m.LatencyP95MS > 500 {
						status, reason = "rejected", "latency_slo_exceeded"
					}
					for _, failures := range m.Failures {
						if failures > 0 {
							status, reason = "rejected", "channel_failure"
						}
					}
				}
				add(entry.Stage, kind, status, reason)
			}
		}
	case *consistencyReport:
		for _, entry := range r.Equivalence {
			status, reason := "deferred", "resource_and_quality_acceptance_pending"
			if entry.Mismatches > 0 {
				status, reason = "rejected", "equivalence_mismatch"
			}
			for _, run := range r.Concurrent {
				if run.Variant == entry.Variant && (run.Inconsistent > 0 || run.ChannelFailures > 0 || run.Errors > 0) {
					status, reason = "rejected", "inconsistent_or_failed_requests"
				}
			}
			add(entry.Variant, "consistency", status, reason)
		}
	case *adversarialReport:
		for _, entry := range r.Samples {
			status, reason := "deferred", "attack_resistance_and_cost_acceptance_pending"
			if entry.IsolationMismatches != 0 || entry.RecoveryVerified != r.Conditions.Repeats {
				status, reason = "rejected", "isolation_or_recovery_failure"
			}
			add(entry.SampleID, entry.AttackType, status, reason)
		}
	default:
		add(t.Manifest.Mode, "overall", "deferred", "source_or_adversarial_acceptance_pending")
	}
	if len(result) == 0 {
		add(t.Manifest.Mode, "overall", "deferred", "no_comparable_independent_evidence")
	}
	return result
}

func (t *manifestTrial) validateResult(value any) error {
	invalid := fmt.Errorf("완료 결과의 조건/회차/문항이 누락되었다")
	checkRuns := func(names []string, runs []runMetrics) error {
		grouped := map[string][]runMetrics{}
		for _, r := range runs {
			if len(r.QuerySamples) != len(t.Manifest.Units) {
				return invalid
			}
			grouped[r.Stage] = append(grouped[r.Stage], r)
		}
		return validateMeasuredRuns(names, grouped)
	}
	switch r := value.(type) {
	case *report:
		names := t.Manifest.Search.Stages
		if t.Manifest.ComparisonAxis == "index_targets" {
			names = []string{"all_layers", "without_source"}
		}
		if len(r.Runs) != 10*len(names) || r.Conditions.Repeats != 10 {
			return invalid
		}
		return checkRuns(names, r.Runs)
	case *businessReport:
		if len(r.Comparisons) != 4 || r.Samples != len(t.Manifest.Units) || r.StartedAt.IsZero() {
			return invalid
		}
	case *originalReport:
		if r.Repeats != 10 || len(r.Attempts) != 10*len(t.Manifest.Units) {
			return invalid
		}
		_, _, err := aggregateOriginals(r.Attempts)
		return err
	case *unitReport:
		var input unitInput
		if err := t.readRole("unit_comparison", &input); err != nil {
			return err
		}
		if r.Repeats != 10 || len(r.Runs) != 20*len(input.Samples) {
			return invalid
		}
	case *embeddingReport:
		var input embeddingInput
		if err := t.readRole("embedding_comparison", &input); err != nil {
			return err
		}
		if r.Repeats != 10 || len(r.Runs) != 20*len(input.Samples) {
			return invalid
		}
	case *continualReport:
		if r.Repeats != 10 || len(r.Runs) != 10*len(t.Manifest.Units) {
			return invalid
		}
	case *adaptiveReport:
		if r.Conditions.Repeats != 10 || len(r.Runs) == 0 {
			return invalid
		}
		var names []string
		for _, run := range r.Runs {
			if !slices.Contains(names, run.Stage) {
				names = append(names, run.Stage)
			}
		}
		return checkRuns(names, r.Runs)
	case *consistencyReport:
		if len(r.Equivalence) != len(consistencyVariants()) || len(r.Load) == 0 || len(r.Concurrent) == 0 {
			return invalid
		}
	case *adversarialReport:
		var input attackSet
		if err := t.readRole("attacks", &input); err != nil {
			return err
		}
		if r.Conditions.Repeats != 10 || len(r.Samples) != len(input.Samples) {
			return invalid
		}
	}
	return nil
}
