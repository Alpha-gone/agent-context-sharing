package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/ingest"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestClientDatabaseIntegration(t *testing.T) {
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("합성 시험 DB가 필요하다")
		}
		t.Skip("TEST_DATABASE_URL 미설정")
	}
	admin, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	id, _ := model.NewID()
	name := "ingest_client_" + strings.ReplaceAll(id.String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	u.RawPath = ""
	q := u.Query()
	q.Del("dbname")
	q.Del("database")
	u.RawQuery = q.Encode()
	pool, err := migrate.NewPool(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	items, err := migrate.Load(migrations.FS, ".", migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if err := migrate.Apply(t.Context(), pool, item); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.New(t.Context(), u.String(), "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	account, _ := model.NewID()
	graph, _ := model.NewID()
	now := time.Now().UTC()
	if err := db.CreateAccount(t.Context(), store.Account{ID: account, LoginID: "ingest_client", PasswordHash: "synthetic", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateGraphWithOwner(t.Context(), model.Graph{ID: graph, Name: "synthetic", CreatedBy: account, CreatedAt: now, LastActivityAt: now, Version: 1}, store.WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueIngestCredential(t.Context(), account, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	handler := ingest.Handler{Backend: db, Gap: time.Minute}
	first := true
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			capture := httptest.NewRecorder()
			handler.ServeHTTP(capture, r)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })
	project, stateDir := setup(t, server.URL, graph)
	t.Setenv("INGEST_CREDENTIAL", token)
	body := "저장할 합성 사용자 발화"
	if err := testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(project, "test-session", body))); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	if err := testRun(t.Context(), []string{"flush", "--host", "claude", "--session", "test-session"}, nil); err == nil {
		t.Fatal("응답 실패 미감지")
	}
	if err := testRun(t.Context(), []string{"pause", "--host", "claude", "--session", "test-session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(project, "test-session", "전송 제외 합성 발화"))); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"resume", "--host", "claude", "--session", "test-session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"flush", "--host", "claude", "--session", "test-session"}, nil); err != nil {
		t.Fatal(err)
	}
	s := loadState(t, stateDir, project, "claude", "test-session")
	if len(s.Pending) != 0 || s.Next != 1 {
		t.Fatal("재전송 뒤 대기 상태")
	}
	rows, err := pool.Query(t.Context(), `SELECT properties ->> 'body'::text,properties ->> 'turn_index'::text FROM agent_context."Context" WHERE properties ->> 'graph_id'::text=$1 AND properties ->> 'conversation_id'::text=$2`, graph.String(), "claude:"+sha256Name(project, "claude", "test-session"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var saved, index string
		if err := rows.Scan(&saved, &index); err != nil {
			t.Fatal(err)
		}
		if saved != body || index != "0" {
			t.Fatalf("저장 원천 불일치: %q %s", saved, index)
		}
		count++
	}
	if err := rows.Err(); err != nil || count != 1 {
		t.Fatal(fmt.Errorf("저장 원천 수 %d: %w", count, err))
	}
	rows.Close()
	otherProject := t.TempDir()
	config := fmt.Sprintf(`{"enabled":true,"graph_id":%q}`, graph.String())
	if err := os.WriteFile(filepath.Join(otherProject, projectFile), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(otherProject, "test-session", "다른 프로젝트 합성 발화"))); err != nil {
		t.Fatal(err)
	}
	t.Chdir(otherProject)
	if err := testRun(t.Context(), []string{"flush", "--host", "claude", "--session", "test-session"}, nil); err != nil {
		t.Fatal(err)
	}
	var otherCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_context."Context" WHERE properties ->> 'graph_id'::text=$1 AND properties ->> 'conversation_id'::text=$2 AND properties ->> 'body'::text=$3`, graph.String(), "claude:"+sha256Name(otherProject, "claude", "test-session"), "다른 프로젝트 합성 발화").Scan(&otherCount); err != nil || otherCount != 1 {
		t.Fatalf("프로젝트 사이 대화 충돌: %d %v", otherCount, err)
	}
}
