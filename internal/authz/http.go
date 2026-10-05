package authz

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
)

// SessionAccountFunc는 검증된 웹 세션의 계정과 최초 인증 시각을 돌려준다.
// 세션 쿠키 검증은 web이 소유하며 인가 서버는 최초 인증부터의 사용 한도를 확인한다.
type SessionAccountFunc func(*http.Request) (model.ID, time.Time, bool)

// maxTokenBodyBytes는 비인증 토큰 교환 요청의 고정 본문 상한이다.
const maxTokenBodyBytes = 64 << 10

// Handler는 「HTTP 진입점」이 인가 서버에 배정한 `/authorize`, `/token`, `/jwks.json`을
// 처리한다. 발급 규칙 자체는 Service가 소유하고 여기에서는 HTTP 표현만 다룬다.
type Handler struct {
	service *Service
	session SessionAccountFunc
	// loginPath에는 「인가 코드 흐름」 5단계에서 아직 로그인하지 않은 사용자를 보낼
	// 화면 경로를 둔다. 로그인 뒤 돌아올 주소는 next 질의 인자로 넘긴다.
	loginPath string
}

// NewHandler는 인가 서버 경로가 쓸 세션 판정과 로그인 화면 경로를 확인해 처리기를 만든다.
func NewHandler(service *Service, session SessionAccountFunc, loginPath string) (*Handler, error) {
	if service == nil || session == nil || !strings.HasPrefix(loginPath, "/") {
		return nil, fmt.Errorf("인가 서버 HTTP 처리기 구성이 올바르지 않다")
	}
	return &Handler{service: service, session: session, loginPath: loginPath}, nil
}

