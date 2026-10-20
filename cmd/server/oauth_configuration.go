package main

import (
	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/config"
)

// oauthConfiguration은 검증된 배포 등록을 업무 계층의 주입 타입으로 옮긴다.
// URI 형식 검증을 복제하지 않으며 서비스 생성자가 이후 변경과 분리해 보관한다.
func oauthConfiguration(cfg config.Config) *authz.OAuthConfig {
	clients := make(map[string]authz.OAuthClient, len(cfg.OAuthClients))
	for id, client := range cfg.OAuthClients {
		clients[id] = authz.OAuthClient{RedirectURIs: client.RedirectURIs, Resources: client.Resources, LoopbackPortPolicy: string(client.LoopbackPortPolicy)}
	}
	return &authz.OAuthConfig{Resource: cfg.MCPResourceURL.String(), Clients: clients}
}
