package main

import (
	"fmt"
	"time"
)

// 실행은 후속 평가 작업이 담당한다. 데이터 준비에서는 연산과 기대 상태만 고정한다.
type supplementStep struct {
	Action   string `json:"action"`
	Actor    string `json:"actor_code"`
	Graph    string `json:"graph_code"`
	Target   string `json:"target_key"`
	Related  string `json:"related_key,omitzero"`
	Expected string `json:"expected"`
}

type supplementActor struct {
	Code    string   `json:"actor_code"`
	Account string   `json:"account_code"`
	Role    string   `json:"initial_role"`
	Graphs  []string `json:"graph_codes"`
}

func prepareSupplement() (preparedData, error) {
	data := newPreparedData("own-supplement-v1", nil, 6)
	kinds := []string{"event", "evidence-state", "isolation", "recovery", "contamination", "handover"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, kind := range kinds {
		unit := fmt.Sprintf("supp-t%02d", i+1)
		contexts := ownGroupContexts(i, "합성 검토 과업", false, base.Add(time.Duration(i)*72*time.Hour))
		relations := []relationSpec{{Type: "part_of", From: ownKey(i, "e1"), To: ownKey(i, "ew")}, {Type: "part_of", From: ownKey(i, "e2"), To: ownKey(i, "ew")}, {Type: "precedes", From: ownKey(i, "ew"), To: ownKey(i, "ef")}}
		data.Memory.Groups = append(data.Memory.Groups, preparedGroup{Code: unit, Contexts: contextSet{Version: "own-supplement-v1", Contexts: contexts, Relations: relations}})
		source, derived, event := ownKey(i, "s00"), ownKey(i, "d0"), ownKey(i, "ew")
		step := func(action, target, related, expected string) supplementStep {
			return supplementStep{Action: action, Actor: "actor-a", Graph: unit, Target: target, Related: related, Expected: expected}
		}
		var steps []supplementStep
		switch kind {
		case "event":
			steps = []supplementStep{step("inspect", event, source, "member_ref_preserved"), step("inspect", ownKey(i, "e1"), event, "part_of_confirmed")}
		case "evidence-state":
			steps = []supplementStep{step("node_discard", source, "", "discarded"), step("inspect", derived, source, "evidence_invalidated"), step("node_keep", derived, "", "judgment_recorded")}
		case "isolation":
			other := step("search", source, "", "absent_from_other_graph")
			other.Graph = "supp-isolated"
			steps = []supplementStep{other, step("inspect", source, "", "active_in_owner_graph")}
			data.Memory.Groups = append(data.Memory.Groups, preparedGroup{Code: "supp-isolated", Contexts: contextSet{Version: "own-supplement-v1", Contexts: []contextSpec{{Key: "isolated-source", Layer: "source", Body: "다른 그래프의 독립 합성 기록", Source: &sourceSpec{Channel: "conversation", Locator: "urn:eval:supp-isolated", OccurredAt: base, OriginKind: "user_utterance"}}}}})
		case "recovery":
			steps = []supplementStep{step("node_discard", source, "", "discarded"), step("node_restore", source, "", "active"), step("inspect", derived, source, "evidence_invalidated_until_review")}
		case "contamination":
			// 외부 원천과 그 파생의 전파 범위를 별도로 판정한다.
			data.Memory.Groups[len(data.Memory.Groups)-1].Contexts.Contexts[0].Source.OriginKind = "external_content"
			steps = []supplementStep{step("trace", source, derived, "origins_include_external_content"), step("node_discard", source, "", "discarded_without_cascade"), step("inspect", derived, source, "evidence_invalidated")}
		case "handover":
			grant := step("grant_owner", "account-b", "", "two_owners")
			revoke := step("revoke_owner", "account-a", "", "account_b_owner_only")
			denied := step("manage", derived, "", "permission_denied")
			allowed := step("manage", derived, "", "allowed")
			allowed.Actor = "actor-b"
			steps = []supplementStep{grant, revoke, denied, allowed}
		}
		q := preparedQuery{Code: unit + "-q01", Group: unit, Unit: unit, Type: kind, Question: "합성 과업의 상태와 근거 계약을 검증한다.", AnswerStatus: "scenario_expectations", EvidenceStatus: "annotated", Evidence: []string{source, derived, event}, AsOf: base.Add(time.Duration(i+1) * 72 * time.Hour), Steps: steps}
		q.Actors = []supplementActor{{Code: "actor-a", Account: "account-a", Role: "owner", Graphs: []string{unit}}}
		if kind == "isolation" {
			q.Actors[0].Graphs = append(q.Actors[0].Graphs, "supp-isolated")
		}
		if kind == "handover" {
			q.Actors = append(q.Actors, supplementActor{Code: "actor-b", Account: "account-b", Role: "none", Graphs: []string{unit}})
		}
		data.addQuery(q, "", unit)
	}
	return data, nil
}
