package main

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

var businessComponents = []string{"agent", "embedding", "summary", "search", "storage"}
var businessPhases = []string{"preparation", "reindex", "execution"}
var businessTokenCategories = []string{"input", "output", "cache_read", "cache_write"}

type businessPricing struct {
	Version    string         `json:"version"`
	Currency   string         `json:"currency"`
	AsOf       string         `json:"as_of"`
	Allocation string         `json:"allocation_version"`
	Rates      []businessRate `json:"rates"`
}

// 토큰은 중복을 제거한 과금 범주다. 문자 예산이나 제공자 raw usage를 추정하지 않는다.
type businessRate struct {
	Component string              `json:"component"`
	Unit      string              `json:"unit"`
	UnitSize  float64             `json:"unit_size"`
	Rates     map[string]*float64 `json:"rates"`
}

type businessCostItem struct {
	Phase      string              `json:"phase"`
	Component  string              `json:"component"`
	Status     string              `json:"status"`
	Quantities map[string]*float64 `json:"quantities,omitempty"`
	Allocation *float64            `json:"allocation,omitzero"`
}

type businessCostTotal struct {
	Status        string  `json:"status"`
	Total         float64 `json:"total"`
	KnownSubtotal float64 `json:"known_subtotal"`
}

type businessCostObservation struct {
	Task       string                       `json:"task_code"`
	Condition  string                       `json:"condition"`
	Repeat     int                          `json:"repeat"`
	IndexState string                       `json:"index_state"`
	TimesMS    map[string]float64           `json:"times_ms"`
	Items      []businessCostItem           `json:"items"`
	Amounts    []float64                    `json:"amounts"`
	Phases     map[string]businessCostTotal `json:"phases"`
	Total      businessCostTotal            `json:"total"`
}

func businessCategories(component string) []string {
	if slices.Contains(businessComponents[:3], component) {
		return businessTokenCategories
	}
	return []string{"usage"}
}

