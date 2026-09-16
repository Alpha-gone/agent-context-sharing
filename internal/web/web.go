// Package web은 웹 세션으로 보호하는 관리 화면을 렌더링한다.
package web

import (
	"context"
	"embed"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

const (
	// SessionAudience는 웹 세션만 받아들이는 JWT audience다.
	SessionAudience = "web"
	sessionCookie   = "agent_context_session"
)

// webAssets에는 CDN에 의존하지 않는 시각화 자산을 보관한다.
//
//go:embed assets/cytoscape.min.js
var webAssets embed.FS

// Session은 현재 요청 안에서만 쓰는 검증된 웹 세션의 최소 정보다.
type Session struct {
	Raw       string
	ID        string
	ExpiresAt time.Time
}

// Authentication은 로그인, 세션 발급·검증과 로그아웃에 필요한 인가 서버 경계다.
type Authentication interface {
	Authenticate(context.Context, string, string) (model.ID, error)
	Register(context.Context, string, string) (model.ID, error)
	WebSession(context.Context, model.ID, string) (Session, error)
	VerifyWebSession(context.Context, string, string) (model.ID, Session, error)
	Revoke(context.Context, Session) error
}

// GraphStore는 웹 화면이 쓰는 조회·관리 규칙이다. 모든 호출 전에 웹이 세션과 등급을 확인한다.
type GraphStore interface {
	perm.GradeStore
	ListGraphs(context.Context, model.ID, model.GraphListFilter, string, int) ([]model.GraphListItem, string, error)
	Graph(context.Context, model.ID) (model.Graph, error)
	GraphVisualization(context.Context, model.ID, int, int) (store.HopResult, error)
	ListActiveContexts(context.Context, model.ID, int) ([]model.Context, error)
	ListDeletedContexts(context.Context, model.ID, int) ([]model.Context, error)
	ListGraphGrants(context.Context, model.ID) ([]model.GrantSubject, error)
	ListManagedTeams(context.Context, model.ID) ([]model.Team, error)
	AccountByLoginID(context.Context, string) (store.Account, error)
	GrantGraph(context.Context, model.ID, model.ID, model.ID, store.GrantSubjectType, model.GraphGrade) error
	RevokeGraphGrantWithAudit(context.Context, model.ID, model.ID, model.ID, store.GrantSubjectType) error
	CreateTeam(context.Context, model.ID, string) (model.Team, error)
	AddTeamMember(context.Context, model.ID, model.ID, model.ID) error
	RemoveTeamMemberWithAudit(context.Context, model.ID, model.ID, model.ID) error
	SetTeamDeleted(context.Context, model.ID, model.ID, bool) error
	DeletionImpact(context.Context, model.ID, model.ID) (model.DeletionImpact, error)
	SetGraphDeleted(context.Context, model.ID, model.ID, bool) error
	SetContextDeleted(context.Context, model.ID, model.ID, model.ID, bool) (model.Context, error)
	ListOwnedDeletedGraphs(context.Context, model.ID) ([]model.Graph, error)
	ListRestoreEligibleGraphs(context.Context, model.ID) ([]model.Graph, error)
	RequestGraphRestore(context.Context, model.ID, model.ID) error
	PendingRestoreRequests(context.Context) ([]model.RestoreRequest, error)
	OperatorRestoreGraph(context.Context, model.ID, model.ID) error
	ListAuditEntries(context.Context, model.ID, int) ([]model.AuditEntry, error)
}

// Config는 웹 화면의 고정된 HTTP 경계를 모은다.
type Config struct {
	// SecureCookie는 현재 요청이 애플리케이션의 TLS 판정을 통과했는지 알려 준다.
	SecureCookie func(*http.Request) bool
	// Plans는 계정별 그래프 목록 페이지 크기를 제공한다.
	Plans plan.AccountPlans
}

// Server는 로그인·등록·로그아웃과 웹 관리 화면 여섯 종을 처리한다.
type Server struct {
	auth      Authentication
	graphs    GraphStore
	config    Config
	templates *template.Template
}

// New는 웹 화면에 필요한 인가·조회 경계를 확인하고 템플릿을 준비한다.
func New(auth Authentication, graphs GraphStore, config Config) (*Server, error) {
	if auth == nil || graphs == nil || config.SecureCookie == nil {
		return nil, fmt.Errorf("웹 서버 구성이 올바르지 않다")
	}
	templates, err := template.New("pages").Funcs(template.FuncMap{
		"formatTime": func(value time.Time) string { return value.UTC().Format(time.RFC3339) },
	}).Parse(pageTemplates)
	if err != nil {
		return nil, fmt.Errorf("웹 템플릿 해석: %w", err)
	}
	return &Server{auth: auth, graphs: graphs, config: config, templates: templates}, nil
}

// ServeHTTP는 계약에 있는 로그인·등록·로그아웃과 웹 관리 경로를 처리한다.
func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/assets/cytoscape.min.js":
		http.ServeFileFS(writer, request, webAssets, "assets/cytoscape.min.js")
	case "/login":
		server.login(writer, request)
	case "/register":
		server.register(writer, request)
	case "/logout":
		server.logout(writer, request)
	case "/graphs":
		server.graphList(writer, request)
	case "/access":
		// 그래프 없이 팀만 관리하는 계정의 진입점이다. 같은 화면을 그리며 등급 영역만 빠진다.
		server.graphAccess(writer, request, model.ID{})
	case "/operator/restores":
		server.operatorRestores(writer, request)
	default:
		server.graphRoute(writer, request)
	}
}

func (server *Server) graphRoute(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "graphs" {
		http.NotFound(writer, request)
		return
	}
	graphID, err := model.ParseID(parts[1])
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	if len(parts) == 2 {
		server.graphDetail(writer, request, graphID)
		return
	}
	if len(parts) != 3 {
		http.NotFound(writer, request)
		return
	}
	switch parts[2] {
	case "access":
		server.graphAccess(writer, request, graphID)
	case "deletion":
		server.graphDeletion(writer, request, graphID)
	case "audit":
		server.graphAudit(writer, request, graphID)
	default:
		http.NotFound(writer, request)
	}
}

