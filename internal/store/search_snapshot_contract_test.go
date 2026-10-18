package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
)

func TestSearchSnapshotSixReadsIntegration(t *testing.T) {
	database, writer := newSmallPoolStore(t), newIntegrationStore(t)
	actor := newTestID(t)
	createTestAccount(t, writer, actor)
	graph := createTestGraph(t, writer, actor)
	source := testSourceContext(t, graph, actor, "api://search01/"+graph.String())
	source.Body = "searchone snapshotboundary"
	source, err := writer.CreateContext(t.Context(), graph, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	createDerived := func(global bool) model.Context {
		t.Helper()
		value := testDerivedContext(t, graph, actor)
		value.Body = source.Body
		if global {
			value.Derived.Kind, value.Derived.SummaryScope = model.DerivationKindSummary, model.SummaryScopeGlobal
		}
		stored, err := writer.CreateContext(t.Context(), graph, value, []model.ID{source.ID})
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	oldSummary := createDerived(true)
	vector := make([]float64, embeddingDimension(t, writer))
	vector[0] = 1
	indexAll := func() {
		t.Helper()
		for {
			result, err := writer.ProcessNextIndexTaskInGraph(t.Context(), graph, func(context.Context, IndexTask) IndexTaskResult {
				return IndexTaskResult{Embedding: vector, ModelID: "search01"}
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Found {
				return
			}
			if !result.Succeeded {
				t.Fatalf("색인 실패: %+v", result)
			}
		}
	}
	indexAll()
	var later model.Context
	stats, err := database.WithReadSnapshot(t.Context(), func(ctx context.Context) error {
		// 다른 풀의 명시적 커밋으로 조정 스냅숏과 이후 상태를 분리한다.
		later = createDerived(false)
		createDerived(true)
		indexAll()
		read, finish, err := database.enterReadScope(ctx)
		if err != nil {
			return err
		}
		var isolation, readonly string
		err = database.reader(read).QueryRow(read, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).Scan(&isolation, &readonly)
		err = errors.Join(err, finish())
		if err != nil {
			return err
		}
		if isolation != "repeatable read" || readonly != "on" {
			return fmt.Errorf("읽기 모드: %s %s", isolation, readonly)
		}
		for _, snapshot := range []bool{true, false} {
			readContext := t.Context()
			candidateCount, summaryCount, hopCount := 2, 2, 4
			if snapshot {
				readContext = ctx
				candidateCount, summaryCount, hopCount = 1, 1, 2
			}
			at := time.Now().UTC()
			for _, channel := range []string{"keyword", "time", "semantic", "global"} {
				var values []model.SearchCandidate
				switch channel {
				case "keyword":
					values, err = database.KeywordCandidates(readContext, graph, "searchone", at, 10)
				case "time":
					values, err = database.TimeCandidates(readContext, graph, at, 10)
				case "semantic":
					values, err = database.SemanticCandidates(readContext, graph, "search01", vector, at, 10)
				case "global":
					values, err = database.GlobalSummaryCandidates(readContext, graph, at, 10)
				}
				want := candidateCount
				if channel == "global" {
					want = summaryCount
				}
				if err != nil || len(values) != want {
					return fmt.Errorf("snapshot=%t %s: count=%d want=%d: %w", snapshot, channel, len(values), want, err)
				}
				if snapshot && channel != "global" && values[0].Context.ID != source.ID {
					return errors.New("후보가 뒤 커밋을 읽었다")
				}
				if snapshot && channel == "global" && values[0].Context.ID != oldSummary.ID {
					return errors.New("전역 요약이 뒤 커밋을 읽었다")
				}
			}
			hops, err := database.SearchHopContextsFrom(readContext, graph, []model.Context{source}, 1, "both", nil, 0)
			if err != nil || len(hops.Contexts) != hopCount {
				return fmt.Errorf("홉 snapshot=%t count=%d: %w", snapshot, len(hops.Contexts), err)
			}
			origins, err := database.ContextOriginKinds(readContext, graph, []model.ID{source.ID, oldSummary.ID})
			if err != nil || len(origins) != 2 {
				return fmt.Errorf("출처 snapshot=%t: %w", snapshot, err)
			}
			_, err = database.ContextOriginKinds(readContext, graph, []model.ID{later.ID})
			if snapshot && !errors.Is(err, ErrNotFound) || !snapshot && err != nil {
				return fmt.Errorf("새 노드 출처 snapshot=%t: %w", snapshot, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Connections != 9 || stats.PeakConnections != 2 || stats.Hold <= 0 {
		t.Fatalf("읽기 집계: %+v", stats)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

type searchWaitingEmbedder struct{ entered chan struct{} }

func (provider searchWaitingEmbedder) Embed(ctx context.Context, _ string) ([]float64, error) {
	close(provider.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (searchWaitingEmbedder) ModelID() string { return "search01" }

func TestSearchProviderWaitReservesNoConnectionsIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	provider := searchWaitingEmbedder{make(chan struct{})}
	service, err := search.New(database, provider, search.Config{Execution: search.ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: search.ConsistencySnapshot}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	finished := make(chan error, 1)
	go func() {
		_, err := service.Flow(ctx, model.FlowInput{GraphID: newTestID(t), WorkContext: "작업"})
		finished <- err
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("제공자에 도달하지 않았다")
	}
	assertConnectionRecovery(t, database, nil, 0)
	cause := errors.New("제공자 대기 취소")
	cancel(cause)
	select {
	case err := <-finished:
		if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
			t.Fatalf("취소 원인: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("취소 후 검색이 끝나지 않았다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}
