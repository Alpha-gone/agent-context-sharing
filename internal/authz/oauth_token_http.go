package authz

import (
	"errors"
	"mime"
	"net/http"
)

const tokenFormLimit = 64 << 10

// Token은 공개 client의 코드 교환을 처리한다. 갱신 grant는 AUTH-09에서 연결한다.
func (h *Handler) Token(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", "POST")
		writeJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request"})
		return
	}
	if request.Context().Err() != nil {
		return
	}
	if h.service.oauth == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "server_error"})
		return
	}
	if request.ContentLength > tokenFormLimit {
		writeJSON(writer, http.StatusRequestEntityTooLarge, map[string]string{"error": "invalid_request"})
		return
	}
	contentTypes := request.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	contentType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || contentType != "application/x-www-form-urlencoded" || request.URL.RawQuery != "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, tokenFormLimit)
	if err := request.ParseForm(); err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(writer, status, map[string]string{"error": "invalid_request"})
		return
	}
	if request.Context().Err() != nil {
		return
	}
	for _, items := range request.PostForm {
		if len(items) != 1 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
	}
	if len(request.Header.Values("Authorization")) != 0 || len(request.PostForm["client_secret"]) != 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_client"})
		return
	}
	if request.PostForm.Get("grant_type") == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if request.PostForm.Get("grant_type") != "authorization_code" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	for _, key := range []string{"code", "client_id", "redirect_uri", "resource", "code_verifier"} {
		if request.PostForm.Get(key) == "" {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
	}
	tokens, err := h.service.ExchangeAuthorizationCode(request.Context(), request.PostForm.Get("code"), request.PostForm.Get("client_id"), request.PostForm.Get("redirect_uri"), request.PostForm.Get("resource"), request.PostForm.Get("code_verifier"))
	if request.Context().Err() != nil {
		return
	}
	if failure, ok := errors.AsType[*OAuthError](err); ok {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": failure.Code})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(writer, http.StatusOK, tokens)
}