func (server *Server) login(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		message := ""
		if request.URL.Query().Get("registered") == "1" {
			message = "계정이 등록되었습니다. 로그인해 주세요."
		}
		server.render(writer, http.StatusOK, "login", pageData{Title: "로그인", Message: message})
	case http.MethodPost:
		if err := request.ParseForm(); err != nil {
			server.render(writer, http.StatusBadRequest, "login", pageData{Title: "로그인", Error: "입력을 처리할 수 없습니다."})
			return
		}
		loginID := request.PostForm.Get("login_id")
		accountID, err := server.auth.Authenticate(request.Context(), loginID, request.PostForm.Get("password"))
		if err != nil {
			server.render(writer, http.StatusUnauthorized, "login", pageData{Title: "로그인", LoginID: loginID, Error: "로그인 아이디 또는 비밀번호가 올바르지 않습니다."})
			return
		}
		session, err := server.auth.WebSession(request.Context(), accountID, SessionAudience)
		if err != nil {
			server.render(writer, http.StatusInternalServerError, "login", pageData{Title: "로그인", LoginID: loginID, Error: "세션을 만들 수 없습니다."})
			return
		}
		server.setSessionCookie(writer, request, session)
		http.Redirect(writer, request, "/graphs", http.StatusSeeOther)
	default:
		writer.Header().Set("Allow", "GET, POST")
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (server *Server) register(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		server.render(writer, http.StatusOK, "register", pageData{Title: "계정 등록"})
	case http.MethodPost:
		if err := request.ParseForm(); err != nil {
			server.render(writer, http.StatusBadRequest, "register", pageData{Title: "계정 등록", Error: "입력을 처리할 수 없습니다."})
			return
		}
		loginID := request.PostForm.Get("login_id")
		if _, err := server.auth.Register(request.Context(), loginID, request.PostForm.Get("password")); err != nil {
			server.render(writer, http.StatusBadRequest, "register", pageData{Title: "계정 등록", LoginID: loginID, Error: "로그인 아이디 또는 비밀번호 형식이 올바르지 않거나 이미 사용 중입니다."})
			return
		}
		http.Redirect(writer, request, "/login?registered=1", http.StatusSeeOther)
	default:
		writer.Header().Set("Allow", "GET, POST")
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (server *Server) logout(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		server.render(writer, http.StatusOK, "logout", pageData{Title: "로그아웃"})
	case http.MethodPost:
		if _, session, ok := server.session(request); ok {
			if err := server.auth.Revoke(request.Context(), session); err != nil {
				server.render(writer, http.StatusInternalServerError, "logout", pageData{Title: "로그아웃", Error: "로그아웃을 완료할 수 없습니다."})
				return
			}
		}
		server.clearSessionCookie(writer, request)
		http.Redirect(writer, request, "/login", http.StatusSeeOther)
	default:
		writer.Header().Set("Allow", "GET, POST")
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (server *Server) graphList(writer http.ResponseWriter, request *http.Request) {
	accountID, _, ok := server.session(request)
	if !ok {
		http.Redirect(writer, request, "/login", http.StatusSeeOther)
		return
	}
	if request.Method == http.MethodPost {
		if err := request.ParseForm(); err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "복구 요청", Error: "입력을 처리할 수 없습니다."})
			return
		}
		graphID, err := model.ParseID(request.PostForm.Get("restore_graph_id"))
		if err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "복구 요청", Error: "그래프 식별자가 올바르지 않습니다."})
			return
		}
		if err := server.graphs.RequestGraphRestore(request.Context(), graphID, accountID); err != nil {
			server.render(writer, http.StatusForbidden, "message", pageData{Title: "복구 요청", Error: "복구 요청 권한이 없거나 자동 삭제된 그래프가 아닙니다."})
			return
		}
		http.Redirect(writer, request, "/graphs", http.StatusSeeOther)
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, "GET, POST")
		return
	}
	filter, err := graphFilter(request)
	if err != nil {
		server.render(writer, http.StatusBadRequest, "graphs", pageData{Title: "그래프", Error: "등급 필터 값이 올바르지 않습니다."})
		return
	}
	limits := server.config.Plans.For(accountID)
	graphs, cursor, err := server.graphs.ListGraphs(request.Context(), accountID, filter, request.URL.Query().Get("cursor"), limits.GraphPage.Default)
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "graphs", pageData{Title: "그래프", Error: "그래프 목록을 읽을 수 없습니다."})
		return
	}
	restoreGraphs, err := server.graphs.ListRestoreEligibleGraphs(request.Context(), accountID)
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "graphs", pageData{Title: "그래프", Error: "자동 삭제된 그래프 목록을 읽을 수 없습니다."})
		return
	}
	// 직접 삭제한 그래프는 목록 조회에서 빠지므로 복구 화면으로 가는 링크를 여기에 둔다.
	// 「운영자 복구」가 이 경로의 복구를 소유자에게 맡겼고 다른 화면에는 진입점이 없다.
	deletedGraphs, err := server.graphs.ListOwnedDeletedGraphs(request.Context(), accountID)
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "graphs", pageData{Title: "그래프", Error: "삭제한 그래프 목록을 읽을 수 없습니다."})
		return
	}
	server.render(writer, http.StatusOK, "graphs", pageData{Title: "그래프", Graphs: graphs, NameFilter: filter.Name, Grades: filter.Grades, NextCursor: cursor, RestoreGraphs: restoreGraphs, DeletedGraphs: deletedGraphs})
}

