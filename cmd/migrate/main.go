// Command migrate는 데이터베이스 스키마를 마이그레이션 파일로 적용한다.
//
// 사용법:
//
//	migrate status   아직 적용되지 않은 마이그레이션을 보여준다
//	migrate up       적용되지 않은 마이그레이션을 버전 순으로 적용한다
//
// 접속 정보와 배포 구성 값은 환경 변수로 받는다. 목록은 .env.example에 있다.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/migrations"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) != 2 {
		log.Fatalf("사용법: %s [status|up]", os.Args[0])
	}
	if err := run(context.Background(), os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, command string) error {
	if command != "status" && command != "up" {
		return fmt.Errorf("알 수 없는 명령 %q. status 또는 up을 쓴다", command)
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL이 비어 있다")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	migrations, err := migrate.Load(migrations.FS, ".", cfg)
	if err != nil {
		return err
	}

	pool, err := migrate.NewPool(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// 이력 확인부터 적용까지를 자문 잠금 안에서 한다. 여러 인스턴스가 동시에 기동하면
	// 이력을 함께 읽고 같은 파일을 함께 적용하려 들기 때문이다. 기다린 쪽은 잠금을 얻은
	// 뒤에 이력을 읽으므로 앞선 실행기가 적용한 결과를 본다.
	return migrate.WithLock(ctx, pool, func(ctx context.Context) error {
		return runLocked(ctx, pool, command, migrations)
	})
}

func runLocked(ctx context.Context, pool *pgxpool.Pool, command string, migrations []migrate.Migration) error {
	if err := migrate.EnsureHistory(ctx, pool); err != nil {
		return err
	}
	applied, err := migrate.AppliedVersions(ctx, pool)
	if err != nil {
		return err
	}
	pending, err := migrate.Pending(migrations, applied)
	if err != nil {
		return err
	}

	if len(pending) == 0 {
		fmt.Printf("적용할 마이그레이션이 없다. 적용된 판 %d개\n", len(applied))
		return nil
	}

	if command == "status" {
		fmt.Printf("적용되지 않은 마이그레이션 %d개\n", len(pending))
		for _, m := range pending {
			fmt.Printf("  %03d_%s\n", m.Version, m.Name)
		}
		return nil
	}

	for _, m := range pending {
		if err := migrate.Apply(ctx, pool, m); err != nil {
			return err
		}
		fmt.Printf("적용함 %03d_%s\n", m.Version, m.Name)
	}
	fmt.Printf("마이그레이션 %d개를 적용했다\n", len(pending))
	return nil
}

func loadConfig() (migrate.Config, error) {
	dimText := os.Getenv("EMBEDDING_DIMENSION")
	dim, err := strconv.Atoi(dimText)
	if err != nil {
		return migrate.Config{}, fmt.Errorf("EMBEDDING_DIMENSION %q 해석: %w", dimText, err)
	}
	cfg := migrate.Config{
		GraphName:  os.Getenv("AGE_GRAPH_NAME"),
		VectorType: os.Getenv("EMBEDDING_VECTOR_TYPE"),
		VectorDim:  dim,
	}
	if err := cfg.Validate(); err != nil {
		return migrate.Config{}, err
	}
	return cfg, nil
}
