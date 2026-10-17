package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/perm"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 실제 서명 키를 쓰므로 공유 DB를 변경하지 않고 시험별 DB를 정식 적용한다.
func permissionDatabase(t *testing.T) *store.Store {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("필수 DB 시험의 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 권한 통합 인수를 건너뛴다")
	}
	admin, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "permission_acceptance_" + strings.ReplaceAll(permissionID(t).String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Errorf("권한 인수 DB 정리: %v", err)
		}
	})
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	pool, err := migrate.NewPool(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	err = migrate.WithLock(t.Context(), pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		loaded, applied, err := migrate.Prepare(ctx, conn, migrations.FS, ".", migrate.Config{
			GraphName: "agent_context", VectorType: "vector", VectorDim: 128, IndexTargetLayers: "all_layers",
		})
		if err != nil {
			return err
		}
		pending, err := migrate.Pending(loaded, applied)
		if err != nil {
			return err
		}
		for _, migration := range pending {
			if err := migrate.Apply(ctx, conn, migration); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 업무 호출에는 DB 핸들을 전달하지 않는다.
	database, err := store.New(t.Context(), parsed.String(), "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	return database
}

type permissionAuthentication struct{ *authz.Service }

func (auth permissionAuthentication) WebSession(ctx context.Context, account model.ID, audience string) (web.Session, error) {
	token, err := auth.Service.WebSession(ctx, account, audience)
	return web.Session{Raw: token.Raw, ID: token.ID, ExpiresAt: token.ExpiresAt}, err
}

func (auth permissionAuthentication) VerifyWebSession(ctx context.Context, raw, audience string) (model.ID, web.Session, error) {
	account, token, err := auth.Verify(ctx, raw, audience)
	return account, web.Session{Raw: token.Raw, ID: token.ID, ExpiresAt: token.ExpiresAt}, err
}

func (auth permissionAuthentication) Revoke(ctx context.Context, session web.Session) error {
	return auth.Service.Revoke(ctx, authz.Token{Raw: session.Raw, ID: session.ID, ExpiresAt: session.ExpiresAt})
}

func permissionID(t *testing.T) model.ID {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func permissionAccount(t *testing.T, database *store.Store) model.ID {
	t.Helper()
	id := permissionID(t)
	if err := database.CreateAccount(t.Context(), store.Account{ID: id, LoginID: permissionLogin(id), PasswordHash: "synthetic-unused-hash", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return id
}

func permissionLogin(id model.ID) string {
	return strings.ReplaceAll(id.String(), "-", "")
}

func permissionGraph(t *testing.T, database *store.Store, owner model.ID) model.ID {
	t.Helper()
	id, now := permissionID(t), time.Now().UTC()
	if _, err := database.CreateGraphWithOwner(t.Context(), model.Graph{ID: id, Name: "permission acceptance", CreatedBy: owner, CreatedAt: now, LastActivityAt: now, Version: 1}, store.WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	return id
}

func permissionRequest(t *testing.T, server *web.Server, token, method, path string, form url.Values, want int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "https://permission.test"+path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://permission.test")
	request.AddCookie(&http.Cookie{Name: "agent_context_session", Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s = %d, want %d", method, path, response.Code, want)
	}
	return response
}

func TestPermissionAcceptanceIntegration(t *testing.T) {
	database := permissionDatabase(t)
	service, err := authz.New(database, authz.Config{Issuer: "https://permission.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.New(permissionAuthentication{service}, database, web.Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name               string
		direct, team, want model.GraphGrade
		empty, deleted     bool
	}{
		{name: "direct_owner", direct: model.GraphGradeOwner, want: model.GraphGradeOwner},
		{name: "direct_editor", direct: model.GraphGradeEditor, want: model.GraphGradeEditor},
		{name: "direct_viewer", direct: model.GraphGradeViewer, want: model.GraphGradeViewer},
		{name: "team_owner", team: model.GraphGradeOwner, want: model.GraphGradeOwner},
		{name: "team_editor", team: model.GraphGradeEditor, want: model.GraphGradeEditor},
		{name: "team_viewer", team: model.GraphGradeViewer, want: model.GraphGradeViewer},
		{name: "higher_team", direct: model.GraphGradeViewer, team: model.GraphGradeEditor, want: model.GraphGradeEditor},
		{name: "higher_direct", direct: model.GraphGradeOwner, team: model.GraphGradeViewer, want: model.GraphGradeOwner},
		{name: "empty_team", team: model.GraphGradeOwner, empty: true},
		{name: "deleted_team", team: model.GraphGradeOwner, deleted: true},
		{name: "no_grade"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			owner, actor, target := permissionAccount(t, database), permissionAccount(t, database), permissionAccount(t, database)
			graph := permissionGraph(t, database, owner)
			// 등급을 갖기 전에 발급한 같은 세션을 모든 전이에 사용한다.
			token, err := service.WebSession(t.Context(), actor, web.SessionAudience)
			if err != nil {
				t.Fatal(err)
			}
			grant := func(id model.ID, kind store.GrantSubjectType, grade model.GraphGrade) {
				t.Helper()
				if err := database.GrantGraph(t.Context(), graph, owner, id, kind, grade); err != nil {
					t.Fatal(err)
				}
			}
			if sample.direct != "" {
				grant(actor, store.GrantSubjectAccount, sample.direct)
			}
			var team model.Team
			if sample.team != "" {
				team, err = database.CreateTeam(t.Context(), owner, sample.name)
				if err != nil {
					t.Fatal(err)
				}
				if !sample.empty {
					if err := database.AddTeamMember(t.Context(), team.ID, owner, actor); err != nil {
						t.Fatal(err)
					}
				}
				grant(team.ID, store.GrantSubjectTeam, sample.team)
				if sample.deleted {
					if err := database.SetTeamDeleted(t.Context(), team.ID, owner, true); err != nil {
						t.Fatal(err)
					}
				}
			}
			// SRS 업무 행렬의 최소 등급을 실제 저장 등급과 대조한다. MCP 처리기 인수와 구분한다.
			for _, operation := range []struct {
				name     string
				required model.GraphGrade
			}{
				{"graph_get", model.GraphGradeViewer}, {"node_get", model.GraphGradeViewer},
				{"hop_get", model.GraphGradeViewer}, {"context_flow", model.GraphGradeViewer},
				{"relation_list", model.GraphGradeViewer}, {"graph_update", model.GraphGradeEditor},
				{"node_create", model.GraphGradeEditor}, {"node_update", model.GraphGradeEditor},
				{"node_discard_restore", model.GraphGradeEditor}, {"relation_confirm_discard", model.GraphGradeEditor},
				{"web_delete", model.GraphGradeOwner}, {"web_grant_revoke", model.GraphGradeOwner},
			} {
				t.Run(operation.name, func(t *testing.T) {
					err := perm.Require(t.Context(), database, graph, actor, operation.required)
					if sample.want == "" {
						if _, ok := errors.AsType[perm.NotFoundError](err); !ok {
							t.Fatalf("유효 등급 없음 = %v", err)
						}
					} else if permissionRank(sample.want) < permissionRank(operation.required) {
						if denied, ok := errors.AsType[perm.DeniedError](err); !ok || denied.Required != operation.required {
							t.Fatalf("등급 부족 = %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				})
			}
			path := "/graphs/" + graph.String()
			readStatus, ownerStatus, mutationStatus := http.StatusOK, http.StatusOK, http.StatusSeeOther
			if sample.want == "" {
				readStatus, ownerStatus = http.StatusNotFound, http.StatusNotFound
			} else if sample.want != model.GraphGradeOwner {
				ownerStatus = http.StatusForbidden
			}
			if sample.want != model.GraphGradeOwner {
				mutationStatus = http.StatusForbidden
			}
			permissionRequest(t, server, token.Raw, http.MethodGet, path, nil, readStatus)
			permissionRequest(t, server, token.Raw, http.MethodGet, path+"/deletion", nil, ownerStatus)
			permissionRequest(t, server, token.Raw, http.MethodGet, path+"/audit", nil, ownerStatus)
			permissionRequest(t, server, token.Raw, http.MethodGet, path+"/access", nil, http.StatusOK)
			permissionRequest(t, server, token.Raw, http.MethodPost, path+"/access", url.Values{"action": {"grant_account"}, "login_id": {permissionLogin(target)}, "grade": {"viewer"}}, mutationStatus)
			permissionRequest(t, server, token.Raw, http.MethodPost, path+"/access", url.Values{"action": {"revoke"}, "subject_type": {"account"}, "subject_id": {target.String()}}, mutationStatus)
			if _, found, err := database.EffectiveGrade(t.Context(), graph, target); err != nil || found {
				t.Fatalf("거부한 부여 또는 완료한 회수 뒤 대상 등급이 남았다: %v", err)
			}
			deletionStatus := ownerStatus
			if sample.want == model.GraphGradeOwner {
				deletionStatus = http.StatusSeeOther
			}
			permissionRequest(t, server, token.Raw, http.MethodPost, path+"/deletion", url.Values{"action": {"delete"}, "graph_name": {"permission acceptance"}}, deletionStatus)
			stored, err := database.Graph(t.Context(), graph)
			if err != nil || (stored.DeletedAt != nil) != (sample.want == model.GraphGradeOwner) {
				t.Fatalf("등급별 웹 삭제 상태 = %v", err)
			}
			if sample.want == model.GraphGradeOwner {
				permissionRequest(t, server, token.Raw, http.MethodPost, path+"/deletion", url.Values{"action": {"restore"}}, http.StatusSeeOther)
			}
			items, _, err := database.ListGraphs(t.Context(), actor, model.GraphListFilter{}, "", 200)
			if err != nil || slices.ContainsFunc(items, func(item model.GraphListItem) bool { return item.ID == graph }) != (sample.want != "") {
				t.Fatalf("graph_list의 권한 필터 = %v", err)
			}
			if sample.direct != "" {
				if err := database.RevokeGraphGrantWithAudit(t.Context(), graph, owner, actor, store.GrantSubjectAccount); err != nil {
					t.Fatal(err)
				}
			}
			if sample.team != "" && !sample.empty {
				if err := database.RemoveTeamMemberWithAudit(t.Context(), team.ID, owner, actor); err != nil {
					t.Fatal(err)
				}
			}
			if account, verified, err := service.Verify(t.Context(), token.Raw, web.SessionAudience); err != nil || account != actor || verified.ID != token.ID {
				t.Fatal("등급 회수 뒤에도 같은 서명 세션은 유효해야 한다")
			}
			permissionRequest(t, server, token.Raw, http.MethodGet, path, nil, http.StatusNotFound)
			permissionRequest(t, server, token.Raw, http.MethodPost, path+"/access", url.Values{"action": {"grant_account"}, "login_id": {permissionLogin(target)}, "grade": {"owner"}}, http.StatusForbidden)
			permissionRequest(t, server, token.Raw, http.MethodGet, "/graphs/"+permissionID(t).String(), nil, http.StatusNotFound)
			items, _, err = database.ListGraphs(t.Context(), actor, model.GraphListFilter{}, "", 200)
			if err != nil || len(items) != 0 {
				t.Fatal("회수 뒤 목록에 그래프가 남았다")
			}
			// 기존 그래프 등급이 없어도 생성할 수 있고 생성자가 소유자다.
			created := permissionGraph(t, database, actor)
			if err := perm.Require(t.Context(), database, created, actor, model.GraphGradeOwner); err != nil {
				t.Fatal(err)
			}
			if sample.want != "" {
				grant(actor, store.GrantSubjectAccount, model.GraphGradeViewer)
				permissionRequest(t, server, token.Raw, http.MethodGet, path, nil, http.StatusOK)
			}
		})
	}
}

func permissionRank(grade model.GraphGrade) int {
	return slices.Index([]model.GraphGrade{"", model.GraphGradeViewer, model.GraphGradeEditor, model.GraphGradeOwner}, grade)
}

type permissionUnavailable struct{ web.GraphStore }

func (permissionUnavailable) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	return "", false, errors.New("synthetic private database failure")
}

func TestPermissionLookupFailureHTTPIntegration(t *testing.T) {
	// 실제 서명 세션을 검증한 뒤 공통 등급 조회에만 장애를 주입한다.
	database := permissionDatabase(t)
	service, err := authz.New(database, authz.Config{Issuer: "https://permission.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	account := permissionAccount(t, database)
	token, err := service.WebSession(t.Context(), account, web.SessionAudience)
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.New(permissionAuthentication{service}, permissionUnavailable{database}, web.Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/deletion", "/audit", "/access"} {
		t.Run(suffix, func(t *testing.T) {
			response := permissionRequest(t, server, token.Raw, http.MethodGet, "/graphs/"+permissionID(t).String()+suffix, nil, http.StatusInternalServerError)
			if strings.Contains(response.Body.String(), "synthetic private") {
				t.Fatal("내부 오류 원문 노출")
			}
		})
	}
}
