// Package plan은 계정 플랜의 고정 항목과 한도 판정을 제공한다.
package plan

import (
	"encoding/json/v2"
	"fmt"
	"strings"

	"agent_context_sharing/internal/model"
)

// Limits는 계정 플랜이 정할 수 있는 14개 항목의 값이다. 0은 해당 한도가 없음을 뜻한다.
type Limits struct {
	MaxHops                  int
	MaxHopNodes              int
	GraphPage                PageSize
	ContextBudget            int
	GraphsPerAccount         int
	StoredCharactersPerGraph int64
	RetentionDays            int
	GraceDays                int
	AuditRetentionDays       int
	TierMoveAfterDays        int
	RejectedOperationDays    int
	WritesPerMinute          int
	ProposalRetentionDays    int
	RelationPage             PageSize
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

	var overrides map[string]limitsOverride
	if err := json.Unmarshal([]byte(raw), &overrides, json.RejectUnknownMembers(true)); err != nil {
		return AccountPlans{}, fmt.Errorf("JSON 해석: %w", err)
	}
	if overrides == nil {
		return AccountPlans{}, fmt.Errorf("JSON 객체가 아니다")
	}

	plans := AccountPlans{limits: make(map[model.ID]Limits, len(overrides))}
	for accountID, override := range overrides {
		parsed, err := model.ParseID(accountID)
		if err != nil {
			return AccountPlans{}, fmt.Errorf("계정 식별자 %q가 UUIDv7이 아니다: %w", accountID, err)
		}
		limits, err := override.apply(Default())
		if err != nil {
			return AccountPlans{}, fmt.Errorf("계정 %s 플랜 값: %w", parsed, err)
		}
		plans.limits[parsed] = limits
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
	MaxHops                  *int              `json:"max_hops"`
	MaxHopNodes              *int              `json:"max_hop_nodes"`
	GraphPage                *pageSizeOverride `json:"graph_page"`
	ContextBudget            *int              `json:"context_budget"`
	GraphsPerAccount         *int              `json:"graphs_per_account"`
	StoredCharactersPerGraph *int64            `json:"stored_characters_per_graph"`
	RetentionDays            *int              `json:"retention_days"`
	GraceDays                *int              `json:"grace_days"`
	AuditRetentionDays       *int              `json:"audit_retention_days"`
	TierMoveAfterDays        *int              `json:"tier_move_after_days"`
	RejectedOperationDays    *int              `json:"rejected_operation_days"`
	WritesPerMinute          *int              `json:"writes_per_minute"`
	ProposalRetentionDays    *int              `json:"proposal_retention_days"`
	RelationPage             *pageSizeOverride `json:"relation_page"`
}

func (override limitsOverride) apply(limits Limits) (Limits, error) {
	var err error
	if limits.MaxHops, err = limit("max_hops", override.MaxHops, limits.MaxHops); err != nil {
		return Limits{}, err
	}
	if limits.MaxHopNodes, err = limit("max_hop_nodes", override.MaxHopNodes, limits.MaxHopNodes); err != nil {
		return Limits{}, err
	}
	if limits.GraphPage, err = override.GraphPage.apply("graph_page", limits.GraphPage); err != nil {
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
	if limits.RejectedOperationDays, err = limit("rejected_operation_days", override.RejectedOperationDays, limits.RejectedOperationDays); err != nil {
		return Limits{}, err
	}
	if limits.WritesPerMinute, err = limit("writes_per_minute", override.WritesPerMinute, limits.WritesPerMinute); err != nil {
		return Limits{}, err
	}
	if limits.ProposalRetentionDays, err = limit("proposal_retention_days", override.ProposalRetentionDays, limits.ProposalRetentionDays); err != nil {
		return Limits{}, err
	}
	if limits.RelationPage, err = override.RelationPage.apply("relation_page", limits.RelationPage); err != nil {
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
		return fmt.Errorf("retention_days가 무기한이면 audit_retention_days도 무기한이어야 한다")
	}
	if limits.AuditRetentionDays < limits.RetentionDays {
		return fmt.Errorf("audit_retention_days는 retention_days보다 작을 수 없다")
	}
	return nil
}

type pageSizeOverride struct {
	Default *int `json:"default"`
	Maximum *int `json:"maximum"`
}

func (override *pageSizeOverride) apply(name string, page PageSize) (PageSize, error) {
	if override == nil {
		return page, nil
	}
	var err error
	if page.Default, err = limit(name+".default", override.Default, page.Default); err != nil {
		return PageSize{}, err
	}
	if page.Maximum, err = limit(name+".maximum", override.Maximum, page.Maximum); err != nil {
		return PageSize{}, err
	}
	if page.Default == 0 {
		return PageSize{}, fmt.Errorf("%s.default는 양수여야 한다", name)
	}
	if page.Maximum > 0 && page.Maximum < page.Default {
		return PageSize{}, fmt.Errorf("%s.maximum은 default보다 작을 수 없다", name)
	}
	return page, nil
}

func limit(name string, value *int, fallback int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	if *value < 0 {
		return 0, fmt.Errorf("%s는 음수일 수 없다", name)
	}
	return *value, nil
}

func limit64(name string, value *int64, fallback int64) (int64, error) {
	if value == nil {
		return fallback, nil
	}
	if *value < 0 {
		return 0, fmt.Errorf("%s는 음수일 수 없다", name)
	}
	return *value, nil
}

// Default는 플랜 정책을 따로 부여하지 않은 계정에 적용하는 기본값이다.
func Default() Limits {
	return Limits{
		MaxHops: 4, MaxHopNodes: 200, GraphPage: PageSize{Default: 50, Maximum: 200},
		ContextBudget: 20_000, GraceDays: 30, RejectedOperationDays: 183,
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
	if allowed > 0 && value > allowed {
		return LimitError{Name: name, Current: value, Allowed: allowed}
	}
	return nil
}

// CheckIncrease는 값을 늘리는 요청만 누적 한도로 막는다. 이미 초과한 상태의 읽기와
// 축소는 허용해 플랜 하향이 기존 데이터를 삭제하지 않게 한다.
func CheckIncrease(name string, current, delta, allowed int64) error {
	if allowed > 0 && delta > 0 && delta > allowed-current {
		return LimitError{Name: name, Current: current, Allowed: allowed}
	}
	return nil
}
