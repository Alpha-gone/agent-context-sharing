package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestIndexReenqueuedTaskIgnoresStaleResultIntegration은 제공자 호출 중 재등록된
// 작업의 이전 성공·실패가 새 작업이나 새 임베딩을 덮어쓰지 않는지 확인한다.
func TestIndexReenqueuedTaskIgnoresStaleResultIntegration(t *testing.T) {
	for _, failure := range []string{"", "retryable", "permanent", "exhausted"} {
		for _, completed := range []bool{false, true} {
			t.Run(fmt.Sprintf("failure=%s/completed=%t", failure, completed), func(t *testing.T) {
				database := newIntegrationStore(t)
				actorID := newTestID(t)
				createTestAccount(t, database, actorID)
				graphID := createIndexScopeGraph(t, database, actorID, "stale-result")
				var targetID string
				if err := database.pool.QueryRow(t.Context(), `SELECT task.context_id::text FROM public.index_task AS task
					JOIN `+database.contextTable()+` AS node ON node.properties ->> 'context_id'::text = task.context_id::text
					WHERE task.graph_id = $1 AND node.properties ->> 'layer'::text = 'derived'`, graphID.String()).Scan(&targetID); err != nil {
					t.Fatalf("색인 대상 조회: %v", err)
				}
				id, err := model.ParseID(targetID)
				if err != nil {
					t.Fatal(err)
				}
				target, err := database.Context(t.Context(), graphID, id)
				if err != nil || target.Layer != model.LayerDerived {
					t.Fatalf("파생 색인 대상 = %+v, %v", target, err)
				}
				readyIndexTasks(t, database, graphID, target.ID)
				if failure == "exhausted" {
					if _, err := database.pool.Exec(t.Context(), `UPDATE public.index_task SET attempts = 4 WHERE context_id = $1`, targetID); err != nil {
						t.Fatal(err)
					}
				}
				oldVector := make([]float64, embeddingDimension(t, database))
				oldVector[0] = 1
				newVector := make([]float64, len(oldVector))
				newVector[1] = 1
				var pending indexTaskStateValue
				var revision int64
				newProcessor := func(_ context.Context, task IndexTask) IndexTaskResult {
					if task.ContextID != target.ID || task.Body != "재등록된 새 본문" {
						t.Fatalf("새 확보분 = %+v", task)
					}
					return IndexTaskResult{Embedding: newVector, ModelID: "stale-result-test"}
				}
				result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, func(ctx context.Context, task IndexTask) IndexTaskResult {
					if task.ContextID != target.ID || task.Body != target.Body {
						t.Fatalf("이전 확보분 = %+v", task)
					}
					next := target
					next.Body = "재등록된 새 본문"
					next.Version++
					if _, err := database.UpdateContext(ctx, graphID, target.Version, next); err != nil {
						t.Fatalf("제공자 호출 중 본문 갱신: %v", err)
					}
					// 호스트와 DB의 시계 차이에 의존하지 않고 새 작업을 확보 가능하게 한다.
					// 등록 시각은 실제 본문 갱신이 쓴 값을 유지한다.
					if _, err := database.pool.Exec(ctx, `UPDATE public.index_task SET next_attempt_at = now() WHERE context_id = $1`, targetID); err != nil {
						t.Fatal(err)
					}
					if completed {
						newResult, err := database.ProcessNextIndexTaskInGraph(ctx, graphID, newProcessor)
						if err != nil || !newResult.Succeeded {
							t.Fatalf("새 확보분 저장 = %+v, %v", newResult, err)
						}
					} else {
						pending = indexTaskState(t, database, target.ID)
					}
					if err := database.pool.QueryRow(ctx, `SELECT content_revision FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&revision); err != nil {
						t.Fatal(err)
					}
					if failure != "" {
						return IndexTaskResult{Failure: "이전 확보분 실패", Retryable: failure != "permanent"}
					}
					return IndexTaskResult{Embedding: oldVector, ModelID: "stale-result-test"}
				})
				if err != nil || !result.Found || result.Succeeded {
					t.Fatalf("무효 확보분 처리 = %+v, %v", result, err)
				}
				var afterRevision int64
				if err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&afterRevision); err != nil || afterRevision != revision {
					t.Fatalf("무효 확보분의 내용 판 = %d, want %d, %v", afterRevision, revision, err)
				}
				if !completed {
					if after := indexTaskState(t, database, target.ID); after != pending {
						t.Fatalf("무효 확보분이 새 작업을 바꿨다: %+v, want %+v", after, pending)
					}
					var count int
					if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.context_embedding WHERE context_id = $1`, targetID).Scan(&count); err != nil || count != 0 {
						t.Fatalf("무효 확보분 임베딩 수 = %d, %v", count, err)
					}
					if newResult, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, newProcessor); err != nil || !newResult.Succeeded {
						t.Fatalf("남은 새 작업 처리 = %+v, %v", newResult, err)
					}
				}
				if count := indexTaskCount(t, database, target.ID); count != 0 {
					t.Fatalf("완료 뒤 작업 수 = %d", count)
				}
				var embedding string
				if err := database.pool.QueryRow(t.Context(), `SELECT embedding::text FROM public.context_embedding WHERE context_id = $1`, targetID).Scan(&embedding); err != nil || embedding != vectorText(newVector) {
					t.Fatalf("최종 임베딩 = %s, want 새 본문 임베딩, %v", embedding, err)
				}
			})
		}
	}
}

