package config

import (
	"strings"
)

func loadConnections(cfg *Config, env Environment) error {
	resource, err := parseHTTPURL("MCP_RESOURCE_URL", env("MCP_RESOURCE_URL"))
	if err != nil {
		return err
	}
	if resource.Scheme != "https" || resource.EscapedPath() != "/mcp" || resource.User != nil || strings.ContainsAny(env("MCP_RESOURCE_URL"), "?#") {
		return invalid("MCP_RESOURCE_URL", "query·fragment·사용자 정보 없는 HTTPS /mcp URI여야 한다", nil)
	}
	cfg.MCPResourceURL = resource
	origins, err := parseList("MCP_ALLOWED_ORIGINS", env("MCP_ALLOWED_ORIGINS"))
	if err != nil {
		return err
	}
	for _, origin := range origins {
		if _, err := parseOriginURL("MCP_ALLOWED_ORIGINS", origin); err != nil {
			return err
		}
	}
	cfg.MCPAllowedOrigins = origins
	return nil
}
