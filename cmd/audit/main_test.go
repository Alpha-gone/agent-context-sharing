package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

type failedOutput struct{ cause error }

func (output failedOutput) Write([]byte) (int, error) { return 0, output.cause }

func TestAuditArgumentsBeforeDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"식별자 형식", []string{"-graph-id", "synthetic-body-secret"}, "UUIDv7"},
		{"지원하지 않는 옵션", []string{"-unknown"}, "flag"},
		{"위치 인자", []string{"synthetic-body-secret"}, "사용법"},
		{"식별자 누락", []string{"-graph-id"}, "argument"},
		{"접속 구성 누락", nil, "DATABASE_URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			found, err := run(t.Context(), test.args, &output)
			if found || err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "synthetic-body-secret") || output.Len() != 0 {
				t.Fatalf("입력 검증=%t/%v/%q", found, err, output.String())
			}
		})
	}
}

func TestAuditHelpAndOutputFailure(t *testing.T) {
	var output bytes.Buffer
	found, err := run(t.Context(), []string{"-help"}, &output)
	if found || err != nil || !strings.Contains(output.String(), "audit [-graph-id UUIDv7]") {
		t.Fatalf("도움말=%t/%v/%q", found, err, output.String())
	}
	cause := errors.New("출력 장치 장애")
	if _, err := run(t.Context(), []string{"-help"}, failedOutput{cause}); !errors.Is(err, cause) {
		t.Fatalf("출력 실패=%v", err)
	}
}

func TestAuditCommandScopeAndFailureIntegration(t *testing.T) {
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 감사 CLI 모듈 시험을 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	for name, value := range map[string]string{
		"DATABASE_URL": address, "AGE_GRAPH_NAME": graphName,
		"EMBEDDING_PROVIDER": "gemini", "EMBEDDING_BASE_URL": "https://generativelanguage.googleapis.com",
		"EMBEDDING_MODEL": "gemini-embedding-2", "EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": "768", "GEMINI_API_KEY": "",
	} {
		t.Setenv(name, value)
	}
	pool, err := migrate.NewPool(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	clean, broken, actor, node, orphan, orphanNode := model.ID{}, model.ID{}, model.ID{}, model.ID{}, model.ID{}, model.ID{}
	for _, id := range []*model.ID{&clean, &broken, &actor, &node, &orphan, &orphanNode} {
		*id, err = model.NewID()
		if err != nil {
			t.Fatal(err)
		}
	}
	table := pgx.Identifier{graphName, "Context"}.Sanitize()
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE properties ->> 'graph_id'::text=ANY($1::text[])", []string{broken.String(), orphan.String()}); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM public.context_graph WHERE graph_id=ANY($1::uuid[])`, []string{clean.String(), broken.String()}); err != nil {
			t.Error(err)
		}
	}()
	for _, id := range []model.ID{clean, broken} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO public.context_graph (graph_id,name,created_by,created_at,last_activity_at)
			VALUES ($1,'audit CLI synthetic',$2,now(),now())`, id.String(), actor.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at=now() WHERE graph_id=$1`, broken.String()); err != nil {
		t.Fatal(err)
	}
	properties, err := json.Marshal(map[string]string{"context_id": node.String(), "graph_id": broken.String(), "layer": "derived", "body": "synthetic-body-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO "+table+" (properties) VALUES ($1::ag_catalog.agtype)", string(properties)); err != nil {
		t.Fatal(err)
	}
	// 등록되지 않은 graph_id의 정점은 그래프 단위 감사 범위 밖이므로 전체 감사만 보고한다.
	orphanProperties, err := json.Marshal(map[string]string{"context_id": orphanNode.String(), "graph_id": orphan.String(), "layer": "source", "body": "synthetic-body-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO "+table+" (properties) VALUES ($1::ag_catalog.agtype)", string(orphanProperties)); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if found, err := run(t.Context(), []string{"-graph-id", clean.String()}, &output); found || err != nil || output.Len() != 0 {
		t.Fatalf("정상 그래프 감사=%t/%v/%q", found, err, output.String())
	}
	for _, args := range [][]string{{"-graph-id", broken.String()}, nil} {
		output.Reset()
		found, err := run(t.Context(), args, &output)
		if !found || err != nil || strings.Contains(output.String(), "synthetic-body-secret") {
			t.Fatalf("삭제 그래프/전체 감사=%t/%v", found, err)
		}
		seen, unregistered := false, false
		for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
			var violation struct {
				Rule    string   `json:"rule"`
				GraphID string   `json:"graph_id"`
				Target  string   `json:"target"`
				IDs     []string `json:"ids"`
			}
			if err := json.Unmarshal([]byte(line), &violation); err != nil {
				t.Fatalf("JSON Lines: %v", err)
			}
			seen = seen || violation.Rule == "reference_cardinality" && violation.GraphID == broken.String()
			unregistered = unregistered || violation.Rule == "graph_registration" && violation.GraphID == orphan.String() && violation.Target == "context" && len(violation.IDs) == 1 && violation.IDs[0] == orphanNode.String()
		}
		if !seen {
			t.Fatal("기본 범위에서 삭제 그래프의 위반을 놓쳤다")
		}
		if unregistered != (args == nil) {
			t.Fatalf("미등록 그래프 정점 보고=%t, 전체 감사=%t", unregistered, args == nil)
		}
	}
	cause := errors.New("위반 출력 실패")
	if found, err := run(t.Context(), []string{"-graph-id", broken.String()}, failedOutput{cause}); found || !errors.Is(err, cause) {
		t.Fatalf("불완전 출력=%t/%v", found, err)
	}
}