// TestIndexReclaimedTaskIgnoresStaleResultIntegration은 본문 재등록 없이 리스만
// 만료·재확보된 경우에도 이전 확보분이 새 재시도 상태를 바꾸지 않는지 확인한다.
func TestIndexReclaimedTaskIgnoresStaleResultIntegration(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprintf("success=%t", success), func(t *testing.T) {
			database := newIntegrationStore(t)
			actorID := newTestID(t)
			createTestAccount(t, database, actorID)
			graphID := createIndexScopeGraph(t, database, actorID, "reclaimed")
			var pending indexTaskStateValue
			result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, func(ctx context.Context, task IndexTask) IndexTaskResult {
				// 2분을 기다리는 대신 이 확보분의 리스를 만료시킨다. 등록 시각은 유지한다.
				if _, err := database.pool.Exec(ctx, `UPDATE public.index_task SET next_attempt_at = now() - interval '1 second'
					WHERE task_id = $1`, task.ID.String()); err != nil {
					t.Fatal(err)
				}
				readyOther := `UPDATE public.index_task SET next_attempt_at = now() + interval '1 hour'
					WHERE graph_id = $1 AND task_id <> $2`
				if _, err := database.pool.Exec(ctx, readyOther, graphID.String(), task.ID.String()); err != nil {
					t.Fatal(err)
				}
				newResult, err := database.ProcessNextIndexTaskInGraph(ctx, graphID, func(_ context.Context, newer IndexTask) IndexTaskResult {
					if newer.ID != task.ID || newer.Body != task.Body {
						t.Fatalf("리스 재확보분 = %+v, want %+v", newer, task)
					}
					return IndexTaskResult{Failure: "새 확보분의 재시도", Retryable: true}
				})
				if err != nil || !newResult.Found || newResult.Succeeded {
					t.Fatalf("리스 재확보 처리 = %+v, %v", newResult, err)
				}
				pending = indexTaskState(t, database, task.ContextID)
				if !success {
					return IndexTaskResult{Failure: "이전 확보분의 영구 실패"}
				}
				vector := make([]float64, embeddingDimension(t, database))
				vector[0] = 1
				return IndexTaskResult{Embedding: vector, ModelID: "reclaimed-test"}
			})
			if err != nil || !result.Found || result.Succeeded {
				t.Fatalf("무효 리스 처리 = %+v, %v", result, err)
			}
			if after := indexTaskState(t, database, result.Task.ContextID); after != pending {
				t.Fatalf("이전 확보분이 재확보 상태를 바꿨다: %+v, want %+v", after, pending)
			}
			var count int
			if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.context_embedding WHERE context_id = $1`, result.Task.ContextID.String()).Scan(&count); err != nil || count != 0 {
				t.Fatalf("무효 리스의 임베딩 수 = %d, %v", count, err)
			}
		})
	}
}

// TestIndexResultStorageFailureRollsBackTaskIntegration은 조건부 작업 삭제 뒤
// 임베딩 저장이 실패하면 기존 임베딩은 보존하고 작업은 재시도 예산에 반영하는지 확인한다.
func TestIndexResultStorageFailureRollsBackTaskIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createIndexScopeGraph(t, database, actorID, "rollback")
	vector := make([]float64, embeddingDimension(t, database))
	vector[0] = 1
	initial, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, func(context.Context, IndexTask) IndexTaskResult {
		return IndexTaskResult{Embedding: vector, ModelID: "rollback-test"}
	})
	if err != nil || !initial.Succeeded {
		t.Fatalf("기존 임베딩 준비 = %+v, %v", initial, err)
	}
	target, err := database.Context(t.Context(), graphID, initial.Task.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.writeTransaction(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := database.enqueueIndexTask(t.Context(), tx, graphID, target.ID, target.Layer); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	readyIndexTasks(t, database, graphID, initial.Task.ContextID)
	var claimed indexTaskStateValue
	var revision int64
	_, err = database.ProcessNextIndexTaskInGraph(t.Context(), graphID, func(ctx context.Context, task IndexTask) IndexTaskResult {
		claimed = indexTaskState(t, database, task.ContextID)
		if err := database.pool.QueryRow(ctx, `SELECT content_revision FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		return IndexTaskResult{Embedding: make([]float64, len(vector)+1), ModelID: "new-model"}
	})
	if err == nil {
		t.Fatal("잘못된 차원의 임베딩 저장이 성공했다")
	}
	if after := indexTaskState(t, database, initial.Task.ContextID); after.attempts != claimed.attempts+1 || after.state != "pending" || after.lastError == "" || !after.nextAttempt.After(time.Now().UTC()) {
		t.Fatalf("저장 실패가 재시도 예산에 반영되지 않았다: %+v", after)
	}
	var embedding, modelID string
	if err := database.pool.QueryRow(t.Context(), `SELECT embedding::text, model_id FROM public.context_embedding WHERE context_id = $1`, initial.Task.ContextID.String()).Scan(&embedding, &modelID); err != nil || embedding != vectorText(vector) || modelID != "rollback-test" {
		t.Fatalf("저장 실패 뒤 기존 임베딩 = %s, %s, %v", embedding, modelID, err)
	}
	var afterRevision int64
	if err := database.pool.QueryRow(t.Context(), `SELECT content_revision FROM public.context_graph WHERE graph_id = $1`, graphID.String()).Scan(&afterRevision); err != nil || afterRevision != revision {
		t.Fatalf("저장 실패 뒤 내용 판 = %d, want %d, %v", afterRevision, revision, err)
	}
}

