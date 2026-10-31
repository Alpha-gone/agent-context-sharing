package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

func adversarialTestConditions(t *testing.T) adversarialConditions {
	t.Helper()
	return adversarialConditions{Repeats: 1, GraphStage: "relations", GlobalFallback: true, Budget: 20000, MaxHops: 3, MaxHopNodes: 100, Execution: "sequential", CandidateLimit: 500, SemanticThreshold: 0.7, FoldThreshold: 0.95, IndexTargets: "all_layers"}
}

func TestAdversarialScenariosIntegration(t *testing.T) {
	database, worker, connection := evalDatabase(t, nil)
	contexts, queries, attacks := adversarialFixture(t)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	contexts.Version += "-" + id.String()
	queries.Version, attacks.Version = contexts.Version, contexts.Version
	conditions := adversarialTestConditions(t)
	service, err := search.New(database, worker, search.Config{Execution: search.ExecutionSequential, CandidateLimit: 500, SemanticThreshold: 0.7, FoldThreshold: 0.95, GraphStage: search.GraphStageRelations, GlobalFallback: true, Consistency: search.ConsistencySnapshot}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var dimension int
	if err := connection.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	expect := store.EmbeddingExpectation{ModelID: worker.ModelID(), Dimension: dimension}
	paths := map[string][]expectedPath{}
	for _, sample := range attacks.Samples {
		for _, path := range sample.ExpectedPaths {
			paths[path.QueryID] = append(paths[path.QueryID], path)
		}
	}
	baseline, err := runBaseline(t.Context(), database, worker, service, contexts, queries, attacks, conditions, expect, newBaseShape(contexts), paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range attacks.Samples {
		t.Run(sample.AttackType, func(t *testing.T) {
			runs := sampleRuns{sample: sample, targets: map[string][]targetRun{}}
			sampleQueries := queries
			sampleQueries.Queries = slices.DeleteFunc(slices.Clone(queries.Queries), func(query querySpec) bool {
				return query.ID != "adv-00" && !slices.Contains(sample.TargetQueryIDs, query.ID)
			})
			warnings, err := runAttackRepeat(t.Context(), database, worker, service, contexts, sampleQueries, sample, conditions, expect, newBaseShape(contexts), paths, baseline, &runs)
			if err != nil {
				t.Fatal(err)
			}
			if runs.isolationMismatches != 0 || runs.recoveryVerified != 1 || len(runs.targets) != len(sample.TargetQueryIDs) {
				t.Fatalf("독립 실행·기록: %+v", runs)
			}
			for id, series := range runs.targets {
				if len(series) != 1 || series[0].PreSuccess != 0 || series[0].Recovered != 1 {
					t.Fatalf("%s 전후·복구: %+v", id, series)
				}
			}
			seen := map[string]bool{}
			for _, warning := range warnings {
				if !slices.Contains([]string{warningMissingContext, warningUnexpectedContext, warningMissingRelation, warningUnexpectedRelation}, warning.Code) || seen[warning.PathID] {
					t.Fatalf("경로별 첫 경고: %+v", warnings)
				}
				seen[warning.PathID] = true
			}
		})
	}
	t.Run("isolation_mismatch_stops_before_attack", func(t *testing.T) {
		changed := map[string]response{}
		for id, r := range baseline {
			changed[id] = r
		}
		changed[queries.Queries[0].ID] = response{}
		runs := sampleRuns{targets: map[string][]targetRun{}}
		if _, err := runAttackRepeat(t.Context(), database, worker, service, contexts, queries, attacks.Samples[0], conditions, expect, newBaseShape(contexts), paths, changed, &runs); err == nil || !strings.Contains(err.Error(), "기준선과 다르다") {
			t.Fatalf("불일치: %v", err)
		}
		if runs.isolationMismatches != 1 || runs.recoveryVerified != 0 {
			t.Fatal("불일치 뒤 공격이 진행됐다")
		}
	})
	var count, active, illegal int
	if err := connection.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE deleted_at IS NULL) FROM public.context_graph WHERE name=$1`, "eval "+contexts.Version).Scan(&count, &active); err != nil {
		t.Fatal(err)
	}
	if count != 8 || active != 0 {
		t.Fatalf("그래프 격리·정리: %d %d", count, active)
	}
	if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM public.operation_log o JOIN public.context_graph g ON g.graph_id=o.graph_id WHERE g.name=$1 AND (o.result<>'applied' OR o.operation_kind NOT IN ('add','discard'))`, "eval "+contexts.Version).Scan(&illegal); err != nil {
		t.Fatal(err)
	}
	if illegal != 0 {
		t.Fatal("명시적 추가·폐기 외의 연산이 기록됐다")
	}
	var relationDiscards, nodeDiscards, confirms int
	if err := connection.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE judgment_input LIKE 'adversarial:recovery_discard:%' AND relation_id IS NOT NULL),count(*) FILTER (WHERE judgment_input LIKE 'adversarial:recovery_discard:%' AND context_id IS NOT NULL),count(*) FILTER (WHERE judgment_input LIKE 'adversarial:recovery_confirm:%') FROM public.operation_log o JOIN public.context_graph g ON g.graph_id=o.graph_id WHERE g.name=$1`, "eval "+contexts.Version).Scan(&relationDiscards, &nodeDiscards, &confirms); err != nil {
		t.Fatal(err)
	}
	if relationDiscards != 10 || nodeDiscards != 12 || confirms != 2 {
		t.Fatalf("명시 복구 기록: relations=%d nodes=%d confirms=%d", relationDiscards, nodeDiscards, confirms)
	}
}

// 공격 컨텍스트는 기준 컨텍스트보다 먼저 기록되어 시간 필터 후보가 기준 컨텍스트로 채워지면
// 적재 순서만으로 시간 채널 후보가 되지 않아야 한다.
func TestAdversarialAttackContextsDoNotLeadTimeChannelIntegration(t *testing.T) {
	database, worker, _ := evalDatabase(t, nil)
	contexts, _, attacks := adversarialFixture(t)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	contexts.Version += "-time-" + id.String()
	sample := attacks.Samples[slices.IndexFunc(attacks.Samples, func(sample attackSample) bool { return len(sample.AddContexts) > 0 })]
	err = withEvaluationGraph(t.Context(), database, worker, contexts, len(contexts.Contexts)*4+64, func(graph loadedGraph) error {
		if err := addAttackContexts(t.Context(), database, graph, sample.AddContexts); err != nil {
			return err
		}
		names := map[model.ID]string{}
		for key, id := range graph.Keys {
			names[id] = key
		}
		candidates, err := database.TimeCandidates(t.Context(), graph.GraphID, time.Now().UTC(), len(contexts.Contexts))
		if err != nil {
			return err
		}
		if len(candidates) != len(contexts.Contexts) {
			t.Fatalf("시간 후보 %d, 기준 컨텍스트 %d", len(candidates), len(contexts.Contexts))
		}
		for _, candidate := range candidates {
			if slices.ContainsFunc(sample.AddContexts, func(spec contextSpec) bool { return names[candidate.Context.ID] == spec.Key }) {
				t.Fatalf("공격 컨텍스트 %q가 기준 컨텍스트보다 앞선 시간 후보다", names[candidate.Context.ID])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdversarialPartialChannelFailureIntegration(t *testing.T) {
	database, worker, connection := evalDatabase(t, nil)
	contexts := lifecycleInput(t)
	err := withEvaluationGraph(t.Context(), database, worker, contexts, 32, func(graph loadedGraph) error {
		service, err := search.New(database, failingEvalWorker(t, database, connection, 400, 0), search.Config{Execution: search.ExecutionSequential, CandidateLimit: 50, FoldThreshold: 0.95, GraphStage: search.GraphStageRelations}, nil)
		if err != nil {
			return err
		}
		_, err = runQueries(t.Context(), service, graph, querySet{Queries: []querySpec{{ID: "q", WorkContext: "독립 원천"}}}, adversarialTestConditions(t))
		if err == nil || !strings.Contains(err.Error(), "검색 채널") {
			t.Fatalf("부분 채널 실패: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdversarialCommandIntegration(t *testing.T) {
	_, _, connection := evalDatabase(t, nil)
	contexts, queries, attacks := compactAdversarialFixture(t)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	contexts.Version += "-cli-" + id.String()
	queries.Version, attacks.Version = contexts.Version, contexts.Version
	if err := validateAttackSet(attacks, contexts, queries); err != nil {
		t.Fatal(err)
	}
	// evalDatabase의 표준 대역과 동일하게 상수 벡터를 반환하며 외부 호출은 없다.
	server, dimension := adversarialEmbeddingServer(t, connection)
	trustEvalEmbeddingServer(t, server)
	for key, value := range map[string]string{"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "AGE_GRAPH_NAME": "agent_context", "EMBEDDING_PROVIDER": "gemini", "EMBEDDING_BASE_URL": server.URL, "EMBEDDING_MODEL": "gemini-embedding-2", "EMBEDDING_VECTOR_TYPE": "vector", "EMBEDDING_DIMENSION": strconv.Itoa(dimension), "GEMINI_API_KEY": "synthetic-key", "SEARCH_CHANNEL_CANDIDATE_LIMIT": "500", "SEARCH_FOLD_SIMILARITY_THRESHOLD": "0.95", "EVAL_TEST_COMMAND_PROCESS": "1"} {
		t.Setenv(key, value)
	}
	dir := t.TempDir()
	for name, value := range map[string]any{"contexts": contexts, "queries": queries, "attacks": attacks} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "report.json")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-test.run=^TestEvalCommandProcess$", "--", "-adversarial", "-contexts="+filepath.Join(dir, "contexts.json"), "-queries="+filepath.Join(dir, "queries.json"), "-attacks="+filepath.Join(dir, "attacks.json"), "-repeat=10", "-budget=20000", "-max-hops=3", "-max-hop-nodes=100", "-out="+output)
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("실제 CLI: %v\n%s", err, raw)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var report adversarialReport
	if err := json.Unmarshal(raw, &report, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if report.Conditions.Repeats != 10 || report.Conditions.AsOf.IsZero() || report.Conditions.GraphStage != "relations" || len(report.Samples) != 6 || report.Samples[0].RecoveryVerified != 10 || report.Overall.Recovery.Value != 1 || report.Overall.AttackSuccess.Units != 1 || report.Overall.AttackSuccess.HalfWidth != -1 {
		t.Fatalf("보고서: %+v", report)
	}
	t.Logf("적대적 CLI 전체 지표: %+v", report.Overall)
	for _, sample := range report.Samples {
		if sample.RecoveryVerified != 10 || sample.IsolationMismatches != 0 || sample.Metrics.Recovery.Value != 1 {
			t.Fatalf("표본 복구: %+v", sample)
		}
	}
	if report.Overall.TargetQueries != 2 || report.Overall.TargetPairs != 7 || report.Overall.AffectedQueries != 2 {
		t.Fatalf("고유 질의 집계: %+v", report.Overall)
	}
	for _, q := range queries.Queries {
		if bytes.Contains(raw, []byte(q.WorkContext)) {
			t.Fatal("질의 노출")
		}
	}
	var allSpecs []contextSpec
	allSpecs = append(allSpecs, contexts.Contexts...)
	for _, sample := range attacks.Samples {
		allSpecs = append(allSpecs, sample.AddContexts...)
	}
	for _, s := range allSpecs {
		if bytes.Contains(raw, []byte(s.Body)) {
			t.Fatal("본문 노출")
		}
		if s.Source != nil && bytes.Contains(raw, []byte(s.Source.Locator)) {
			t.Fatal("원문 경로 노출")
		}
	}
	var count, active int
	if err := connection.QueryRow(context.WithoutCancel(t.Context()), `SELECT count(*),count(*) FILTER (WHERE deleted_at IS NULL) FROM public.context_graph WHERE name=$1`, "eval "+contexts.Version).Scan(&count, &active); err != nil {
		t.Fatal(err)
	}
	if count != 70 || active != 0 {
		t.Fatalf("10회 기준선·공격 그래프: %d %d", count, active)
	}
}

func adversarialEmbeddingServer(t *testing.T, connection *pgx.Conn) (*httptest.Server, int) {
	t.Helper()
	var dimension int
	if err := connection.QueryRow(t.Context(), `SELECT substring(format_type(atttypid,atttypmod) FROM '[0-9]+')::int FROM pg_attribute WHERE attrelid='public.context_embedding'::regclass AND attname='embedding'`).Scan(&dimension); err != nil {
		t.Fatal(err)
	}
	vector := make([]float64, dimension)
	vector[0] = 1
	raw, err := json.Marshal(map[string]any{"embedding": map[string]any{"values": vector}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	return server, dimension
}

// 여섯 유형의 성립 조건을 유지하고 같은 질의가 여러 공격에 등장하도록 구성한다.
// 큰 공개 품질 표본 대신 CLI의 10회·고유 집계·복구·정리를 인수하는 작은 합성 입력이다.
func compactAdversarialFixture(t *testing.T) (contextSet, querySet, attackSet) {
	t.Helper()
	contexts, queries, attacks := adversarialFixture(t)
	keep := []string{"hub-s", "hub", "a01-sa", "a01-sb", "a01-a", "a01-b", "a02-sb", "a02-b"}
	contexts.Contexts = slices.DeleteFunc(contexts.Contexts, func(s contextSpec) bool { return !slices.Contains(keep, s.Key) })
	normal := relationSpec{Type: "causes", From: "a01-a", To: "a01-b"}
	contexts.Relations = []relationSpec{normal, {Type: "relates_to", From: "hub", To: "a01-a"}, {Type: "relates_to", From: "hub", To: "a01-b"}, {Type: "relates_to", From: "hub", To: "a02-b"}}
	queries.Queries = []querySpec{
		{ID: "control", IndependentUnit: "control", UseCase: useCaseAssociative, WorkContext: "정상 회의 결과를 확인한다.", Answers: []string{"a02-b"}},
		{ID: "target1", IndependentUnit: "same-task", UseCase: useCaseAssociative, WorkContext: "표식 Q01 회차 뒤의 결과", Answers: []string{"a01-b"}},
		{ID: "target2", IndependentUnit: "same-task", UseCase: useCaseAssociative, WorkContext: "표식 Q01 이후의 회차", Answers: []string{"a01-b"}},
	}
	for i := range attacks.Samples {
		sample := &attacks.Samples[i]
		sample.TargetQueryIDs = []string{"target1"}
		var returns, omits []attackRef
		switch sample.AttackType {
		case attackInjection:
			returns = []attackRef{{Key: "x1"}}
		case attackShared:
			sample.TargetQueryIDs = append(sample.TargetQueryIDs, "target2")
			sample.AddRelations = []relationSpec{{Type: "relates_to", From: "a01-a", To: "a01-b"}}
			returns = []attackRef{{Relation: &sample.AddRelations[0]}}
		case attackHub:
			sample.DiscardRelations = []relationSpec{{Type: "relates_to", From: "hub", To: "a01-a"}}
			sample.AddRelations = []relationSpec{{Type: "relates_to", From: "a02-b", To: "a01-a"}}
			returns, omits = []attackRef{{Relation: &sample.AddRelations[0]}}, []attackRef{{Key: "hub"}}
		case attackBridge:
			sample.DiscardRelations = []relationSpec{normal}
			sample.AddRelations = []relationSpec{{Type: "causes", From: "a01-a", To: "a02-b"}}
			returns, omits = []attackRef{{Relation: &sample.AddRelations[0]}}, []attackRef{{Key: "a01-b"}}
		case attackChain:
			sample.AddRelations[0].From = "a01-a"
			returns = []attackRef{{Key: "f3"}}
		case attackMutual:
			sample.AddRelations[0].From = "a01-a"
			sample.AddRelations[2].To = "a01-a"
			returns = []attackRef{{Key: "m1"}, {Key: "m2"}}
		}
		sample.ContaminatedRefs = nil
		for _, spec := range sample.AddContexts {
			sample.ContaminatedRefs = append(sample.ContaminatedRefs, attackRef{Key: spec.Key})
		}
		for _, relation := range sample.AddRelations {
			sample.ContaminatedRefs = append(sample.ContaminatedRefs, attackRef{Relation: &relation})
		}
		sample.SuccessCriteria = nil
		sample.ExpectedPaths = nil
		for _, id := range sample.TargetQueryIDs {
			sample.SuccessCriteria = append(sample.SuccessCriteria, successCriteria{QueryID: id, MustReturn: returns, MustOmit: omits})
			sample.ExpectedPaths = append(sample.ExpectedPaths, expectedPath{PathID: sample.SampleID + "-" + id, QueryID: id, Steps: []pathStep{{Key: "a01-a"}, {Relation: &normal}, {Key: "a01-b"}}})
		}
	}
	if err := validateAttackSet(attacks, contexts, queries); err != nil {
		t.Fatal(err)
	}
	return contexts, queries, attacks
}

func TestAdversarialRepeatBoundary(t *testing.T) {
	for _, repeat := range []int{0, 1, 9, 11} {
		t.Run(strconv.Itoa(repeat), func(t *testing.T) {
			_, err := runAdversarial(t.Context(), nil, nil, nil, contextSet{}, querySet{}, attackSet{}, adversarialConditions{Repeats: repeat}, store.EmbeddingExpectation{})
			if err == nil || !strings.Contains(err.Error(), "10회") {
				t.Fatalf("반복 경계: %v", err)
			}
		})
	}
}