func (server *Server) graphDetail(writer http.ResponseWriter, request *http.Request, graphID model.ID) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	accountID, ok := server.requireGrade(writer, request, graphID, model.GraphGradeViewer)
	if !ok {
		return
	}
	graph, err := server.graphs.Graph(request.Context(), graphID)
	if err != nil || graph.DeletedAt != nil {
		server.render(writer, http.StatusNotFound, "message", pageData{Title: "그래프", Error: "그래프를 찾을 수 없습니다."})
		return
	}
	limits := server.config.Plans.For(accountID)
	hops, err := server.graphs.GraphVisualization(request.Context(), graphID, limits.MaxHops, limits.MaxHopNodes)
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "그래프", Error: "시각화 데이터를 읽을 수 없습니다."})
		return
	}
	value := pageData{Title: graph.Name, AccountID: accountID, Graph: graph, Contexts: hops.Contexts, Edges: hops.Edges, MaxHops: limits.MaxHops}
	// 상한이 자른 경계는 오류가 아니라 「정상 응답의 부분 상태」의 표시다.
	if hops.Truncated {
		value.HopBoundary = &hops.Boundary
	}
	server.render(writer, http.StatusOK, "graph_detail", value)
}

// graphAccess는 등급 부여·회수와 팀 관리를 한 화면에 둔다.
//
// 화면 전체를 소유자로 좁히지 않는 이유는 FR-AGENT_CONTEXT-043이 팀 생성과 구성원
// 추가·제거를 인증된 계정 전부에 열어 두었기 때문이다. 등급 부여·회수는 FR-AGENT_CONTEXT-041
// 대로 소유자만 할 수 있고, 등급 표도 소유자에게만 보인다. 팀 동작의 관리자 판정은
// store가 트랜잭션 안에서 한다.
func (server *Server) graphAccess(writer http.ResponseWriter, request *http.Request, graphID model.ID) {
	accountID, _, ok := server.session(request)
	if !ok {
		http.Redirect(writer, request, "/login", http.StatusSeeOther)
		return
	}
	owner := false
	if graphID.IsV7() {
		var err error
		if owner, err = server.hasGrade(request.Context(), graphID, accountID, model.GraphGradeOwner); err != nil {
			server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "권한과 팀 관리", Error: "등급을 확인할 수 없습니다."})
			return
		}
	}
	if request.Method == http.MethodPost {
		server.changeGraphAccess(writer, request, graphID, accountID, owner)
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, "GET, POST")
		return
	}
	value := pageData{Title: "권한과 팀 관리", GraphID: graphID, Owner: owner}
	if owner {
		grants, err := server.graphs.ListGraphGrants(request.Context(), graphID)
		if err != nil {
			server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "권한과 팀 관리", Error: "등급 목록을 읽을 수 없습니다."})
			return
		}
		value.Grants = grants
	}
	teams, err := server.graphs.ListManagedTeams(request.Context(), accountID)
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "권한과 팀 관리", Error: "팀 목록을 읽을 수 없습니다."})
		return
	}
	value.Teams = teams
	server.render(writer, http.StatusOK, "access", value)
}

// hasGrade는 등급 충족 여부를 값으로 돌려준다. 등급이 없거나 모자란 것은 화면 구성의
// 분기이므로 오류로 올리지 않되, 등급 판정 자체는 perm이 계속 담당한다.
func (server *Server) hasGrade(ctx context.Context, graphID, accountID model.ID, required model.GraphGrade) (bool, error) {
	err := perm.Require(ctx, server.graphs, graphID, accountID, required)
	if err == nil {
		return true, nil
	}
	if _, found := errors.AsType[perm.NotFoundError](err); found {
		return false, nil
	}
	if _, found := errors.AsType[perm.DeniedError](err); found {
		return false, nil
	}
	return false, err
}

func (server *Server) changeGraphAccess(writer http.ResponseWriter, request *http.Request, graphID, actorID model.ID, owner bool) {
	if err := request.ParseForm(); err != nil {
		server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "입력을 처리할 수 없습니다."})
		return
	}
	ctx := request.Context()
	action := request.PostForm.Get("action")
	if (action == "grant_account" || action == "revoke") && !owner {
		server.render(writer, http.StatusForbidden, "message", pageData{Title: "접근 제한", Error: "등급을 바꿀 권한이 없습니다."})
		return
	}
	switch action {
	case "grant_account":
		account, err := server.graphs.AccountByLoginID(ctx, request.PostForm.Get("login_id"))
		if err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "대상 계정을 찾을 수 없습니다."})
			return
		}
		grade := model.GraphGrade(request.PostForm.Get("grade"))
		if !grade.Valid() {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "등급이 올바르지 않습니다."})
			return
		}
		if err := server.graphs.GrantGraph(ctx, graphID, actorID, account.ID, store.GrantSubjectAccount, grade); err != nil {
			server.render(writer, http.StatusConflict, "message", pageData{Title: "권한과 팀 관리", Error: "등급을 부여할 수 없습니다."})
			return
		}
	case "revoke":
		subjectID, err := model.ParseID(request.PostForm.Get("subject_id"))
		if err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "대상 식별자가 올바르지 않습니다."})
			return
		}
		subjectType := store.GrantSubjectType(request.PostForm.Get("subject_type"))
		if subjectType != store.GrantSubjectAccount && subjectType != store.GrantSubjectTeam {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "대상 종류가 올바르지 않습니다."})
			return
		}
		if err := server.graphs.RevokeGraphGrantWithAudit(ctx, graphID, actorID, subjectID, subjectType); err != nil {
			server.render(writer, http.StatusConflict, "message", pageData{Title: "권한과 팀 관리", Error: "마지막 소유자 등급은 회수할 수 없습니다."})
			return
		}
	case "create_team":
		if _, err := server.graphs.CreateTeam(ctx, actorID, strings.TrimSpace(request.PostForm.Get("team_name"))); err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "팀을 만들 수 없습니다."})
			return
		}
	case "add_member", "remove_member":
		teamID, err := model.ParseID(request.PostForm.Get("team_id"))
		if err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "팀 식별자가 올바르지 않습니다."})
			return
		}
		account, err := server.graphs.AccountByLoginID(ctx, request.PostForm.Get("login_id"))
		if err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "대상 계정을 찾을 수 없습니다."})
			return
		}
		if request.PostForm.Get("action") == "add_member" {
			err = server.graphs.AddTeamMember(ctx, teamID, actorID, account.ID)
		} else {
			err = server.graphs.RemoveTeamMemberWithAudit(ctx, teamID, actorID, account.ID)
		}
		if err != nil {
			server.render(writer, http.StatusConflict, "message", pageData{Title: "권한과 팀 관리", Error: "팀 구성원을 변경할 수 없습니다."})
			return
		}
	case "delete_team", "restore_team":
		teamID, err := model.ParseID(request.PostForm.Get("team_id"))
		if err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "팀 식별자가 올바르지 않습니다."})
			return
		}
		if err := server.graphs.SetTeamDeleted(ctx, teamID, actorID, request.PostForm.Get("action") == "delete_team"); err != nil {
			server.render(writer, http.StatusConflict, "message", pageData{Title: "권한과 팀 관리", Error: "팀 상태를 바꿀 수 없습니다."})
			return
		}
	default:
		server.render(writer, http.StatusBadRequest, "message", pageData{Title: "권한과 팀 관리", Error: "지원하지 않는 관리 동작입니다."})
		return
	}
	http.Redirect(writer, request, accessPath(graphID), http.StatusSeeOther)
}

