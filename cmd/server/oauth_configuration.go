package main

import (
	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
)

// oauthConfiguration은 고정 resource와 공유 동적 client registry를 주입한다.
func oauthConfiguration(cfg config.Config, clients authz.OAuthClientStore) *authz.OAuthConfig {
	return &authz.OAuthConfig{Resource: cfg.MCPResourceURL.String(), Clients: clients}
}
