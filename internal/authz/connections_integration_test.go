package authz

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type cancelCommittedConnection struct {
	ConnectionStore
	cancel context.CancelFunc
}

func (source cancelCommittedConnection) RevokeOAuthGrant(ctx context.Context, account, grant model.ID) error {
	err := source.ConnectionStore.RevokeOAuthGrant(ctx, account, grant)
	if err == nil {
		source.cancel()
	}
	return err
}

func TestConnectionsDatabaseIntegration(t *testing.T) {
	firstStore, secondStore, pool := newAuthenticationContractDatabase(t)
	config := oauthTestConfig()
	makeService := func(backend *store.Store) *Service {
		t.Helper()
		cfg := config
		cfg.AuthorizationCodes, cfg.AccessTokens, cfg.RefreshTokens, cfg.Connections = backend, backend, backend, backend
		s, err := New(backend, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first, second := makeService(firstStore), makeService(secondStore)
	owner, err := first.Register(t.Context(), "connections_owner", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	other, err := first.Register(t.Context(), "connections_other", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(t *testing.T, account model.ID) (OAuthTokens, model.ID) {
		t.Helper()
		code, err := first.IssueAuthorizationCode(t.Context(), account, validAuthorizationRequest())
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := first.ExchangeAuthorizationCode(t.Context(), code, "first", "http://127.0.0.1:49152/callback", config.OAuth.Resource, oauthVerifier)
		if err != nil {
			t.Fatal(err)
		}
		p, err := second.VerifyBearer(t.Context(), tokens.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		return tokens, p.GrantID
	}
	denied := func(t *testing.T, tokens OAuthTokens) {
		t.Helper()
		for _, s := range []*Service{first, second, makeService(secondStore)} {
			if _, err := s.VerifyBearer(t.Context(), tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
				t.Fatal("revocation not shared")
			}
			result, err := s.Refresh(t.Context(), tokens.RefreshToken, "first", config.OAuth.Resource, "")
			failure, ok := errors.AsType[*OAuthError](err)
			if !ok || failure.Code != "invalid_grant" || result != (OAuthTokens{}) {
				t.Fatal("refresh survived revocation")
			}
		}
	}
	state := func(t *testing.T, grant model.ID) (time.Time, string) {
		t.Helper()
		var at time.Time
		var reason string
		if err := pool.QueryRow(t.Context(), `SELECT revoked_at,revocation_reason FROM public.oauth_grant WHERE grant_id=$1`, grant.String()).Scan(&at, &reason); err != nil {
			t.Fatal(err)
		}
		return at, reason
	}
	t.Run("owner isolation and independent credentials", func(t *testing.T) {
		tokens, grant := issue(t, owner)
		agent, err := firstStore.RegisterAgent(t.Context(), owner, "first", "private_registration_key")
		if err != nil {
			t.Fatal(err)
		}
		session, err := first.WebSession(t.Context(), owner, "web")
		if err != nil {
			t.Fatal(err)
		}
		page, err := second.ListConnections(t.Context(), other, model.ID{})
		if err != nil || len(page.Grants) != 0 || len(page.Agents) != 0 {
			t.Fatal("foreign listing")
		}
		missing, _ := model.NewID()
		for _, action := range []string{"revoke_grant", "revoke_agent"} {
			for _, target := range []model.ID{grant, agent, missing} {
				if err := second.RevokeConnection(t.Context(), other, action, target); !errors.Is(err, model.ErrNotFound) {
					t.Fatal("foreign or absent distinguishable")
				}
			}
		}
		if err := second.RevokeConnection(t.Context(), owner, "revoke_grant", grant); err != nil {
			t.Fatal(err)
		}
		denied(t, tokens)
		at, reason := state(t, grant)
		if err := first.RevokeConnection(t.Context(), owner, "revoke_grant", grant); err != nil {
			t.Fatal(err)
		}
		at2, reason2 := state(t, grant)
		if at != at2 || reason != "owner_revoked" || reason != reason2 {
			t.Fatal("repeated revoke changed provenance")
		}
		if _, _, err := second.Verify(t.Context(), session.Raw, "web"); err != nil {
			t.Fatal("web session revoked")
		}
		if err := secondStore.ValidateAgent(t.Context(), owner, "first", agent); err != nil {
			t.Fatal("grant revoked independent agent")
		}
		fresh, _ := issue(t, owner)
		if same, err := secondStore.RegisterAgent(t.Context(), owner, "first", "private_registration_key"); err != nil || same != agent {
			t.Fatal("reauthorization lost registration")
		}
		if err := first.RevokeConnection(t.Context(), owner, "revoke_agent", agent); err != nil {
			t.Fatal(err)
		}
		if err := secondStore.ValidateAgent(t.Context(), owner, "first", agent); err == nil {
			t.Fatal("revoked agent accepted")
		}
		if _, err := second.VerifyBearer(t.Context(), fresh.AccessToken); err != nil {
			t.Fatal("registration revoked bearer")
		}
		page, err = second.ListConnections(t.Context(), owner, model.ID{})
		if err != nil || len(page.Agents) != 0 {
			t.Fatal("revoked agent visible")
		}
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.agent_registration WHERE agent_id=$1 AND revoked_at IS NOT NULL`, agent.String()).Scan(&count); err != nil || count != 1 {
			t.Fatal("registration history deleted")
		}
	})
	t.Run("commit survives canceled response", func(t *testing.T) {
		tokens, grant := issue(t, owner)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		source := first.config.Connections
		first.config.Connections = cancelCommittedConnection{ConnectionStore: source, cancel: cancel}
		err := first.RevokeConnection(ctx, owner, "revoke_grant", grant)
		first.config.Connections = source
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled response became success")
		}
		denied(t, tokens)
		_, reason := state(t, grant)
		if reason != "owner_revoked" {
			t.Fatal("committed revocation was compensated")
		}
	})
	t.Run("bounded stable owner pages", func(t *testing.T) {
		account, err := first.Register(t.Context(), "connections_paging", "correct password")
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		now := time.Now().UTC().Truncate(time.Second)
		for range 57 {
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id.String())
			_, err = pool.Exec(t.Context(), `INSERT INTO public.oauth_grant(grant_id,account_id,client_id,resource,scope,authorized_at,last_rotated_at,absolute_expires_at,idle_expires_at,retain_until) VALUES($1,$2,'first',$3,'agent-context',$4,$4,$5,$6,$7)`, id.String(), account.String(), config.OAuth.Resource, now, now.Add(30*24*time.Hour), now.Add(7*24*time.Hour), now.Add(30*24*time.Hour+time.Hour))
			if err != nil {
				t.Fatal(err)
			}
		}
		slices.Sort(ids)
		slices.Reverse(ids)
		page, err := second.ListConnections(t.Context(), account, model.ID{})
		if err != nil || len(page.Grants) != 50 || !page.Next.IsV7() {
			t.Fatal("first bounded page")
		}
		next, err := first.ListConnections(t.Context(), account, page.Next)
		if err != nil || len(next.Grants) != 7 || next.Next.IsV7() {
			t.Fatal("second page")
		}
		for i, g := range append(page.Grants, next.Grants...) {
			if g.ID.String() != ids[i] {
				t.Fatal("paging duplicate or ordering")
			}
		}
	})
	t.Run("revocation serializes with rotation", func(t *testing.T) {
		tokens, grant := issue(t, owner)
		lock, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback(context.WithoutCancel(t.Context()))
		if _, err := lock.Exec(t.Context(), `SELECT 1 FROM public.oauth_grant WHERE grant_id=$1 FOR UPDATE`, grant.String()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		type refreshResult struct {
			tokens OAuthTokens
			err    error
		}
		rotated := make(chan refreshResult, 1)
		revoked := make(chan error, 1)
		var workers sync.WaitGroup
		defer workers.Wait()
		workers.Go(func() {
			v, e := second.Refresh(ctx, tokens.RefreshToken, "first", config.OAuth.Resource, "")
			rotated <- refreshResult{v, e}
		})
		waitRefreshLock(t, ctx, pool, "oauth_grant")
		workers.Go(func() { revoked <- first.RevokeConnection(ctx, owner, "revoke_grant", grant) })
		if err := lock.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-revoked; err != nil {
			t.Fatal(err)
		}
		result := <-rotated
		if result.err != nil {
			failure, ok := errors.AsType[*OAuthError](result.err)
			if !ok || failure.Code != "invalid_grant" {
				t.Fatal("unexpected rotation failure")
			}
		} else {
			denied(t, result.tokens)
		}
		denied(t, tokens)
	})
	t.Run("cancel waiting revoke rolls back", func(t *testing.T) {
		tokens, grant := issue(t, owner)
		lock, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback(context.WithoutCancel(t.Context()))
		if _, err := lock.Exec(t.Context(), `SELECT 1 FROM public.oauth_grant WHERE grant_id=$1 FOR UPDATE`, grant.String()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- second.RevokeConnection(ctx, owner, "revoke_grant", grant) }()
		wait, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		waitRefreshLock(t, wait, pool, "oauth_grant")
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("cancel cause lost")
		}
		if err := lock.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := first.VerifyBearer(t.Context(), tokens.AccessToken); err != nil {
			t.Fatal("cancel revoked grant")
		}
		// pgx가 취소 연결을 닫은 뒤 PostgreSQL의 backend 종료 반영까지 관찰한다.
		for {
			var idle int
			if err := pool.QueryRow(wait, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='idle in transaction'`).Scan(&idle); err != nil {
				t.Fatal(err)
			}
			if idle == 0 {
				break
			}
			select {
			case <-wait.Done():
				t.Fatal("transaction did not recover")
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	t.Run("storage failure keeps credential", func(t *testing.T) {
		tokens, grant := issue(t, owner)
		_, err := pool.Exec(t.Context(), `CREATE FUNCTION public.auth10_revoke_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic-connection-secret'; END $$; CREATE TRIGGER auth10_revoke_failure BEFORE UPDATE ON public.oauth_grant FOR EACH ROW EXECUTE FUNCTION public.auth10_revoke_failure()`)
		if err != nil {
			t.Fatal(err)
		}
		err = second.RevokeConnection(t.Context(), owner, "revoke_grant", grant)
		_, dropErr := pool.Exec(t.Context(), `DROP TRIGGER auth10_revoke_failure ON public.oauth_grant; DROP FUNCTION public.auth10_revoke_failure()`)
		if dropErr != nil {
			t.Fatal(dropErr)
		}
		if !errors.Is(err, ErrUnavailable) || err.Error() != "unavailable" {
			t.Fatal("storage failure classification")
		}
		if _, err := first.VerifyBearer(t.Context(), tokens.AccessToken); err != nil {
			t.Fatal("failed revoke committed")
		}
	})
	t.Run("reuse cause survives explicit revoke and rotation", func(t *testing.T) {
		tokens, grant := issue(t, owner)
		rotated, err := second.Refresh(t.Context(), tokens.RefreshToken, "first", config.OAuth.Resource, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := first.Refresh(t.Context(), tokens.RefreshToken, "first", config.OAuth.Resource, ""); err == nil {
			t.Fatal("reuse accepted")
		}
		at, reason := state(t, grant)
		if reason != "refresh_token_reuse" {
			t.Fatal("reuse cause missing")
		}
		if err := first.RotateSigningKey(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := second.RevokeConnection(t.Context(), owner, "revoke_grant", grant); err != nil {
			t.Fatal(err)
		}
		after, reason2 := state(t, grant)
		if !at.Equal(after) || reason != reason2 {
			t.Fatal("original revoke cause changed")
		}
		denied(t, tokens)
		denied(t, rotated)
	})

}
