// Package plan은 계정 플랜의 고정 항목과 한도 판정을 제공한다.
package plan

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"

	"agent_context_sharing/internal/model"
)

// Limits는 계정 플랜이 정할 수 있는 14개 항목의 값이다. 0은 해당 한도가 없음을 뜻한다.
type Limits struct {
	MaxHops                    int
	MaxHopNodes                int
	GraphPage                  PageSize
	ContextBudget              int
	GraphsPerAccount           int
	StoredCharactersPerGraph   int64
	RetentionDays              int
	GraceDays                  int
	AuditRetentionDays         int
	TierMoveAfterDays          int
	RejectedOperationRetention RetentionPeriod
	WritesPerMinute            int
	ProposalRetentionDays      int
	RelationPage               PageSize
}

// PageSize는 목록 페이지 크기 한 항목의 기본값과 상한을 함께 표현한다.
type PageSize struct{ Default, Maximum int }

// AccountPlans는 배포 구성이 계정별로 덮어쓴 플랜 값 집합이다.
type AccountPlans struct{ limits map[model.ID]Limits }

// ParseAccountPlans는 ACCOUNT_PLAN_LIMITS JSON 객체를 계정별 플랜 값으로 해석한다.
func ParseAccountPlans(raw string) (AccountPlans, error) {
	if strings.TrimSpace(raw) == "" {
		return AccountPlans{}, nil
	}

	var overrides map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		return AccountPlans{}, validationError("ACCOUNT_PLAN_LIMITS", "플랜 구성 JSON이 올바르지 않다", err)
	}
	if overrides == nil {
		return AccountPlans{}, validationError("ACCOUNT_PLAN_LIMITS", "플랜 구성은 JSON 객체여야 한다", nil)
	}

	// 모든 계정 키를 먼저 정규화한다. 값의 유효성·JSON 멤버 순서와 무관하게
	// 같은 계정의 별칭을 거부하며 구성 오류의 선택도 맵 순회에 기대지 않는다.
	keys := slices.Sorted(maps.Keys(overrides))
	accountIDs := make(map[string]model.ID, len(keys))
	seen := make(map[model.ID]struct{}, len(keys))
	for _, key := range keys {
		id, err := model.ParseID(key)
		if err != nil {
			return AccountPlans{}, validationError("ACCOUNT_PLAN_LIMITS", "플랜 계정 키는 UUIDv7이어야 한다", err)
		}
		if _, exists := seen[id]; exists {
			return AccountPlans{}, validationError("ACCOUNT_PLAN_LIMITS", "정규화한 플랜 계정 키가 중복됐다", nil)
		}
		accountIDs[key], seen[id] = id, struct{}{}
	}

	plans := AccountPlans{limits: make(map[model.ID]Limits, len(keys))}
	for _, key := range keys {
		value := overrides[key]
		if value.Kind() != jsontext.KindBeginObject {
			return AccountPlans{}, validationError("ACCOUNT_PLAN_LIMITS", "계정 플랜 값은 부분 JSON 객체여야 한다", nil)
		}
		var override limitsOverride
		if err := json.Unmarshal(value, &override, json.RejectUnknownMembers(true)); err != nil {
			return AccountPlans{}, validationError("ACCOUNT_PLAN_LIMITS", "계정 플랜에 잘못된 키 또는 값이 있다", err)
		}
		limits, err := override.apply(Default())
		if err != nil {
			return AccountPlans{}, err
		}
		plans.limits[accountIDs[key]] = limits
	}
	return plans, nil
}

// For는 계정에 배정한 플랜을 반환하고 값이 없으면 기본 플랜을 반환한다.
func (plans AccountPlans) For(accountID model.ID) Limits {
	if limits, ok := plans.limits[accountID]; ok {
		return limits
	}
	return Default()
}

