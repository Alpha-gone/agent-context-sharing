package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

type auditRetentionProbe struct {
	fakePeriodicStore
	at                                   time.Time
	account                              model.ID
	defaultDays, explicitDays, auditDays int
	err                                  error
}

type planRetentionMatrixProbe struct {
	fakePeriodicStore
	accounts        []model.ID
	audit, rejected []int
	at              time.Time
}

func (probe *planRetentionMatrixProbe) CleanupAuditRecords(_ context.Context, at time.Time, auditFor, rejectedFor store.RetentionDays) (int, error) {
	probe.at = at
	for _, account := range probe.accounts {
		probe.audit = append(probe.audit, auditFor(account))
		probe.rejected = append(probe.rejected, rejectedFor(account))
	}
	return len(probe.accounts), nil
}

func TestPeriodicPlanRetentionMatrix(t *testing.T) {
	finite, err := model.ParseID("019a0000-abcd-7000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	unlimited, err := model.ParseID("019a0000-abcd-7000-8000-000000000003")
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"retention_days":365,"audit_retention_days":400,"rejected_operation_days":30},"%s":{"rejected_operation_days":0}}`, finite, unlimited))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := newPeriodicWorker(&store.Store{}, plans, nil)
	if err != nil {
		t.Fatal(err)
	}
	probe := &planRetentionMatrixProbe{accounts: []model.ID{{}, finite, unlimited}}
	worker.store = probe
	at := time.Date(2028, 11, 1, 1, 34, 56, 0, time.FixedZone("KST", 9*60*60))
	worker.now = func() time.Time { return at }
	for _, task := range worker.tasks {
		if task.name != "audit_cleanup" {
			continue
		}
		if _, err := task.run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !probe.at.Equal(at) || probe.at.Location() != time.UTC {
			t.Fatalf("회차 UTC 기준=%v", probe.at)
		}
		for index, want := range []struct{ audit, rejected int }{{0, 184}, {400, 30}, {0, 0}} {
			if probe.audit[index] != want.audit || probe.rejected[index] != want.rejected {
				t.Fatalf("계정 %d 보존=%d/%d, want %+v", index, probe.audit[index], probe.rejected[index], want)
			}
		}
		return
	}
	t.Fatal("감사 정리 작업이 없다")
}

func (probe *auditRetentionProbe) CleanupAuditRecords(_ context.Context, at time.Time, auditFor, rejectedFor store.RetentionDays) (int, error) {
	probe.at = at
	probe.defaultDays = rejectedFor(model.ID{})
	probe.explicitDays = rejectedFor(probe.account)
	probe.auditDays = auditFor(probe.account)
	return 7, probe.err
}

func TestPeriodicAuditUsesOneCalendarReference(t *testing.T) {
	const accountKey = "019a0000-abcd-7000-8000-000000000001"
	account, err := model.ParseID(accountKey)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.ParseAccountPlans(`{"` + accountKey + `":{"retention_days":365,"audit_retention_days":365,"rejected_operation_days":0}}`)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := newPeriodicWorker(&store.Store{}, plans, nil)
	if err != nil {
		t.Fatal(err)
	}
	probe := &auditRetentionProbe{account: account, err: errors.New("synthetic database failure")}
	worker.store = probe
	at := time.Date(2024, 10, 31, 12, 34, 56, 0, time.UTC)
	clockCalls := 0
	worker.now = func() time.Time { clockCalls++; return at.AddDate(0, 0, clockCalls-1) }
	for _, task := range worker.tasks {
		if task.name != "audit_cleanup" {
			continue
		}
		count, err := task.run(t.Context())
		if count != 7 || !errors.Is(err, probe.err) || clockCalls != 1 || !probe.at.Equal(at) || probe.defaultDays != 184 || probe.explicitDays != 0 || probe.auditDays != 365 {
			t.Fatalf("플랜 기간 전달: count=%d err=%v clock=%d probe=%+v", count, err, clockCalls, probe)
		}
		return
	}
	t.Fatal("감사 정리 작업이 없다")
}
