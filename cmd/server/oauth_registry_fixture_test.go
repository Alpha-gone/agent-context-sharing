package main

import (
	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/store"
)

// testClients는 기존 토큰·권한 시험에 등록 조회 경계를 주입하는 시험 전용 저장소다.
type testClients map[string]authz.OAuthClient

func (c testClients) OAuthClient(ctx context.Context, id string) (store.OAuthClient, error) {
	if err := ctx.Err(); err != nil {
		return store.OAuthClient{}, err
	}
	client, ok := c[id]
	if !ok {
		return store.OAuthClient{}, store.ErrNotFound
	}
	client.RedirectURIs = slices.Clone(client.RedirectURIs)
	client.Resources = slices.Clone(client.Resources)
	return client, nil
}
func (c testClients) RegisterOAuthClient(ctx context.Context, client store.OAuthClient) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c[client.ID] = client
	return nil
}

func oauthTestConfiguration(t *testing.T, cfg config.Config, database *store.Store) *authz.OAuthConfig {
	t.Helper()
	resource := cfg.MCPResourceURL.String()
	clients := testClients{
		"codex-cli-first-test": {RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{resource}, LoopbackPortPolicy: "exact", CreatedAt: time.Now()},
		"native":               {RedirectURIs: []string{"http://127.0.0.1:4000/callback"}, Resources: []string{resource}, LoopbackPortPolicy: "same_ip", CreatedAt: time.Now()},
	}
	for id, client := range clients {
		if _, err := database.OAuthClient(t.Context(), id); errors.Is(err, store.ErrNotFound) {
			client.ID = id
			if err := database.RegisterOAuthClient(t.Context(), client); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	return &authz.OAuthConfig{Resource: resource, Clients: database}
}
func testClientCallback(t *testing.T, clients authz.OAuthClientStore, id string) string {
	t.Helper()
	client, err := clients.OAuthClient(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return client.RedirectURIs[0]
}