type limitsOverride struct {
	MaxHops                  jsontext.Value `json:"max_hops"`
	MaxHopNodes              jsontext.Value `json:"max_hop_nodes"`
	GraphPage                jsontext.Value `json:"graph_page"`
	ContextBudget            jsontext.Value `json:"context_budget"`
	GraphsPerAccount         jsontext.Value `json:"graphs_per_account"`
	StoredCharactersPerGraph jsontext.Value `json:"stored_characters_per_graph"`
	RetentionDays            jsontext.Value `json:"retention_days"`
	GraceDays                jsontext.Value `json:"grace_days"`
	AuditRetentionDays       jsontext.Value `json:"audit_retention_days"`
	TierMoveAfterDays        jsontext.Value `json:"tier_move_after_days"`
	RejectedOperationDays    jsontext.Value `json:"rejected_operation_days"`
	WritesPerMinute          jsontext.Value `json:"writes_per_minute"`
	ProposalRetentionDays    jsontext.Value `json:"proposal_retention_days"`
	RelationPage             jsontext.Value `json:"relation_page"`
}

func (override limitsOverride) apply(limits Limits) (Limits, error) {
	var err error
	if limits.MaxHops, err = limit("max_hops", override.MaxHops, limits.MaxHops); err != nil {
		return Limits{}, err
	}
	if limits.MaxHopNodes, err = limit("max_hop_nodes", override.MaxHopNodes, limits.MaxHopNodes); err != nil {
		return Limits{}, err
	}
	if limits.GraphPage, err = applyPage("graph_page", override.GraphPage, limits.GraphPage); err != nil {
		return Limits{}, err
	}
	if limits.ContextBudget, err = limit("context_budget", override.ContextBudget, limits.ContextBudget); err != nil {
		return Limits{}, err
	}
	if limits.GraphsPerAccount, err = limit("graphs_per_account", override.GraphsPerAccount, limits.GraphsPerAccount); err != nil {
		return Limits{}, err
	}
	if limits.StoredCharactersPerGraph, err = limit64("stored_characters_per_graph", override.StoredCharactersPerGraph, limits.StoredCharactersPerGraph); err != nil {
		return Limits{}, err
	}
	if limits.RetentionDays, err = limit("retention_days", override.RetentionDays, limits.RetentionDays); err != nil {
		return Limits{}, err
	}
	if limits.GraceDays, err = limit("grace_days", override.GraceDays, limits.GraceDays); err != nil {
		return Limits{}, err
	}
	if limits.AuditRetentionDays, err = limit("audit_retention_days", override.AuditRetentionDays, limits.AuditRetentionDays); err != nil {
		return Limits{}, err
	}
	if limits.TierMoveAfterDays, err = limit("tier_move_after_days", override.TierMoveAfterDays, limits.TierMoveAfterDays); err != nil {
		return Limits{}, err
	}
	if len(override.RejectedOperationDays) > 0 {
		days, parseErr := limit("rejected_operation_days", override.RejectedOperationDays, 0)
		if parseErr != nil {
			return Limits{}, parseErr
		}
		limits.RejectedOperationRetention = RetentionPeriod{days: days}
	}
	if limits.WritesPerMinute, err = limit("writes_per_minute", override.WritesPerMinute, limits.WritesPerMinute); err != nil {
		return Limits{}, err
	}
	if limits.ProposalRetentionDays, err = limit("proposal_retention_days", override.ProposalRetentionDays, limits.ProposalRetentionDays); err != nil {
		return Limits{}, err
	}
	if limits.RelationPage, err = applyPage("relation_page", override.RelationPage, limits.RelationPage); err != nil {
		return Limits{}, err
	}
	if err := validRetention(limits); err != nil {
		return Limits{}, err
	}
	return limits, nil
}

// validRetention은 「플랜이 정하는 항목」이 기록 보존을 대상 보관 기간 이상으로 정한
// 규칙을 지킨다. 기록이 대상보다 먼저 사라지면 복구 판정이 성립하지 않는다. 0은 기간을
// 두지 않는다는 뜻이므로 대상이 무기한이면 기록도 무기한이어야 한다.
func validRetention(limits Limits) error {
	if limits.AuditRetentionDays == 0 {
		return nil
	}
	if limits.RetentionDays == 0 {
		return validationError("audit_retention_days", "대상 보관이 무기한이면 감사 보존도 무기한이어야 한다", nil)
	}
	if limits.AuditRetentionDays < limits.RetentionDays {
		return validationError("audit_retention_days", "감사 보존은 대상 보관보다 짧을 수 없다", nil)
	}
	return nil
}

type pageSizeOverride struct {
	Default jsontext.Value `json:"default"`
	Maximum jsontext.Value `json:"maximum"`
}

