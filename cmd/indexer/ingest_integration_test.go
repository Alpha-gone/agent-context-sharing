package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIngestExternalIndexerConsumesQueueIntegration(t *testing.T) {
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("시험 DB가 필요하다")
		}
		t.Skip("시험 DB 미설정")
	}
	admin, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	id, _ := model.NewID()
	name := "ingest_external_" + strings.ReplaceAll(id.String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
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
	loaded, err := migrate.Load(migrations.FS, ".", migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768})
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, item := range loaded {
		if err = migrate.Apply(t.Context(), pool, item); err != nil {
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
	if err := db.CreateAccount(t.Context(), store.Account{ID: account, LoginID: "external_ingest", PasswordHash: "synthetic", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateGraphWithOwner(t.Context(), model.Graph{ID: graph, Name: "external ingest", CreatedBy: account, CreatedAt: now, LastActivityAt: now, Version: 1}, store.WriteLimits{}); err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueIngestCredential(t.Context(), account, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, agent, err := db.ValidateIngestCredential(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 2; i++ {
		id, _ := model.NewID()
		source := model.Context{ID: id, GraphID: graph, Layer: model.LayerSource, Body: "합성 발화", RecordedAt: now, CreatedBy: account, CreatedByAgent: agent, Version: 1, Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelConversation, Locator: fmt.Sprintf("conversation:///external/%d", i)}, OccurredAt: now.Add(time.Duration(i) * time.Second), OriginKind: model.OriginKindUserUtterance, ConversationID: "synthetic", TurnIndex: new(i)}}
		if _, _, err := db.CreateIngestSource(t.Context(), graph, source, store.WriteLimits{}, time.Minute, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE public.conversation_segment_task SET next_attempt_at=statement_timestamp() WHERE graph_id=$1`, graph.String()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			values[key] = strings.Trim(value, "'")
		}
	}
	base, err := os.MkdirTemp(".", ".ingest-indexer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	logDir, err := filepath.Abs(filepath.Join(base, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	values["DATABASE_URL"], values["GEMINI_API_KEY"], values["INDEX_WORKER_PLACEMENT"], values["DIAGNOSTIC_LOG_DIR"], values["CONVERSATION_INGEST_ENABLED"], values["CONVERSATION_SEGMENT_MAX_TURNS"] = u.String(), "synthetic-key", "external", logDir, "true", "2"
	cfg, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	client := &http.Client{Transport: indexerRoundTrip(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected provider call") })}
	go func() { done <- runConfiguredWithClient(ctx, cfg, client) }()
	deadline := time.Now().Add(15 * time.Second)
	segments := 0
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.conversation_segment WHERE graph_id=$1`, graph.String()).Scan(&segments); err != nil {
			t.Fatal(err)
		}
		if segments > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("분리 작업자 조기 종료: %v", err)
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("분리 작업자 종료 지연")
	}
	if segments != 1 {
		t.Fatal("분리 작업자가 사건을 만들지 않았다")
	}
}
