package authz

import (
	"errors"
	"mime"
	"net/http"
)

const tokenFormLimit = 64 << 10

// Token은 공개 client의 최초 코드 교환과 갱신 회전을 처리한다.
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
		h.tokenValidationFailure(request, "token_content_type")
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	contentType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		h.tokenValidationFailure(request, "token_content_type")
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if request.URL.RawQuery != "" {
		h.tokenValidationFailure(request, "token_query")
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, tokenFormLimit)
	if err := request.ParseForm(); err != nil {
		h.tokenValidationFailure(request, "token_form")
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
			h.tokenValidationFailure(request, "token_duplicate")
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
	}
	if len(request.Header.Values("Authorization")) != 0 || len(request.PostForm["client_secret"]) != 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_client"})
		return
	}
	if request.PostForm.Get("grant_type") == "" {
		h.tokenValidationFailure(request, "token_missing_grant_type")
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	var required []string
	switch request.PostForm.Get("grant_type") {
	case "authorization_code":
		required = []string{"code", "client_id", "redirect_uri", "resource", "code_verifier"}
	case "refresh_token":
		required = []string{"refresh_token", "client_id"}
		if _, present := request.PostForm["resource"]; present && request.PostForm.Get("resource") == "" {
			h.tokenValidationFailure(request, "token_missing_resource")
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
	default:
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	for _, key := range required {
		if request.PostForm.Get(key) == "" {
			h.tokenValidationFailure(request, "token_missing_"+key)
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
	}
	var tokens OAuthTokens
	if request.PostForm.Get("grant_type") == "authorization_code" {
		tokens, err = h.service.ExchangeAuthorizationCode(request.Context(), request.PostForm.Get("code"), request.PostForm.Get("client_id"), request.PostForm.Get("redirect_uri"), request.PostForm.Get("resource"), request.PostForm.Get("code_verifier"))
	} else {
		resource := request.PostForm.Get("resource")
		if _, present := request.PostForm["resource"]; !present {
			resource = h.service.oauth.Resource
		}
		tokens, err = h.service.Refresh(request.Context(), request.PostForm.Get("refresh_token"), request.PostForm.Get("client_id"), resource, request.PostForm.Get("scope"))
	}
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

func (h *Handler) tokenValidationFailure(request *http.Request, reason string) {
	if h.service.diagnostics != nil {
		h.service.diagnostics.RecordDiagnostic(request.Context(), "WARN", "validation", "rejected", reason)
	}
}