// Authorize는 「인가 코드 흐름」의 1~6단계를 처리한다.
//
// 1단계와 2단계의 실패만 리다이렉트하지 않는다. 돌려보낼 곳을 아직 믿을 수 없으므로
// 검증되지 않은 redirect_uri로 오류를 실어 보내면 그 자체가 열린 리다이렉션이 된다.
func (h *Handler) Authorize(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	authorize := AuthorizeRequest{
		ClientID:            query.Get("client_id"),
		RedirectURI:         query.Get("redirect_uri"),
		CodeChallenge:       query.Get("code_challenge"),
		CodeChallengeMethod: query.Get("code_challenge_method"),
		Resource:            query.Get("resource"),
		ResponseType:        query.Get("response_type"),
		Scope:               query.Get("scope"),
	}
	redirect, err := h.service.ValidateRedirectTarget(authorize)
	if err != nil {
		// 1~2단계 실패다. 화면으로만 알린다.
		writeAuthorizeError(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.service.ValidateAuthorizeRequest(authorize); err != nil {
		// 3~6단계 실패다. 검증된 redirect_uri로만 오류를 돌려보낸다.
		h.redirectError(writer, request, redirect, query.Get("state"), authorizeErrorCode(h.service, authorize))
		return
	}
	accountID, authenticatedAt, ok := h.session(request)
	if !ok || !validAuthenticationTime(authenticatedAt, time.Now().UTC()) {
		// 7단계다. 로그인 뒤 같은 인가 요청으로 돌아온다.
		h.redirectLogin(writer, request)
		return
	}
	code, err := h.service.Authorize(request.Context(), accountID, authenticatedAt, authorize)
	if err != nil {
		if errors.Is(err, ErrInvalidCredential) {
			h.redirectLogin(writer, request)
			return
		}
		h.redirectError(writer, request, redirect, query.Get("state"), "server_error")
		return
	}
	// 8단계다. 원문은 저장하지 않았으므로 이 응답이 코드를 전달하는 유일한 자리다.
	values := redirect.Query()
	values.Set("code", code)
	values.Set("iss", h.service.config.Issuer)
	if state := query.Get("state"); state != "" {
		values.Set("state", state)
	}
	target := *redirect
	target.RawQuery = values.Encode()
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

func (h *Handler) redirectLogin(writer http.ResponseWriter, request *http.Request) {
	next := url.URL{Path: h.loginPath, RawQuery: url.Values{"next": {request.URL.RequestURI()}}.Encode()}
	http.Redirect(writer, request, next.String(), http.StatusSeeOther)
}

// Token은 「인가 코드 흐름」의 9~10단계를 처리한다.
func (h *Handler) Token(writer http.ResponseWriter, request *http.Request) {
	if request.ContentLength > maxTokenBodyBytes {
		writeTokenError(writer, http.StatusRequestEntityTooLarge, "invalid_request")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxTokenBodyBytes)
	defer request.Body.Close()
	if err := request.ParseForm(); err != nil {
		status := http.StatusBadRequest
		if _, oversized := errors.AsType[*http.MaxBytesError](err); oversized {
			status = http.StatusRequestEntityTooLarge
		}
		writeTokenError(writer, status, "invalid_request")
		return
	}
	if request.PostForm.Get("grant_type") != "authorization_code" {
		writeTokenError(writer, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	proof := request.Header.Values("DPoP")
	if len(proof) != 1 || proof[0] == "" {
		writeTokenError(writer, http.StatusBadRequest, "invalid_dpop_proof")
		return
	}
	token, err := h.service.Exchange(request.Context(),
		request.PostForm.Get("code"),
		request.PostForm.Get("client_id"),
		request.PostForm.Get("redirect_uri"),
		request.PostForm.Get("code_verifier"),
		request.PostForm.Get("resource"),
		proof[0])
	if err != nil {
		if errors.Is(err, ErrInvalidDPoPProof) {
			writeTokenError(writer, http.StatusBadRequest, "invalid_dpop_proof")
			return
		}
		if errors.Is(err, ErrUnavailable) {
			writeTokenError(writer, http.StatusInternalServerError, "server_error")
			return
		}
		writeTokenError(writer, http.StatusBadRequest, "invalid_grant")
		return
	}
	// 갱신 토큰을 두지 않으므로 refresh_token은 응답에 넣지 않는다.
	//
	// scope에는 리소스 서버 URL이 아니라 확정된 scope 값을 담는다. 두 값은 다른
	// 것이며, 「계정 매핑과 인가 범위」가 정한 것은 후자다.
	writeJSON(writer, http.StatusOK, map[string]any{
		"access_token": token.Raw,
		"token_type":   "DPoP",
		"expires_in":   int(accessTokenLifetime.Seconds()),
		"scope":        h.service.config.Scope,
	})
}

// JWKS는 「토큰 검증」이 요구한 공개 키 목록을 비공개 키 없이 제공한다.
func (h *Handler) JWKS(writer http.ResponseWriter, request *http.Request) {
	set, err := h.service.JWKS(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": "server_error"})
		return
	}
	writeJSON(writer, http.StatusOK, set)
}

// authorizeErrorCode는 3~6단계의 실패를 「인가 코드 흐름」이 정한 코드로 나눈다.
// 판정 순서는 그 표의 단계 순서와 같아야 첫 실패의 코드가 나온다.
func authorizeErrorCode(service *Service, request AuthorizeRequest) string {
	switch {
	case request.ResponseType != "code":
		return "unsupported_response_type"
	case request.Scope != "" && request.Scope != service.config.Scope:
		return "invalid_scope"
	case request.CodeChallenge == "" || request.CodeChallengeMethod != "S256":
		return "invalid_request"
	default:
		return "invalid_target"
	}
}

// redirectError는 오류 응답에도 iss를 싣는다. 클라이언트는 발신자를 확인한 뒤에야
// error를 신뢰할 수 있으므로, 1~2단계를 지나 리다이렉트하는 모든 응답에 함께 보낸다.
func (h *Handler) redirectError(writer http.ResponseWriter, request *http.Request, redirect *url.URL, state, code string) {
	values := redirect.Query()
	values.Set("error", code)
	values.Set("iss", h.service.config.Issuer)
	if state != "" {
		values.Set("state", state)
	}
	target := *redirect
	target.RawQuery = values.Encode()
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

func writeAuthorizeError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	fmt.Fprintf(writer, "인가 요청을 처리할 수 없습니다: %s\n", code)
}

func writeTokenError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, map[string]any{"error": code})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	if err := json.MarshalWrite(writer, value); err != nil {
		return
	}
}
