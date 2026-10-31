// 평가 실행기. 컨텍스트 집합과 질의 집합을 받아 비교 단계마다 반복 측정하고
// 기준선 대비 개선폭과 유의성으로 판정한다.
//
// 이 도구는 「패키지 경계」가 정한 열 개 업무 패키지 밖에 두며 마이그레이션 실행기와
// 같은 자리다. 실행 중인 서버를 거치지 않고 검색 실행기를 직접 만드는 이유는 비교
// 단계 전환이 배포 구성 값이라, 서버를 거치면 단계마다 재기동해야 하기 때문이다.
// 같은 프로세스에서 단계만 바꿔 돌아야 입력·표현·예산이 통제된 상태로 남는다.
package main

import (
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
	InputPreparation *preparationReport     `json:"input_preparation,omitzero"`
	StartedAt        time.Time              `json:"started_at"`
	ContextVersion   string                 `json:"context_dataset_version"`
	QueryVersion     string                 `json:"query_dataset_version"`
	Conditions       conditions             `json:"conditions"`
	Runs             []runMetrics           `json:"runs"`
	Judgements       []stageJudgement       `json:"judgements"`
	Marginals        []marginal             `json:"marginal_contributions"`
	Assessments      []comparisonAssessment `json:"activation_assessments,omitempty"`
}

