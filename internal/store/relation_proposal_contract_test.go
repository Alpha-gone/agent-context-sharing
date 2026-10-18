package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

func lifeProposalConfig(limit int) *RelationProposalConfig {
	return &RelationProposalConfig{AdjacencyWindow: time.Hour, SimilarityThreshold: 0.8, Limit: limit}
}

func lifeProposalFixture(t *testing.T, database *Store) (model.ID, model.ID, model.Context) {
	t.Helper()
	graph, actor, source := boundaryGraph(t, database)
	return graph, actor, lifeDerived(t, database, graph, actor, source.ID)
}

func lifeProposalIdentity(t *testing.T, database *Store, graph model.ID, kind model.RelationType, from, to model.ID) model.Relation {
	t.Helper()
	identity := normalizeRelation(proposedRelation(graph, kind, from, to))
	stored, err := database.relationByIdentity(t.Context(), database.pool, graph, kind, identity.FromContextID, identity.ToContextID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestLifeProposalSignals(t *testing.T) {
	graph, leftID, rightID := newTestID(t), newTestID(t), newTestID(t)
	now := time.Now().UTC()
	for _, sample := range []struct {
		name  string
		end   *time.Time
		start time.Time
		want  bool
	}{
		{"touching", new(now), now, true},
		{"window_boundary", new(now), now.Add(time.Hour), true},
		{"beyond_window", new(now), now.Add(time.Hour + time.Nanosecond), false},
		{"overlap", new(now.Add(time.Second)), now, false},
		{"both_ongoing", nil, now, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			left := model.Context{ID: leftID, Event: &model.EventAttributes{Start: now.Add(-time.Minute), End: sample.end}}
			right := model.Context{ID: rightID, Event: &model.EventAttributes{Start: sample.start}}
			for _, pair := range [][2]model.Context{{left, right}, {right, left}} {
				candidate, _, ok := adjacentPrecedesCandidate(graph, pair[0], pair[1], time.Hour)
				if ok != sample.want || ok && (candidate.FromContextID != leftID || candidate.ToContextID != rightID) {
					t.Fatalf("시간 신호=%+v/%t", candidate, ok)
				}
			}
		})
	}
}

// 상한은 새 제안 수에 적용하고 이미 판단한 정체성·제안 시각은 보존한다.
func TestLifeProposalRankingAndRetentionIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, member := lifeProposalFixture(t, database)
	now := time.Now().UTC().Truncate(time.Second)
	var events []model.Context
	for _, offset := range []time.Duration{0, 3 * time.Minute, 6 * time.Minute, 10 * time.Minute} {
		events = append(events, proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now.Add(offset), new(now.Add(offset+time.Minute))))
	}
	database.relationProposals = lifeProposalConfig(1)
	target := events[3]
	for i, index := range []int{2, 1, 0} {
		if err := database.ProposeEventRelations(t.Context(), graph, target.ID); err != nil {
			t.Fatal(err)
		}
		if got := len(proposalRelations(t, database, graph)); got != i+1 {
			t.Fatalf("상한/다음 후보=%d", got)
		}
		candidate := lifeProposalIdentity(t, database, graph, model.RelationTypePrecedes, events[index].ID, target.ID)
		if candidate.State != model.RelationStateProposed || candidate.ProposedBy != model.ProposalSourceSystem {
			t.Fatal(candidate)
		}
		if i == 0 {
			if _, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, candidate.Type, candidate.FromContextID, candidate.ToContextID), nil); err != nil {
				t.Fatal(err)
			}
		} else if i == 1 {
			if _, err := database.DiscardRelation(t.Context(), graph, candidate.ID, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := proposalRelations(t, database, graph)
	if err := database.ProposeEventRelations(t.Context(), graph, target.ID); err != nil {
		t.Fatal(err)
	}
	for _, prior := range before {
		current := lifeProposalIdentity(t, database, graph, prior.Type, prior.FromContextID, prior.ToContextID)
		if current.ID != prior.ID || current.State != prior.State || !current.ProposedAt.Equal(prior.ProposedAt) {
			t.Fatal("중복 제안이 판단·이력을 변경했다")
		}
	}
	hops, err := database.HopContexts(t.Context(), graph, events[0].ID, 1, "out", []string{"precedes"}, 0)
	if err != nil || len(hops.Contexts) != 1 || len(hops.Edges) != 0 {
		t.Fatalf("미확정 후보가 탐색에 노출됐다: %+v/%v", hops, err)
	}
	old := lifeProposalIdentity(t, database, graph, model.RelationTypePrecedes, events[0].ID, target.ID)
	count, err := database.CleanupExpiredProposals(t.Context(), time.Now().AddDate(0, 0, 91), func(id model.ID) int {
		if id == actor {
			return 90
		}
		return 0
	})
	if err != nil || count != 1 {
		t.Fatalf("제안 보존 정리=%d/%v", count, err)
	}
	bodyOnly := target
	bodyOnly.Body += " 본문만 변경"
	bodyOnly.Version++
	bodyOnly, err = database.UpdateContext(t.Context(), graph, target.Version, bodyOnly)
	if err != nil || len(proposalRelations(t, database, graph)) != 2 {
		t.Fatalf("본문만 갱신했는데 저장 신호가 실행됐다: %v", err)
	}
	next := bodyOnly
	attributes := *target.Event
	attributes.End = new(attributes.End.Add(time.Second))
	next.Event = &attributes
	next.Version++
	if _, err := database.UpdateContext(t.Context(), graph, bodyOnly.Version, next); err != nil {
		t.Fatal(err)
	}
	current := lifeProposalIdentity(t, database, graph, old.Type, old.FromContextID, old.ToContextID)
	if current.ID == old.ID || current.State != model.RelationStateProposed {
		t.Fatal("정리한 유효 정체성이 다음 저장 방아쇠에서 재제안되지 않았다")
	}
	if got := len(proposalRelations(t, database, graph)); got != 3 {
		t.Fatalf("판단한 관계가 정리/재생성됐다: %d", got)
	}
	assertConnectionRecovery(t, database, nil, 0)
}

func TestLifeProposalStrictSubsetIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, first := lifeProposalFixture(t, database)
	second := lifeDerived(t, database, graph, actor, first.ID)
	third := lifeDerived(t, database, graph, actor, first.ID)
	now := time.Now().UTC()
	target := proposalEvent(t, database, graph, actor, []model.ID{first.ID, second.ID, third.ID}, now, nil)
	twin := proposalEvent(t, database, graph, actor, []model.ID{third.ID, first.ID, second.ID}, now, nil)
	small := proposalEvent(t, database, graph, actor, []model.ID{first.ID}, now, nil)
	large := proposalEvent(t, database, graph, actor, []model.ID{first.ID, second.ID}, now, nil)
	database.relationProposals = lifeProposalConfig(1)
	for _, want := range []model.ID{large.ID, small.ID} {
		if err := database.ProposeEventRelations(t.Context(), graph, target.ID); err != nil {
			t.Fatal(err)
		}
		lifeProposalIdentity(t, database, graph, model.RelationTypePartOf, want, target.ID)
	}
	for _, relation := range proposalRelations(t, database, graph) {
		if relation.Type != model.RelationTypePartOf || relation.FromContextID == twin.ID || relation.ToContextID == twin.ID {
			t.Fatalf("진부분집합 이외의 자동 후보=%+v", relation)
		}
	}
}

