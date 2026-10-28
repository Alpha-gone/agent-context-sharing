//go:build browser_manual

package web_test

import (
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
)

// 수동 인수용 표본의 준비·TLS 로그인만 검증한다. 이 시험의 PASS는
// Safari/VoiceOver 조작이나 실제 음성을 관찰했다는 뜻이 아니다.
func TestTEST06ManualBrowserFixture(t *testing.T) {
	wait := time.Duration(0)
	if raw := os.Getenv("WEB_MANUAL_WAIT"); raw != "" {
		var err error
		wait, err = time.ParseDuration(raw)
		if err != nil || wait <= 0 || wait > 15*time.Minute {
			t.Fatal("WEB_MANUAL_WAIT는 0보다 크고 15m 이하인 기간이어야 합니다")
		}
	}
	database := permissionDatabase(t)
	service := web08Service(t, database)
	const login, password = "test06_manual_owner", "test06-manual-synthetic-password"
	owner, err := service.Register(t.Context(), login, password)
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.Register(t.Context(), "test06_manual_member", "test06-manual-member-password")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	detail := web03Graph(t, database, owner, "수동 인수 상세", now)
	empty := web03Graph(t, database, owner, "수동 인수 빈 그래프", now)
	single := web03Graph(t, database, owner, "수동 인수 단일 그래프", now)
	viewer := web03Graph(t, database, member, "수동 인수 열람 전용", now)
	web03Grant(t, database, viewer.ID, member, owner, store.GrantSubjectAccount, model.GraphGradeViewer)
	web05Context(t, database, single.ID, owner, model.LayerSource, "단일 원천", nil)
	isolated := web05Context(t, database, detail.ID, owner, model.LayerSource, "고립 원천", nil)
	source := web05Context(t, database, detail.ID, owner, model.LayerSource, "두 단계 근거의 원천", nil)
	derived := web05Context(t, database, detail.ID, owner, model.LayerDerived, "원천을 참조하는 선택 파생", []model.ID{source.ID})
	descendant := web05Context(t, database, detail.ID, owner, model.LayerDerived, "선택 파생을 참조하는 후손", []model.ID{derived.ID})
	event := web05Context(t, database, detail.ID, owner, model.LayerEvent, "원천을 묶는 사건", []model.ID{source.ID})
	_, grant := web08Grant(t, service, owner)
	agent, err := database.RegisterAgent(t.Context(), owner, "first", "test06-manual-registration")
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"graph_page":{"default":2,"maximum":3},"max_hops":0}}`, owner))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := web.New(permissionAuthentication{service}, database, web.Config{Connections: service, Plans: plans, SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadHeaderTimeout = 5 * time.Second
	server.Config.ReadTimeout = 30 * time.Second
	server.Config.WriteTimeout = 30 * time.Second
	server.Config.IdleTimeout = 30 * time.Second
	server.StartTLS()
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/login", strings.NewReader(url.Values{"login_id": {login}, "password": {password}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", server.URL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("시험 TLS 로그인 실패", err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || response.Request.URL.Path != "/graphs" {
		t.Fatal("수동 표본 로그인/세션 준비 실패")
	}
	for _, graph := range []model.ID{detail.ID, empty.ID, single.ID, viewer.ID} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/graphs/"+graph.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("수동 표본 상세 조회 실패", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>") {
			t.Fatal("수동 표본 상세 렌더링 실패")
		}
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(map[string]any{
		"url": server.URL, "login_id": login, "password": password,
		"member_login_id": "test06_manual_member", "member_account_id": member.String(),
		"detail_graph": detail.ID.String(), "empty_graph": empty.ID.String(), "single_graph": single.ID.String(),
		"viewer_graph": viewer.ID.String(), "grant": grant.String(),
		"isolated": isolated.ID.String(), "source": source.ID.String(), "derived": derived.ID.String(),
		"descendant": descendant.ID.String(), "event": event.ID.String(), "agent": agent.String(),
		"observations": "not_performed", "tls": "test certificate; browser trust requires user action",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "connection.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(directory, "server-certificate.pem"), certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	// 자격 원문은 로그에 남기지 않는다. 서버/자료는 시험 종료와 함께 정리한다.
	t.Logf("TEST06 fixture_only=true tls_login_and_graphs=passed browser_observations=not_performed url=%s private_metadata=%s", server.URL, filepath.Join(directory, "connection.json"))
	if wait == 0 {
		return
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt)
	defer stop()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	t.Logf("수동 관찰 대기: 최대 %s, 종료 파일=%s", wait, filepath.Join(directory, "STOP"))
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(directory, "STOP")); err == nil {
				return
			} else if !os.IsNotExist(err) {
				t.Fatal("수동 관찰 종료 파일 확인 실패", err)
			}
		}
	}
}
