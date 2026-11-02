package store

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// 커밋 직후 별도 세션이 그래프 행을 잠가 실제 제안의 FOR UPDATE 대기를 재현한다.
func TestPostCommitProposalBudgetIntegration(t *testing.T) {
	for _, tool := range []string{"node_create", "node_update"} {
		for _, keyed := range []bool{false, true} {
			for _, mode := range []string{"deadline", "cancel", "budget"} {
				t.Run(fmt.Sprintf("%s/keyed=%t/%s", tool, keyed, mode), func(t *testing.T) {
					database := newIsolatedIntegrationStore(t)
					graph, actor, member := lifeProposalFixture(t, database)
					now := time.Now().UTC()
					proposalEvent(t, database, graph, actor, []model.ID{member.ID}, now, new(now.Add(time.Minute)))
					input := testEventContext(t, graph, actor, member.ID)
					input.Event = &model.EventAttributes{MemberIDs: []model.ID{member.ID}, Start: now.Add(2 * time.Minute)}
					if tool == "node_update" {
						var err error
						input, err = database.CreateContext(t.Context(), graph, input, nil)
						if err != nil {
							t.Fatal(err)
						}
						next := *input.Event
						next.Start = now.Add(3 * time.Minute)
						input.Event = &next
						input.Version++
					}
					database.relationProposals = lifeProposalConfig(1)
					blocker, err := database.pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer rollbackTransaction(t.Context(), blocker)
					monitor, stopMonitor := context.WithTimeout(t.Context(), 8*time.Second)
					defer stopMonitor()
					var request context.Context
					var cancel context.CancelFunc
					if mode == "deadline" {
						request, cancel = context.WithTimeout(t.Context(), 400*time.Millisecond)
					} else {
						request, cancel = context.WithCancel(t.Context())
					}
					defer cancel()
					locked := make(chan struct{})
					done := make(chan struct {
						result []byte
						err    error
					}, 1)
					apply := func(ctx context.Context) ([]byte, error) {
						if !deferAfterWriteCommit(ctx, func() {
							if err := requireActiveGraph(monitor, blocker, graph); err != nil {
								t.Error(err)
							}
							close(locked)
						}) {
							return nil, fmt.Errorf("커밋 후 제안 경계가 없습니다")
						}
						var saved model.Context
						var err error
						if tool == "node_create" {
							saved, err = database.CreateContext(ctx, graph, input, nil)
						} else {
							saved, err = database.UpdateContext(ctx, graph, input.Version-1, input)
						}
						if err != nil {
							return nil, err
						}
						return json.Marshal(map[string]any{"context_id": saved.ID.String(), "version": saved.Version})
					}
					key := ""
					if keyed {
						key = "postcommit_budget_001"
					}
					var logs bytes.Buffer
					previousLogger := slog.Default()
					slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
					t.Cleanup(func() { slog.SetDefault(previousLogger) })
					started := time.Now()
					go func() {
						result, err := database.ExecuteWrite(request, actor, key, tool, []byte(`{}`), writeReplayCheck, apply)
						done <- struct {
							result []byte
							err    error
						}{result, err}
					}()
					// 실패 시에도 blocker를 반환한 뒤 작업 goroutine을 회수한다.
					finished := false
					defer func() {
						cancel()
						_ = rollbackTransaction(t.Context(), blocker)
						if !finished {
							select {
							case <-done:
							case <-time.After(5 * time.Second):
								t.Error("쓰기 goroutine 미반환")
							}
						}
					}()
					select {
					case <-locked:
					case result := <-done:
						finished = true
						t.Fatalf("커밋 후 잠금 전 반환: %v", result.err)
					case <-monitor.Done():
						t.Fatal("커밋 후 잠금 확보 실패")
					}
					waitGrantWaiters(t, monitor, database, int(blocker.Conn().PgConn().PID()), 1)
					budget := connectionBudget(database.pool.Stat().MaxConns())
					if !database.reservations.TryAcquire(budget) {
						t.Error("커밋 뒤 제안이 쓰기 연결 예약을 점유했습니다")
					} else {
						database.reservations.Release(budget)
					}
					if mode == "cancel" {
						cancel()
					}
					var result struct {
						result []byte
						err    error
					}
					select {
					case result = <-done:
						finished = true
					case <-time.After(3 * time.Second):
						t.Error("그래프 잠금에서 제안 실행 예산을 초과했습니다")
						_ = rollbackTransaction(t.Context(), blocker)
						select {
						case result = <-done:
							finished = true
						case <-monitor.Done():
							t.Fatal("제안 중단 뒤 응답 미반환")
						}
					}
					if result.err != nil || len(result.result) == 0 {
						t.Fatalf("커밋된 쓰기를 제안 실패로 바꿨습니다: %v", result.err)
					}
					if !strings.Contains(logs.String(), `"reason_code":"proposal_failed"`) || strings.Contains(logs.String(), `"error":`) {
						t.Error("제안 실패의 고정 진단 기록 누락 또는 원문 노출")
					}
					if elapsed := time.Since(started); elapsed > 3*time.Second {
						t.Errorf("커밋 후 제안 응답 지연=%s", elapsed)
					}
					assertConnectionRecovery(t, database, nil, 1)
					if err := rollbackTransaction(t.Context(), blocker); err != nil {
						t.Fatal(err)
					}
					assertConnectionRecovery(t, database, nil, 0)
					stored, err := database.Context(t.Context(), graph, input.ID)
					if err != nil || stored.Version != input.Version {
						t.Fatalf("커밋 저장 상태: version=%d err=%v", stored.Version, err)
					}
					if count := len(proposalRelations(t, database, graph)); count != 0 {
						t.Fatalf("취소된 제안이 관계를 남겼습니다: %d", count)
					}
					if keyed {
						replayed, err := database.ExecuteWrite(t.Context(), actor, key, tool, []byte(`{}`), writeReplayCheck, func(context.Context) ([]byte, error) {
							t.Error("완료 쓰기를 다시 적용했습니다")
							return nil, fmt.Errorf("unexpected replay apply")
						})
						if err != nil || !bytes.Equal(replayed, result.result) {
							t.Fatalf("완료 결과 재생: %s %v", replayed, err)
						}
					}
				})
			}
		}
	}
}