func lifeSeedProposalEmbedding(t *testing.T, database *Store, event model.Context, modelID string, similarity float64) {
	t.Helper()
	vector := make([]float64, embeddingDimension(t, database))
	vector[0], vector[1] = similarity, math.Sqrt(1-similarity*similarity)
	claimExec(t, database, `INSERT INTO public.context_embedding(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier)
		VALUES($1,$2,$3,$4,now(),now(),'hot')`, event.ID.String(), event.GraphID.String(), vectorText(vector), modelID)
}

func TestLifeProposalSimilarityIntegration(t *testing.T) {
	for _, typ := range []string{"vector", "halfvec"} {
		t.Run(typ, func(t *testing.T) {
			database := newEmbeddingContractStore(t, typ, 8)
			graph, actor, member := lifeProposalFixture(t, database)
			now := time.Now().UTC()
			target := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, nil)
			lifeSeedProposalEmbedding(t, database, target, "current", 1)
			var candidates []model.Context
			for _, similarity := range []float64{1, 0.98, 0.95, 0.9, 0.7} {
				event := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, nil)
				lifeSeedProposalEmbedding(t, database, event, "current", similarity)
				candidates = append(candidates, event)
			}
			otherModel := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, nil)
			lifeSeedProposalEmbedding(t, database, otherModel, "previous", 1)
			removed := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, nil)
			if _, err := database.DiscardContext(t.Context(), graph, removed.ID, nil, WriteLimits{}); err != nil {
				t.Fatal(err)
			}
			lifeSeedProposalEmbedding(t, database, removed, "current", 1)
			lifeSeedProposalEmbedding(t, database, member, "current", 1) // 파생은 비교 대상이 아니다.
			foreignGraph, foreignActor, foreignMember := lifeProposalFixture(t, database)
			foreign := proposalEvent(t, database, foreignGraph, foreignActor, []model.ID{foreignMember.ID}, now, nil)
			lifeSeedProposalEmbedding(t, database, foreign, "current", 1)
			for i, event := range candidates[:2] {
				confirmed, err := database.ConfirmRelation(t.Context(), graph, confirmable(t, graph, model.RelationTypeRelatesTo, target.ID, event.ID), nil)
				if err != nil {
					t.Fatal(err)
				}
				if i == 1 {
					if _, err := database.DiscardRelation(t.Context(), graph, confirmed.ID, nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			database.relationProposals = lifeProposalConfig(1)
			if err := database.ProposeSimilarEventRelations(t.Context(), graph, target.ID, "missing"); err != nil {
				t.Fatal(err)
			}
			for i, event := range candidates[2:4] {
				if err := database.ProposeSimilarEventRelations(t.Context(), graph, target.ID, "current"); err != nil {
					t.Fatal(err)
				}
				if len(proposalRelations(t, database, graph)) != 3+i {
					t.Fatal("기존 정체성이 유사도 후보 상한을 차지했다")
				}
				lifeProposalIdentity(t, database, graph, model.RelationTypeRelatesTo, target.ID, event.ID)
			}
			if err := database.ProposeSimilarEventRelations(t.Context(), graph, target.ID, "current"); err != nil {
				t.Fatal(err)
			}
			if len(proposalRelations(t, database, graph)) != 4 {
				t.Fatal("임계값·모델·계층·그래프·삭제 필터 위반")
			}
		})
	}
}