// accessPath는 그래프가 있는 진입과 팀만 관리하는 진입의 되돌아갈 주소를 나눈다.
func accessPath(graphID model.ID) string {
	if !graphID.IsV7() {
		return "/access"
	}
	return "/graphs/" + graphID.String() + "/access"
}

// graphDeletion은 그래프와 노드의 소프트 삭제·복구를 한 화면에서 처리한다.
//
// 노드를 같은 화면에 두는 이유는 「운영자 복구」가 사용자 직접 삭제를 소유자의 웹 복구로
// 넘겼기 때문이다. 삭제된 컨텍스트가 보이는 자리는 「화면 접근 제어」가 정한 대로 이
// 복구 화면뿐이다. 그래프 삭제만 이름 입력을 받는 것은 FR-AGENT_CONTEXT-107이 확정했다.
func (server *Server) graphDeletion(writer http.ResponseWriter, request *http.Request, graphID model.ID) {
	accountID, ok := server.requireGrade(writer, request, graphID, model.GraphGradeOwner)
	if !ok {
		return
	}
	graph, err := server.graphs.Graph(request.Context(), graphID)
	if err != nil {
		server.render(writer, http.StatusNotFound, "message", pageData{Title: "삭제와 복구", Error: "그래프를 찾을 수 없습니다."})
		return
	}
	switch request.Method {
	case http.MethodGet:
		server.renderDeletion(writer, request, graphID, accountID, graph)
	case http.MethodPost:
		if err := request.ParseForm(); err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "삭제와 복구", Error: "입력을 처리할 수 없습니다."})
			return
		}
		switch action := request.PostForm.Get("action"); action {
		case "delete", "restore":
			if action == "delete" && request.PostForm.Get("graph_name") != graph.Name {
				server.render(writer, http.StatusBadRequest, "message", pageData{Title: "삭제와 복구", Error: "그래프 이름이 일치하지 않습니다."})
				return
			}
			if err := server.graphs.SetGraphDeleted(request.Context(), graphID, accountID, action == "delete"); err != nil {
				server.render(writer, http.StatusConflict, "message", pageData{Title: "삭제와 복구", Error: "현재 상태에서는 처리할 수 없습니다."})
				return
			}
			http.Redirect(writer, request, "/graphs", http.StatusSeeOther)
		case "delete_context", "restore_context":
			contextID, err := model.ParseID(request.PostForm.Get("context_id"))
			if err != nil {
				server.render(writer, http.StatusBadRequest, "message", pageData{Title: "삭제와 복구", Error: "컨텍스트 식별자가 올바르지 않습니다."})
				return
			}
			if _, err := server.graphs.SetContextDeleted(request.Context(), graphID, contextID, accountID, action == "delete_context"); err != nil {
				server.render(writer, http.StatusConflict, "message", pageData{Title: "삭제와 복구", Error: "현재 상태에서는 처리할 수 없습니다."})
				return
			}
			http.Redirect(writer, request, "/graphs/"+graphID.String()+"/deletion", http.StatusSeeOther)
		default:
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "삭제와 복구", Error: "지원하지 않는 삭제 동작입니다."})
		}
	default:
		methodNotAllowed(writer, "GET, POST")
	}
}

// renderDeletion은 그래프 영향과 노드 목록을 모아 복구 화면을 그린다. context_id를 받은
// 요청은 그 노드의 연쇄 영향을 함께 보여 삭제 전 안내를 노드에도 적용한다.
func (server *Server) renderDeletion(writer http.ResponseWriter, request *http.Request, graphID, accountID model.ID, graph model.Graph) {
	ctx := request.Context()
	impact, err := server.graphs.DeletionImpact(ctx, graphID, model.ID{})
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "삭제와 복구", Error: "삭제 영향을 읽을 수 없습니다."})
		return
	}
	pageSize := server.config.Plans.For(accountID).GraphPage.Default
	active, err := server.graphs.ListActiveContexts(ctx, graphID, pageSize)
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "삭제와 복구", Error: "컨텍스트 목록을 읽을 수 없습니다."})
		return
	}
	deleted, err := server.graphs.ListDeletedContexts(ctx, graphID, pageSize)
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "삭제와 복구", Error: "삭제된 컨텍스트 목록을 읽을 수 없습니다."})
		return
	}
	value := pageData{Title: "삭제와 복구", GraphID: graphID, Graph: graph, Impact: impact, ActiveContexts: active, DeletedContexts: deleted}
	if raw := request.URL.Query().Get("context_id"); raw != "" {
		contextID, err := model.ParseID(raw)
		if err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "삭제와 복구", Error: "컨텍스트 식별자가 올바르지 않습니다."})
			return
		}
		index := slices.IndexFunc(active, func(candidate model.Context) bool { return candidate.ID == contextID })
		if index == -1 {
			server.render(writer, http.StatusNotFound, "message", pageData{Title: "삭제와 복구", Error: "활성 컨텍스트를 찾을 수 없습니다."})
			return
		}
		selectedImpact, err := server.graphs.DeletionImpact(ctx, graphID, contextID)
		if err != nil {
			server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "삭제와 복구", Error: "삭제 영향을 읽을 수 없습니다."})
			return
		}
		value.Selected, value.SelectedImpact = &active[index], selectedImpact
	}
	server.render(writer, http.StatusOK, "deletion", value)
}

