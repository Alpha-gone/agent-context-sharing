package authz

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type connectionStoreStub struct {
	list   func(context.Context, model.ID, model.ID) (store.ConnectionPage, error)
	revoke func(context.Context, model.ID, model.ID) error
}

func (s connectionStoreStub) ListConnections(ctx context.Context, account, before model.ID) (store.ConnectionPage, error) {
	return s.list(ctx, account, before)
}
func (s connectionStoreStub) RevokeOAuthGrant(ctx context.Context, account, target model.ID) error {
	return s.revoke(ctx, account, target)
}
func (s connectionStoreStub) RevokeAgent(ctx context.Context, account, target model.ID) error {
	return s.revoke(ctx, account, target)
}

func TestConnectionManagementErrorsAndCancellation(t *testing.T) {
	binding := refreshBinding(t)
	for _, action := range []string{"list", "revoke_grant", "revoke_agent"} {
		for _, mode := range []string{"success", "missing store", "invalid account", "invalid target", "missing target", "field target", "storage", "cancel before", "cancel after"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				account, target := binding.AccountID, binding.ID
				calls := 0
				failure := error(nil)
				switch mode {
				case "invalid account":
					account = model.ID{}
				case "invalid target":
					target = model.ID{}
				case "missing target":
					failure = model.ErrNotFound
				case "field target":
					failure = model.FieldError{Field: "agent_id", Message: "synthetic-connection-secret"}
				case "storage":
					failure = errors.New("synthetic-connection-secret")
				case "cancel before":
					cancel()
				}
				call := func(_ context.Context, a, b model.ID) error {
					calls++
					if a != account || b != target {
						t.Fatal("binding")
					}
					if mode == "cancel after" {
						cancel()
					}
					return failure
				}
				config := oauthTestConfig()
				config.Connections = connectionStoreStub{revoke: call, list: func(ctx context.Context, a, b model.ID) (store.ConnectionPage, error) {
					err := call(ctx, a, b)
					return store.ConnectionPage{Next: binding.ID}, err
				}}
				if mode == "missing store" {
					config.Connections = nil
				}
				service, err := New(newMemoryStore(), config)
				if err != nil {
					t.Fatal(err)
				}
				event := &droppedDiagnostic{}
				service.SetDiagnostics(event)
				if action == "list" {
					var page store.ConnectionPage
					page, err = service.ListConnections(ctx, account, target)
					if err != nil && page.Next.IsV7() {
						t.Fatal("partial result on error")
					}
				} else {
					err = service.RevokeConnection(ctx, account, action, target)
				}
				if mode == "success" || (mode == "invalid target" && action == "list") {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("failure accepted")
				}
				if err != nil && (strings.Contains(err.Error(), "synthetic-connection-secret") || strings.Contains(event.reason, "synthetic")) {
					t.Fatal("private cause leaked")
				}
				if strings.HasPrefix(mode, "cancel") && !errors.Is(err, context.Canceled) {
					t.Fatal("cancel lost")
				}
				if mode == "storage" && (!errors.Is(err, ErrUnavailable) || !errors.Is(err, failure)) {
					t.Fatal("cause lost")
				}
				if (mode == "missing target" || mode == "field target") && action != "list" && !errors.Is(err, model.ErrNotFound) {
					t.Fatal("target classification")
				}
				if (mode == "missing store" || mode == "invalid account" || mode == "cancel before" || (mode == "invalid target" && action != "list")) && calls != 0 {
					t.Fatal("store called before validation")
				}
				if mode == "success" && action != "list" && event.reason != "completed" {
					t.Fatal("diagnostic drop affected success")
				}
			})
		}
	}
}
