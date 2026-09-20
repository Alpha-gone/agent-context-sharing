// 평가 실행기. 컨텍스트 집합과 질의 집합을 받아 비교 단계마다 반복 측정하고
// 기준선 대비 개선폭과 유의성으로 판정한다.
//
// 이 도구는 「패키지 경계」가 정한 열 개 업무 패키지 밖에 두며 마이그레이션 실행기와
// 같은 자리다. 실행 중인 서버를 거치지 않고 검색 실행기를 직접 만드는 이유는 비교
// 단계 전환이 배포 구성 값이라, 서버를 거치면 단계마다 재기동해야 하기 때문이다.
// 같은 프로세스에서 단계만 바꿔 돌아야 입력·표현·예산이 통제된 상태로 남는다.
package main

import (
	"context"
	json "encoding/json/v2"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

// report는 결과 파일 전체다. 「검증」이 조건, 반복 회차, 지표 값과 데이터셋 판을
// 함께 남기라고 요구한다.
type report struct {
	StartedAt      time.Time        `json:"started_at"`
	ContextVersion string           `json:"context_dataset_version"`
	QueryVersion   string           `json:"query_dataset_version"`
	Conditions     conditions       `json:"conditions"`
	Runs           []runMetrics     `json:"runs"`
	Judgements     []stageJudgement `json:"judgements"`
}

// conditions는 단계를 제외하고 회차마다 고정한 통제 조건이다.
type conditions struct {
	Stages            []string `json:"stages"`
	Repeats           int      `json:"repeats"`
	Budget            int      `json:"budget"`
	MaxHops           int      `json:"max_hops"`
	MaxHopNodes       int      `json:"max_hop_nodes"`
	Execution         string   `json:"channel_execution"`
	CandidateLimit    int      `json:"channel_candidate_limit"`
	SemanticThreshold float64  `json:"semantic_similarity_threshold"`
	FoldThreshold     float64  `json:"fold_threshold"`
	EmbeddingModel    string   `json:"embedding_model"`
	IndexTargets      string   `json:"index_targets"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("평가 종료", "error", err)
		os.Exit(1)
	}
}

func run() error {
	defaultLimits := plan.Default()
	contextsPath := flag.String("contexts", "", "컨텍스트 집합 파일 경로")
	queriesPath := flag.String("queries", "", "질의 집합 파일 경로")
	stageList := flag.String("stages", "baseline,references,relations,global", "누적 비교 단계를 순서대로 지정한다")
	repeats := flag.Int("repeat", minimumRepeats, "단계마다 반복할 회차 수")
	budget := flag.Int("budget", 4000, "검색 예산 문자 수. 0은 한도 없음이다")
	maxHops := flag.Int("max-hops", defaultLimits.MaxHops, "그래프 확장 최대 홉 수")
	maxHopNodes := flag.Int("max-hop-nodes", defaultLimits.MaxHopNodes, "그래프 확장 최대 노드 수")
	indexTargets := flag.String("index-targets", string(store.IndexTargetsAllLayers), "색인 대상 계층. all_layers 또는 without_source")
	convert := flag.String("convert", "", "데이터셋을 변환하거나 생성한다. hipporag, own, own-global 또는 own-global-auto")
	convertSource := flag.String("convert-source", "", "변환할 벤치마크 파일 경로")
	convertName := flag.String("convert-name", "", "데이터셋 판에 넣을 세트 이름")
	convertLimit := flag.Int("convert-questions", 0, "변환에 쓸 질의 수. 0이면 전부 쓴다")
	convertUseCase := flag.String("convert-use-case", "", "변환 결과의 질의를 fact, associative 또는 global 사용 사례로 제한한다")
	outPath := flag.String("out", "", "결과 JSON 경로. 비우면 표준 출력에 쓴다")
	flag.Parse()

	if *contextsPath == "" || *queriesPath == "" {
		return fmt.Errorf("-contexts와 -queries가 필요하다")
	}
	// 변환은 데이터베이스에 닿지 않는다. 같은 두 경로를 출력 자리로 쓰므로 변환한
	// 파일을 그대로 측정에 넘길 수 있다.
	if *convert != "" {
		return runConvert(*convert, *convertSource, *convertName, *convertLimit, *convertUseCase, *contextsPath, *queriesPath)
	}
	if *repeats < minimumRepeats {
		return fmt.Errorf("반복 회차는 %d 이상이어야 한다. 「검증」이 최소 세 번으로 정했다", minimumRepeats)
	}
	stages, err := parseStages(*stageList)
	if err != nil {
		return err
	}
	contexts, err := loadContextSet(*contextsPath)
	if err != nil {
		return err
	}
	queries, err := loadQuerySet(*queriesPath, contexts)
	if err != nil {
		return err
	}

	settings, err := loadSettings()
	if err != nil {
		return err
	}
	ctx := context.Background()
	// 관계 후보 제안과 유예 기간을 주지 않는다. 제안은 확정되기 전까지 그래프 경로
	// 채널에 쓰이지 않으므로 측정에 들어가지 않고, 끄면 회차마다 같은 그래프 상태에서
	// 재는 것이 보장된다. 유예 판정은 주기 작업의 몫이며 평가는 그 경로를 타지 않는다.
	// 색인 대상 계층은 적재 시점의 등록 조건이므로 그래프를 만들기 전에 정해야 한다.
	// 「색인 대상 비교」는 이 값만 바꾼 두 실행의 결과를 견주는 것이며, 단계 비교처럼
	// 한 실행 안에서 바꿀 수 있는 축이 아니다.
	database, err := store.New(ctx, settings.databaseURL, settings.graphName, nil, nil, store.IndexTargets(*indexTargets))
	if err != nil {
		return fmt.Errorf("데이터베이스 풀 준비: %w", err)
	}
	defer database.Close()
	worker, err := index.New(database, settings.index, nil, slog.Default())
	if err != nil {
		return fmt.Errorf("색인 작업자 준비: %w", err)
	}

	graph, err := loadGraph(ctx, database, contexts)
	if err != nil {
		return err
	}
	// 평가 그래프는 측정이 끝나면 소프트 삭제해 다음 회차의 검색 대상에서 뺀다.
	// 「소프트 삭제 수명주기」가 영구 삭제를 하지 않으므로 기록은 남는다.
	defer func() {
		if err := database.SetGraphDeleted(ctx, graph.GraphID, graph.AccountID, true); err != nil {
			slog.Error("평가 그래프 정리 실패", "graph_id", graph.GraphID.String(), "error", err)
		}
	}()
	if err := drainIndexQueue(ctx, worker, graph.GraphID, len(contexts.Contexts)*4+64); err != nil {
		return err
	}

	result := report{
		StartedAt:      time.Now().UTC(),
		ContextVersion: contexts.Version,
		QueryVersion:   queries.Version,
		Conditions: conditions{
			Stages:            stages,
			Repeats:           *repeats,
			Budget:            *budget,
			MaxHops:           *maxHops,
			MaxHopNodes:       *maxHopNodes,
			Execution:         string(settings.execution),
			CandidateLimit:    settings.candidateLimit,
			SemanticThreshold: settings.semanticThreshold,
			FoldThreshold:     settings.foldThreshold,
			EmbeddingModel:    worker.ModelID(),
			IndexTargets:      *indexTargets,
		},
	}
	runs := map[string][]runMetrics{}
	for _, stage := range stages {
		service, err := search.New(database, worker, search.Config{
			Execution:         settings.execution,
			CandidateLimit:    settings.candidateLimit,
			SemanticThreshold: settings.semanticThreshold,
			FoldThreshold:     settings.foldThreshold,
			GraphStage:        search.GraphStage(stage),
			GlobalFallback:    stage == string(search.GraphStageGlobal),
		}, slog.Default())
		if err != nil {
			return fmt.Errorf("단계 %q 검색 실행기 준비: %w", stage, err)
		}
		for repeat := 1; repeat <= *repeats; repeat++ {
			started := time.Now()
			measured, err := measure(ctx, service, graph, queries, *budget, *maxHops, *maxHopNodes)
			if err != nil {
				return fmt.Errorf("단계 %q 회차 %d: %w", stage, repeat, err)
			}
			elapsed := time.Since(started)
			run := runMetrics{Stage: stage, Repeat: repeat, DurationMS: elapsed.Milliseconds(), UseCases: measured.UseCases, QuerySamples: measured.QuerySamples}
			runs[stage] = append(runs[stage], run)
			result.Runs = append(result.Runs, run)
			slog.Info("회차 완료", "stage", stage, "repeat", repeat, "duration", elapsed.String())
		}
	}
	result.Judgements = judge(stages, runs)
	return writeReport(*outPath, result)
}

// runConvert는 공개 벤치마크를 데이터셋 두 파일로 옮기고 끝낸다.
func runConvert(format, source, name string, limit int, useCase, contextsPath, queriesPath string) error {
	var contexts contextSet
	var queries querySet
	var err error
	switch format {
	case convertHippoRAG:
		if source == "" || name == "" {
			return fmt.Errorf("-convert-source와 -convert-name이 필요하다")
		}
		contexts, queries, err = convertHippoRAGSet(source, name, limit)
	case convertOwn:
		if name == "" {
			return fmt.Errorf("-convert-name이 필요하다")
		}
		contexts, queries, err = convertOwnSet(name, limit)
	case convertOwnGlobal:
		if name == "" {
			return fmt.Errorf("-convert-name이 필요하다")
		}
		contexts, queries, err = convertOwnGlobalSet(name, limit)
	case convertOwnGlobalAuto:
		if name == "" {
			return fmt.Errorf("-convert-name이 필요하다")
		}
		contexts, queries, err = convertOwnGlobalAutoSet(name, limit)
	default:
		return fmt.Errorf("변환 형식 %q를 알 수 없다", format)
	}
	if err != nil {
		return err
	}
	queries, err = filterQuerySet(queries, useCase)
	if err != nil {
		return err
	}
	if err := writeDataset(contextsPath, contexts); err != nil {
		return err
	}
	if err := writeDataset(queriesPath, queries); err != nil {
		return err
	}
	// 만든 파일을 바로 읽어 적재 전 검증을 통과하는지 확인한다. 변환이 형식을 어기면
	// 측정 단계가 아니라 여기에서 드러나야 한다.
	loaded, err := loadContextSet(contextsPath)
	if err != nil {
		return err
	}
	if _, err := loadQuerySet(queriesPath, loaded); err != nil {
		return err
	}
	slog.Info("변환 완료", "version", contexts.Version, "contexts", len(contexts.Contexts), "queries", len(queries.Queries))
	return nil
}

// filterQuerySet은 컨텍스트 표현은 그대로 두고 측정할 사용 사례의 질의 파일만 분리한다.
// 그래프 효과 비교의 통제 조건을 유지하려면 사용 사례마다 컨텍스트를 잘라서는 안 된다.
func filterQuerySet(set querySet, useCase string) (querySet, error) {
	if useCase == "" {
		return set, nil
	}
	if !slices.Contains(useCases, useCase) {
		return querySet{}, fmt.Errorf("변환 사용 사례 %q를 알 수 없다", useCase)
	}
	filtered := make([]querySpec, 0, len(set.Queries))
	for _, query := range set.Queries {
		if query.UseCase == useCase {
			filtered = append(filtered, query)
		}
	}
	if len(filtered) == 0 {
		return querySet{}, fmt.Errorf("변환 결과에 %q 사용 사례 질의가 없다", useCase)
	}
	set.Version += "-" + useCase
	set.Queries = filtered
	return set, nil
}

func parseStages(raw string) ([]string, error) {
	known := []string{
		string(search.GraphStageBaseline),
		string(search.GraphStageReferences),
		string(search.GraphStageRelations),
		string(search.GraphStageGlobal),
	}
	stages := make([]string, 0, len(known))
	for value := range strings.SplitSeq(raw, ",") {
		stage := strings.TrimSpace(value)
		if stage == "" {
			continue
		}
		if !slices.Contains(known, stage) {
			return nil, fmt.Errorf("비교 단계 %q를 알 수 없다", stage)
		}
		if slices.Contains(stages, stage) {
			return nil, fmt.Errorf("비교 단계 %q가 중복된다", stage)
		}
		stages = append(stages, stage)
	}
	if len(stages) == 0 {
		return nil, fmt.Errorf("비교 단계가 없다")
	}
	return stages, nil
}

// settings는 평가가 읽는 배포 구성이다. 마이그레이션 실행기와 같이 애플리케이션
// 구성 전체를 요구하지 않고 필요한 값만 직접 읽는다.
type settings struct {
	databaseURL       string
	graphName         string
	index             index.Config
	execution         search.Execution
	candidateLimit    int
	semanticThreshold float64
	foldThreshold     float64
}

func loadSettings() (settings, error) {
	loaded := settings{
		databaseURL: os.Getenv("DATABASE_URL"),
		graphName:   os.Getenv("AGE_GRAPH_NAME"),
		execution:   search.Execution(envOr("SEARCH_CHANNEL_EXECUTION", string(search.ExecutionParallel))),
	}
	if loaded.databaseURL == "" || loaded.graphName == "" {
		return settings{}, fmt.Errorf("DATABASE_URL과 AGE_GRAPH_NAME이 필요하다")
	}
	baseURL, err := url.Parse(os.Getenv("EMBEDDING_BASE_URL"))
	if err != nil {
		return settings{}, fmt.Errorf("EMBEDDING_BASE_URL 해석: %w", err)
	}
	dimension, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EMBEDDING_DIMENSION")))
	if err != nil {
		return settings{}, fmt.Errorf("EMBEDDING_DIMENSION 해석: %w", err)
	}
	loaded.index = index.Config{
		BaseURL:    baseURL,
		Model:      os.Getenv("EMBEDDING_MODEL"),
		VectorType: os.Getenv("EMBEDDING_VECTOR_TYPE"),
		Dimension:  dimension,
	}
	if loaded.candidateLimit, err = strconv.Atoi(strings.TrimSpace(envOr("SEARCH_CHANNEL_CANDIDATE_LIMIT", "50"))); err != nil {
		return settings{}, fmt.Errorf("SEARCH_CHANNEL_CANDIDATE_LIMIT 해석: %w", err)
	}
	if loaded.semanticThreshold, err = strconv.ParseFloat(strings.TrimSpace(envOr("SEARCH_SEMANTIC_SIMILARITY_THRESHOLD", "0.5")), 64); err != nil {
		return settings{}, fmt.Errorf("SEARCH_SEMANTIC_SIMILARITY_THRESHOLD 해석: %w", err)
	}
	if loaded.foldThreshold, err = strconv.ParseFloat(strings.TrimSpace(envOr("SEARCH_FOLD_SIMILARITY_THRESHOLD", "0.9")), 64); err != nil {
		return settings{}, fmt.Errorf("SEARCH_FOLD_SIMILARITY_THRESHOLD 해석: %w", err)
	}
	return loaded, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// writeReport는 결과를 JSON으로 남긴다. 질의문과 컨텍스트 본문은 어느 필드에도
// 들어가지 않으며 식별자와 집계값만 싣는다.
func writeReport(path string, value report) error {
	raw, err := json.Marshal(value, json.FormatNilSliceAsNull(false))
	if err != nil {
		return fmt.Errorf("결과 직렬화: %w", err)
	}
	if path == "" {
		fmt.Println(string(raw))
		return nil
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("결과 기록: %w", err)
	}
	return nil
}
