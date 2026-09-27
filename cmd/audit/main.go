// Command audit는 「그래프 불변식 규칙과 감사」의 전체 감사를 읽기 전용으로 실행한다.
//
// 사용법:
//
//	audit                 모든 활성·삭제 그래프를 감사한다
//	audit -graph-id <id>  그래프 하나를 감사한다
//
// 위반은 표준 출력에 JSON Lines로 한 줄씩 쓴다. 각 줄은 규칙 식별자, graph_id, 대상 종류와
// 위반 정점·간선·행의 식별자만 담고 컨텍스트 본문과 임베딩 값은 담지 않는다. 위반이 없으면
// 종료 상태 0, 위반이 있으면 1, 감사를 끝내지 못하면 2다. 저장 상태를 삭제·수정하지 않는다.
//
// 접속 정보와 임베딩 구성은 환경 변수로 받는다. 목록은 .env.example에 있다.
package main

import (
	"context"
	json "encoding/json/v2"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func main() {
	found, err := run()
	if err != nil {
		slog.Error("감사 종료", "error", err)
		os.Exit(2)
	}
	if found {
		os.Exit(1)
	}
}

// run은 감사를 실행하고 위반이 있었는지 돌려준다.
func run() (bool, error) {
	graphFlag := flag.String("graph-id", "", "감사할 그래프 하나. 비우면 모든 활성·삭제 그래프를 감사한다")
	flag.Parse()
	databaseURL, graphName := os.Getenv("DATABASE_URL"), os.Getenv("AGE_GRAPH_NAME")
	if databaseURL == "" || graphName == "" {
		return false, fmt.Errorf("DATABASE_URL과 AGE_GRAPH_NAME이 필요하다")
	}
	dimension, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EMBEDDING_DIMENSION")))
	if err != nil || dimension < 1 {
		return false, fmt.Errorf("EMBEDDING_DIMENSION이 양의 정수가 아니다")
	}
	modelName, vectorType := strings.TrimSpace(os.Getenv("EMBEDDING_MODEL")), strings.TrimSpace(os.Getenv("EMBEDDING_VECTOR_TYPE"))
	if modelName == "" || vectorType == "" {
		return false, fmt.Errorf("EMBEDDING_MODEL과 EMBEDDING_VECTOR_TYPE이 필요하다")
	}
	expect := store.EmbeddingExpectation{ModelID: index.ModelID(modelName, vectorType, dimension), Dimension: dimension}

	ctx := context.Background()
	database, err := store.New(ctx, databaseURL, graphName, nil, nil, "")
	if err != nil {
		return false, fmt.Errorf("데이터베이스 풀 준비: %w", err)
	}
	defer database.Close()
	graphIDs, err := graphScope(ctx, database, *graphFlag)
	if err != nil {
		return false, err
	}
	started := time.Now()
	found := 0
	for _, graphID := range graphIDs {
		violations, err := database.AuditGraph(ctx, graphID, expect)
		if err != nil {
			return false, fmt.Errorf("그래프 %s 감사: %w", graphID, err)
		}
		for _, violation := range violations {
			line, err := json.Marshal(violation)
			if err != nil {
				return false, fmt.Errorf("위반 직렬화: %w", err)
			}
			fmt.Println(string(line))
		}
		found += len(violations)
	}
	slog.Info("감사 완료", "graphs", len(graphIDs), "violations", found, "duration", time.Since(started).String())
	return found > 0, nil
}

func graphScope(ctx context.Context, database *store.Store, raw string) ([]model.ID, error) {
	if raw == "" {
		return database.AuditGraphIDs(ctx)
	}
	graphID, err := model.ParseID(raw)
	if err != nil || !graphID.IsV7() {
		return nil, fmt.Errorf("-graph-id %q가 UUIDv7이 아니다", raw)
	}
	return []model.ID{graphID}, nil
}
