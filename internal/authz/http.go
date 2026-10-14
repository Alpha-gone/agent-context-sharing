package authz

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
)

// Handler는 웹 세션 검증에 사용하는 서명 공개 키를 제공한다.
type Handler struct{ service *Service }

// NewHandler는 서명 공개 키 처리기를 만든다.
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

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	if err := json.MarshalWrite(writer, value); err != nil {
		return
	}
}
