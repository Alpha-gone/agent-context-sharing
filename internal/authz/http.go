package authz

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
)

// Handler는 서명 공개 키와 OAuth 발견 메타데이터를 제공한다.
type Handler struct{ service *Service }

// NewHandler는 공개 키·OAuth 발견 처리기를 만든다.
func NewHandler(service *Service) (*Handler, error) {
	if service == nil {
		return nil, fmt.Errorf("서명 공개 키 HTTP 처리기 구성이 올바르지 않다")
	}
	return &Handler{service: service}, nil
}

// JWKS는 비공개 키를 제외한 공개 키 목록을 제공한다.
func (h *Handler) JWKS(writer http.ResponseWriter, request *http.Request) {
	set, err := h.service.JWKS(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": "server_error"})
		return
	}
	writeJSON(writer, http.StatusOK, set)
}

// ProtectedResourceMetadata는 고정된 공개 리소스의 발견 정보를 제공한다.
func (h *Handler) ProtectedResourceMetadata(writer http.ResponseWriter, request *http.Request) {
	if h.service.oauth == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"resource":                 h.service.oauth.Resource,
		"authorization_servers":    []string{h.service.config.Issuer},
		"scopes_supported":         []string{"agent-context"},
		"bearer_methods_supported": []string{"header"},
	})
}

// AuthorizationServerMetadata는 요청 Host와 무관한 issuer·공개 endpoint를 제공한다.
func (h *Handler) AuthorizationServerMetadata(writer http.ResponseWriter, request *http.Request) {
	if h.service.oauth == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "server_error"})
		return
	}
	issuer := h.service.config.Issuer
	writeJSON(writer, http.StatusOK, map[string]any{
		"issuer":                                         issuer,
		"authorization_endpoint":                         issuer + "/authorize",
		"token_endpoint":                                 issuer + "/token",
		"jwks_uri":                                       issuer + "/jwks.json",
		"scopes_supported":                               []string{"agent-context"},
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	if err := json.MarshalWrite(writer, value); err != nil {
		return
	}
}
