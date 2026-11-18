package store

import (
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestIngestCredentialDatabase(t *testing.T) {
	first, second, pool := newAgentContractDatabase(t)
	account, other := newTestID(t), newTestID(t)
	createTestAccount(t, first, account)
	createTestAccount(t, first, other)
	token, err := first.IssueIngestCredential(t.Context(), account, "합성 자격")
	if err != nil {
		t.Fatal(err)
	}
	items, err := second.ListIngestCredentials(t.Context(), account)
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v, %d", err, len(items))
	}
	var saved []byte
	if err := pool.QueryRow(t.Context(), `SELECT token_hash FROM public.ingest_credential WHERE credential_id=$1`, items[0].ID.String()).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(token))
	if string(saved) != string(hash[:]) {
		t.Fatal("자격 해시 불일치")
	}
	gotAccount, agent, err := second.ValidateIngestCredential(t.Context(), token)
	if err != nil || gotAccount != account || !agent.IsV7() {
		t.Fatal("자격 검증 실패", err)
	}
	ordinary, err := first.RegisterAgent(t.Context(), account, "client", strings.ReplaceAll(items[0].ID.String(), "-", ""))
	if err != nil || ordinary == agent {
		t.Fatal("agent_register가 수집 결합 등록을 재사용했다", err)
	}
	if err := first.ValidateAgent(t.Context(), account, "client", agent); err == nil {
		t.Fatal("MCP 행위자로 허용")
	}
	if err := first.RevokeAgent(t.Context(), account, agent); err == nil {
		t.Fatal("일반 등록 폐기로 자격 등록 변경")
	}
	if err := second.RevokeIngestCredential(t.Context(), other, items[0].ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("다른 계정 폐기 허용", err)
	}
	if _, _, err := second.ValidateIngestCredential(t.Context(), "oauth-token"); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("다른 토큰 허용")
	}
	if err := first.RevokeIngestCredential(t.Context(), account, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := second.ValidateIngestCredential(t.Context(), token); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("폐기 자격 허용")
	}
	for _, change := range []string{
		"issued_at=now()-interval '31 days',last_used_at=NULL",
		"issued_at=now()-interval '1 year 1 day',last_used_at=now()",
	} {
		expired, err := first.IssueIngestCredential(t.Context(), account, "만료 시험")
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(expired))
		if _, err := pool.Exec(t.Context(), `UPDATE public.ingest_credential SET `+change+` WHERE token_hash=$1`, digest[:]); err != nil {
			t.Fatal(err)
		}
		if _, _, err := second.ValidateIngestCredential(t.Context(), expired); !errors.Is(err, model.ErrNotFound) {
			t.Fatal("만료 자격 허용", err)
		}
	}
	for range 20 {
		if _, err := first.IssueIngestCredential(t.Context(), account, "합성"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.IssueIngestCredential(t.Context(), account, "초과"); err == nil {
		t.Fatal("자격 한도 초과 허용")
	} else if _, ok := errors.AsType[plan.LimitError](err); !ok {
		t.Fatal(err)
	}
	limited := newTestID(t)
	createTestAccount(t, first, limited)
	for range 128 {
		id := newTestID(t)
		if _, err := pool.Exec(t.Context(), `INSERT INTO public.agent_registration (agent_id,account_id,client_id,registration_key,created_at) VALUES ($1,$2,'test',$3,now())`, id.String(), limited.String(), strings.ReplaceAll(id.String(), "-", "")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.IssueIngestCredential(t.Context(), limited, "등록 초과"); err == nil {
		t.Fatal("등록 한도 초과 허용")
	} else if limit, ok := errors.AsType[plan.LimitError](err); !ok || limit.Name != "agent_registrations_per_account" {
		t.Fatal(err)
	}

}
