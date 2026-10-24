package web

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestWEB01EmbeddedAssets(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	for path, contentType := range map[string]string{
		"app.css": "text/css; charset=utf-8", "cytoscape.min.js": "text/javascript; charset=utf-8",
		"PretendardVariable.woff2": "font/woff2", "CYTOSCAPE-LICENSE.txt": "text/plain; charset=utf-8",
		"PRETENDARD-LICENSE.txt": "text/plain; charset=utf-8",
	} {
		t.Run(path, func(t *testing.T) {
			target := "/assets/" + path
			first := httptest.NewRecorder()
			server.ServeHTTP(first, httptest.NewRequest(http.MethodGet, target, nil))
			if first.Code != http.StatusOK || first.Body.Len() == 0 {
				t.Fatalf("자산 응답 = %d, %d bytes", first.Code, first.Body.Len())
			}
			if got := first.Header().Get("Content-Type"); got != contentType {
				t.Errorf("Content-Type = %q, want %q", got, contentType)
			}
			etag := fmt.Sprintf(`"%x"`, sha256.Sum256(first.Body.Bytes()))
			if got := first.Header().Get("ETag"); got != etag {
				t.Errorf("ETag = %q, want %q", got, etag)
			}
			if first.Header().Get("Cache-Control") != "public, max-age=86400" || first.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("캐시·콘텐츠 형식 보호가 없다")
			}
			for name, match := range map[string]string{"exact": etag, "weak": "W/" + etag, "list": `"old", ` + etag, "any": "*"} {
				t.Run(name, func(t *testing.T) {
					request := httptest.NewRequest(http.MethodGet, target, nil)
					request.Header.Set("If-None-Match", match)
					response := httptest.NewRecorder()
					server.ServeHTTP(response, request)
					if response.Code != http.StatusNotModified || response.Body.Len() != 0 || response.Header().Get("ETag") != etag || response.Header().Get("Cache-Control") != "public, max-age=86400" {
						t.Fatalf("재검증 응답 = %d, %d bytes, %v", response.Code, response.Body.Len(), response.Header())
					}
				})
			}
			t.Run("changed", func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, target, nil)
				request.Header.Set("If-None-Match", `"old"`)
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != http.StatusOK || response.Body.String() != first.Body.String() {
					t.Fatal("이전 ETag에 현재 자산을 반환하지 않았다")
				}
			})
			t.Run("head", func(t *testing.T) {
				response := httptest.NewRecorder()
				server.ServeHTTP(response, httptest.NewRequest(http.MethodHead, target, nil))
				if response.Code != http.StatusOK || response.Body.Len() != 0 || response.Header().Get("Content-Length") != strconv.Itoa(first.Body.Len()) || response.Header().Get("ETag") != etag {
					t.Fatalf("HEAD 응답 = %d, %d bytes, %v", response.Code, response.Body.Len(), response.Header())
				}
			})
			t.Run("range", func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, target, nil)
				request.Header.Set("Range", "bytes=0-15")
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != http.StatusPartialContent || response.Body.String() != first.Body.String()[:16] {
					t.Fatalf("범위 응답 = %d, %d bytes", response.Code, response.Body.Len())
				}
			})
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
				t.Run(method, func(t *testing.T) {
					response := httptest.NewRecorder()
					server.ServeHTTP(response, httptest.NewRequest(method, target, nil))
					if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
						t.Fatalf("비조회 요청 = %d, Allow %q", response.Code, response.Header().Get("Allow"))
					}
				})
			}
		})
	}
}

func TestWEB01AssetPathsAreClosed(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	for _, path := range []string{
		"/assets/", "/assets/app.css/", "/assets/README.md", "/assets/unknown.js",
		"/assets/../web.go", "/assets/%2e%2e/web.go", "/templates/layout.html", "/assets/templates/layout.html",
	} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("미등록 경로 = %d, want 404", response.Code)
			}
		})
	}
}

