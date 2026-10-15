package plan

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

const contractAccount = "019a0000-abcd-7000-8000-000000000001"

func contractPlans(t *testing.T, override string) (AccountPlans, model.ID) {
	t.Helper()
	plans, err := ParseAccountPlans(`{"` + contractAccount + `":` + override + `}`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := model.ParseID(contractAccount)
	if err != nil {
		t.Fatal(err)
	}
	return plans, id
}

func TestDefaultPlanMatchesFourteenItems(t *testing.T) {
	want := Limits{
		MaxHops: 4, MaxHopNodes: 200, GraphPage: PageSize{Default: 50, Maximum: 200},
		ContextBudget: 20_000, GraphsPerAccount: 0, StoredCharactersPerGraph: 0,
		RetentionDays: 0, GraceDays: 30, AuditRetentionDays: 0, TierMoveAfterDays: 0,
		RejectedOperationRetention: RetentionPeriod{months: 6}, WritesPerMinute: 0,
		ProposalRetentionDays: 90, RelationPage: PageSize{Default: 50, Maximum: 200},
	}
	if Default() != want || reflect.TypeFor[Limits]().NumField() != 14 {
		t.Fatalf("플랜 항목·기본값: %+v", Default())
	}
	for _, raw := range []string{"", " \t\n", "{}"} {
		plans, err := ParseAccountPlans(raw)
		if err != nil || plans.For(newTestID(t)) != want {
			t.Fatalf("기본 구성: %v", err)
		}
	}
	plans, id := contractPlans(t, "{}")
	if plans.For(id) != want {
		t.Fatal("부분 객체의 생략 값을 바꿨다")
	}
}

func TestAllPlanOverridesAndValueIsolation(t *testing.T) {
	plans, id := contractPlans(t, `{
		"max_hops":7,"max_hop_nodes":321,"graph_page":{"default":60,"maximum":240},
		"context_budget":31000,"graphs_per_account":8,"stored_characters_per_graph":9876543210,
		"retention_days":365,"grace_days":40,"audit_retention_days":366,"tier_move_after_days":20,
		"rejected_operation_days":45,"writes_per_minute":99,"proposal_retention_days":75,
		"relation_page":{"default":70,"maximum":260}}`)
	want := Limits{
		MaxHops: 7, MaxHopNodes: 321, GraphPage: PageSize{Default: 60, Maximum: 240},
		ContextBudget: 31000, GraphsPerAccount: 8, StoredCharactersPerGraph: 9876543210,
		RetentionDays: 365, GraceDays: 40, AuditRetentionDays: 366, TierMoveAfterDays: 20,
		RejectedOperationRetention: RetentionPeriod{days: 45}, WritesPerMinute: 99,
		ProposalRetentionDays: 75, RelationPage: PageSize{Default: 70, Maximum: 260},
	}
	if got := plans.For(id); got != want {
		t.Fatalf("플랜 덮어쓰기: %+v", got)
	}
	copy := plans.For(id)
	copy.MaxHops = 1
	copy.GraphPage.Default = 1
	if plans.For(id) != want || plans.For(newTestID(t)) != Default() {
		t.Fatal("값 복사가 원래 플랜을 바꿨다")
	}
	other, otherID := contractPlans(t, `{"max_hops":2}`)
	if other.For(otherID).MaxHops != 2 || plans.For(id) != want {
		t.Fatal("구성 재해석이 기존 집합을 바꿨다")
	}
}

func TestScalarPlanValuesRequireNonnegativeIntegers(t *testing.T) {
	for _, field := range []string{
		"max_hops", "max_hop_nodes", "context_budget", "graphs_per_account",
		"stored_characters_per_graph", "retention_days", "grace_days", "audit_retention_days",
		"tier_move_after_days", "rejected_operation_days", "writes_per_minute", "proposal_retention_days",
	} {
		for _, value := range []string{"null", "-1", "1.5", `"1"`, "true", "[]", "{}", "9223372036854775808"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				_, err := ParseAccountPlans(`{"` + contractAccount + `":{"` + field + `":` + value + `}}`)
				violation, ok := errors.AsType[model.FieldError](err)
				if !ok || violation.Field != field {
					t.Fatalf("위반 필드: %v", err)
				}
			})
		}
	}
	plans, id := contractPlans(t, `{"max_hops":0,"max_hop_nodes":0,"context_budget":0,"graphs_per_account":0,"stored_characters_per_graph":0,"retention_days":0,"grace_days":0,"audit_retention_days":0,"tier_move_after_days":0,"rejected_operation_days":0,"writes_per_minute":0,"proposal_retention_days":0}`)
	got := plans.For(id)
	if got.MaxHops != 0 || got.MaxHopNodes != 0 || got.ContextBudget != 0 || got.GraceDays != 0 || got.RejectedOperationRetention.DaysAt(time.Now()) != 0 || got.ProposalRetentionDays != 0 {
		t.Fatalf("명시한 0: %+v", got)
	}
}

