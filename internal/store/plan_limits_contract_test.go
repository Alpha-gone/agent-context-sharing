package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
)

func TestPlanStorageAccountingIntegration(t *testing.T) {
	database := newSmallPoolStore(t)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	grantAccount(t, database, graph, actor, model.GraphGradeOwner)
	source := testSourceContext(t, graph, actor, "api://perm05/"+graph.String())
	source.Body = "한글🙂a"
	if _, err := database.CreateContextWithOperation(t.Context(), graph, source, nil, nil, WriteLimits{StoredCharsPerGraph: 4}); err != nil {
		t.Fatal(err)
	}
	node := testDerivedContext(t, graph, actor)
	node.Body = "가나다🙂"
	node, err := database.CreateContextWithOperation(t.Context(), graph, node, []model.ID{source.ID}, nil, WriteLimits{StoredCharsPerGraph: 8})
	if err != nil {
		t.Fatal(err)
	}
	assertPlanStoredChars(t, database, graph, 8)
	next := node
	next.Version++
	next.Body += "추"
	before := planStateFingerprint(t, database, graph)
	_, err = database.UpdateContextWithOperation(t.Context(), graph, node.Version, next, registrationOperation(next, OperationUpdate), WriteLimits{StoredCharsPerGraph: 8})
	assertPlanLimit(t, err, 8, 8)
	if after := planStateFingerprint(t, database, graph); before != after {
		t.Fatal("거부한 확장 뒤 정점·누계·색인·감사가 달라졌다")
	}
	// 하향 뒤 축소와 동일 크기의 수정은 허용한다. 실제 활성 합계는 유지한다.
	next.Body = "🙂"
	node, err = database.UpdateContextWithOperation(t.Context(), graph, node.Version, next, nil, WriteLimits{StoredCharsPerGraph: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertPlanStoredChars(t, database, graph, 5)
	next = node
	next.Version++
	node, err = database.UpdateContextWithOperation(t.Context(), graph, node.Version, next, nil, WriteLimits{StoredCharsPerGraph: 2})
	if err != nil {
		t.Fatal(err)
	}
	newSource := testSourceContext(t, graph, actor, "api://perm05/new/"+graph.String())
	newSource.Body = "한"
	before = planStateFingerprint(t, database, graph)
	_, err = database.CreateContextWithOperation(t.Context(), graph, newSource, nil, nil, WriteLimits{StoredCharsPerGraph: 2})
	assertPlanLimit(t, err, 5, 2)
	if before != planStateFingerprint(t, database, graph) {
		t.Fatal("거부한 생성이 상태를 바꿨다")
	}
	if _, err := database.CreateContextWithOperation(t.Context(), graph, newSource, nil, nil, WriteLimits{StoredCharsPerGraph: 6}); err != nil {
		t.Fatal(err)
	}
	assertPlanStoredChars(t, database, graph, 6)
	node, err = database.DiscardContext(t.Context(), graph, node.ID, testMCPDiscardOperation(node), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	assertPlanStoredChars(t, database, graph, 5)
	for _, web := range []bool{false, true} {
		before = planStateFingerprint(t, database, graph)
		if web {
			_, err = database.SetContextDeleted(t.Context(), graph, node.ID, actor, false, 5)
		} else {
			_, err = database.RestoreContext(t.Context(), graph, node.ID, nil, WriteLimits{StoredCharsPerGraph: 5})
		}
		assertPlanLimit(t, err, 5, 5)
		if before != planStateFingerprint(t, database, graph) {
			t.Fatal("거부한 복구의 삭제 상태·작업·감사가 달라졌다")
		}
	}
	// 일반 갱신의 복구·삭제 상태 우회와 삭제 본문 수정은 허용하지 않는다.
	for _, restore := range []bool{false, true} {
		next = node
		next.Version++
		next.Body += "추"
		if restore {
			next.DeletedAt = nil
		}
		_, err = database.UpdateContextWithOperation(t.Context(), graph, node.Version, next, nil, WriteLimits{})
		if !errors.Is(err, ErrInvalidState) {
			t.Fatalf("삭제 본문 갱신/우회 복구=%v", err)
		}
	}
	node, err = database.RestoreContext(t.Context(), graph, node.ID, nil, WriteLimits{StoredCharsPerGraph: 6})
	if err != nil {
		t.Fatal(err)
	}
	assertPlanStoredChars(t, database, graph, 6)
	next = node
	next.Version++
	next.DeletedAt = new(nowUTC())
	if _, err := database.UpdateContextWithOperation(t.Context(), graph, node.Version, next, nil, WriteLimits{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("일반 갱신의 삭제=%v", err)
	}
	newSource.ID = newTestID(t)
	newSource.DeletedAt = new(nowUTC())
	if _, err := database.CreateContext(t.Context(), graph, newSource, nil); err == nil {
		t.Fatal("삭제 상태의 생성 허용")
	}
	if _, err := database.SetContextDeleted(t.Context(), graph, node.ID, actor, true, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetContextDeleted(t.Context(), graph, node.ID, actor, false, 6); err != nil {
		t.Fatal(err)
	}
	assertPlanStoredChars(t, database, graph, 6)
	assertConnectionRecovery(t, database, nil, 0)
}

func TestPlanParallelGrowthIntegration(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "update"
		if restore {
			name = "restore"
		}
		t.Run(name, func(t *testing.T) {
			database := newSmallPoolStore(t)
			graph, actor, source := boundaryGraph(t, database)
			base := int64(utf8.RuneCountInString(source.Body))
			nodes := make([]model.Context, 3)
			for i := range nodes {
				value := testDerivedContext(t, graph, actor)
				value.Body = "한"
				var err error
				nodes[i], err = database.CreateContext(t.Context(), graph, value, []model.ID{source.ID})
				if err != nil {
					t.Fatal(err)
				}
				if restore {
					nodes[i], err = database.DiscardContext(t.Context(), graph, nodes[i].ID, testMCPDiscardOperation(nodes[i]), WriteLimits{})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if !restore {
				base += 3
			}
			allowed := base + 1
			start := make(chan struct{})
			results := make(chan error, len(nodes))
			var group sync.WaitGroup
			for _, node := range nodes {
				group.Go(func() {
					<-start
					limits := WriteLimits{StoredCharsPerGraph: allowed}
					var err error
					if restore {
						_, err = database.RestoreContext(t.Context(), graph, node.ID, nil, limits)
					} else {
						next := node
						next.Version++
						next.Body = "한🙂"
						_, err = database.UpdateContextWithOperation(t.Context(), graph, node.Version, next, nil, limits)
					}
					results <- err
				})
			}
			close(start)
			group.Wait()
			close(results)
			succeeded := 0
			for err := range results {
				if err == nil {
					succeeded++
				} else {
					assertPlanLimit(t, err, allowed, allowed)
				}
			}
			if succeeded != 1 {
				t.Fatalf("병행 증가 성공=%d, want 1", succeeded)
			}
			assertPlanStoredChars(t, database, graph, allowed)
			assertConnectionRecovery(t, database, nil, 0)
		})
	}
}

func TestPlanNegativeWriteLimits(t *testing.T) {
	graph, actor := newTestID(t), newTestID(t)
	value := testSourceContext(t, graph, actor, "api://perm05/negative")
	for _, sample := range []struct {
		name   string
		limits WriteLimits
	}{
		{"stored_characters_per_graph", WriteLimits{StoredCharsPerGraph: -1}},
		{"graphs_per_account", WriteLimits{GraphsPerAccount: -1}},
		{"writes_per_minute", WriteLimits{WritesPerMinute: -1}},
	} {
		for name, call := range map[string]func(*Store) error{
			"graph": func(s *Store) error {
				_, err := s.CreateGraphWithOwner(t.Context(), model.Graph{}, sample.limits)
				return err
			},
			"create": func(s *Store) error {
				_, err := s.CreateContextWithOperation(t.Context(), graph, value, nil, nil, sample.limits)
				return err
			},
			"update": func(s *Store) error {
				_, err := s.UpdateContextWithOperation(t.Context(), graph, 1, value, nil, sample.limits)
				return err
			},
			"restore": func(s *Store) error {
				_, err := s.RestoreContext(t.Context(), graph, value.ID, nil, sample.limits)
				return err
			},
		} {
			t.Run(sample.name+"/"+name, func(t *testing.T) {
				err := call(&Store{})
				if field, ok := errors.AsType[model.FieldError](err); !ok || field.Field != sample.name {
					t.Fatalf("잘못된 한도=%v", err)
				}
			})
		}
	}
}

func assertPlanLimit(t *testing.T, err error, current, allowed int64) {
	t.Helper()
	value, ok := errors.AsType[plan.LimitError](err)
	if !ok || value.Name != "stored_characters_per_graph" || value.Current != current || value.Allowed != allowed {
		t.Fatalf("저장량 한도 오류=%v, want current=%d allowed=%d", err, current, allowed)
	}
}

func assertPlanStoredChars(t *testing.T, database *Store, graph model.ID, want int64) {
	t.Helper()
	var stored, actual int64
	err := database.pool.QueryRow(t.Context(), `SELECT stored_chars, (SELECT COALESCE(sum(char_length(properties ->> 'body'::text)),0)
		FROM `+database.contextTable()+` WHERE properties ->> 'graph_id'::text=$1::text AND properties ->> 'deleted_at'::text IS NULL)
		FROM public.context_graph WHERE graph_id=$1::uuid`, graph.String()).Scan(&stored, &actual)
	if err != nil || stored != want || actual != want {
		t.Fatalf("활성 문자 누계=%d, 실제=%d, want=%d: %v", stored, actual, want, err)
	}
}

func planStateFingerprint(t *testing.T, database *Store, graph model.ID) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"public.context_graph", "public.index_task", "public.operation_log", "public.web_audit_log", database.contextTable()} {
		where := "graph_id=$1::uuid"
		if table == database.contextTable() {
			where = "properties ->> 'graph_id'::text=$1::text"
		}
		parts = append(parts, `(SELECT COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY row_to_json(t)::text),'') FROM `+table+` t WHERE `+where+`)`)
	}
	var result string
	if err := database.pool.QueryRow(t.Context(), `SELECT md5(`+strings.Join(parts, " || ")+`)`, graph.String()).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