func (server *Server) graphAudit(writer http.ResponseWriter, request *http.Request, graphID model.ID) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	accountID, ok := server.requireGrade(writer, request, graphID, model.GraphGradeOwner)
	if !ok {
		return
	}
	entries, err := server.graphs.ListAuditEntries(request.Context(), graphID, server.config.Plans.For(accountID).GraphPage.Default)
	if err != nil {
		server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "감사 기록", Error: "감사 기록을 읽을 수 없습니다."})
		return
	}
	server.render(writer, http.StatusOK, "audit", pageData{Title: "감사 기록", GraphID: graphID, AuditEntries: entries})
}

func (server *Server) operatorRestores(writer http.ResponseWriter, request *http.Request) {
	accountID, _, ok := server.session(request)
	if !ok {
		http.Redirect(writer, request, "/login", http.StatusSeeOther)
		return
	}
	switch request.Method {
	case http.MethodGet:
		requests, err := server.graphs.PendingRestoreRequests(request.Context())
		if err != nil {
			server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "운영자 복구", Error: "대기 요청을 읽을 수 없습니다."})
			return
		}
		server.render(writer, http.StatusOK, "operator_restores", pageData{Title: "운영자 복구", RestoreRequests: requests})
	case http.MethodPost:
		if err := request.ParseForm(); err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "운영자 복구", Error: "입력을 처리할 수 없습니다."})
			return
		}
		graphID, err := model.ParseID(request.PostForm.Get("graph_id"))
		if err != nil {
			server.render(writer, http.StatusBadRequest, "message", pageData{Title: "운영자 복구", Error: "그래프 식별자가 올바르지 않습니다."})
			return
		}
		if err := server.graphs.OperatorRestoreGraph(request.Context(), graphID, accountID); err != nil {
			server.render(writer, http.StatusConflict, "message", pageData{Title: "운영자 복구", Error: "처리 가능한 복구 요청이 아닙니다."})
			return
		}
		http.Redirect(writer, request, "/operator/restores", http.StatusSeeOther)
	default:
		methodNotAllowed(writer, "GET, POST")
	}
}

func (server *Server) requireGrade(writer http.ResponseWriter, request *http.Request, graphID model.ID, required model.GraphGrade) (model.ID, bool) {
	accountID, _, ok := server.session(request)
	if !ok {
		http.Redirect(writer, request, "/login", http.StatusSeeOther)
		return model.ID{}, false
	}
	if err := perm.Require(request.Context(), server.graphs, graphID, accountID, required); err != nil {
		if _, found := errors.AsType[perm.NotFoundError](err); found {
			server.render(writer, http.StatusNotFound, "message", pageData{Title: "그래프", Error: "그래프를 찾을 수 없습니다."})
		} else {
			server.render(writer, http.StatusForbidden, "message", pageData{Title: "접근 제한", Error: "이 화면에 접근할 권한이 없습니다."})
		}
		return model.ID{}, false
	}
	return accountID, true
}

func methodNotAllowed(writer http.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	writer.WriteHeader(http.StatusMethodNotAllowed)
}

func graphFilter(request *http.Request) (model.GraphListFilter, error) {
	filter := model.GraphListFilter{Name: strings.TrimSpace(request.URL.Query().Get("name"))}
	for _, value := range request.URL.Query()["grade"] {
		grade := model.GraphGrade(value)
		if !grade.Valid() {
			return model.GraphListFilter{}, fmt.Errorf("등급 필터 %q가 올바르지 않다", value)
		}
		filter.Grades = append(filter.Grades, grade)
	}
	return filter, nil
}

func (server *Server) session(request *http.Request) (model.ID, Session, bool) {
	cookie, err := request.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return model.ID{}, Session{}, false
	}
	accountID, session, err := server.auth.VerifyWebSession(request.Context(), cookie.Value, SessionAudience)
	if err != nil {
		return model.ID{}, Session{}, false
	}
	return accountID, session, true
}

func (server *Server) setSessionCookie(writer http.ResponseWriter, request *http.Request, session Session) {
	http.SetCookie(writer, &http.Cookie{
		Name: sessionCookie, Value: session.Raw, Path: "/", Expires: session.ExpiresAt,
		HttpOnly: true, Secure: server.config.SecureCookie(request), SameSite: http.SameSiteLaxMode,
	})
}

func (server *Server) clearSessionCookie(writer http.ResponseWriter, request *http.Request) {
	http.SetCookie(writer, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: server.config.SecureCookie(request), SameSite: http.SameSiteLaxMode,
	})
}

