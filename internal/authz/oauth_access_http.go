package authz

import (
	"agent_context_sharing/internal/model"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ProtectBearer는 전송 보안 판정 뒤 사용할 보호 리소스 인증 경계다.
// 다음 처리기는 검증된 결합만 받으며 MCP 조립·현재 등급 검사는 별도 책임이다.
func (h *Handler) ProtectBearer(next func(http.ResponseWriter, *http.Request, BearerPrincipal)) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Context().Err() != nil {
			return
		}
		if h.service.oauth == nil || h.service.config.AccessTokens == nil || next == nil {
			h.service.authObservation(request.Context(), "token_validation", model.ID{}, "", model.ID{}, ErrUnavailable)
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		values := request.Header.Values("Authorization")
		if len(values) == 0 {
			h.service.authObservation(request.Context(), "token_validation", model.ID{}, "", model.ID{}, ErrInvalidCredential)
			h.bearerFailure(writer, http.StatusUnauthorized, "")
			return
		}
		if len(values) != 1 {
			h.service.authObservation(request.Context(), "token_validation", model.ID{}, "", model.ID{}, ErrInvalidCredential)
			h.bearerFailure(writer, http.StatusUnauthorized, "invalid_token")
			return
		}
		scheme, raw, ok := strings.Cut(values[0], " ")
		// RFC 6750의 1*SP를 허용하고 탭·목록·후행 공백은 토큰으로 받지 않는다.
		raw = strings.TrimLeft(raw, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" || strings.ContainsAny(raw, " \t\r\n,") {
			h.service.authObservation(request.Context(), "token_validation", model.ID{}, "", model.ID{}, ErrInvalidCredential)
			h.bearerFailure(writer, http.StatusUnauthorized, "invalid_token")
			return
		}
		principal, err := h.service.VerifyBearer(request.Context(), raw)
		if request.Context().Err() != nil {
			return
		}
		switch {
		case err == nil:
			next(writer, request, principal)
		case errors.Is(err, ErrInsufficientScope):
			h.bearerFailure(writer, http.StatusForbidden, "insufficient_scope")
		case errors.Is(err, ErrInvalidCredential):
			h.bearerFailure(writer, http.StatusUnauthorized, "invalid_token")
		default:
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		}
	})
}

func (h *Handler) bearerFailure(writer http.ResponseWriter, status int, code string) {
	resource, _ := url.Parse(h.service.oauth.Resource) // 구성 계층이 검증한 고정 공개 URI다.
	metadata := resource.Scheme + "://" + resource.Host + "/.well-known/oauth-protected-resource/mcp"
	challenge := `Bearer resource_metadata="` + metadata + `", scope="agent-context"`
	if code != "" {
		challenge += `, error="` + code + `"`
	}
	writer.Header().Set("WWW-Authenticate", challenge)
	body := code
	if body == "" {
		body = "unauthorized"
	}
	writeJSON(writer, status, map[string]string{"error": body})
}