func TestWEB01PagesShareLayoutAndEscapeInput(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	const payload = `</script><script>alert("web01")</script><img src=x onerror="alert(1)">`
	data := pageData{
		Title: payload, Message: payload, Error: payload, LoginID: payload,
		Graph:           model.Graph{Name: payload, Description: payload},
		GraphID:         testID(t),
		Owner:           true,
		Graphs:          []model.GraphListItem{{ID: testID(t), Name: payload, Description: payload}},
		Contexts:        []model.Context{{ID: testID(t), Layer: model.LayerSource, Body: payload}},
		Grants:          []model.GrantSubject{{Name: payload}},
		Teams:           []model.Team{{Name: payload, MemberLoginIDs: []string{payload}}},
		AuditEntries:    []model.AuditEntry{{Action: payload, Target: payload, Detail: payload}},
		RestoreRequests: []model.RestoreRequest{{GraphName: payload}},
	}
	for _, page := range []string{"login", "register", "logout", "message", "graphs", "graph_detail", "access", "deletion", "audit", "operator_restores", "authorize", "connections"} {
		t.Run(page, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.render(response, http.StatusOK, page, data)
			body := response.Body.String()
			if response.Code != http.StatusOK || strings.Contains(body, payload) || strings.Contains(body, "ZgotmplZ") {
				t.Fatalf("공통 렌더링·이스케이프 실패: %d", response.Code)
			}
			for _, expected := range []string{`<html lang="ko">`, `href="/assets/app.css"`, `href="#content"`, `<main id="content" tabindex="-1">`, `&lt;script&gt;`, `href="/assets/PRETENDARD-LICENSE.txt"`} {
				if !strings.Contains(body, expected) {
					t.Errorf("공통 화면 요소 없음: %s", expected)
				}
			}
			if strings.Contains(body, "<style>") || strings.Contains(body, `src="https://`) || strings.Contains(body, `href="https://`) {
				t.Error("공통 내장 스타일 대신 인라인·외부 자산을 사용한다")
			}
			graphAsset := strings.Contains(body, `<script src="/assets/cytoscape.min.js"></script>`)
			if graphAsset != (page == "graph_detail") {
				t.Error("시각화 자산을 그래프 상세에서만 읽지 않는다")
			}
		})
	}
}

func TestWEB01AccountFormsKeepLabelsAndHints(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	for page, expected := range map[string][]string{
		"login":    {`action="/login"`, `for="login-id"`, `id="login-id"`, `for="login-password"`, `id="login-password"`, `autocomplete="current-password"`},
		"register": {`action="/register"`, `for="register-id"`, `id="register-id"`, `aria-describedby="register-id-help"`, `id="register-id-help"`, `for="register-password"`, `aria-describedby="register-password-help"`, `id="register-password-help"`, `autocomplete="new-password"`},
	} {
		t.Run(page, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+page, nil))
			for _, markup := range append(expected, `method="post"`, `autocorrect="off"`, `autocapitalize="off"`, `spellcheck="false"`) {
				if !strings.Contains(response.Body.String(), markup) {
					t.Errorf("양식의 이름·도움말·제출 계약 없음: %s", markup)
				}
			}
		})
	}
}

func TestWEB01TemplateFailureDoesNotSendPartialSuccess(t *testing.T) {
	server := newTestServer(t, model.ID{}, model.ID{})
	// 공통 head가 만들어진 뒤 필요한 데이터가 없는 템플릿을 실행한다.
	if _, err := server.templates.New("broken").Parse(`{{template "head" .}}internal-template-marker{{.MissingField}}`); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.render(response, http.StatusOK, "broken", pageData{Title: "표본"})
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "internal-template-marker") || strings.Contains(response.Body.String(), "MissingField") {
		t.Fatalf("템플릿 실패가 성공·부분 본문·내부 원문으로 노출됐다: %d, %s", response.Code, response.Body.String())
	}
}

func TestWEB01PinnedThirdPartyAssets(t *testing.T) {
	for path, digest := range map[string]string{
		"PretendardVariable.woff2": "9599f12fd42fc0bce1cd50b47a0c022e108d7aa64dd0d1bb0ed44f3282d900b4",
		"PRETENDARD-LICENSE.txt":   "d31ddd9f2bed32fd7e302a205cf2380ba0de6529152d239ef99cfb6f261bfc04",
		"cytoscape.min.js":         "5f3b5b529546d5af1fc5628590af033b74511a5b6f789f5f4682845863228b91",
		"CYTOSCAPE-LICENSE.txt":    "eb319c6e6f233607f71e8e2f450391751883cfc0eeb3ca7ef574c13d1d9c2203",
	} {
		t.Run(path, func(t *testing.T) {
			content, err := webFiles.ReadFile("assets/" + path)
			if err != nil {
				t.Fatal(err)
			}
			if actual := fmt.Sprintf("%x", sha256.Sum256(content)); actual != digest {
				t.Fatalf("공식 배포 파일 해시 = %s, want %s", actual, digest)
			}
		})
	}
}
