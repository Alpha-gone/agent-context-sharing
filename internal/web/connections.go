package web

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// ConnectionManagement는 세션에서 확인한 계정만 받는 연결 관리 업무 경계다.
type ConnectionManagement interface {
	ListConnections(context.Context, model.ID, model.ID) (store.ConnectionPage, error)
	RevokeConnection(context.Context, model.ID, string, model.ID) error
}

func (server *Server) connections(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		writer.Header().Set("Allow", "GET, POST")
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	account, _, ok := server.session(writer, request)
	if !ok {
		return
	}
	if server.config.Connections == nil {
		server.connectionError(writer, http.StatusServiceUnavailable)
		return
	}
	if request.Method == http.MethodPost {
		server.revokeConnection(writer, request, account)
		return
	}
	if len(request.URL.RawQuery) > 128 {
		server.connectionError(writer, http.StatusBadRequest)
		return
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || !singleAuthorizationValues(query) || len(query) > 1 || (len(query) == 1 && len(query["before"]) != 1) {
		server.connectionError(writer, http.StatusBadRequest)
		return
	}
	var before model.ID
	if len(query) != 0 {
		before, err = model.ParseID(query.Get("before"))
		if err != nil {
			server.connectionError(writer, http.StatusBadRequest)
			return
		}
	}
	page, err := server.config.Connections.ListConnections(request.Context(), account, before)
	if request.Context().Err() != nil {
		return
	}
	if err != nil {
		server.connectionError(writer, http.StatusInternalServerError)
		return
	}
	server.render(writer, http.StatusOK, "connections", pageData{Title: "연결 관리", Connections: page})
}

func (server *Server) revokeConnection(writer http.ResponseWriter, request *http.Request, account model.ID) {
	if request.URL.RawQuery != "" {
		server.connectionError(writer, http.StatusBadRequest)
		return
	}
	if request.ContentLength > authFormLimit {
		server.connectionError(writer, http.StatusRequestEntityTooLarge)
		return
	}
	types := request.Header.Values("Content-Type")
	if len(types) != 1 {
		server.connectionError(writer, http.StatusBadRequest)
		return
	}
	media, _, err := mime.ParseMediaType(types[0])
	if err != nil || media != "application/x-www-form-urlencoded" {
		server.connectionError(writer, http.StatusBadRequest)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, authFormLimit)
	err = request.ParseForm()
	if request.Context().Err() != nil {
		return
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		server.connectionError(writer, http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil || !singleAuthorizationValues(request.PostForm) || len(request.PostForm) != 2 || len(request.PostForm["target_id"]) != 1 || len(request.PostForm["action"]) != 1 {
		server.connectionError(writer, http.StatusBadRequest)
		return
	}
	target, err := model.ParseID(request.PostForm.Get("target_id"))
	action := request.PostForm.Get("action")
	if err != nil || (action != "revoke_grant" && action != "revoke_agent") {
		server.connectionError(writer, http.StatusBadRequest)
		return
	}
	err = server.config.Connections.RevokeConnection(request.Context(), account, action, target)
	if request.Context().Err() != nil {
		return
	}
	if !errors.Is(err, ErrAuthenticationUnavailable) && errors.Is(err, model.ErrNotFound) {
		server.connectionError(writer, http.StatusNotFound)
		return
	}
	if err != nil {
		server.connectionError(writer, http.StatusInternalServerError)
		return
	}
	http.Redirect(writer, request, "/connections", http.StatusSeeOther)
}

func (server *Server) connectionError(writer http.ResponseWriter, status int) {
	message := "연결을 처리할 수 없습니다. 잠시 후 다시 시도해 주세요."
	switch status {
	case http.StatusNotFound:
		message = "대상 연결을 찾을 수 없습니다."
	case http.StatusBadRequest:
		message = "연결 요청의 입력을 확인해 주세요."
	case http.StatusRequestEntityTooLarge:
		message = "연결 요청의 입력이 너무 큽니다."
	}
	server.render(writer, status, "message", pageData{Title: "연결 관리", Error: message})
}

func connectionStatus(grant store.OAuthGrantSummary) string {
	if grant.RevokedAt != nil {
		return "폐기됨"
	}
	now := time.Now()
	if !now.Before(connectionExpiry(grant)) {
		return "갱신 만료"
	}
	return "활성"
}

func connectionExpiry(grant store.OAuthGrantSummary) time.Time {
	if grant.AbsoluteExpiresAt.Before(grant.IdleExpiresAt) {
		return grant.AbsoluteExpiresAt
	}
	return grant.IdleExpiresAt
}

const connectionTemplates = `
{{define "connections"}}{{template "head" .}}
<style>.connection-list { overflow-wrap: anywhere; } .connection-list button { min-height: 48px; } .connection-list dt { font-weight: bold; } .connection-list dd { margin-inline-start: 0; margin-block-end: 8px; }</style>
<nav><h1>연결 관리</h1><a href="/graphs">그래프 목록</a><a href="/logout">로그아웃</a></nav>
<div class="connection-list">
<section aria-labelledby="grant-heading"><h2 id="grant-heading">클라이언트 연결</h2>
<p id="grant-revoke-help">연결을 폐기하면 남은 접근 자격과 갱신이 차단됩니다. 웹 로그인은 유지됩니다. 갱신이 만료되어도 이미 발급한 접근 자격은 별도 수명을 가지므로 필요하면 연결을 폐기해 주세요.</p>
{{range .Connections.Grants}}<article class="panel"><h3>{{.ClientID}}</h3><dl>
<dt>연결 식별자</dt><dd><code>{{.ID}}</code></dd><dt>상태</dt><dd>{{connectionStatus .}}</dd>
<dt>최초 허용</dt><dd>{{formatTime .AuthorizedAt}}</dd><dt>최근 갱신</dt><dd>{{formatTime .LastRotatedAt}}</dd>
<dt>갱신 유효 만료</dt><dd>{{formatTime (connectionExpiry .)}}</dd><dt>절대 만료</dt><dd>{{formatTime .AbsoluteExpiresAt}}</dd><dt>비활동 만료</dt><dd>{{formatTime .IdleExpiresAt}}</dd>
{{if .RevokedAt}}<dt>폐기 시각</dt><dd>{{formatTime .RevokedAt}}</dd>{{end}}</dl>
{{if not .RevokedAt}}<form method="post" action="/connections" aria-describedby="grant-revoke-help"><input type="hidden" name="action" value="revoke_grant"><input type="hidden" name="target_id" value="{{.ID}}"><button type="submit">{{.ClientID}} 연결 폐기 · {{.ID}}</button></form>{{end}}</article>
{{else}}<p>표시할 연결이 없습니다.</p>{{end}}
<nav aria-label="연결 목록 페이지"><a href="/connections">첫 페이지</a>{{if .Connections.Next.IsV7}}<a href="/connections?before={{.Connections.Next}}">다음 페이지</a>{{end}}</nav></section>
<section aria-labelledby="agent-heading"><h2 id="agent-heading">활성 에이전트 등록</h2><p id="agent-revoke-help">등록을 폐기하면 해당 에이전트의 이후 쓰기를 차단합니다. 기존 기여와 감사 기록은 보존됩니다. 클라이언트 연결은 별도로 관리합니다.</p>
{{range .Connections.Agents}}<article class="panel"><h3>{{.ClientID}}</h3><p>등록 식별자: <code>{{.ID}}</code></p><p>등록 시각: {{formatTime .CreatedAt}}</p>
<form method="post" action="/connections" aria-describedby="agent-revoke-help"><input type="hidden" name="action" value="revoke_agent"><input type="hidden" name="target_id" value="{{.ID}}"><button type="submit">{{.ClientID}} 에이전트 등록 폐기 · {{.ID}}</button></form></article>
{{else}}<p>활성 에이전트 등록이 없습니다.</p>{{end}}</section></div>{{template "foot" .}}{{end}}`
