package search

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type queryEmbeddingFunc func(context.Context, string) ([]float64, error)

func (embed queryEmbeddingFunc) Embed(ctx context.Context, text string) ([]float64, error) {
	return embed(ctx, text)
}
func (queryEmbeddingFunc) ModelID() string { return "test:vector:1" }

type inputSnapshotStore struct {
	Store
	opened atomic.Int32
	closed atomic.Int32
	fail   error
}

func (database *inputSnapshotStore) WithReadSnapshot(ctx context.Context, read func(context.Context) error) (model.ReadStats, error) {
	database.opened.Add(1)
	defer database.closed.Add(1)
	err := read(context.WithValue(ctx, snapshotMarker{}, true))
	return model.ReadStats{Connections: 4, PeakConnections: 4}, errors.Join(err, database.fail)
}

func TestSearchInputRejectsBeforeIO(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000001")
	for _, sample := range []struct {
		name, field string
		change      func(*Input)
	}{
		{"graph", "graph_id", func(input *Input) { input.GraphID = model.ID{} }},
		{"empty", "work_context", func(input *Input) { input.WorkContext = "" }},
		{"blank", "work_context", func(input *Input) { input.WorkContext = " \t\r\n\u3000\u00a0" }},
		{"too_long", "work_context", func(input *Input) { input.WorkContext = strings.Repeat("한", 8001) }},
		{"invalid_utf8", "work_context", func(input *Input) { input.WorkContext = string([]byte{0xff}) }},
		{"nul", "work_context", func(input *Input) { input.WorkContext = "질의\x00" }},
		{"timezone", "as_of", func(input *Input) { input.AsOf = time.Now().In(time.FixedZone("KST", 9*3600)) }},
		{"zero_offset", "as_of", func(input *Input) { input.AsOf = time.Now().In(time.FixedZone("zero", 0)) }},
		{"scope", "scope", func(input *Input) { input.Scope = "LOCAL" }},
		{"budget", "budget", func(input *Input) { input.Budget = -1 }},
		{"hops", "max_hops", func(input *Input) { input.MaxHops = -1 }},
		{"nodes", "max_hop_nodes", func(input *Input) { input.MaxHopNodes = -1 }},
	} {
		t.Run(sample.name, func(t *testing.T) {
			// 미검증 입력이 DB의 어느 메서드에 도달해도 실패하도록 Store를 주입하지 않는다.
			database := &inputSnapshotStore{}
			embedCalls := 0
			embed := queryEmbeddingFunc(func(context.Context, string) ([]float64, error) {
				embedCalls++
				return nil, nil
			})
			service, err := New(database, embed, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: ConsistencySnapshot}, nil)
			if err != nil {
				t.Fatal(err)
			}
			input := Input{GraphID: graph, WorkContext: "현재 작업"}
			sample.change(&input)
			flow, err := service.Flow(t.Context(), input)
			field, ok := errors.AsType[model.FieldError](err)
			if !ok || field.Field != sample.field || embedCalls != 0 || database.opened.Load() != 0 || !reflect.DeepEqual(flow, Flow{}) {
				t.Fatalf("입력 경계: field=%+v err=%v embed=%d snapshot=%d flow=%+v", field, err, embedCalls, database.opened.Load(), flow)
			}
		})
	}
}

func TestSearchEmbeddingFinishesBeforeSnapshot(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000001")
	for _, mode := range []string{"success", "failure", "cancel", "pre_canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("제공자 중단")
			entered, proceed := make(chan struct{}), make(chan struct{})
			database := &inputSnapshotStore{Store: &fakeStore{}}
			embed := queryEmbeddingFunc(func(ctx context.Context, text string) ([]float64, error) {
				close(entered)
				select {
				case <-proceed:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if mode == "failure" {
					return nil, cause
				}
				if text != strings.Repeat("한", 8000) {
					return nil, errors.New("질의 원문이 변경됐다")
				}
				return []float64{1}, nil
			})
			service, err := New(database, embed, Config{Execution: ExecutionParallel, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: ConsistencySnapshot}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "pre_canceled" {
				cancel(cause)
			}
			type outcome struct {
				flow Flow
				err  error
			}
			finished := make(chan outcome, 1)
			go func() {
				flow, err := service.Flow(ctx, Input{GraphID: graph, WorkContext: strings.Repeat("한", 8000)})
				finished <- outcome{flow, err}
			}()
			if mode != "pre_canceled" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("제공자를 호출하지 않았다")
				}
				if database.opened.Load() != 0 {
					t.Fatal("제공자 대기 중 스냅숏을 열었다")
				}
				if mode == "cancel" {
					cancel(cause)
				} else {
					close(proceed)
				}
			}
			select {
			case got := <-finished:
				if mode == "cancel" || mode == "pre_canceled" {
					if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, cause) || database.opened.Load() != 0 || !reflect.DeepEqual(got.flow, Flow{}) {
						t.Fatalf("취소 결과: %+v %v opened=%d", got.flow, got.err, database.opened.Load())
					}
				} else if got.err != nil || database.opened.Load() != 1 || database.closed.Load() != 1 || (got.flow.Channels["semantic"].Failure != "") != (mode == "failure") {
					t.Fatalf("제공자 완료 뒤 경계: %+v %v", got.flow, got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("검색이 끝나지 않았다")
			}
		})
	}
}

type cancelOriginStore struct {
	Store
	cancel context.CancelCauseFunc
	cause  error
}

func (database cancelOriginStore) ContextOriginKinds(context.Context, model.ID, []model.ID) (map[model.ID][]model.OriginKind, error) {
	database.cancel(database.cause)
	return nil, database.cause
}

func TestSearchSnapshotFailureDropsAssembledResult(t *testing.T) {
	graph := testID(t, "019a0000-0000-7000-8000-000000000001")
	value := testContext(t, graph, "019a0000-0000-7000-8000-000000000002", "본문", 1)
	for _, mode := range []string{"close_failure", "assembly_cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("읽기 경계 실패")
			var source Store = &fakeStore{time: []model.SearchCandidate{{Context: value}}}
			database := &inputSnapshotStore{}
			if mode == "assembly_cancel" {
				source = cancelOriginStore{source, cancel, cause}
			} else {
				database.fail = cause
			}
			database.Store = source
			service, err := New(database, fakeEmbedder{}, Config{Execution: ExecutionSequential, CandidateLimit: 5, FoldThreshold: 0.9, Consistency: ConsistencySnapshot}, nil)
			if err != nil {
				t.Fatal(err)
			}
			flow, err := service.Flow(ctx, Input{GraphID: graph, WorkContext: "질의"})
			if !errors.Is(err, cause) || len(flow.Contexts) != 0 || len(flow.Edges) != 0 || len(flow.EntryPoints) != 0 || database.closed.Load() != 1 || flow.Read.PeakConnections != 4 {
				t.Fatalf("실패 결과·정리: %+v %v closed=%d", flow, err, database.closed.Load())
			}
			if mode == "assembly_cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("취소 분류가 사라졌다")
			}
		})
	}
}
