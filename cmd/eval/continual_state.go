package main

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
)

// 품질과 별개로 실제 저장 상태와 반환 근거를 대조한다. 본문·식별자는 보고서에 싣지 않는다.
func verifyContinualState(ctx context.Context, database *store.Store, graph loadedGraph, scenario continualScenario, baseline, current search.Flow) (map[string]bool, error) {
	checks := map[string]bool{}
	answer := graph.Keys["answer"]
	checks["answer_retrievable"] = flowContains(baseline, answer) && flowContains(current, answer)
	switch scenario.name {
	case "online_update", "replay", "recovery":
		value, err := database.Context(ctx, graph.GraphID, answer)
		if err != nil {
			return nil, err
		}
		for _, item := range baseline.Contexts {
			if item.Value.ID == answer {
				before := item.Value
				if scenario.name == "recovery" {
					before.Version = value.Version
				}
				checks["answer_content_preserved"] = reflect.DeepEqual(before, value)
			}
		}
		checks["answer_active"] = value.DeletedAt == nil
		if scenario.name == "online_update" {
			for i := range 3 {
				added, err := database.Context(ctx, graph.GraphID, graph.Keys[fmt.Sprintf("online-noise-%d", i)])
				if err != nil {
					return nil, err
				}
				checks[fmt.Sprintf("added_%d_active", i)] = added.Layer == model.LayerSource && added.DeletedAt == nil
			}
		}
	case "transfer":
		evidence := graph.Keys["evidence"]
		checks["baseline_evidence_path"] = continualEvidencePath(baseline, answer, evidence)
		checks["current_evidence_path"] = continualEvidencePath(current, answer, evidence)
		event, err := database.Context(ctx, graph.GraphID, graph.Keys["transfer-event"])
		if err != nil {
			return nil, err
		}
		checks["new_event_uses_knowledge"] = event.DeletedAt == nil && event.Event != nil && slices.Contains(event.Event.MemberIDs, answer) && slices.Contains(event.Event.MemberIDs, graph.Keys["transfer-target"])
	case "forgetting":
		expiring, discarded := graph.Keys["expiring"], graph.Keys["discarded"]
		value, err := database.Context(ctx, graph.GraphID, expiring)
		if err != nil {
			return nil, err
		}
		checks["expired_direct_read"] = value.DeletedAt == nil && value.Derived != nil && value.Derived.ValidTo != nil && value.Derived.ValidTo.Before(*scenario.query.AsOf)
		checks["default_excludes_expired_and_discarded"] = !flowContains(current, expiring) && !flowContains(current, discarded)
		deleted, err := database.Context(ctx, graph.GraphID, discarded)
		if err != nil {
			return nil, err
		}
		checks["discarded_state"] = deleted.DeletedAt != nil
		// 지정한 과거 시점에는 만료 전 파생을 포함하고, 폐기 상태는 시각과 무관하게 제외한다.
		if value.Derived == nil || value.Derived.ValidTo == nil {
			return checks, nil
		}
		past, err := database.TimeCandidates(ctx, graph.GraphID, value.Derived.ValidTo.AddDate(0, 0, -1), 50)
		if err != nil {
			return nil, err
		}
		checks["past_includes_expired"] = false
		checks["past_excludes_discarded"] = true
		for _, candidate := range past {
			if candidate.Context.ID == expiring {
				checks["past_includes_expired"] = true
			}
			if candidate.Context.ID == discarded {
				checks["past_excludes_discarded"] = false
			}
		}
		hops, err := database.HopContexts(ctx, graph.GraphID, answer, 1, "in", []string{"derived_from"}, 10)
		if err != nil {
			return nil, err
		}
		checks["expired_hop_path"] = false
		for _, edge := range hops.Edges {
			if edge.FromID == expiring && edge.ToID == answer && edge.Kind == "derived_from" {
				checks["expired_hop_path"] = true
			}
		}
	case "conflict_resolution":
		disputed := graph.Keys["disputed"]
		checks["opinions_returned_together"] = flowContains(baseline, answer) && flowContains(baseline, disputed)
		checks["confidence_transition"] = confidenceInFlow(baseline, disputed) == model.ConfidenceStateDisputed && confidenceInFlow(current, disputed) == model.ConfidenceStateUncertain
		checks["supported_opinion_preserved"] = confidenceInFlow(current, answer) == model.ConfidenceStateSupported
	default:
		return nil, fmt.Errorf("알 수 없는 지속 평가 시나리오 %q", scenario.name)
	}
	return checks, nil
}

func continualEvidencePath(flow search.Flow, answer, evidence model.ID) bool {
	if !flowContains(flow, answer) || !flowContains(flow, evidence) {
		return false
	}
	return slices.Contains(flow.Edges, model.HopEdge{FromID: answer, ToID: evidence, Kind: "derived_from"})
}

func verifyContinualJudgments(ctx context.Context, database *store.Store, graph loadedGraph, scenario continualScenario) (map[string]bool, error) {
	checks := make(map[string]bool, len(scenario.judgments))
	for _, judgment := range scenario.judgments {
		found, err := database.HasAppliedOperationJudgment(ctx, graph.GraphID, judgment)
		if err != nil {
			return nil, err
		}
		checks[judgment] = found
	}
	return checks, nil
}