// conditions는 단계를 제외하고 회차마다 고정한 통제 조건이다.
type conditions struct {
	GraphGroups       int                 `json:"measured_graph_groups,omitzero"`
	ComparisonAxis    string              `json:"comparison_axis,omitempty"`
	Variants          []comparisonVariant `json:"variants,omitempty"`
	PreparationPolicy string              `json:"preparation_policy,omitempty"`
	AsOf              time.Time           `json:"default_as_of,omitzero"`
	Stages            []string            `json:"stages"`
	Repeats           int                 `json:"repeats"`
	Budget            int                 `json:"budget"`
	MaxHops           int                 `json:"max_hops"`
	MaxHopNodes       int                 `json:"max_hop_nodes"`
	Execution         string              `json:"channel_execution"`
	CandidateLimit    int                 `json:"channel_candidate_limit"`
	SemanticThreshold float64             `json:"semantic_similarity_threshold"`
	FoldThreshold     float64             `json:"fold_threshold"`
	EmbeddingModel    string              `json:"embedding_model"`
	IndexTargets      string              `json:"index_targets"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("평가 종료", "error", err)
		os.Exit(1)
	}
}

func run() error { return runEvaluation(os.Args[1:], nil) }

func runEvaluation(args []string, trial *trialProgress) (err error) {
	flags := flag.NewFlagSet("eval", flag.ContinueOnError)
	manifestRun := flags.String("manifest", "", "manifest와 실제 입력을 대조해 결과 묶음을 새 디렉터리에 저장한다")
	results := flags.String("results", "", "manifest 결과를 저장할 루트 디렉터리")
	var bindings trialBindings
	flags.Var(&bindings, "trial-input", "manifest input code=로컬 파일. 여러 번 지정할 수 있다")
	trialPreparedDirectory := flags.String("trial-prepared", "", "manifest와 대조할 준비 데이터 디렉터리")
	defaultLimits := plan.Default()
	manifestPath := flags.String("check-manifest", "", "실행 manifest만 검증한다. 데이터 읽기·DB 접속·평가는 실행하지 않는다")
	prepareFormat := flags.String("prepare-data", "", "고정 공개 데이터 또는 자체 보완 세트를 준비한다. longmemeval, locomo, own-supplement")
	prepareSource := flags.String("prepare-source", "", "고정 checksum을 확인할 로컬 원본 파일")
	prepareOutput := flags.String("prepare-out", "", "준비 파일을 생성할 새 디렉터리")
	preparedInput := flags.String("prepared-input", "", "통제 비교에 사용할 EVAL-02 준비 디렉터리")
	contextsPath := flags.String("contexts", "", "컨텍스트 집합 파일 경로")
	queriesPath := flags.String("queries", "", "질의 집합 파일 경로")
	stageList := flags.String("stages", "baseline,references,relations,global,evidence", "누적 비교 단계를 순서대로 지정한다")
	repeats := flags.Int("repeat", initialEvaluationRepeats, "조건별 대응 반복 회차 수. 초기 평가는 10회다")
	budget := flags.Int("budget", 4000, "검색 예산 문자 수. 0은 한도 없음이다")
	maxHops := flags.Int("max-hops", defaultLimits.MaxHops, "그래프 확장 최대 홉 수")
	maxHopNodes := flags.Int("max-hop-nodes", defaultLimits.MaxHopNodes, "그래프 확장 최대 노드 수")
	compareIndex := flags.Bool("compare-index-targets", false, "고정 그래프 단계에서 all_layers와 without_source를 별도로 비교한다")
	indexTargets := flags.String("index-targets", string(store.IndexTargetsAllLayers), "색인 대상 계층. all_layers 또는 without_source")
	convert := flags.String("convert", "", "데이터셋을 변환하거나 생성한다. hipporag, own, own-global, own-global-auto 또는 own-adversarial")
	convertSource := flags.String("convert-source", "", "변환할 벤치마크 파일 경로")
	convertName := flags.String("convert-name", "", "데이터셋 판에 넣을 세트 이름")
	convertLimit := flags.Int("convert-questions", 0, "변환에 쓸 질의 수. 0이면 전부 쓴다")
	convertUseCase := flags.String("convert-use-case", "", "변환 결과의 질의를 fact, associative 또는 global 사용 사례로 제한한다")
	continual := flags.Bool("continual", false, "온라인 갱신부터 상충 해소까지 여섯 지속 평가 시나리오를 실행한다")
	businessPath := flags.String("business", "", "업무 효과 통제 과업의 집계 지표 파일 경로")
	businessTasksPath := flags.String("business-tasks", "", "비식별 과업 trace의 성공률·토큰·총비용·축소 통제 비교 입력")
	unitPath := flags.String("unit-comparison", "", "원천을 공유하는 두 파생 경계 정책의 완전성·검색·과업 비교 입력")
	embeddingPath := flags.String("embedding-comparison", "", "임베딩 한 축의 순위·관계/전역 임계값 통제 비교 입력")
	originalPath := flags.String("original-reproduction", "", "수집 당시 원본과 승인한 HTTPS 출처의 판·범위·내용을 비교하는 입력")
	adaptive := flags.Bool("adaptive", false, "기존 auto와 질의 적응형 라우팅을 세 검색 경로의 오프라인 최적 경로로 견준다")
	adaptiveStage := flags.String("adaptive-stage", string(search.GraphStageRelations), "적응형 비교의 그래프 관계 범위. baseline은 쓸 수 없다")
	adaptiveEvidence := flags.Bool("adaptive-evidence", false, "적응형 비교의 두 구성에 근거 경로 보존 선택을 켠다")
	directThresholds := flags.String("direct-thresholds", "0.5,0.6,0.7,0.8", "직접 충분성 하한 후보. 쉼표로 나눈다")
	directMargins := flags.String("direct-margins", "0,0.02,0.05,0.1", "직접 후보 분리 폭 후보. 쉼표로 나눈다")
	consistency := flags.Bool("consistency", false, "Read Committed·동기화 스냅숏·평가 전용 내용 판 재시도의 동등성과 동시 쓰기를 견준다")
	consistencyRequests := flags.Int("consistency-requests", 100, "동시 쓰기 비교에서 구성마다 보낼 요청 수")
	consistencyLoad := flags.String("consistency-load", "1,8,16,32", "동시 요청 부하 비교의 동시 요청자 수 후보. 쉼표로 나눈다")
	consistencyLoadRequests := flags.Int("consistency-load-requests", 200, "동시 요청 부하 비교에서 조건마다 보낼 요청 수")
	consistencyPauses := flags.String("consistency-write-pauses", "0,50,250", "동시 쓰기 비교에서 쓰기 사이에 쉬는 밀리초 후보. 쉼표로 나눈다")
	consistencyStage := flags.String("consistency-stage", string(search.GraphStageReferences), "일관 읽기 비교의 그래프 관계 범위")
	adversarial := flags.Bool("adversarial", false, "관계·경로 오염 공격 표본을 공격 전후와 복구 후에 평가하고 홉별 첫 불일치를 진단한다")
	attacksPath := flags.String("attacks", "", "공격 표본 파일 경로. -adversarial의 입력이자 own-adversarial 생성의 출력이다")
	outPath := flags.String("out", "", "결과 JSON 경로. 비우면 표준 출력에 쓴다")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("위치 인자는 허용하지 않는다")
	}
	trialRequested := false
	flags.Visit(func(f *flag.Flag) {
		trialRequested = trialRequested || slices.Contains([]string{"manifest", "results", "trial-input", "trial-prepared"}, f.Name)
	})
	if trialRequested {
		conflict := false
		flags.Visit(func(f *flag.Flag) {
			conflict = conflict || !slices.Contains([]string{"manifest", "results", "trial-input", "trial-prepared"}, f.Name)
		})
		if conflict || *manifestRun == "" || *results == "" {
			return fmt.Errorf("manifest 실행에는 manifest·results·trial-input·trial-prepared만 지정할 수 있다")
		}
		return runManifestTrial(*manifestRun, *results, bindings, *trialPreparedDirectory)
	}
	manifestRequested := false
	flags.Visit(func(f *flag.Flag) { manifestRequested = manifestRequested || f.Name == "check-manifest" })
	if manifestRequested {
		conflict := false
		flags.Visit(func(f *flag.Flag) { conflict = conflict || f.Name != "check-manifest" })
		if conflict || *manifestPath == "" {
			return fmt.Errorf("manifest 검사 모드는 다른 실행 옵션과 함께 지정할 수 없다")
		}
		manifest, err := loadManifest(*manifestPath)
		if err != nil {
			return err
		}
		slog.Info("manifest 검증 완료", "trial_id", manifest.TrialID, "mode", manifest.Mode)
		return nil
	}
	prepareRequested := false
	prepareConflict := false
	flags.Visit(func(f *flag.Flag) {
		if strings.HasPrefix(f.Name, "prepare-") {
			prepareRequested = true
		} else {
			prepareConflict = true
		}
	})
	if prepareRequested {
		if prepareConflict || *prepareFormat == "" {
			return fmt.Errorf("데이터 준비 모드는 다른 실행 옵션과 함께 지정할 수 없다")
		}
		return runPrepareData(*prepareFormat, *prepareSource, *prepareOutput)
	}
	var modes []string
	for _, mode := range []struct {
		name    string
		enabled bool
	}{
		{"compare-index-targets", *compareIndex}, {"convert", *convert != ""}, {"business", *businessPath != ""}, {"continual", *continual},
		{"adaptive", *adaptive}, {"consistency", *consistency}, {"adversarial", *adversarial},
		{"unit-comparison", *unitPath != ""},
		{"original-reproduction", *originalPath != ""},
		{"embedding-comparison", *embeddingPath != ""},
		{"business-tasks", *businessTasksPath != ""},
	} {
		if mode.enabled {
			modes = append(modes, mode.name)
		}
	}
	if *preparedInput != "" && ((*contextsPath != "" || *queriesPath != "") || (len(modes) > 0 && !*compareIndex)) {
		return fmt.Errorf("prepared-input은 직접 contexts/queries 없이 통제 비교에만 지정할 수 있다")
	}
	if len(modes) > 1 {
		return fmt.Errorf("평가 모드는 하나만 지정해야 한다: %s", strings.Join(modes, ", "))
	}
	if err := validateModeFlagsFor(flags, *convert, *adaptive, *consistency, *adversarial); err != nil {
		return err
	}
	if *budget < 0 || *maxHops < 0 || *maxHopNodes < 0 || !slices.Contains([]string{string(store.IndexTargetsAllLayers), string(store.IndexTargetsWithoutSource)}, *indexTargets) {
		return fmt.Errorf("문자·홉·노드 예산 또는 색인 대상이 올바르지 않다")
	}
	var consistencyOptions manifestConsistency
	if *consistency {
		candidateLimit, err := strconv.Atoi(strings.TrimSpace(envOr("SEARCH_CHANNEL_CANDIDATE_LIMIT", "50")))
		if err != nil {
			return fmt.Errorf("SEARCH_CHANNEL_CANDIDATE_LIMIT 해석: %w", err)
		}
		concurrency, err := parseIntegerOptions(*consistencyLoad, 1)
		if err != nil {
			return err
		}
		pauses, err := parseIntegerOptions(*consistencyPauses, 0)
		if err != nil {
			return err
		}
		consistencyOptions = manifestConsistency{Stage: *consistencyStage, Requests: *consistencyRequests, LoadRequests: *consistencyLoadRequests, Concurrency: concurrency, WritePauses: pauses}
		if err := validateConsistencyOptions(consistencyOptions, candidateLimit); err != nil {
			return err
		}
	}

	// 변환은 데이터베이스에 닿지 않는다. 같은 두 경로를 출력 자리로 쓰므로 변환한
	// 파일을 그대로 측정에 넘길 수 있다.
	if *convert != "" {
		if *contextsPath == "" || *queriesPath == "" {
			return fmt.Errorf("-contexts와 -queries가 필요하다")
		}
		return runConvert(*convert, *convertSource, *convertName, *convertLimit, *convertUseCase, *contextsPath, *queriesPath, *attacksPath)
	}
	if *businessTasksPath != "" {
		conflict := false
		flags.Visit(func(f *flag.Flag) { conflict = conflict || !slices.Contains([]string{"business-tasks", "out"}, f.Name) })
		if conflict {
			return fmt.Errorf("업무 과업 비교는 입력에 고정한 통제 조건만 사용한다")
		}
		return runBusinessTasks(*businessTasksPath, *outPath)
	}
	if *businessPath != "" {
		conflict := false
		flags.Visit(func(f *flag.Flag) { conflict = conflict || !slices.Contains([]string{"business", "out"}, f.Name) })
		if conflict {
			return fmt.Errorf("업무 효과 비교는 입력에 고정한 통제 조건만 사용한다")
		}
		return runBusiness(*businessPath, *outPath)
	}
	if *repeats != initialEvaluationRepeats {
		return fmt.Errorf("초기 평가의 조건별 반복 회차는 %d여야 한다", initialEvaluationRepeats)
	}
	if *embeddingPath != "" {
		conflict := false
		flags.Visit(func(f *flag.Flag) {
			conflict = conflict || !slices.Contains([]string{"embedding-comparison", "out", "repeat"}, f.Name)
		})
		if conflict {
			return fmt.Errorf("임베딩 비교는 입력에 고정한 통제 조건만 사용한다")
		}
		ctx, stop := trialContext(trial)
		defer stop()
		return runEmbeddingCommand(ctx, *embeddingPath, *outPath)
	}
	if *originalPath != "" {
		conflict := false
		flags.Visit(func(f *flag.Flag) {
			conflict = conflict || !slices.Contains([]string{"original-reproduction", "out", "repeat"}, f.Name)
		})
		if conflict {
			return fmt.Errorf("원본 비교는 입력에 고정한 출처·요청 설정만 사용한다")
		}
		ctx, stop := trialContext(trial)
		defer stop()
		return runOriginalCommand(ctx, *originalPath, *outPath)
	}
	if *unitPath != "" {
		conflict := false
		flags.Visit(func(f *flag.Flag) {
			conflict = conflict || !slices.Contains([]string{"unit-comparison", "out", "repeat"}, f.Name)
		})
		if conflict {
			return fmt.Errorf("검색 단위 비교는 입력에 고정한 통제 조건만 사용한다")
		}
		input, err := loadUnitInput(*unitPath)
		if err != nil {
			return err
		}
		settings, err := loadSettings()
		if err != nil {
			return err
		}
		ctx, stop := trialContext(trial)
		defer stop()
		return runUnitCommand(ctx, settings, input, *outPath)
	}
	if *continual {
		// 지속 평가는 내장 시나리오의 그래프·단계·색인 대상과 고정 예산을 쓴다.
		conflict := false
		flags.Visit(func(f *flag.Flag) {
			conflict = conflict || !slices.Contains([]string{"continual", "out", "repeat", "max-hops", "max-hop-nodes"}, f.Name)
		})
		if conflict {
			return fmt.Errorf("지속 평가는 내장 시나리오와 고정 검색 예산만 사용한다")
		}
		ctx, stop := trialContext(trial)
		defer stop()
		return runContinual(ctx, *outPath, *repeats, *maxHops, *maxHopNodes)
	}
	if *preparedInput == "" && (*contextsPath == "" || *queriesPath == "") {
		return fmt.Errorf("-contexts와 -queries가 필요하다")
	}
	if *adversarial && *attacksPath == "" {
		return fmt.Errorf("-adversarial에는 -attacks가 필요하다")
	}
	stages, err := parseStages(*stageList)
	if err != nil {
		return err
	}
	if !*adaptive && !*consistency && !*adversarial {
		if _, err := comparisonVariants(stages, *indexTargets, *compareIndex); err != nil {
			return err
		}
		if *compareIndex {
			var explicitStages, explicitTargets bool
			flags.Visit(func(f *flag.Flag) {
				explicitStages = explicitStages || f.Name == "stages"
				explicitTargets = explicitTargets || f.Name == "index-targets"
			})
			if !explicitStages || explicitTargets {
				return fmt.Errorf("색인 대상 비교에는 -stages의 고정 단계 하나가 필요하며 -index-targets를 함께 지정할 수 없다")
			}
		}
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
	var contexts contextSet
	var queries querySet
	var prepared comparisonInput
	if *preparedInput != "" {
		prepared, err = loadPreparedComparison(*preparedInput)
		if err != nil {
			return err
		}
	} else {
		contexts, err = loadContextSet(*contextsPath)
		if err != nil {
			return err
		}
		queries, err = loadQuerySet(*queriesPath, contexts)
		if err != nil {
			return err
		}
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
	if *consistency {
		if err := validateConsistencyCandidateLimit(settings.candidateLimit); err != nil {
			return err
		}
	}
	ctx, stop := trialContext(trial)
	defer stop()
	if !*adaptive && !*consistency && !*adversarial {
		if *preparedInput != "" {
			return runControlledInput(ctx, settings, prepared, controlledOptions{Stages: stages, IndexTargets: *indexTargets, CompareIndex: *compareIndex, Repeats: *repeats, Budget: *budget, MaxHops: *maxHops, MaxHopNodes: *maxHopNodes}, *outPath)
		}
		return runControlledCommand(ctx, settings, contexts, queries, controlledOptions{
			Stages: stages, IndexTargets: *indexTargets, CompareIndex: *compareIndex,
			Repeats: *repeats, Budget: *budget, MaxHops: *maxHops, MaxHopNodes: *maxHopNodes,
		}, *outPath)
	}
	// 관계 후보 제안과 유예 기간을 주지 않는다. 제안은 확정되기 전까지 그래프 경로
	// 채널에 쓰이지 않으므로 측정에 들어가지 않고, 끄면 회차마다 같은 그래프 상태에서
	// 재는 것이 보장된다. 유예 판정은 주기 작업의 몫이며 평가는 그 경로를 타지 않는다.
	// 색인 대상 계층은 적재 시점의 등록 조건이므로 그래프를 만들기 전에 정해야 한다.
	// 색인 대상 통제 비교는 위의 별도 실행 경로가 조건별 저장소를 만든다.
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
	if *consistency {
		var result consistencyReport
		err := withEvaluationGraph(ctx, database, worker, contexts, len(contexts.Contexts)*4+64, func(graph loadedGraph) error {
			var measureErr error
			result, measureErr = runConsistency(ctx, database, worker, graph, contexts, queries, settings, consistencyConditions{
				IndexTargets: *indexTargets, FoldThreshold: settings.foldThreshold,
				Requests: consistencyOptions.Requests, WritePausesMS: consistencyOptions.WritePauses, LoadConcurrency: consistencyOptions.Concurrency, LoadRequests: consistencyOptions.LoadRequests, ToggledContexts: consistencyToggled, GraphStage: consistencyOptions.Stage,
				Budget: *budget, MaxHops: *maxHops, MaxHopNodes: *maxHopNodes,
				CandidateLimit: settings.candidateLimit, SemanticThreshold: settings.semanticThreshold, EmbeddingModel: worker.ModelID(),
			})
			return measureErr
		})
		if err != nil {
			return err
		}
		return writeReport(*outPath, result)
	}
	if *adaptive {
		result, err := runAdaptive(ctx, database, worker, contexts, queries, settings, adaptiveConditions{
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
	return fmt.Errorf("평가 모드 실행 경로가 없다")
}

// runConvert는 공개 벤치마크를 데이터셋 두 파일로 옮기고 끝낸다.
func runConvert(format, source, name string, limit int, useCase, contextsPath, queriesPath, attacksPath string) error {
	if err := validateConversion(format, useCase); err != nil {
		return err
	}
	if limit < 0 {
		return fmt.Errorf("변환 질의 수는 0 이상이어야 한다")
	}
	outputs := []string{contextsPath, queriesPath}
	if format == convertOwnAdversarial {
		outputs = append(outputs, attacksPath)
	}
	if err := distinctOutputPaths(outputs); err != nil {
		return err
	}
	var contexts contextSet
	var queries querySet
	var attacks attackSet
	var err error
	switch format {
	case convertOwnAdversarial:
		if name == "" || attacksPath == "" {
			return fmt.Errorf("-convert-name과 -attacks가 필요하다")
		}
		if contexts, queries, attacks, err = convertOwnAdversarialSet(name, max(limit, 12)); err != nil {
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
	// 기존 출력은 모든 검증을 통과한 뒤에만 바꾼다. 메모리 검증으로 중복 식별자 등을 먼저
	// 거르고, 같은 디렉터리의 임시 파일에 써서 다시 읽은 결과까지 통과해야 교체한다.
	if _, err := validateContextSet(contexts, contextsPath); err != nil {
		return err
	}
	if _, err := validateQuerySet(queries, contexts, queriesPath); err != nil {
		return err
	}
	values := []any{contexts, queries}
	if format == convertOwnAdversarial {
		if err := validateAttackSet(attacks, contexts, queries); err != nil {
			return err
		}
		values = append(values, attacks)
	}
	staged := make([]string, 0, len(outputs))
	defer func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
	}()
	for index, path := range outputs {
		temporary, err := stageDataset(path, values[index])
		if err != nil {
			return err
		}
		staged = append(staged, temporary)
	}
	// 만든 파일을 바로 읽어 적재 전 검증을 통과하는지 확인한다. 변환이 형식을 어기면
	// 측정 단계가 아니라 여기에서 드러나야 한다.
	loaded, err := loadContextSet(staged[0])
	if err != nil {
		return err
	}
	loadedQueries, err := loadQuerySet(staged[1], loaded)
	if err != nil {
		return err
	}
	if format == convertOwnAdversarial {
		if _, err := loadAttackSet(staged[2], loaded, loadedQueries); err != nil {
			return err
		}
	}
	for index, path := range outputs {
		if err := os.Rename(staged[index], path); err != nil {
			return fmt.Errorf("데이터셋 교체: %w", err)
		}
	}
	staged = nil
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
		return settings{}, fmt.Errorf("EMBEDDING_BASE_URL이 올바른 HTTP 주소가 아니다")
	}
	dimension, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EMBEDDING_DIMENSION")))
	if err != nil {
		return settings{}, fmt.Errorf("EMBEDDING_DIMENSION이 양의 정수가 아니다")
	}
	loaded.index = index.Config{
		Provider:   strings.TrimSpace(os.Getenv("EMBEDDING_PROVIDER")),
		APIKey:     os.Getenv("GEMINI_API_KEY"),
		BaseURL:    baseURL,
		Model:      strings.TrimSpace(os.Getenv("EMBEDDING_MODEL")),
		VectorType: strings.TrimSpace(os.Getenv("EMBEDDING_VECTOR_TYPE")),
		Dimension:  dimension,
	}
	if err := loaded.index.Validate(); err != nil {
		return settings{}, err
	}
	loaded.index.APIKey = strings.TrimSpace(loaded.index.APIKey)
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
