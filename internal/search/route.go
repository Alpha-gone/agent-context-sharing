// 질의 적응형 라우팅은 scope=auto의 진입 채널 결과만 보고 직접, 국소 확장과 전역 진입 중
// 하나를 고르는 결정적 규칙이다. 「질의 적응형 라우팅」이 정한 순서를 그대로 따른다.
package search

import (
	"slices"

	"agent_context_sharing/internal/model"
)

// Route는 검색 경로다.
type Route = model.SearchRoute

const (
	// RouteDirect는 진입 채널 결과만 결합하고 그래프 확장과 전역 진입을 실행하지 않는다.
	RouteDirect Route = "direct"
	// RouteLocal은 관련 국소 후보에서 확정 참조와 사건 관계를 최대 1홉까지 확장한다.
	RouteLocal Route = "local"
	// RouteGlobal은 전역 요약에서 derived_from을 따라 내려간다.
	RouteGlobal Route = "global"
)

// 이유 코드는 「질의 적응형 라우팅」의 표와 같다. 적응형이 아닌 경로에는 경로를 정한 주체를
// 이유 코드로 남긴다.
const (
	ReasonNoRelevantLocal       = "no_relevant_local"
	ReasonEventAnchor           = "event_anchor"
	ReasonCrossChannelAgreement = "cross_channel_agreement"
	ReasonSemanticSeparation    = "semantic_separation"
	ReasonAmbiguousLocal        = "ambiguous_local"
	ReasonGlobalSummaryAbsent   = "global_summary_absent"
	// ReasonForced는 평가 실행기가 Config.Route로 경로를 강제한 경우다.
	ReasonForced = "forced"
	// ReasonLegacyAuto는 적응형 라우팅을 끈 기존 auto 전환 규칙이 경로를 정한 경우다.
	ReasonLegacyAuto = "legacy_auto"
	// ReasonExplicitScope는 호출자가 local 또는 global을 지정한 경우다.
	ReasonExplicitScope = "explicit_scope"
)

// RouteSignals에 대한 경로 판정은 검색 패키지가 소유한다.
type RouteSignals model.RouteSignals

// Decide는 「질의 적응형 라우팅」의 다섯 규칙을 순서대로 적용한다. 전역 요약 부재에 따른
// 직접 경로 되돌림은 요약을 조회한 뒤에만 알 수 있으므로 여기서 판정하지 않는다.
func (signals RouteSignals) Decide(directThreshold, marginThreshold float64) (Route, string) {
	if !signals.RelevantLocal {
		return RouteGlobal, ReasonNoRelevantLocal
	}
	// 사건 관계는 사건끼리만 이으므로 1위가 사건이면 답이 한 홉 떨어진 사건일 수 있다.
	if signals.TopIsEvent {
		return RouteLocal, ReasonEventAnchor
	}
	if signals.CrossChannelAgreement {
		return RouteDirect, ReasonCrossChannelAgreement
	}
	// 의미 후보가 하나뿐이면 분리 폭을 계산할 근거가 없으므로 이 조건을 성립시키지 않는다.
	if signals.SemanticCount >= 2 && signals.SemanticTop >= directThreshold && signals.SemanticTop-signals.SemanticSecond >= marginThreshold {
		return RouteDirect, ReasonSemanticSeparation
	}
	return RouteLocal, ReasonAmbiguousLocal
}

// localChannelCandidates는 채널 배열의 순서와 무관하게 관련성 판정의 입력을 고른다.
// 실패 채널은 결합과 마찬가지로 라우팅·국소 진입점에서도 제외한다.
func localChannelCandidates(results []channelResult) (semantic, keyword []model.SearchCandidate) {
	for _, result := range results {
		if result.metric.Failure != "" {
			continue
		}
		switch result.name {
		case "semantic":
			semantic = result.candidates
		case "keyword":
			keyword = result.candidates
		}
	}
	return semantic, keyword
}

// routeSignals는 진입 채널 결과와 그 임시 결합에서 라우터 신호를 만든다.
func routeSignals(results []channelResult, combined []combinedCandidate, semanticThreshold float64) RouteSignals {
	semantic, keyword := localChannelCandidates(results)
	signals := RouteSignals{
		RelevantLocal: hasRelevantSemantic(semantic, semanticThreshold) || len(keyword) > 0,
		SemanticCount: len(semantic),
	}
	if len(combined) > 0 {
		channels := combined[0].channels
		signals.TopIsEvent = combined[0].value.Layer == model.LayerEvent
		signals.CrossChannelAgreement = slices.Contains(channels, "semantic") && slices.Contains(channels, "keyword")
	}
	if len(semantic) > 0 {
		signals.SemanticTop = semantic[0].Similarity
	}
	if len(semantic) > 1 {
		signals.SemanticSecond = semantic[1].Similarity
	}
	return signals
}

// relevantLocalContexts는 국소 확장의 진입점인 관련 국소 후보를 통합 순위 순서로 고른다.
// 유사도 하한을 넘은 의미 후보와 키워드 후보만 관련 후보다. 시간 필터 후보는 질의 관련성이
// 없어 결합에는 남지만 확장 시작점으로 쓰지 않는다.
func relevantLocalContexts(results []channelResult, combined []combinedCandidate, semanticThreshold float64) []model.Context {
	semantic, keyword := localChannelCandidates(results)
	relevant := map[model.ID]struct{}{}
	for _, candidate := range semantic {
		if candidate.Similarity >= semanticThreshold {
			relevant[candidate.Context.ID] = struct{}{}
		}
	}
	for _, candidate := range keyword {
		relevant[candidate.Context.ID] = struct{}{}
	}
	values := make([]model.Context, 0, len(relevant))
	for _, candidate := range combined {
		if _, ok := relevant[candidate.value.ID]; ok {
			values = append(values, candidate.value)
		}
	}
	return values
}

// hasRelevantSemantic은 의미 후보 중 유사도 하한을 넘은 것이 있는지 판정한다.
func hasRelevantSemantic(candidates []model.SearchCandidate, threshold float64) bool {
	for _, candidate := range candidates {
		if candidate.Similarity >= threshold {
			return true
		}
	}
	return false
}