func TestPageOverridesArePartialObjects(t *testing.T) {
	for _, field := range []string{"graph_page", "relation_page"} {
		for _, value := range []string{"null", "[]", "1", `{"default":null}`, `{"maximum":null}`, `{"default":0}`, `{"default":201}`, `{"default":-1}`, `{"maximum":-1}`, `{"other":1}`, `{"Default":1}`, `{"default":1,"default":2}`} {
			t.Run(field+"/"+value, func(t *testing.T) {
				if _, err := ParseAccountPlans(`{"` + contractAccount + `":{"` + field + `":` + value + `}}`); err == nil {
					t.Fatal("잘못된 페이지를 수락했다")
				}
			})
		}
		for _, value := range []string{"{}", `{"default":200}`, `{"maximum":0}`, `{"default":300,"maximum":0}`} {
			contractPlans(t, `{"`+field+`":`+value+`}`)
		}
	}
}

func TestPlanJSONShapeDuplicatesAndSafeMessages(t *testing.T) {
	for _, raw := range []string{
		"null", "[]", "1", "true", `"object"`, "{", "{}{}",
		`{"` + contractAccount + `":null}`, `{"` + contractAccount + `":[]}`,
		`{"` + contractAccount + `":{"max_hops":1,"max_hops":2}}`,
		`{"` + contractAccount + `":{},"` + contractAccount + `":{}}`,
		`{"` + contractAccount + `":{},"\u0030` + contractAccount[1:] + `":{}}`,
		`{"` + contractAccount + `":{"MaxHops":1}}`,
		`{"` + contractAccount + `":{"audit_retention_days":1}}`,
		`{"` + contractAccount + `":{"retention_days":2,"audit_retention_days":1}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			plans, err := ParseAccountPlans(raw)
			if err == nil || plans.For(newTestID(t)) != Default() {
				t.Fatalf("구성 거부·실패 집합: %v", err)
			}
		})
	}
	for _, raw := range []string{
		`{"secret-account-key":{}}`,
		`{"` + contractAccount + `":{"secret-field":"credential-secret"}}`,
		`{"` + contractAccount + `":{"max_hops":"credential-secret"}}`,
	} {
		_, err := ParseAccountPlans(raw)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), contractAccount) {
			t.Fatalf("구성 입력을 메시지에 복사했다: %v", err)
		}
	}
	_, err := ParseAccountPlans(`{"` + contractAccount + `":{"unexpected":1}}`)
	if !errors.Is(err, json.ErrUnknownName) {
		t.Fatalf("JSON 의미 오류 원인: %v", err)
	}
	_, err = ParseAccountPlans(`{"` + contractAccount + `":{"max_hops":1,"max_hops":2}}`)
	if !errors.Is(err, jsontext.ErrDuplicateName) {
		t.Fatalf("JSON 중복 원인: %v", err)
	}
}

func TestEquivalentKeysRejectedBeforeValues(t *testing.T) {
	alias := strings.ToUpper(contractAccount)
	for _, reversed := range []bool{false, true} {
		first, second := contractAccount, alias
		if reversed {
			first, second = second, first
		}
		_, err := ParseAccountPlans(fmt.Sprintf(`{"%s":{"max_hops":-1},"%s":null}`, first, second))
		if err == nil || !strings.Contains(err.Error(), "중복") {
			t.Fatalf("값 적용 전 중복 검사: %v", err)
		}
	}
}

func TestLimitBoundariesAndTypedMetadata(t *testing.T) {
	for _, sample := range []struct {
		name                    string
		current, delta, allowed int64
		rejected                bool
	}{
		{"boundary", 9, 1, 10, false}, {"above", 9, 2, 10, true}, {"existing_above", 11, 1, 10, true},
		{"read_after_downgrade", 11, 0, 10, false}, {"shrink_after_downgrade", 11, -1, 10, false},
		{"unlimited", math.MaxInt64, math.MaxInt64, 0, false},
		{"max_boundary", math.MaxInt64 - 1, 1, math.MaxInt64, false},
		{"max_above", math.MaxInt64, 1, math.MaxInt64, true},
		{"sum_would_overflow", math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			err := CheckIncrease("storage", sample.current, sample.delta, sample.allowed)
			limit, ok := errors.AsType[LimitError](err)
			if ok != sample.rejected || (!sample.rejected && err != nil) {
				t.Fatalf("누적 판정: %v", err)
			}
			if ok && (limit.Name != "storage" || limit.Current != sample.current || limit.Allowed != sample.allowed) {
				t.Fatalf("한도 오류 메타데이터: %+v", limit)
			}
		})
	}
	if CheckRequest("hop", 4, 4) != nil || CheckRequest("hop", math.MaxInt64, 0) != nil {
		t.Fatal("요청 경계 또는 무제한 거부")
	}
	err := CheckRequest("hop", 5, 4)
	limit, ok := errors.AsType[LimitError](err)
	if !ok || limit.Name != "hop" || limit.Current != 5 || limit.Allowed != 4 {
		t.Fatalf("요청 오류 메타데이터: %v", err)
	}
	for _, err := range []error{CheckRequest("hop", -1, 0), CheckRequest("hop", 1, -1), CheckIncrease("storage", -1, 1, 0), CheckIncrease("storage", 1, 1, -1)} {
		if _, ok := errors.AsType[model.FieldError](err); !ok {
			t.Fatalf("음수 인자 거부: %v", err)
		}
	}
}
