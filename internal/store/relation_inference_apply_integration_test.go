package store

import (
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/relinfer"
)

const testInferenceProfile = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testInferenceOtherProfile = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type inferenceApplyFixture struct {
	db                *Store
	graph, actor      model.ID
	target, candidate model.Context
	task              model.RelationInferenceTask
}

func TestInferenceProcessorPreparedToCommitIntegration(t *testing.T) {
	db := newIsolatedIntegrationStore(t)
	graph, actor, member := lifeProposalFixture(t, db)
	candidateInput := testEventContext(t, graph, actor, member.ID)
	candidateInput.Body = "candidate evidence"
	candidate, err := db.CreateContext(t.Context(), graph, candidateInput, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.inferenceEnabled = true
	targetInput := testEventContext(t, graph, actor, member.ID)
	targetInput.Body = "target evidence"
	target, err := db.CreateContextWithOperation(t.Context(), graph, targetInput, nil, inferenceCreateOperation(targetInput), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	profile := relinfer.Profile{Provider: "fake", Model: "fake", PromptVersion: "1", SchemaVersion: "1", EmbeddingModel: "test", IndexWait: time.Minute, AdjacencyWindow: time.Hour, Channels: []string{"member"}, CandidateLimit: 4, TargetChars: 100, CandidateChars: 100, InputChars: 3000}
	output := `{"schema_version":"1","decisions":[{"candidate":"C1","relation_type":"relates_to","decision":"relation","direction":"","evidence":{"target":"target","candidate":"candidate"},"reason":""},{"candidate":"C1","relation_type":"causes","decision":"none","direction":"C_to_T","evidence":{"target":"","candidate":""},"reason":""}]}`
	processor := &relinfer.DecisionProcessor{Builder: relinfer.InputBuilder{Source: db, Profile: profile}, Judge: relinfer.StaticJudge{Result: relinfer.JudgeResult{Output: []byte(output)}}, Sink: db, Mode: "propose"}
	worker, err := relinfer.New(db, processor, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOnce(t.Context())
	if err != nil || !worked {
		t.Fatalf("회차 실행: %v %v", worked, err)
	}
	relations, _, err := db.ListRelations(t.Context(), graph, "", 10)
	if err != nil || len(relations) != 1 || relations[0].State != model.RelationStateProposed || relations[0].Type != model.RelationTypeRelatesTo || relations[0].InferenceDecisionID.IsZero() {
		t.Fatalf("반영 결과: %+v %v", relations, err)
	}
	if relations[0].FromContextID != target.ID && relations[0].ToContextID != target.ID || relations[0].FromContextID != candidate.ID && relations[0].ToContextID != candidate.ID {
		t.Fatal("다른 사건에 관계를 반영했다")
	}
}

func newInferenceApplyFixture(t *testing.T) inferenceApplyFixture {
	t.Helper()
	db := newIsolatedIntegrationStore(t)
	graph, actor, member := lifeProposalFixture(t, db)
	candidate, err := db.CreateContext(t.Context(), graph, testEventContext(t, graph, actor, member.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	db.inferenceEnabled = true
	value := testEventContext(t, graph, actor, member.ID)
	target, err := db.CreateContextWithOperation(t.Context(), graph, value, nil, inferenceCreateOperation(value), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	task, found, err := db.ClaimRelationInferenceTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatalf("작업 확보: %v %v", found, err)
	}
	return inferenceApplyFixture{db, graph, actor, target, candidate, task}
}
func (f inferenceApplyFixture) request(t *testing.T, mode, accepted string) model.InferenceApplication {
	t.Helper()
	return model.InferenceApplication{Task: f.task, RunID: newTestID(t), ProfileFingerprint: testInferenceProfile, ProfileVersions: "{}", Candidates: "[]", Mode: mode, AcceptedProfile: accepted, AutoConfirmTypes: []model.RelationType{model.RelationTypeRelatesTo}, Attempts: 1, Decisions: []model.InferenceDecision{{CandidateID: f.candidate.ID, CandidateVersion: f.candidate.Version, RelationType: model.RelationTypeRelatesTo, Decision: "relation", TargetEvidence: "대상", CandidateEvidence: "후보"}}}
}
func TestInferenceApplyModesAndPolicyIntegration(t *testing.T) {
	for _, tc := range []struct {
		mode, accepted string
		want           model.RelationState
	}{{"shadow", testInferenceProfile, ""}, {"propose", testInferenceProfile, model.RelationStateProposed}, {"apply", testInferenceProfile, model.RelationStateConfirmed}, {"apply", testInferenceOtherProfile, model.RelationStateProposed}} {
		t.Run(tc.mode+"/"+tc.accepted, func(t *testing.T) {
			f := newInferenceApplyFixture(t)
			req := f.request(t, tc.mode, tc.accepted)
			stale, err := f.db.ApplyInferenceResult(t.Context(), req)
			if err != nil || stale {
				t.Fatalf("반영 %v %v", stale, err)
			}
			relations, _, err := f.db.ListRelations(t.Context(), f.graph, "", 20)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(relations) != 0 {
					t.Fatalf("shadow 관계 %+v", relations)
				}
			} else {
				if len(relations) != 1 || relations[0].State != tc.want || relations[0].ProposedBy != model.ProposalSourceInference || !relations[0].InferenceDecisionID.IsV7() {
					t.Fatalf("관계 %+v", relations)
				}
				if tc.want == model.RelationStateConfirmed && (relations[0].ConfirmedSource != model.ConfirmationSourceInference || relations[0].ConfirmedBy != f.actor || !relations[0].ConfirmedByAgent.IsZero()) {
					t.Fatalf("확정 출처 %+v", relations[0])
				}
			}
			var count int
			if err := f.db.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.relation_inference_task WHERE task_id=$1`, f.task.ID.String()).Scan(&count); err != nil || count != 0 {
				t.Fatalf("작업 삭제 %d %v", count, err)
			}
			if tc.mode == "apply" && tc.accepted != testInferenceProfile {
				var reason *string
				if err := f.db.pool.QueryRow(t.Context(), `SELECT validation_reason FROM public.relation_inference_decision WHERE run_id=$1`, req.RunID.String()).Scan(&reason); err != nil || reason == nil || *reason != "policy_downgraded" {
					t.Fatalf("정책 강등 %v %v", reason, err)
				}
			}
		})
	}
}
func TestInferenceApplyStaleAndRollbackIntegration(t *testing.T) {
	for _, scenario := range []string{"stale", "target_changed", "candidate_changed", "identity_settled", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			f := newInferenceApplyFixture(t)
			req := f.request(t, "apply", testInferenceProfile)
			switch scenario {
			case "stale":
				if _, err := f.db.pool.Exec(t.Context(), `UPDATE public.relation_inference_task SET enqueued_at=enqueued_at+interval '1 second' WHERE task_id=$1`, f.task.ID.String()); err != nil {
					t.Fatal(err)
				}
			case "target_changed":
				req.Task.TargetVersion++
			case "candidate_changed":
				req.Decisions[0].CandidateVersion++
			case "identity_settled":
				if _, err := f.db.ConfirmRelation(t.Context(), f.graph, confirmable(t, f.graph, model.RelationTypeRelatesTo, f.target.ID, f.candidate.ID), nil); err != nil {
					t.Fatal(err)
				}
			case "rollback":
				remove := lifeFailGraphWrite(t, f.db, f.graph, "operation_log", "INSERT", false)
				defer remove()
			}
			stale, err := f.db.ApplyInferenceResult(t.Context(), req)
			if scenario == "rollback" {
				if err == nil {
					t.Fatal("감사 기록 실패를 허용했다")
				}
				var count int
				if e := f.db.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.relation_inference_run WHERE run_id=$1`, req.RunID.String()).Scan(&count); e != nil || count != 0 {
					t.Fatalf("판정 롤백 %d %v", count, e)
				}
				if e := f.db.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.relation_inference_task WHERE task_id=$1`, f.task.ID.String()).Scan(&count); e != nil || count != 1 {
					t.Fatalf("작업 롤백 %d %v", count, e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if stale != (scenario == "stale" || scenario == "target_changed") {
				t.Fatalf("이전 확보분=%v", stale)
			}
			if stale {
				var reason string
				if e := f.db.pool.QueryRow(t.Context(), `SELECT COALESCE(validation_reason,'') FROM public.relation_inference_decision WHERE run_id=$1`, req.RunID.String()).Scan(&reason); e != nil || reason != "stale" {
					t.Fatalf("이전 판정 기록 %q %v", reason, e)
				}
				if scenario == "stale" {
					snapshot, e := f.db.InferenceCandidates(t.Context(), model.InferenceCandidateRequest{Task: f.task, ProfileFingerprint: req.ProfileFingerprint, EmbeddingModel: "test", Channels: []string{"member"}, Limit: 10, IndexWait: time.Minute, AdjacencyWindow: time.Hour})
					if e != nil || len(snapshot.Channels) != 1 || len(snapshot.Channels[0].Candidates) != 1 || snapshot.Channels[0].Candidates[0].Event.ID != f.candidate.ID {
						t.Fatalf("이전 판정 뒤 후보 재검색: %+v %v", snapshot, e)
					}
				}
			}
			if !stale {
				var reason string
				if e := f.db.pool.QueryRow(t.Context(), `SELECT COALESCE(validation_reason,'') FROM public.relation_inference_decision WHERE run_id=$1`, req.RunID.String()).Scan(&reason); e != nil || reason != scenario {
					t.Fatalf("거부 %s %q %v", scenario, reason, e)
				}
			}
		})
	}
}

func TestInferenceApplyDelegationAndDuplicateWorkersIntegration(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		f := newInferenceApplyFixture(t)
		editor := newTestID(t)
		createTestAccount(t, f.db, editor)
		grantAccount(t, f.db, f.graph, editor, model.GraphGradeEditor)
		if _, err := f.db.pool.Exec(t.Context(), `UPDATE public.relation_inference_task SET trigger_account_id=$2 WHERE task_id=$1`, f.task.ID.String(), editor.String()); err != nil {
			t.Fatal(err)
		}
		f.task.TriggerAccountID = editor
		if err := f.db.RevokeGraphGrant(t.Context(), f.graph, GrantSubjectAccount, editor); err != nil {
			t.Fatal(err)
		}
		req := f.request(t, "apply", testInferenceProfile)
		stale, err := f.db.ApplyInferenceResult(t.Context(), req)
		if err != nil || stale {
			t.Fatalf("권한 재검사 %v %v", stale, err)
		}
		var reason string
		if err := f.db.pool.QueryRow(t.Context(), `SELECT validation_reason FROM public.relation_inference_decision WHERE run_id=$1`, req.RunID.String()).Scan(&reason); err != nil || reason != "delegation_invalid" {
			t.Fatalf("권한 회수 %q %v", reason, err)
		}
		relations, _, err := f.db.ListRelations(t.Context(), f.graph, "", 20)
		if err != nil || len(relations) != 0 {
			t.Fatalf("권한 없는 관계 %+v %v", relations, err)
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		f := newInferenceApplyFixture(t)
		requests := []model.InferenceApplication{f.request(t, "apply", testInferenceProfile), f.request(t, "apply", testInferenceProfile)}
		type outcome struct {
			stale bool
			err   error
		}
		start := make(chan struct{})
		done := make(chan outcome, 2)
		for _, request := range requests {
			go func() {
				<-start
				stale, err := f.db.ApplyInferenceResult(t.Context(), request)
				done <- outcome{stale, err}
			}()
		}
		close(start)
		staleCount := 0
		for range 2 {
			result := <-done
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.stale {
				staleCount++
			}
		}
		if staleCount != 1 {
			t.Fatalf("이전 확보분 %d", staleCount)
		}
		relations, _, err := f.db.ListRelations(t.Context(), f.graph, "", 20)
		if err != nil || len(relations) != 1 || relations[0].State != model.RelationStateConfirmed {
			t.Fatalf("중복 반영 %+v %v", relations, err)
		}
	})
}

func TestInferenceApplyCausesAndDiscardedIdentityIntegration(t *testing.T) {
	t.Run("causes_remains_proposed", func(t *testing.T) {
		f := newInferenceApplyFixture(t)
		req := f.request(t, "apply", testInferenceProfile)
		req.Decisions[0].RelationType = model.RelationTypeCauses
		req.Decisions[0].Direction = "C_to_T"
		stale, err := f.db.ApplyInferenceResult(t.Context(), req)
		if err != nil || stale {
			t.Fatalf("인과 반영 %v %v", stale, err)
		}
		relations, _, err := f.db.ListRelations(t.Context(), f.graph, "", 20)
		if err != nil || len(relations) != 1 || relations[0].State != model.RelationStateProposed || relations[0].Type != model.RelationTypeCauses {
			t.Fatalf("인과 상한 %+v %v", relations, err)
		}
	})
	t.Run("discarded_identity", func(t *testing.T) {
		f := newInferenceApplyFixture(t)
		relation, err := f.db.ConfirmRelation(t.Context(), f.graph, confirmable(t, f.graph, model.RelationTypeRelatesTo, f.target.ID, f.candidate.ID), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.DiscardRelation(t.Context(), f.graph, relation.ID, nil); err != nil {
			t.Fatal(err)
		}
		req := f.request(t, "propose", "")
		stale, err := f.db.ApplyInferenceResult(t.Context(), req)
		if err != nil || stale {
			t.Fatalf("폐기 정체성 %v %v", stale, err)
		}
		var reason string
		if err := f.db.pool.QueryRow(t.Context(), `SELECT validation_reason FROM public.relation_inference_decision WHERE run_id=$1`, req.RunID.String()).Scan(&reason); err != nil || reason != "identity_settled" {
			t.Fatalf("폐기 정체성 %q %v", reason, err)
		}
	})
}

func TestInferenceApplyPartOfSignalRecheckedIntegration(t *testing.T) {
	db := newIsolatedIntegrationStore(t)
	graph, actor, source := boundaryGraph(t, db)
	member := lifeDerived(t, db, graph, actor, source.ID)
	second := lifeDerived(t, db, graph, actor, source.ID)
	start := time.Now().UTC().Add(-time.Minute)
	candidate := proposalEvent(t, db, graph, actor, []model.ID{member.ID, second.ID}, start, nil)
	db.inferenceEnabled = true
	value := testEventContext(t, graph, actor, member.ID)
	target, err := db.CreateContextWithOperation(t.Context(), graph, value, nil, inferenceCreateOperation(value), WriteLimits{})
	if err != nil {
		t.Fatal(err)
	}
	task, found, err := db.ClaimRelationInferenceTask(t.Context(), time.Minute)
	if err != nil || !found {
		t.Fatalf("작업 확보: %v %v", found, err)
	}
	f := inferenceApplyFixture{db, graph, actor, target, candidate, task}
	req := f.request(t, "apply", testInferenceProfile)
	req.AutoConfirmTypes = []model.RelationType{model.RelationTypePartOf}
	req.Decisions[0].RelationType = model.RelationTypePartOf
	req.Decisions[0].Direction = "T_to_C"
	stale, err := db.ApplyInferenceResult(t.Context(), req)
	if err != nil || stale {
		t.Fatalf("부분 관계 반영 %v %v", stale, err)
	}
	relations, _, err := db.ListRelations(t.Context(), graph, "", 10)
	if err != nil || len(relations) != 1 || relations[0].Type != model.RelationTypePartOf || relations[0].State != model.RelationStateConfirmed || relations[0].FromContextID != target.ID || relations[0].ToContextID != candidate.ID {
		t.Fatalf("부분 관계 %+v %v", relations, err)
	}
}
func TestInferenceApplyInvalidEvidenceIsRejectedIntegration(t *testing.T) {
	f := newInferenceApplyFixture(t)
	req := f.request(t, "apply", testInferenceProfile)
	req.Decisions[0].InvalidReason = "evidence_unverified"
	stale, err := f.db.ApplyInferenceResult(t.Context(), req)
	if err != nil || stale {
		t.Fatalf("인용 거부 %v %v", stale, err)
	}
	var reason string
	if err := f.db.pool.QueryRow(t.Context(), `SELECT validation_reason FROM public.relation_inference_decision WHERE run_id=$1`, req.RunID.String()).Scan(&reason); err != nil || reason != "evidence_unverified" {
		t.Fatalf("인용 거부 사유 %q %v", reason, err)
	}
	relations, _, err := f.db.ListRelations(t.Context(), f.graph, "", 10)
	if err != nil || len(relations) != 0 {
		t.Fatalf("인용 위반 반영 %+v %v", relations, err)
	}
}

func TestInferenceApplyEndpointChangedAfterClaimIntegration(t *testing.T) {
	for _, scenario := range []string{"target_changed", "candidate_changed", "endpoint_inactive"} {
		t.Run(scenario, func(t *testing.T) {
			f := newInferenceApplyFixture(t)
			req := f.request(t, "apply", testInferenceProfile)
			f.db.inferenceEnabled = false
			switch scenario {
			case "target_changed", "candidate_changed":
				original := f.target
				if scenario == "candidate_changed" {
					original = f.candidate
				}
				changed := original
				changed.Body += " changed"
				changed.Version++
				if _, err := f.db.UpdateContext(t.Context(), f.graph, original.Version, changed); err != nil {
					t.Fatal(err)
				}
			case "endpoint_inactive":
				if _, err := f.db.DiscardContext(t.Context(), f.graph, f.candidate.ID, nil, WriteLimits{}); err != nil {
					t.Fatal(err)
				}
			}
			stale, err := f.db.ApplyInferenceResult(t.Context(), req)
			if err != nil || stale {
				t.Fatalf("갱신 뒤 반영 %v %v", stale, err)
			}
			var reason string
			if err := f.db.pool.QueryRow(t.Context(), `SELECT validation_reason FROM public.relation_inference_decision WHERE run_id=$1`, req.RunID.String()).Scan(&reason); err != nil || reason != scenario {
				t.Fatalf("갱신 거부 %q %v", reason, err)
			}
			relations, _, err := f.db.ListRelations(t.Context(), f.graph, "", 10)
			if err != nil || len(relations) != 0 {
				t.Fatalf("오래된 반영 %+v %v", relations, err)
			}
		})
	}
}
