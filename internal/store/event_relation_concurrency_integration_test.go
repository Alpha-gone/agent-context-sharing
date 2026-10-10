package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestEventDeletionSerializationIntegration은 바깥 트랜잭션의 커밋을 늦춰
// 사건 폐기와 관계 확정·폐기, 사건 갱신의 양쪽 실행 순서에서 실제 DB 경합을 확인한다.
func TestEventDeletionSerializationIntegration(t *testing.T) {
	for _, endpoint := range []string{"from", "to"} {
		for _, action := range []string{"confirm", "discard", "update"} {
			for _, eventFirst := range []bool{false, true} {
				name := endpoint + "/" + action + "/other_first"
				if eventFirst {
					name = endpoint + "/" + action + "/event_first"
				}
				t.Run(name, func(t *testing.T) {
					database := newIntegrationStore(t)
					graphID, from, to := relationTestEvents(t, database)
					event := from
					if endpoint == "to" {
						event = to
					}
					request := confirmable(t, graphID, model.RelationTypePrecedes, from.ID, to.ID)
					candidate := proposedRelation(graphID, request.Type, from.ID, to.ID)
					candidate.ID = request.ID
					relation, err := database.CreateRelation(t.Context(), graphID, candidate)
					if err != nil {
						t.Fatalf("관계 후보 준비: %v", err)
					}
					if action == "discard" {
						relation, err = database.ConfirmRelation(t.Context(), graphID, request, nil)
						if err != nil {
							t.Fatalf("관계 확정 준비: %v", err)
						}
					}
					eventAction := func(ctx context.Context) error {
						_, err := database.DiscardContext(ctx, graphID, event.ID, testMCPDiscardOperation(event), WriteLimits{})
						return err
					}
					otherAction := func(ctx context.Context) error {
						if action == "update" {
							next := event
							next.Body, next.Version = "동시에 갱신한 사건 본문", event.Version+1
							_, err := database.UpdateContext(ctx, graphID, event.Version, next)
							return err
						}
						if action == "confirm" {
							_, err := database.ConfirmRelation(ctx, graphID, request, nil)
							return err
						}
						_, err := database.DiscardRelation(ctx, graphID, relation.ID, nil)
						return err
					}
					firstAction, secondAction := otherAction, eventAction
					if eventFirst {
						firstAction, secondAction = eventAction, otherAction
					}
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					first, err := database.pool.Begin(ctx)
					if err != nil {
						t.Fatalf("선행 트랜잭션 준비: %v", err)
					}
					defer first.Rollback(t.Context())
					if err := firstAction(context.WithValue(ctx, writeTransactionContextKey{}, first)); err != nil {
						t.Fatalf("선행 연산: %v", err)
					}
					second, err := database.pool.Begin(ctx)
					if err != nil {
						t.Fatalf("후행 트랜잭션 준비: %v", err)
					}
					var waitGroup sync.WaitGroup
					defer func() { cancel(); waitGroup.Wait(); second.Rollback(t.Context()) }()
					firstPID, secondPID := first.Conn().PgConn().PID(), second.Conn().PgConn().PID()
					result := make(chan error, 1)
					waitGroup.Go(func() {
						result <- secondAction(context.WithValue(ctx, writeTransactionContextKey{}, second))
					})
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for blocked := false; !blocked; {
						select {
						case err := <-result:
							t.Fatalf("선행 커밋 전에 후행 연산이 끝났다: %v", err)
						case <-ctx.Done():
							t.Fatalf("DB 잠금 대기 확인: %v", ctx.Err())
						case <-ticker.C:
							if err := database.pool.QueryRow(ctx, `SELECT $1::integer = ANY(pg_blocking_pids($2::integer))`, firstPID, secondPID).Scan(&blocked); err != nil {
								t.Fatalf("DB 잠금 대기 조회: %v", err)
							}
						}
					}
					if err := first.Commit(ctx); err != nil {
						t.Fatalf("선행 커밋: %v", err)
					}
					secondErr := <-result
					if !eventFirst {
						if secondErr != nil {
							t.Fatalf("선행 연산 뒤 사건 폐기: %v", secondErr)
						}
					} else {
						want := ErrInvalidRelation
						if action == "discard" {
							want = ErrInvalidState
						}
						if action == "update" {
							conflict, ok := errors.AsType[VersionConflictError](secondErr)
							if !ok || conflict.Current != event.Version+1 {
								t.Fatalf("사건 폐기 뒤 갱신 = %v, want 현재 판 %d의 충돌", secondErr, event.Version+1)
							}
						} else if !errors.Is(secondErr, want) {
							t.Fatalf("사건 폐기 뒤 관계 전이 = %v, want %v", secondErr, want)
						}
					}
					if err := second.Commit(ctx); err != nil {
						t.Fatalf("후행 커밋: %v", err)
					}
					deleted, err := database.Context(t.Context(), graphID, event.ID)
					if err != nil || deleted.DeletedAt == nil {
						t.Fatalf("사건 폐기 상태 = %#v, 오류 %v", deleted, err)
					}
					if action == "update" && !eventFirst && (deleted.Body != "동시에 갱신한 사건 본문" || deleted.Version != event.Version+2) {
						t.Fatalf("잠금 대기 뒤 폐기가 새 사건 본문·판을 보존하지 않았다: %#v", deleted)
					}
					stored, err := database.relationByID(t.Context(), database.pool, graphID, relation.ID)
					if err != nil || stored.State == model.RelationStateConfirmed {
						t.Fatalf("폐기된 사건의 관계 = %#v, 오류 %v; 확정 관계가 없어야 한다", stored, err)
					}
					if action == "discard" || (action == "confirm" && !eventFirst) {
						if stored.State != model.RelationStateDiscarded || stored.DeletedAt == nil || stored.ConfirmedAt != nil {
							t.Fatalf("관계 폐기 속성이 남지 않았다: %#v", stored)
						}
					}
					// 사건 복구는 관계를 다시 확정하지 않는다.
					if _, err := database.RestoreContext(t.Context(), graphID, event.ID, nil, WriteLimits{}); err != nil {
						t.Fatalf("사건 복구: %v", err)
					}
					restoredRelation, err := database.relationByID(t.Context(), database.pool, graphID, relation.ID)
					if err != nil || restoredRelation.State != stored.State {
						t.Fatalf("사건 복구가 관계 상태를 바꿨다: %#v, 오류 %v", restoredRelation, err)
					}
				})
			}
		}
	}
}
