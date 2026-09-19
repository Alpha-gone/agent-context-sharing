package authz

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"agent_context_sharing/internal/model"
)

// SessionAccountFunc는 현재 요청의 웹 세션 계정을 돌려준다. 세션 쿠키의 이름과 수명은
// `web`이 소유하므로 인가 서버는 판정 결과만 주입받는다.
type SessionAccountFunc func(*http.Request) (model.ID, bool)

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
	}
	redirect, err := h.service.ValidateRedirectTarget(authorize)
	if err != nil {
		// 1~2단계 실패다. 화면으로만 알린다.
		writeAuthorizeError(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.service.ValidateAuthorizeRequest(authorize); err != nil {
		// 3~4단계 실패다. 검증된 redirect_uri로만 오류를 돌려보낸다.
		redirectError(writer, request, redirect, query.Get("state"), authorizeErrorCode(authorize))
		return
	}
	accountID, ok := h.session(request)
	if !ok {
		// 5단계다. 로그인 뒤 같은 인가 요청으로 돌아온다.
		next := url.URL{Path: h.loginPath, RawQuery: url.Values{"next": {request.URL.RequestURI()}}.Encode()}
		http.Redirect(writer, request, next.String(), http.StatusSeeOther)
		return
	}
	code, err := h.service.Authorize(request.Context(), accountID, authorize)
	if err != nil {
		redirectError(writer, request, redirect, query.Get("state"), "server_error")
		return
	}
	// 6단계다. 원문은 저장하지 않았으므로 이 응답이 코드를 전달하는 유일한 자리다.
	values := redirect.Query()
	values.Set("code", code)
	if state := query.Get("state"); state != "" {
		values.Set("state", state)
	}
	target := *redirect
	target.RawQuery = values.Encode()
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

// Token은 「인가 코드 흐름」의 7단계를 처리한다.
func (h *Handler) Token(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writeTokenError(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	if request.PostForm.Get("grant_type") != "authorization_code" {
		writeTokenError(writer, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	token, err := h.service.Exchange(request.Context(),
		request.PostForm.Get("code"),
		request.PostForm.Get("client_id"),
		request.PostForm.Get("redirect_uri"),
		request.PostForm.Get("code_verifier"))
	if err != nil {
		writeTokenError(writer, http.StatusBadRequest, "invalid_grant")
		return
	}
	// 갱신 토큰을 두지 않으므로 refresh_token은 응답에 넣지 않는다.
	writeJSON(writer, http.StatusOK, map[string]any{
		"access_token": token.Raw,
		"token_type":   "Bearer",
		"expires_in":   int(accessTokenLifetime.Seconds()),
		"scope":        h.service.config.Resource,
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

// authorizeErrorCode는 3단계와 4단계의 실패를 「인가 코드 흐름」이 정한 코드로 나눈다.
func authorizeErrorCode(request AuthorizeRequest) string {
	if request.CodeChallenge == "" || request.CodeChallengeMethod != "S256" {
		return "invalid_request"
	}
	return "invalid_target"
}

func redirectError(writer http.ResponseWriter, request *http.Request, redirect *url.URL, state, code string) {
	values := redirect.Query()
	values.Set("error", code)
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
