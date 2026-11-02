package authz

import (
	"context"
	"slices"

	"agent_context_sharing/internal/store"
)

// testClients는 기존 토큰·권한 시험에 등록 조회 경계를 주입하는 시험 전용 저장소다.
type testClients map[string]OAuthClient

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
