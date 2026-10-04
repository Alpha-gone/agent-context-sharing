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
	Marginals      []marginal       `json:"marginal_contributions"`
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
	stageList := flag.String("stages", "baseline,references,relations,global,evidence", "누적 비교 단계를 순서대로 지정한다")
	repeats := flag.Int("repeat", minimumRepeats, "단계마다 반복할 회차 수")
	budget := flag.Int("budget", 4000, "검색 예산 문자 수. 0은 한도 없음이다")
	maxHops := flag.Int("max-hops", defaultLimits.MaxHops, "그래프 확장 최대 홉 수")
	maxHopNodes := flag.Int("max-hop-nodes", defaultLimits.MaxHopNodes, "그래프 확장 최대 노드 수")
	indexTargets := flag.String("index-targets", string(store.IndexTargetsAllLayers), "색인 대상 계층. all_layers 또는 without_source")
	convert := flag.String("convert", "", "데이터셋을 변환하거나 생성한다. hipporag, own, own-global, own-global-auto 또는 own-adversarial")
	convertSource := flag.String("convert-source", "", "변환할 벤치마크 파일 경로")
	convertName := flag.String("convert-name", "", "데이터셋 판에 넣을 세트 이름")
	convertLimit := flag.Int("convert-questions", 0, "변환에 쓸 질의 수. 0이면 전부 쓴다")
	convertUseCase := flag.String("convert-use-case", "", "변환 결과의 질의를 fact, associative 또는 global 사용 사례로 제한한다")
	continual := flag.Bool("continual", false, "온라인 갱신부터 상충 해소까지 여섯 지속 평가 시나리오를 실행한다")
	businessPath := flag.String("business", "", "업무 효과 통제 과업의 집계 지표 파일 경로")
	adaptive := flag.Bool("adaptive", false, "기존 auto와 질의 적응형 라우팅을 세 검색 경로의 오프라인 최적 경로로 견준다")
	adaptiveStage := flag.String("adaptive-stage", string(search.GraphStageRelations), "적응형 비교의 그래프 관계 범위. baseline은 쓸 수 없다")
	adaptiveEvidence := flag.Bool("adaptive-evidence", false, "적응형 비교의 두 구성에 근거 경로 보존 선택을 켠다")
	directThresholds := flag.String("direct-thresholds", "0.5,0.6,0.7,0.8", "직접 충분성 하한 후보. 쉼표로 나눈다")
	directMargins := flag.String("direct-margins", "0,0.02,0.05,0.1", "직접 후보 분리 폭 후보. 쉼표로 나눈다")
	consistency := flag.Bool("consistency", false, "Read Committed 검색과 동기화 스냅숏의 결과 동등성과 동시 쓰기 일관성을 견준다")
	consistencyRequests := flag.Int("consistency-requests", 100, "동시 쓰기 비교에서 구성마다 보낼 요청 수")
	consistencyLoad := flag.String("consistency-load", "1,8,16,32", "동시 요청 부하 비교의 동시 요청자 수 후보. 쉼표로 나눈다")
	consistencyLoadRequests := flag.Int("consistency-load-requests", 200, "동시 요청 부하 비교에서 조건마다 보낼 요청 수")
	consistencyPauses := flag.String("consistency-write-pauses", "0,50,250", "동시 쓰기 비교에서 쓰기 사이에 쉬는 밀리초 후보. 쉼표로 나눈다")
	consistencyStage := flag.String("consistency-stage", string(search.GraphStageReferences), "일관 읽기 비교의 그래프 관계 범위")
	adversarial := flag.Bool("adversarial", false, "관계·경로 오염 공격 표본을 공격 전후와 복구 후에 평가하고 홉별 첫 불일치를 진단한다")
	attacksPath := flag.String("attacks", "", "공격 표본 파일 경로. -adversarial의 입력이자 own-adversarial 생성의 출력이다")
	outPath := flag.String("out", "", "결과 JSON 경로. 비우면 표준 출력에 쓴다")
	flag.Parse()

	// 변환은 데이터베이스에 닿지 않는다. 같은 두 경로를 출력 자리로 쓰므로 변환한
	// 파일을 그대로 측정에 넘길 수 있다.
	if *convert != "" {
		if *contextsPath == "" || *queriesPath == "" {
			return fmt.Errorf("-contexts와 -queries가 필요하다")
		}
		return runConvert(*convert, *convertSource, *convertName, *convertLimit, *convertUseCase, *contextsPath, *queriesPath, *attacksPath)
	}
	if *businessPath != "" {
		return runBusiness(*businessPath, *outPath)
	}
	if *repeats < minimumRepeats {
		return fmt.Errorf("반복 회차는 %d 이상이어야 한다. 「검증」이 최소 세 번으로 정했다", minimumRepeats)
	}
	if *continual {
		return runContinual(*outPath, *repeats, *maxHops, *maxHopNodes)
	}
	if *contextsPath == "" || *queriesPath == "" {
		return fmt.Errorf("-contexts와 -queries가 필요하다")
	}
	stages, err := parseStages(*stageList)
	if err != nil {
		return err
	}
	var directs, margins []float64
	if *adaptive {
		// 국소 확장이 확장할 관계가 없는 baseline은 직접 경로와 같아져 비교가 성립하지 않는다.
		if !slices.Contains([]string{string(search.GraphStageReferences), string(search.GraphStageRelations), string(search.GraphStageGlobal)}, *adaptiveStage) {
			return fmt.Errorf("-adaptive-stage는 references, relations, global 중 하나여야 한다")
		}
		if directs, err = parseFloats(*directThresholds, "직접 충분성 하한"); err != nil {
			return err
		}
		if margins, err = parseFloats(*directMargins, "직접 후보 분리 폭"); err != nil {
			return err
		}
	}
	contexts, err := loadContextSet(*contextsPath)
	if err != nil {
		return err
	}
	queries, err := loadQuerySet(*queriesPath, contexts)
	if err != nil {
		return err
	}

	var attacks attackSet
	if *adversarial {
		if *attacksPath == "" {
			return fmt.Errorf("-adversarial에는 -attacks가 필요하다")
		}
		if attacks, err = loadAttackSet(*attacksPath, contexts, queries); err != nil {
			return err
		}
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
	if err := database.CheckEmbeddingSchema(ctx, settings.index.VectorType, settings.index.Dimension); err != nil {
		return err
	}
	worker, err := index.New(database, settings.index, nil, slog.Default())
	if err != nil {
		return fmt.Errorf("색인 작업자 준비: %w", err)
	}

	if *adversarial {
		return runAdversarialCommand(ctx, database, worker, settings, contexts, queries, attacks, adversarialConditions{
			IndexTargets: *indexTargets,
			Repeats:      *repeats, GraphStage: string(search.GraphStageRelations), GlobalFallback: true,
			Budget: *budget, MaxHops: *maxHops, MaxHopNodes: *maxHopNodes,
			Execution: string(settings.execution), CandidateLimit: settings.candidateLimit,
			SemanticThreshold: settings.semanticThreshold, FoldThreshold: settings.foldThreshold, EmbeddingModel: worker.ModelID(),
		}, *outPath)
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

	if *consistency {
		if !slices.Contains([]string{string(search.GraphStageReferences), string(search.GraphStageRelations), string(search.GraphStageGlobal)}, *consistencyStage) || *consistencyRequests < 1 {
			return fmt.Errorf("-consistency-stage는 references, relations, global 중 하나이고 -consistency-requests는 양수여야 한다")
		}
		var pauses []int
		for part := range strings.SplitSeq(*consistencyPauses, ",") {
			pause, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || pause < 0 {
				return fmt.Errorf("-consistency-write-pauses의 %q가 0 이상의 정수가 아니다", part)
			}
			pauses = append(pauses, pause)
		}
		var concurrency []int
		for part := range strings.SplitSeq(*consistencyLoad, ",") {
			value, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || value < 1 {
				return fmt.Errorf("-consistency-load의 %q가 양의 정수가 아니다", part)
			}
			concurrency = append(concurrency, value)
		}
		if *consistencyLoadRequests < 1 {
			return fmt.Errorf("-consistency-load-requests는 양수여야 한다")
		}
		result, err := runConsistency(ctx, database, worker, graph, contexts, queries, settings, consistencyConditions{
			IndexTargets: *indexTargets, FoldThreshold: settings.foldThreshold,
			Requests: *consistencyRequests, WritePausesMS: pauses, LoadConcurrency: concurrency, LoadRequests: *consistencyLoadRequests, ToggledContexts: consistencyToggled, GraphStage: *consistencyStage,
			Budget: *budget, MaxHops: *maxHops, MaxHopNodes: *maxHopNodes,
			CandidateLimit: settings.candidateLimit, SemanticThreshold: settings.semanticThreshold, EmbeddingModel: worker.ModelID(),
		})
		if err != nil {
			return err
		}
		return writeReport(*outPath, result)
	}
	if *adaptive {
		result, err := runAdaptive(ctx, database, worker, graph, contexts, queries, settings, adaptiveConditions{
			Repeats: *repeats, Budget: *budget, MaxHops: *maxHops, MaxHopNodes: *maxHopNodes,
			GraphStage: *adaptiveStage, EvidencePathSelection: *adaptiveEvidence,
			Execution: string(settings.execution), CandidateLimit: settings.candidateLimit,
			SemanticThreshold: settings.semanticThreshold, FoldThreshold: settings.foldThreshold,
			EmbeddingModel: worker.ModelID(), IndexTargets: *indexTargets,
		}, directs, margins)
		if err != nil {
			return err
		}
		return writeReport(*outPath, result)
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
	services := make(map[string]*search.Service, len(stages))
	for _, stage := range stages {
		graphStage, evidence := stageConfig(stage)
		service, err := search.New(database, worker, search.Config{
			Execution:             settings.execution,
			CandidateLimit:        settings.candidateLimit,
			SemanticThreshold:     settings.semanticThreshold,
			FoldThreshold:         settings.foldThreshold,
			GraphStage:            graphStage,
			GlobalFallback:        graphStage == search.GraphStageGlobal,
			EvidencePathSelection: evidence,
		}, slog.Default())
		if err != nil {
			return fmt.Errorf("단계 %q 검색 실행기 준비: %w", stage, err)
		}
		services[stage] = service
	}
	// 단계 순서를 회차마다 돌린다. 한 단계의 반복을 몰아 재면 뒤 단계만 데워진 캐시를 써서
	// 단계 사이 지연 차이에 실행 순서가 섞인다.
	for repeat := 1; repeat <= *repeats; repeat++ {
		for offset := range stages {
			stage := stages[(offset+repeat-1)%len(stages)]
			service := services[stage]
			started := time.Now()
			measured, err := measure(ctx, service, graph, queries, *budget, *maxHops, *maxHopNodes)
			if err != nil {
				return fmt.Errorf("단계 %q 회차 %d: %w", stage, repeat, err)
			}
			elapsed := time.Since(started)
			run := runMetrics{Stage: stage, Repeat: repeat, DurationMS: elapsed.Milliseconds(), UseCases: measured.UseCases, QuerySamples: measured.QuerySamples, recovered: measured.Recovered}
			runs[stage] = append(runs[stage], run)
			result.Runs = append(result.Runs, run)
			slog.Info("회차 완료", "stage", stage, "repeat", repeat, "duration", elapsed.String())
		}
	}
	result.Judgements = judge(stages, runs)
	result.Marginals = marginals(stages, runs)
	return writeReport(*outPath, result)
}

// runConvert는 공개 벤치마크를 데이터셋 두 파일로 옮기고 끝낸다.
func runConvert(format, source, name string, limit int, useCase, contextsPath, queriesPath, attacksPath string) error {
	var contexts contextSet
	var queries querySet
	var err error
	switch format {
	case convertOwnAdversarial:
		if name == "" || attacksPath == "" {
			return fmt.Errorf("-convert-name과 -attacks가 필요하다")
		}
		var attacks attackSet
		if contexts, queries, attacks, err = convertOwnAdversarialSet(name, max(limit, 12)); err != nil {
			return err
		}
		if err := writeDataset(attacksPath, attacks); err != nil {
			return err
		}
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
	loadedQueries, err := loadQuerySet(queriesPath, loaded)
	if err != nil {
		return err
	}
	if format == convertOwnAdversarial {
		if _, err := loadAttackSet(attacksPath, loaded, loadedQueries); err != nil {
			return err
		}
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

// stageEvidence는 「그래프 효과 비교」의 4단계다. 3단계와 같은 채널 후보에 근거 경로
// 보존 선택만 더하므로 그래프 단계는 global과 같다.
const stageEvidence = "evidence"

// stageConfig는 비교 단계 이름을 검색 구성의 그래프 단계와 근거 경로 선택 여부로 바꾼다.
func stageConfig(stage string) (search.GraphStage, bool) {
	if stage == stageEvidence {
		return search.GraphStageGlobal, true
	}
	return search.GraphStage(stage), false
}

func parseStages(raw string) ([]string, error) {
	known := []string{
		string(search.GraphStageBaseline),
		string(search.GraphStageReferences),
		string(search.GraphStageRelations),
		string(search.GraphStageGlobal),
		stageEvidence,
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
		Provider:   strings.TrimSpace(os.Getenv("EMBEDDING_PROVIDER")),
		APIKey:     strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		BaseURL:    baseURL,
		Model:      os.Getenv("EMBEDDING_MODEL"),
		VectorType: os.Getenv("EMBEDDING_VECTOR_TYPE"),
		Dimension:  dimension,
	}
	if err := loaded.index.Validate(); err != nil {
		return settings{}, err
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
func writeReport(path string, value any) error {
	raw, err := json.Marshal(value, json.FormatNilSliceAsNull(false), json.Deterministic(true))
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
