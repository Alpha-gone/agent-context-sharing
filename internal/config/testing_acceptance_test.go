package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestTEST02PlanAliasRejectionAtConfigurationBoundary(t *testing.T) {
	const account = "019a0000-abcd-7000-8000-000000000001"
	for _, alias := range []string{strings.ToUpper(account), strings.ReplaceAll(account, "-", "")} {
		for _, reversed := range []bool{false, true} {
			for _, secondValue := range []string{`{"max_hops":1}`, `{"max_hops":2}`, `null`} {
				t.Run(fmt.Sprintf("%s/%t/%s", alias, reversed, secondValue), func(t *testing.T) {
					first, second := account, alias
					if reversed {
						first, second = second, first
					}
					values := validValues()
					values["ACCOUNT_PLAN_LIMITS"] = fmt.Sprintf(`{"%s":{"max_hops":1},"%s":%s}`, first, second, secondValue)
					err := rejectValues(t, values)
					field, ok := errors.AsType[model.FieldError](err)
					if !ok || field.Field != "ACCOUNT_PLAN_LIMITS" || !strings.Contains(err.Error(), "중복") || strings.Contains(err.Error(), account[:8]) {
						t.Fatalf("기동 전 중복 거부·안전한 필드 오류: %v", err)
					}
				})
			}
		}
	}
}

func TestTEST02RetentionAtConfigurationBoundary(t *testing.T) {
	const account = "019a0000-abcd-7000-8000-000000000001"
	id, err := model.ParseID(account)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		target, audit int
		bad           bool
	}{
		{0, 0, false}, {0, 1, true},
		{1, 0, false}, {1, 1, false}, {1, 2, false},
		{365, 0, false}, {365, 364, true}, {365, 365, false}, {365, 366, false},
	} {
		t.Run(fmt.Sprintf("%d/%d", sample.target, sample.audit), func(t *testing.T) {
			values := validValues()
			values["ACCOUNT_PLAN_LIMITS"] = fmt.Sprintf(`{"%s":{"retention_days":%d,"audit_retention_days":%d}}`, account, sample.target, sample.audit)
			if sample.bad {
				field, ok := errors.AsType[model.FieldError](rejectValues(t, values))
				if !ok || field.Field != "audit_retention_days" {
					t.Fatalf("보존 하한 오류 필드 = %s", field.Field)
				}
				return
			}
			cfg, err := loadValues(values)
			if err != nil {
				t.Fatal(err)
			}
			limits := cfg.AccountPlans.For(id)
			if limits.RetentionDays != sample.target || limits.AuditRetentionDays != sample.audit {
				t.Fatalf("적용된 보존 기간: %+v", limits)
			}
		})
	}
}