// TestIndexProviderCallDoesNotBlockSavesIntegration은 제공자 호출이 저장 경로를 막지
// 않는지 확인한다. 작업 행 잠금을 쥔 채 제공자를 부르면 같은 컨텍스트의 저장이 제공자
// 응답까지 기다리게 되어 「색인 처리」의 "대역이 응답을 늦춰도 저장 지연이 늘지 않아야
// 한다"가 성립하지 않는다.
func TestIndexProviderCallDoesNotBlockSavesIntegration(t *testing.T) {
	database := newIntegrationStore(t)
	actorID := newTestID(t)
	createTestAccount(t, database, actorID)
	graphID := createTestGraph(t, database, actorID)
	source, err := database.CreateContext(t.Context(), graphID, testSourceContext(t, graphID, actorID, "https://example.test/lease-"+graphID.String()), nil)
	if err != nil {
		t.Fatalf("원천 생성: %v", err)
	}
	// 색인 대상은 본문을 고칠 수 있는 파생으로 둔다. 같은 컨텍스트의 수정이 색인 작업을
	// 다시 등록하므로 작업 행에서 실제로 부딪힌다.
	target, err := database.CreateContext(t.Context(), graphID, testDerivedContext(t, graphID, actorID), []model.ID{source.ID})
	if err != nil {
		t.Fatalf("파생 생성: %v", err)
	}

	// 공유 개발 데이터베이스에는 다른 테스트가 남긴 대기 작업이 있다. 대상 작업만 앞으로
	// 옮기면 남은 행 중에 더 앞선 시각을 가진 것이 있을 때 그쪽이 먼저 잡힌다. 나머지
	// 대기 작업을 함께 미루는 readyIndexTasks로 확보 순서를 고정한다.
	//
	// 옮긴 표식은 실패한 회차가 남기면 다음 회차를 가로채므로 반드시 되돌린다. 이 테스트가
	// 만든 컨텍스트의 행만 지우므로 다른 테스트의 대기 작업은 건드리지 않는다.
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := database.pool.Exec(ctx, `DELETE FROM public.index_task WHERE context_id = ANY($1::uuid[])`, []string{source.ID.String(), target.ID.String()}); err != nil {
			t.Errorf("색인 작업 정리: %v", err)
		}
	})
	readyIndexTasks(t, database, graphID, target.ID)

	const providerDelay = 2 * time.Second
	saved := make(chan time.Duration, 1)
	processor := func(ctx context.Context, task IndexTask) IndexTaskResult {
		if task.ContextID != target.ID {
			t.Errorf("대상이 아닌 작업을 잡았다: %s", task.ContextID)
			return IndexTaskResult{Failure: "대상 아님", Retryable: true}
		}
		// 제공자가 응답하는 동안 같은 컨텍스트의 수정이 진행되는지 잰다. 수정은 색인
		// 작업을 다시 등록하므로 작업 행 잠금을 쥐고 있으면 여기에서 막힌다.
		go func() {
			started := time.Now()
			next := target
			next.Body = "제공자 호출 중 수정한 본문"
			next.Version = target.Version + 1
			if _, err := database.UpdateContext(context.WithoutCancel(ctx), graphID, target.Version, next); err != nil {
				t.Errorf("제공자 호출 중 저장: %v", err)
			}
			saved <- time.Since(started)
		}()
		time.Sleep(providerDelay)
		// 배포 구성의 차원과 맞춰야 임베딩 열에 들어간다.
		embedding := make([]float64, embeddingDimension(t, database))
		embedding[0] = 1
		return IndexTaskResult{Embedding: embedding, ModelID: "lease-test"}
	}

	result, err := database.ProcessNextIndexTaskInGraph(t.Context(), graphID, processor)
	if err != nil {
		t.Fatalf("색인 작업 처리: %v", err)
	}
	if !result.Found || result.Task.ContextID != target.ID {
		t.Fatalf("확보한 작업 = %#v; 대상 %s를 잡아야 한다", result, target.ID)
	}

	select {
	case elapsed := <-saved:
		// 제공자 지연의 절반을 넘으면 저장이 제공자 응답을 기다린 것이다.
		if elapsed > providerDelay/2 {
			t.Fatalf("제공자 호출 중 저장에 %v가 걸렸다; 제공자 지연 %v를 기다렸다", elapsed, providerDelay)
		}
	case <-time.After(providerDelay * 3):
		t.Fatal("제공자 호출 중 저장이 끝나지 않았다")
	}
}

// embeddingDimension은 배포가 만든 임베딩 열의 차원을 읽는다.
func embeddingDimension(t testing.TB, database *Store) int {
	t.Helper()
	var dimension int
	if err := database.pool.QueryRow(t.Context(), `
		SELECT COALESCE(substring(format_type(atttypid, atttypmod) FROM '[0-9]+')::int, 0)
		FROM pg_attribute
		WHERE attrelid = 'public.context_embedding'::regclass AND attname = 'embedding'`).Scan(&dimension); err != nil || dimension <= 0 {
		t.Fatalf("임베딩 차원 조회 = %d, %v", dimension, err)
	}
	return dimension
}
