package main

import (
	"context"
	"errors"
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