func validateBusinessPricing(p businessPricing) error {
	invalid := fmt.Errorf("업무 과업 단가 판/통화/산정일/과금 단위가 올바르지 않다")
	if !manifestCode.MatchString(p.Version) || !manifestCode.MatchString(p.Allocation) || len(p.Currency) != 3 || strings.Trim(p.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || len(p.Rates) != len(businessComponents) {
		return invalid
	}
	if _, err := time.Parse(time.DateOnly, p.AsOf); err != nil {
		return invalid
	}
	seen := map[string]bool{}
	for _, rate := range p.Rates {
		if !slices.Contains(businessComponents, rate.Component) || seen[rate.Component] || !manifestCode.MatchString(rate.Unit) || !validMetric(rate.UnitSize) || rate.UnitSize <= 0 || rate.UnitSize > 1e12 {
			return invalid
		}
		seen[rate.Component] = true
		if slices.Contains(businessComponents[:3], rate.Component) && rate.Unit != "tokens" {
			return invalid
		}
		categories := businessCategories(rate.Component)
		if len(rate.Rates) != len(categories) {
			return invalid
		}
		for _, category := range categories {
			value, ok := rate.Rates[category]
			if !ok || (value != nil && (!validMetric(*value) || *value > 1e12)) {
				return invalid
			}
		}
	}
	return nil
}

func validateBusinessCosts(items []businessCostItem, pricing businessPricing) error {
	invalid := fmt.Errorf("업무 과업 비용의 구간/항목/상태/필수 사용량/배분율이 올바르지 않다")
	if len(items) != len(businessPhases)*len(businessComponents) {
		return invalid
	}
	seen := map[string]bool{}
	for _, item := range items {
		key := item.Phase + "/" + item.Component
		if !slices.Contains(businessPhases, item.Phase) || !slices.Contains(businessComponents, item.Component) || seen[key] || !slices.Contains([]string{"measured", "unmeasured", "not_applicable"}, item.Status) {
			return invalid
		}
		seen[key] = true
		if item.Status != "measured" {
			if item.Allocation != nil || len(item.Quantities) != 0 {
				return invalid
			}
			continue
		}
		if item.Allocation == nil || !validMetric(*item.Allocation) || *item.Allocation <= 0 || *item.Allocation > 1 {
			return invalid
		}
		categories := businessCategories(item.Component)
		if len(item.Quantities) != len(categories) {
			return invalid
		}
		for _, category := range categories {
			quantity, ok := item.Quantities[category]
			if !ok || quantity == nil || !validMetric(*quantity) || *quantity > 1e12 || (category != "usage" && math.Trunc(*quantity) != *quantity) {
				return invalid
			}
		}
		if amount := businessItemAmount(item, pricing); amount != undefinedMetric && !validMetric(amount) {
			return invalid
		}
	}
	return nil
}

func businessItemAmount(item businessCostItem, pricing businessPricing) float64 {
	if item.Status == "unmeasured" {
		return undefinedMetric
	}
	if item.Status == "not_applicable" {
		return 0
	}
	rate := pricing.Rates[slices.IndexFunc(pricing.Rates, func(r businessRate) bool { return r.Component == item.Component })]
	amount := 0.0
	for _, category := range slices.Sorted(maps.Keys(item.Quantities)) {
		if rate.Rates[category] == nil {
			return undefinedMetric
		}
		amount += *item.Quantities[category] * *rate.Rates[category] / rate.UnitSize
	}
	return amount * *item.Allocation
}

func businessRunCost(task string, run businessTaskRun, pricing businessPricing) businessCostObservation {
	result := businessCostObservation{Task: task, Condition: run.Condition, Repeat: run.Repeat, Items: slices.Clone(run.Costs), Phases: map[string]businessCostTotal{}}
	result.IndexState = run.IndexState
	result.TimesMS = map[string]float64{"execution": float64(*run.ElapsedMS), "preparation": undefinedMetric, "reindex": undefinedMetric}
	if run.PreparationMS != nil {
		result.TimesMS["preparation"] = float64(*run.PreparationMS)
	}
	if run.ReindexMS != nil {
		result.TimesMS["reindex"] = float64(*run.ReindexMS)
	}
	slices.SortFunc(result.Items, func(a, b businessCostItem) int {
		return strings.Compare(a.Phase+"/"+a.Component, b.Phase+"/"+b.Component)
	})
	for _, phase := range businessPhases {
		total := businessCostTotal{Status: "measured"}
		for _, item := range result.Items {
			if item.Phase != phase {
				continue
			}
			amount := businessItemAmount(item, pricing)
			if amount < 0 {
				total.Status = "unmeasured"
			} else {
				total.KnownSubtotal += amount
			}
		}
		total.Total = total.KnownSubtotal
		if total.Status == "unmeasured" {
			total.Total = undefinedMetric
		}
		result.Phases[phase] = total
		result.Total.KnownSubtotal += total.KnownSubtotal
	}
	result.Total.Status, result.Total.Total = "measured", result.Total.KnownSubtotal
	for _, phase := range businessPhases {
		if result.Phases[phase].Status == "unmeasured" {
			result.Total.Status, result.Total.Total = "unmeasured", undefinedMetric
		}
	}
	for _, item := range result.Items {
		result.Amounts = append(result.Amounts, businessItemAmount(item, pricing))
	}
	return result
}

func businessRunTokens(run businessTaskRun, phase string) map[string]float64 {
	result := map[string]float64{}
	complete := true
	for _, item := range run.Costs {
		if (phase != "" && item.Phase != phase) || !slices.Contains(businessComponents[:3], item.Component) {
			continue
		}
		if item.Status == "unmeasured" {
			complete = false
		}
		for _, category := range businessTokenCategories {
			if value := item.Quantities[category]; value != nil {
				result[category] += *value
			}
		}
	}
	for _, category := range businessTokenCategories {
		if !complete {
			result[category] = undefinedMetric
		}
	}
	return result
}
