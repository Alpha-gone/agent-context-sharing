package plan

import (
	"testing"

	"agent_context_sharing/internal/model"
)

// newTestID는 계정별 플랜 구성에 쓸 UUIDv7 식별자를 만든다.
func newTestID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("UUIDv7 생성: %v", err)
	}
	return id
}

func TestParseAccountPlans(t *testing.T) {
	accountID := newTestID(t)
	plans, err := ParseAccountPlans(`{"` + accountID.String() + `":{"max_hops":6,"grace_days":0,"graph_page":{"default":100,"maximum":300}}}`)
	if err != nil {
		t.Fatalf("계정 플랜 해석: %v", err)
	}
	got := plans.For(accountID)
	if got.MaxHops != 6 || got.GraceDays != 0 || got.GraphPage != (PageSize{Default: 100, Maximum: 300}) {
		t.Fatalf("계정 플랜 = %+v", got)
	}
	if got := plans.For(newTestID(t)); got != Default() {
		t.Fatalf("기본 플랜 = %+v, want %+v", got, Default())
	}
}

// TestParseAccountPlansAllowsRetentionAtOrAboveTarget은 기록 보존 하한을 만족하는
// 조합이 거부되지 않는지 확인한다. 하한 검사가 정상 구성까지 막으면 안 된다.
func TestParseAccountPlansAllowsRetentionAtOrAboveTarget(t *testing.T) {
	accountID := newTestID(t)
	tests := map[string]string{
		"기록 보존이 대상 보관과 같음":   `{"retention_days":365,"audit_retention_days":365}`,
		"기록 보존이 대상 보관보다 긺":   `{"retention_days":365,"audit_retention_days":730}`,
		"대상 보관만 정하면 기록은 무기한": `{"retention_days":365}`,
		"둘 다 무기한":            `{"retention_days":0,"audit_retention_days":0}`,
	}
	for name, override := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAccountPlans(`{"` + accountID.String() + `":` + override + `}`); err != nil {
				t.Fatalf("정상 구성이 거부됐다: %v", err)
			}
		})
	}
}

func TestParseAccountPlansRejectsInvalidValues(t *testing.T) {
	accountID := newTestID(t)
	tests := map[string]string{
		"JSON이 객체가 아님":      `[]`,
		"잘못된 계정 식별자":        `{"not-an-id":{"max_hops":6}}`,
		"UUIDv4 계정 식별자":     `{"9f1b0c3e-4d5a-4b6c-8d7e-9f0a1b2c3d4e":{"max_hops":6}}`,
		"목록 밖 구성 키":         `{"` + accountID.String() + `":{"other":1}}`,
		"음수 한도":             `{"` + accountID.String() + `":{"max_hops":-1}}`,
		"페이지 기본값이 0":        `{"` + accountID.String() + `":{"graph_page":{"default":0}}}`,
		"페이지 상한이 기본값보다 작음":  `{"` + accountID.String() + `":{"relation_page":{"default":100,"maximum":50}}}`,
		"변형 비트가 틀린 계정 식별자":  `{"01991b3c-8f2a-7c3d-1e4f-5a6b7c8d9e0f":{"max_hops":6}}`,
		"영 계정 식별자":          `{"00000000-0000-7000-0000-000000000000":{"max_hops":6}}`,
		"기록 보존이 대상 보관보다 짧음": `{"` + accountID.String() + `":{"retention_days":365,"audit_retention_days":30}}`,
		"무기한 보관에 유한한 기록 보존": `{"` + accountID.String() + `":{"audit_retention_days":90}}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAccountPlans(raw); err == nil {
				t.Fatal("잘못된 플랜 값이 허용됐다")
			}
		})
	}
}

func TestChecks(t *testing.T) {
	if err := CheckRequest("hop", 5, 4); err == nil {
		t.Fatal("요청 한도 초과가 허용됐다")
	}
	if err := CheckIncrease("storage", 11, 1, 10); err == nil {
		t.Fatal("초과 상태 확장이 허용됐다")
	}
	if err := CheckIncrease("storage", 11, -1, 10); err != nil {
		t.Fatalf("초과 상태 축소가 거부됐다: %v", err)
	}
	if err := CheckIncrease("storage", 9, 2, 10); err == nil {
		t.Fatal("한 번의 확장으로 한도를 넘는 요청이 허용됐다")
	}
	if err := CheckIncrease("storage", 9, 1, 10); err != nil {
		t.Fatalf("한도까지의 확장이 거부됐다: %v", err)
	}
}