func applyPage(name string, value jsontext.Value, page PageSize) (PageSize, error) {
	if len(value) == 0 {
		return page, nil
	}
	if value.Kind() != jsontext.KindBeginObject {
		return PageSize{}, validationError(name, "페이지 크기는 부분 JSON 객체여야 한다", nil)
	}
	var override pageSizeOverride
	if err := json.Unmarshal(value, &override, json.RejectUnknownMembers(true)); err != nil {
		return PageSize{}, validationError(name, "페이지 크기에 잘못된 키 또는 값이 있다", err)
	}
	var err error
	if page.Default, err = limit(name+".default", override.Default, page.Default); err != nil {
		return PageSize{}, err
	}
	if page.Maximum, err = limit(name+".maximum", override.Maximum, page.Maximum); err != nil {
		return PageSize{}, err
	}
	if page.Default == 0 {
		return PageSize{}, validationError(name+".default", "페이지 기본값은 양수여야 한다", nil)
	}
	if page.Maximum > 0 && page.Maximum < page.Default {
		return PageSize{}, validationError(name+".maximum", "페이지 상한은 기본값보다 작을 수 없다", nil)
	}
	return page, nil
}

func limit(name string, value jsontext.Value, fallback int) (int, error) {
	parsed, err := limit64(name, value, int64(fallback))
	if err != nil {
		return 0, err
	}
	if int64(int(parsed)) != parsed {
		return 0, validationError(name, "플랜 값이 지원하는 정수 범위를 벗어났다", nil)
	}
	return int(parsed), nil
}

func limit64(name string, value jsontext.Value, fallback int64) (int64, error) {
	if len(value) == 0 {
		return fallback, nil
	}
	if value.Kind() != jsontext.KindNumber {
		return 0, validationError(name, "플랜 값은 정수여야 한다", nil)
	}
	var parsed int64
	if err := json.Unmarshal(value, &parsed); err != nil {
		return 0, validationError(name, "플랜 값이 정수가 아니거나 지원 범위를 벗어났다", err)
	}
	if parsed < 0 {
		return 0, validationError(name, "플랜 값은 음수일 수 없다", nil)
	}
	return parsed, nil
}

// validationError는 알려진 필드와 고정 메시지만 출력한다.
// 원인 판별은 Unwrap으로 유지하며 계정 키·구성 원문을 메시지에 복사하지 않는다.
func validationError(field, message string, cause error) error {
	return model.FieldError{Field: field, Message: message, Cause: cause}
}

// Default는 플랜 정책을 따로 부여하지 않은 계정에 적용하는 기본값이다.
func Default() Limits {
	return Limits{
		MaxHops: 4, MaxHopNodes: 200, GraphPage: PageSize{Default: 50, Maximum: 200},
		ContextBudget: 20_000, GraceDays: 30, RejectedOperationRetention: RetentionPeriod{months: 6},
		ProposalRetentionDays: 90, RelationPage: PageSize{Default: 50, Maximum: 200},
	}
}

// LimitError는 한도 초과 항목과 현재·허용 값을 호출자에게 전달한다.
type LimitError struct {
	Name    string
	Current int64
	Allowed int64
}

// Error는 한도 초과를 사람이 읽을 수 있는 형태로 만든다.
func (error LimitError) Error() string {
	return fmt.Sprintf("%s 한도를 초과했다: 현재 %d, 허용 %d", error.Name, error.Current, error.Allowed)
}

// CheckRequest는 하나의 요청 값이 설정된 한도를 넘는지 확인한다.
func CheckRequest(name string, value, allowed int64) error {
	if value < 0 || allowed < 0 {
		return validationError(name, "요청 값과 한도는 음수일 수 없다", nil)
	}
	if allowed > 0 && value > allowed {
		return LimitError{Name: name, Current: value, Allowed: allowed}
	}
	return nil
}

// CheckIncrease는 값을 늘리는 요청만 누적 한도로 막는다. 이미 초과한 상태의 읽기와
// 축소는 허용해 플랜 하향이 기존 데이터를 삭제하지 않게 한다.
func CheckIncrease(name string, current, delta, allowed int64) error {
	if current < 0 || allowed < 0 {
		return validationError(name, "현재 값과 누적 한도는 음수일 수 없다", nil)
	}
	if allowed > 0 && delta > 0 && delta > allowed-current {
		return LimitError{Name: name, Current: current, Allowed: allowed}
	}
	return nil
}