func (server *Server) render(writer http.ResponseWriter, status int, name string, value pageData) {
	if name == "graph_detail" {
		// 본문은 사용자 입력일 수 있다. HTML 종료 태그로 스크립트 문맥을 벗어나지 않게
		// JSON의 HTML 위험 문자를 이스케이프한 뒤에만 template.JS로 전달한다.
		data, err := json.Marshal(newVisualizationData(value.Contexts, value.Edges), jsontext.EscapeForHTML(true))
		if err != nil {
			server.render(writer, http.StatusInternalServerError, "message", pageData{Title: value.Title, Error: "시각화 데이터를 만들 수 없습니다."})
			return
		}
		value.VisualizationJSON = template.JS(data)
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	if err := server.templates.ExecuteTemplate(writer, name, value); err != nil {
		return
	}
}

type pageData struct {
	Title             string
	Message           string
	Error             string
	LoginID           string
	AccountID         model.ID
	GraphID           model.ID
	Graph             model.Graph
	Graphs            []model.GraphListItem
	RestoreGraphs     []model.Graph
	DeletedGraphs     []model.Graph
	Contexts          []model.Context
	ActiveContexts    []model.Context
	DeletedContexts   []model.Context
	Selected          *model.Context
	SelectedImpact    model.DeletionImpact
	Edges             []store.HopEdge
	VisualizationJSON template.JS
	MaxHops           int
	// HopBoundary에는 결과 상한이 그래프 확장을 자른 홉 경계를 둔다.
	HopBoundary *int
	// Owner에는 요청 계정이 이 그래프의 소유자인지를 둔다.
	Owner           bool
	Grants          []model.GrantSubject
	Teams           []model.Team
	Impact          model.DeletionImpact
	AuditEntries    []model.AuditEntry
	RestoreRequests []model.RestoreRequest
	NameFilter      string
	Grades          []model.GraphGrade
	NextCursor      string
}

type visualizationData struct {
	Nodes []visualizationNode `json:"nodes"`
	Edges []visualizationEdge `json:"edges"`
}

type visualizationNode struct {
	Data    visualizationNodeData `json:"data"`
	Classes string                `json:"classes,omitempty"`
}

type visualizationNodeData struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Layer string `json:"layer"`
	Body  string `json:"body"`
}

type visualizationEdge struct {
	Data visualizationEdgeData `json:"data"`
}

type visualizationEdgeData struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

func newVisualizationData(contexts []model.Context, edges []store.HopEdge) visualizationData {
	result := visualizationData{Nodes: make([]visualizationNode, 0, len(contexts)), Edges: make([]visualizationEdge, 0, len(edges))}
	for _, context := range contexts {
		classes := string(context.Layer)
		if context.Derived != nil && context.Derived.EvidenceInvalidated {
			classes += " evidence-invalidated"
		}
		if context.Derived != nil && context.Derived.ConfidenceState == model.ConfidenceStateDisputed {
			classes += " disputed"
		}
		result.Nodes = append(result.Nodes, visualizationNode{Data: visualizationNodeData{ID: context.ID.String(), Label: string(context.Layer), Layer: string(context.Layer), Body: context.Body}, Classes: classes})
	}
	for index, edge := range edges {
		result.Edges = append(result.Edges, visualizationEdge{Data: visualizationEdgeData{ID: fmt.Sprintf("%d-%s", index, edge.Kind), Source: edge.FromID.String(), Target: edge.ToID.String(), Kind: edge.Kind}})
	}
	return result
}

