package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"agent_context_sharing/internal/model"
)

// Authorization은 authz 타입을 웹에 노출하지 않는 인가 어댑터 계약이다.
type Authorization interface {
	ValidateAuthorizationTarget(string, string, string) error
	ValidateAuthorizationRequest(url.Values) string
	IssueAuthorizationCode(context.Context, model.ID, url.Values) (string, error)
	AuthorizationIssuer() string
}

func setAuthorizationHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Frame-Options", "DENY")
	// form-action 'self'는 동의 POST 뒤 등록 callback으로 이동하는 브라우저 흐름도 막을 수 있다.
	writer.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
}

func (server *Server) authorize(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		writer.Header().Set("Allow", "GET, POST")
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if server.config.Authorization == nil {
		server.authorizationError(writer, http.StatusServiceUnavailable)
		return
	}
	// POST는 출처 검사 뒤 공유 보호를 거친 다음에만 query·세션을 해석한다.
	if request.Method == http.MethodPost && !server.parseAuthenticationForm(writer, request, "message") {
		return
	}
	if len(request.URL.RawQuery) > authFormLimit {
		server.authorizationError(writer, http.StatusRequestEntityTooLarge)
		return
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || !singleAuthorizationValues(values) {
		server.authorizationError(writer, http.StatusBadRequest)
		return
	}
	authorization := server.config.Authorization
	if err := authorization.ValidateAuthorizationTarget(values.Get("client_id"), values.Get("redirect_uri"), values.Get("resource")); err != nil {
		server.authorizationError(writer, http.StatusBadRequest)
		return
	}
	if code := authorization.ValidateAuthorizationRequest(values); code != "" {
		server.authorizationRedirect(writer, request, values, "error", code)
		return
	}
	account, _, err := server.verifiedSession(request)
	if request.Context().Err() != nil {
		return
	}
	if errors.Is(err, ErrAuthenticationUnavailable) {
		server.sessionUnavailable(writer)
		return
	}
	if err != nil {
		if request.Method == http.MethodPost {
			server.authorizationError(writer, http.StatusUnauthorized)
			return
		}
		http.Redirect(writer, request, "/login?"+url.Values{"next": {request.URL.RequestURI()}}.Encode(), http.StatusSeeOther)
		return
	}
	if request.Method == http.MethodGet {
		server.render(writer, http.StatusOK, "authorize", pageData{Title: "연결 허용", AccountID: account, AuthorizationClient: values.Get("client_id"), AuthorizationResource: values.Get("resource"), AuthorizationAction: "/authorize?" + values.Encode()})
		return
	}
	if !singleAuthorizationValues(request.PostForm) || len(request.PostForm["decision"]) != 1 {
		server.authorizationRedirect(writer, request, values, "error", "invalid_request")
		return
	}
	switch request.PostForm.Get("decision") {
	case "deny":
		server.authorizationRedirect(writer, request, values, "error", "access_denied")
	case "allow":
		code, err := authorization.IssueAuthorizationCode(request.Context(), account, values)
		if request.Context().Err() != nil {
			return
		}
		if err != nil {
			server.authorizationError(writer, http.StatusInternalServerError)
			return
		}
		server.authorizationRedirect(writer, request, values, "code", code)
	default:
		server.authorizationRedirect(writer, request, values, "error", "invalid_request")
	}
}

func singleAuthorizationValues(values url.Values) bool {
	for _, items := range values {
		if len(items) != 1 {
			return false
		}
	}
	return true
}

func (server *Server) authorizationError(writer http.ResponseWriter, status int) {
	server.render(writer, status, "message", pageData{Title: "연결 허용", Error: "연결 요청을 처리할 수 없습니다. 클라이언트에서 다시 시작해 주세요."})
}

func (server *Server) authorizationRedirect(writer http.ResponseWriter, request *http.Request, values url.Values, key, value string) {
	callback, err := url.Parse(values.Get("redirect_uri"))
	if err != nil {
		server.authorizationError(writer, http.StatusBadRequest)
		return
	}
	query := callback.Query()
	// callback의 고정 query가 OAuth 응답으로 오인되지 않게 예약 이름은 교체한다.
	for _, name := range []string{"code", "error", "error_description", "error_uri", "state", "iss"} {
		query.Del(name)
	}
	query.Set(key, value)
	query.Set("iss", server.config.Authorization.AuthorizationIssuer())
	if state, exists := values["state"]; exists {
		query.Set("state", state[0])
	}
	callback.RawQuery = query.Encode()
	http.Redirect(writer, request, callback.String(), http.StatusSeeOther)
}

const authorizationTemplates = `
{{define "authorize"}}{{template "head" .}}
<h1>클라이언트 연결 허용</h1>
<section class="panel"><p>클라이언트 <strong>{{.AuthorizationClient}}</strong>가 현재 계정 <code>{{.AccountID}}</code>으로 연결하려고 합니다.</p>
<p id="authorization-help">대상: {{.AuthorizationResource}}. 접근 가능한 그래프를 조회하고 현재 등급이 허용하는 작업을 수행합니다. 연결을 허용해도 그래프 등급은 바뀌지 않습니다.</p>
<form method="post" action="{{.AuthorizationAction}}" aria-describedby="authorization-help">
<fieldset><legend>이 클라이언트의 연결을 허용하시겠습니까?</legend>
<button type="submit" name="decision" value="deny">연결 거절</button>
<button type="submit" name="decision" value="allow">연결 허용</button>
</fieldset></form></section>{{template "foot" .}}{{end}}`