func TestLifeProposalRechecksLockedStateIntegration(t *testing.T) {
	for _, mode := range []string{"time", "members", "deleted", "model", "vector", "confirmed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, member := lifeProposalFixture(t, database)
			other := lifeDerived(t, database, graph, actor, member.ID)
			now := time.Now().UTC()
			from := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, new(now.Add(time.Minute)))
			to := proposalEvent(t, database, graph, actor, []model.ID{member.ID, other.ID}, now.Add(2*time.Minute), nil)
			kind := model.RelationTypePrecedes
			modelID := ""
			if mode == "members" {
				kind = model.RelationTypePartOf
				to.Event.Start = now
				to.Version++
				var err error
				to, err = database.UpdateContext(t.Context(), graph, to.Version-1, to)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "model" || mode == "vector" {
				kind = model.RelationTypeRelatesTo
				modelID = "current"
				lifeSeedProposalEmbedding(t, database, from, modelID, 1)
				lifeSeedProposalEmbedding(t, database, to, modelID, 1)
			}
			database.relationProposals = lifeProposalConfig(1)
			candidate := normalizeRelation(proposedRelation(graph, kind, from.ID, to.ID))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := database.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackTransaction(t.Context(), tx)
			if err := requireActiveGraph(ctx, tx, graph); err != nil {
				t.Fatal(err)
			}
			request, stop := context.WithCancel(ctx)
			var group sync.WaitGroup
			defer func() { stop(); group.Wait() }()
			result := make(chan error, 1)
			group.Go(func() {
				created, err := database.createProposedRelation(request, graph, candidate, modelID)
				if created {
					err = errors.New("갱신 전 신호로 후보를 생성했다")
				}
				result <- err
			})
			waitGrantWaiters(t, ctx, database, int(tx.Conn().PgConn().PID()), 1)
			writeCtx := context.WithValue(ctx, writeTransactionContextKey{}, tx)
			writeCtx = context.WithValue(writeCtx, afterWriteCommitContextKey{}, &afterWriteCommit{})
			switch mode {
			case "time", "members":
				next := from
				attributes := *from.Event
				if mode == "time" {
					attributes.End = new(now.Add(3 * time.Minute))
				} else {
					attributes.MemberIDs = to.Event.MemberIDs
				}
				next.Event = &attributes
				next.Version++
				if _, err := database.UpdateContext(writeCtx, graph, from.Version, next); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if _, err := database.DiscardContext(writeCtx, graph, to.ID, nil, WriteLimits{}); err != nil {
					t.Fatal(err)
				}
			case "model":
				if _, err := tx.Exec(ctx, `UPDATE public.context_embedding SET model_id='previous' WHERE context_id=$1`, to.ID.String()); err != nil {
					t.Fatal(err)
				}
			case "vector":
				vector := make([]float64, embeddingDimension(t, database))
				vector[1] = 1
				if _, err := tx.Exec(ctx, `UPDATE public.context_embedding SET embedding=$2 WHERE context_id=$1`, to.ID.String(), vectorText(vector)); err != nil {
					t.Fatal(err)
				}
			case "confirmed":
				if _, err := database.ConfirmRelation(writeCtx, graph, confirmable(t, graph, kind, from.ID, to.ID), nil); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				stop()
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-result
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("잠금 취소=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			relations := proposalRelations(t, database, graph)
			if mode == "confirmed" {
				if len(relations) != 1 || relations[0].State != model.RelationStateConfirmed {
					t.Fatal(relations)
				}
			} else if len(relations) != 0 {
				t.Fatal(relations)
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestLifeProposalOuterCommitIntegration(t *testing.T) {
	for _, mode := range []string{"commit", "rollback", "commit_failure"} {
		t.Run(mode, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, member := lifeProposalFixture(t, database)
			now := time.Now().UTC()
			proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, new(now.Add(time.Minute)))
			database.relationProposals = lifeProposalConfig(1)
			if mode == "commit_failure" {
				lifeFailGraphWrite(t, database, graph, "context_graph", "UPDATE", true)
			}
			input := testEventContext(t, graph, actor, member.ID)
			input.Event = &model.EventAttributes{MemberIDs: []model.ID{member.ID}, Start: now.Add(2 * time.Minute)}
			request := IdempotencyRequest{AccountID: actor, Key: newTestID(t), ToolName: "node_create"}
			calls := 0
			run := func(ctx context.Context) ([]byte, error) {
				calls++
				if _, err := database.CreateContext(ctx, graph, input, nil); err != nil {
					return nil, err
				}
				if len(proposalRelations(t, database, graph)) != 0 {
					t.Error("바깥 커밋 전에 후보가 생성됐다")
				}
				if mode == "rollback" {
					return nil, errors.New("합성 롤백")
				}
				return []byte(`{"ok":true}`), nil
			}
			_, err := database.ReplayIdempotent(t.Context(), request, run)
			if mode != "commit" {
				if err == nil || len(proposalRelations(t, database, graph)) != 0 {
					t.Fatalf("실패한 바깥 쓰기의 후보=%v", err)
				}
				return
			}
			if err != nil || len(proposalRelations(t, database, graph)) != 1 {
				t.Fatalf("커밋 후 후보=%v", err)
			}
			previous := proposalRelations(t, database, graph)[0]
			if _, err := database.ReplayIdempotent(t.Context(), request, run); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || proposalRelations(t, database, graph)[0].ID != previous.ID {
				t.Fatal("멱등 재생이 후보를 다시 실행했다")
			}
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

type lifeProposalFailureTracer struct{ fail atomic.Bool }

func (tracer *lifeProposalFailureTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "CREATE (from)-[edge:PRECEDES") && tracer.fail.Swap(false) {
		ctx, cancel := context.WithCancelCause(ctx)
		cancel(errors.New("private proposal marker"))
		return ctx
	}
	return ctx
}

func (*lifeProposalFailureTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestLifeProposalFailurePreservesSaveIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	graph, actor, member := lifeProposalFixture(t, database)
	now := time.Now().UTC()
	proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, new(now.Add(time.Minute)))
	tracer := new(lifeProposalFailureTracer)
	tracer.fail.Store(true)
	configureTierPool(t, database, tracer)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	database.relationProposals = lifeProposalConfig(1)
	event := proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now.Add(2*time.Minute), nil)
	if _, err := database.Context(t.Context(), graph, event.ID); err != nil {
		t.Fatal(err)
	}
	if tracer.fail.Load() || indexTaskCount(t, database, event.ID) != 1 || len(proposalRelations(t, database, graph)) != 0 {
		t.Fatal("제안 실패가 사건 저장/큐를 되돌리거나 부분 간선을 남겼다")
	}
	output := logs.String()
	if strings.Contains(output, "private proposal marker") || strings.Contains(output, `"error"`) || !strings.Contains(output, `"reason_code":"proposal_failed"`) {
		t.Fatalf("제안 실패 지표=%s", output)
	}
	if err := database.ProposeEventRelations(t.Context(), graph, event.ID); err != nil {
		t.Fatal(err)
	}
	if len(proposalRelations(t, database, graph)) != 1 {
		t.Fatal("제안 실패 뒤 재시도가 복구되지 않았다")
	}
	assertConnectionRecovery(t, database, nil, 0)
}