const pageTemplates = `{{define "head"}}<!doctype html><html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}} · 에이전트 컨텍스트</title><style>
:root { font-family: system-ui, sans-serif; color: #1f2933; background: #f7f8fa; }
body { margin: 0; } main { max-width: 1120px; margin: 32px auto; padding: 0 16px; } h1 { margin: 0 0 16px; font-size: 24px; } section, form, table { margin: 16px 0; } .panel { padding: 16px; background: #fff; border: 1px solid #d9dee5; } label { display: block; margin: 8px 0; } input, select, button { box-sizing: border-box; padding: 8px; font: inherit; } input { width: 100%; } button { cursor: pointer; } .notice { padding: 8px; background: #e8f3ee; } .error { padding: 8px; background: #fdecec; } table { width: 100%; border-collapse: collapse; background: #fff; } th, td { padding: 8px; text-align: left; border: 1px solid #d9dee5; } nav { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; } .inline { display: inline; } .actions { display: flex; gap: 8px; align-items: end; flex-wrap: wrap; } .actions label { min-width: 140px; flex: 1; } .graph-layout { display: grid; grid-template-columns: 1fr 260px; gap: 16px; } #context-graph { min-height: 540px; border: 1px solid #d9dee5; } #node-details { white-space: pre-wrap; } @media (max-width: 720px) { .graph-layout { grid-template-columns: 1fr; } #context-graph { min-height: 420px; } }
</style></head><body><main>{{if .Message}}<p class="notice">{{.Message}}</p>{{end}}{{if .Error}}<p class="error">{{.Error}}</p>{{end}}{{end}}
{{define "foot"}}</main></body></html>{{end}}
{{define "login"}}{{template "head" .}}<h1>로그인</h1><form class="panel" method="post" action="/login"><label>로그인 아이디<input name="login_id" value="{{.LoginID}}" autocomplete="username" required></label><label>비밀번호<input type="password" name="password" autocomplete="current-password" required></label><button type="submit">로그인</button></form><p><a href="/register">계정 등록</a></p>{{template "foot" .}}{{end}}
{{define "register"}}{{template "head" .}}<h1>계정 등록</h1><form class="panel" method="post" action="/register"><label>로그인 아이디<input name="login_id" value="{{.LoginID}}" autocomplete="username" pattern="[a-z0-9_]{3,32}" required></label><p>영문 소문자, 숫자, 밑줄을 사용해 3~32자로 입력합니다.</p><label>비밀번호<input type="password" name="password" autocomplete="new-password" minlength="8" maxlength="128" required></label><p>비밀번호는 8~128자입니다.</p><button type="submit">등록</button></form><p><a href="/login">로그인으로 돌아가기</a></p>{{template "foot" .}}{{end}}
{{define "logout"}}{{template "head" .}}<h1>로그아웃</h1><form class="panel" method="post" action="/logout"><p>이 브라우저의 세션을 종료합니다.</p><button type="submit">로그아웃</button></form>{{template "foot" .}}{{end}}
{{define "message"}}{{template "head" .}}<h1>{{.Title}}</h1><p><a href="/graphs">그래프 목록</a></p>{{template "foot" .}}{{end}}
{{define "graphs"}}{{template "head" .}}<nav><h1>그래프</h1><a href="/access">팀 관리</a><a href="/operator/restores">운영자 복구</a><a href="/logout">로그아웃</a></nav><form class="panel actions" method="get" action="/graphs"><label>이름 필터<input name="name" value="{{.NameFilter}}"></label><label>등급<select name="grade"><option value="">모든 등급</option><option value="owner">소유자</option><option value="editor">편집자</option><option value="viewer">열람자</option></select></label><button type="submit">적용</button></form>{{if .Graphs}}<table><thead><tr><th>이름</th><th>설명</th><th>마지막 활동</th><th>내 등급</th></tr></thead><tbody>{{range .Graphs}}<tr><td><a href="/graphs/{{.ID}}">{{.Name}}</a></td><td>{{.Description}}</td><td>{{formatTime .LastActivityAt}}</td><td>{{.Grade}}</td></tr>{{end}}</tbody></table>{{else}}<p>접근 가능한 그래프가 없습니다.</p>{{end}}{{if .NextCursor}}<p><a href="/graphs?cursor={{.NextCursor}}">더 보기</a></p>{{end}}{{if .DeletedGraphs}}<section class="panel"><h2>내가 삭제한 그래프</h2>{{range .DeletedGraphs}}<p><a href="/graphs/{{.ID}}/deletion">{{.Name}} 복구</a></p>{{end}}</section>{{end}}{{if .RestoreGraphs}}<section class="panel"><h2>자동 삭제된 그래프</h2>{{range .RestoreGraphs}}<form class="inline" method="post" action="/graphs"><input type="hidden" name="restore_graph_id" value="{{.ID}}"><button type="submit">{{.Name}} 복구 요청</button></form>{{end}}</section>{{end}}{{template "foot" .}}{{end}}
{{define "graph_detail"}}{{template "head" .}}<nav><h1>{{.Graph.Name}}</h1><a href="/graphs">목록</a><a href="/graphs/{{.Graph.ID}}/access">권한과 팀 관리</a><a href="/graphs/{{.Graph.ID}}/deletion">삭제와 복구</a><a href="/graphs/{{.Graph.ID}}/audit">감사 기록</a></nav><section class="panel"><p>{{.Graph.Description}}</p><p>전체 보기와, 선택한 노드 중심의 국소 보기를 제공합니다.</p><label>국소 보기 홉 범위 <input id="hop-range" type="range" min="0" max="{{.MaxHops}}" value="{{.MaxHops}}"></label>{{if .HopBoundary}}<p class="notice">결과 상한으로 {{.HopBoundary}}홉 경계에서 잘렸습니다. 표시된 범위가 그래프 전체가 아닙니다.</p>{{end}}</section><section class="graph-layout"><div id="context-graph" aria-label="컨텍스트 그래프"></div><aside class="panel"><h2>선택한 노드</h2><p id="node-details">노드를 선택하면 본문과 근거 경로를 표시합니다.</p></aside></section><section class="panel"><h2>목록 보기</h2>{{range .Contexts}}<article><strong>{{.Layer}}</strong> <code>{{.ID}}</code><p>{{.Body}}</p>{{if and .Derived .Derived.EvidenceInvalidated}}<span>근거 무효</span>{{end}}{{if and .Derived (eq .Derived.ConfidenceState "disputed")}}<span>상충</span>{{end}}</article>{{else}}<p>표시할 활성 컨텍스트가 없습니다.</p>{{end}}</section><script src="/assets/cytoscape.min.js"></script><script>(function(){const elements={{.VisualizationJSON}};const details=document.getElementById('node-details');const range=document.getElementById('hop-range');const cy=cytoscape({container:document.getElementById('context-graph'),elements:elements,style:[{selector:'node',style:{'label':'data(label)','color':'#fff','text-valign':'center','text-halign':'center','width':42,'height':42,'font-size':10}},{selector:'node.source',style:{'background-color':'#2563eb'}},{selector:'node.derived',style:{'background-color':'#7c3aed'}},{selector:'node.event',style:{'background-color':'#047857'}},{selector:'node.disputed',style:{'border-width':4,'border-color':'#f59e0b'}},{selector:'node.evidence-invalidated',style:{'shape':'diamond'}},{selector:'edge',style:{'curve-style':'bezier','target-arrow-shape':'triangle','target-arrow-color':'#64748b','line-color':'#64748b','width':2,'label':'data(kind)','font-size':8,'text-rotation':'autorotate'}},{selector:'edge.evidence',style:{'line-color':'#dc2626','target-arrow-color':'#dc2626','width':5}}],layout:{name:'cose',animate:false}});let selected=null;function applyScope(){cy.elements().show();if(!selected)return;const hops=Number(range.value);if(hops===0){cy.elements().hide();selected.show();return;}let scope=selected;for(let i=0;i<hops;i++)scope=scope.closedNeighborhood();cy.elements().hide();scope.show();}function showDetails(node){selected=node;cy.edges().removeClass('evidence');/* DERIVED_FROM은 파생에서 근거로 향하므로 나가는 간선만 따라가면 근거 원천에 닿는다. 들어오는 간선까지 따르면 이 노드에 기대는 파생 후손까지 강조된다. */let frontier=node;const seen={};seen[node.id()]=true;while(frontier.length){const edges=frontier.outgoers('edge[kind = "derived_from"]');edges.addClass('evidence');frontier=edges.targets().filter(function(n){if(seen[n.id()])return false;seen[n.id()]=true;return true;});}details.textContent=node.data('layer')+'\n'+node.data('body');applyScope();}cy.on('tap','node',function(event){showDetails(event.target);});range.addEventListener('input',applyScope);})();</script>{{template "foot" .}}{{end}}
{{define "access"}}{{template "head" .}}<nav><h1>권한과 팀 관리</h1>{{if .GraphID.IsV7}}<a href="/graphs/{{.GraphID}}">그래프 상세</a>{{end}}<a href="/graphs">그래프 목록</a></nav>{{if .Owner}}<section class="panel"><h2>현재 등급</h2><table><thead><tr><th>대상</th><th>종류</th><th>등급</th><th>부여 방식</th><th>회수</th></tr></thead><tbody>{{range .Grants}}<tr><td>{{.Name}}</td><td>{{.Type}}</td><td>{{.Grade}}</td><td>{{if .Inherited}}팀 상속{{else}}직접{{end}}</td><td>{{if .Inherited}}상속 등급{{else if .CanRevoke}}<form class="inline" method="post"><input type="hidden" name="action" value="revoke"><input type="hidden" name="subject_id" value="{{.ID}}"><input type="hidden" name="subject_type" value="{{.Type}}"><button type="submit">회수</button></form>{{else}}<button disabled title="그래프에는 소유자가 최소 하나 필요합니다.">마지막 소유자</button>{{end}}</td></tr>{{end}}</tbody></table><form method="post" class="actions"><input type="hidden" name="action" value="grant_account"><label>계정 로그인 아이디<input name="login_id" required></label><label>등급<select name="grade"><option value="viewer">열람자</option><option value="editor">편집자</option><option value="owner">소유자</option></select></label><button type="submit">계정 등급 부여</button></form></section>{{else if .GraphID.IsV7}}<p class="notice">등급 부여와 회수는 이 그래프의 소유자만 할 수 있습니다.</p>{{end}}<section class="panel"><h2>관리 팀</h2><form method="post" class="actions"><input type="hidden" name="action" value="create_team"><label>새 팀 이름<input name="team_name" required></label><button type="submit">팀 생성</button></form>{{range .Teams}}<article><h3>{{.Name}} {{if .DeletedAt}}(삭제됨){{end}}</h3><form class="inline" method="post"><input type="hidden" name="team_id" value="{{.ID}}">{{if .DeletedAt}}<input type="hidden" name="action" value="restore_team"><button type="submit">팀 복구</button>{{else}}<input type="hidden" name="action" value="delete_team"><button type="submit">팀 삭제</button>{{end}}</form><form method="post" class="actions"><input type="hidden" name="team_id" value="{{.ID}}"><input type="hidden" name="action" value="add_member"><label>구성원 로그인 아이디<input name="login_id" required></label><button type="submit">추가</button></form><form method="post" class="actions"><input type="hidden" name="team_id" value="{{.ID}}"><input type="hidden" name="action" value="remove_member"><label>제거할 구성원 로그인 아이디<input name="login_id" required></label><button type="submit">제거</button></form></article>{{else}}<p>관리하는 팀이 없습니다.</p>{{end}}</section>{{template "foot" .}}{{end}}
{{define "deletion"}}{{template "head" .}}<nav><h1>삭제와 복구</h1><a href="/graphs/{{.GraphID}}">그래프 상세</a><a href="/graphs">그래프 목록</a></nav><section class="panel"><h2>{{.Graph.Name}}</h2><p>영향: 활성 컨텍스트 {{.Impact.Contexts}}개, 확정 관계 {{.Impact.Relations}}개, 접근이 차단될 계정 {{.Impact.Accounts}}개</p>{{if .Graph.DeletedAt}}<form method="post"><input type="hidden" name="action" value="restore"><button type="submit">그래프 복구</button></form>{{else}}<form method="post"><input type="hidden" name="action" value="delete"><label>그래프 이름 확인<input name="graph_name" required></label><button type="submit">소프트 삭제</button></form>{{end}}</section><section class="panel"><h2>노드 삭제</h2>{{if .Selected}}<article><strong>{{.Selected.Layer}}</strong> <code>{{.Selected.ID}}</code><p>{{.Selected.Body}}</p><p>영향: 이 컨텍스트를 근거로 둔 파생 {{.SelectedImpact.Derived}}개, 확정 관계 {{.SelectedImpact.Relations}}개</p><p>연쇄 삭제는 하지 않습니다. 파생에는 근거 무효 표시만 남습니다.</p><form method="post"><input type="hidden" name="action" value="delete_context"><input type="hidden" name="context_id" value="{{.Selected.ID}}"><button type="submit">이 노드 소프트 삭제</button></form></article>{{end}}{{range .ActiveContexts}}<p><a href="/graphs/{{$.GraphID}}/deletion?context_id={{.ID}}">{{.Layer}} · {{.ID}}</a></p>{{else}}<p>삭제할 활성 컨텍스트가 없습니다.</p>{{end}}</section><section class="panel"><h2>삭제된 노드</h2>{{range .DeletedContexts}}<article><strong>{{.Layer}}</strong> <code>{{.ID}}</code><p>{{.Body}}</p><form class="inline" method="post"><input type="hidden" name="action" value="restore_context"><input type="hidden" name="context_id" value="{{.ID}}"><button type="submit">복구</button></form></article>{{else}}<p>삭제된 컨텍스트가 없습니다.</p>{{end}}</section>{{template "foot" .}}{{end}}
{{define "audit"}}{{template "head" .}}<nav><h1>감사 기록</h1><a href="/graphs/{{.GraphID}}">그래프 상세</a></nav><table><thead><tr><th>시각</th><th>종류</th><th>동작</th><th>대상</th><th>상세</th></tr></thead><tbody>{{range .AuditEntries}}<tr><td>{{formatTime .OccurredAt}}</td><td>{{.Kind}}</td><td>{{.Action}}</td><td>{{.Target}}</td><td>{{.Detail}}</td></tr>{{end}}</tbody></table>{{template "foot" .}}{{end}}
{{define "operator_restores"}}{{template "head" .}}<nav><h1>운영자 복구</h1><a href="/graphs">그래프 목록</a></nav><p>대기 중인 요청만 표시하며 컨텍스트 본문은 조회하지 않습니다.</p><table><thead><tr><th>그래프</th><th>요청 계정</th><th>요청 시각</th><th>처리</th></tr></thead><tbody>{{range .RestoreRequests}}<tr><td>{{.GraphName}}</td><td>{{.RequestedBy}}</td><td>{{formatTime .RequestedAt}}</td><td><form class="inline" method="post"><input type="hidden" name="graph_id" value="{{.GraphID}}"><button type="submit">복구</button></form></td></tr>{{else}}<tr><td colspan="4">대기 중인 요청이 없습니다.</td></tr>{{end}}</tbody></table>{{template "foot" .}}{{end}}`
