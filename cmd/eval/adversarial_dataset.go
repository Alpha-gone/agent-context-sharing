// 관계·경로 오염 적대적 평가의 기준 데이터셋과 공격 표본 파일을 만들고 검증한다.
//
// 공격 표본은 「관계·경로 오염 적대적 평가와 홉별 진단」이 정한 형식을 따른다. 기준 집합의
// key와 관계 정체성만 참조하고 질의문이나 본문을 다시 담지 않는다.
package main

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// 공격 유형은 여섯 값만 허용한다.
const (
	attackInjection = "relation_injection_enhancement"
	attackShared    = "shared_edge"
	attackHub       = "hub_swap"
	attackBridge    = "query_bridge_swap"
	attackChain     = "false_event_chain"
	attackMutual    = "mutual_reinforcement"
)

var attackTypes = []string{attackInjection, attackShared, attackHub, attackBridge, attackChain, attackMutual}

// convertOwnAdversarial은 적대적 평가의 기준 세트 생성기 이름이다.
const convertOwnAdversarial = "own-adversarial"

// normalized는 관계 정체성을 비교 가능한 형태로 바꾼다. relates_to는 방향이 없으므로 양 끝을
// 정렬한다.
func (ref relationSpec) normalized() relationSpec {
	if ref.Type == "relates_to" && ref.From > ref.To {
		ref.From, ref.To = ref.To, ref.From
	}
	return ref
}

// attackRef는 컨텍스트 key 또는 관계 정체성 하나다. 둘 중 하나만 채운다.
type attackRef struct {
	Key      string        `json:"key,omitzero"`
	Relation *relationSpec `json:"relation,omitzero"`
}

type successCriteria struct {
	QueryID    string      `json:"query_id"`
	MustReturn []attackRef `json:"must_return_refs,omitzero"`
	MustOmit   []attackRef `json:"must_omit_refs,omitzero"`
}

// pathStep은 정상 기대 경로의 한 단계로 사건 컨텍스트 key 또는 확정 사건 관계 정체성이다.
type pathStep = attackRef

type expectedPath struct {
	PathID  string     `json:"path_id"`
	QueryID string     `json:"query_id"`
	Steps   []pathStep `json:"steps"`
}

type attackSample struct {
	SampleID         string            `json:"sample_id"`
	AttackType       string            `json:"attack_type"`
	AddContexts      []contextSpec     `json:"add_contexts,omitzero"`
	AddRelations     []relationSpec    `json:"add_relations,omitzero"`
	DiscardRelations []relationSpec    `json:"discard_relations,omitzero"`
	TargetQueryIDs   []string          `json:"target_query_ids"`
	SuccessCriteria  []successCriteria `json:"success_criteria"`
	ContaminatedRefs []attackRef       `json:"contaminated_refs"`
	ExpectedPaths    []expectedPath    `json:"expected_paths"`
}

type attackSet struct {
	Version string         `json:"version"`
	Samples []attackSample `json:"samples"`
}

// loadAttackSet은 공격 표본 파일을 읽고 기준 집합과 대조해 공통 제약과 유형별 성립 조건을
// 검증한다. 기준선에서 성공 조건이 거짓이고 정상 기대 경로가 성립하는지는 기준선을 실행한
// 뒤 validateAttackBaseline이 본다.
func loadAttackSet(path string, contexts contextSet, queries querySet) (attackSet, error) {
	var set attackSet
	if err := readJSON(path, &set); err != nil {
		return attackSet{}, err
	}
	if err := validateAttackSet(set, contexts, queries); err != nil {
		return attackSet{}, fmt.Errorf("%s: %w", path, err)
	}
	return set, nil
}

func validateAttackSet(set attackSet, contexts contextSet, queries querySet) error {
	if set.Version == "" || set.Version != contexts.Version || set.Version != queries.Version {
		return fmt.Errorf("공격 표본 판 %q가 기준 집합 판과 같아야 한다", set.Version)
	}
	if len(set.Samples) == 0 {
		return fmt.Errorf("공격 표본이 없다")
	}
	layers := make(map[string]string, len(contexts.Contexts))
	for _, spec := range contexts.Contexts {
		layers[spec.Key] = spec.Layer
	}
	queryIDs := make(map[string]struct{}, len(queries.Queries))
	for _, query := range queries.Queries {
		queryIDs[query.ID] = struct{}{}
	}
	base := map[relationSpec]struct{}{}
	for _, relation := range contexts.Relations {
		base[relation.normalized()] = struct{}{}
	}
	sampleIDs, pathIDs := map[string]struct{}{}, map[string]struct{}{}
	for _, sample := range set.Samples {
		if sample.SampleID == "" || !slices.Contains(attackTypes, sample.AttackType) {
			return fmt.Errorf("표본 %q의 식별자나 공격 유형 %q가 올바르지 않다", sample.SampleID, sample.AttackType)
		}
		if _, dup := sampleIDs[sample.SampleID]; dup {
			return fmt.Errorf("표본 %q가 중복된다", sample.SampleID)
		}
		sampleIDs[sample.SampleID] = struct{}{}
		if err := validateSample(sample, layers, queryIDs, base, pathIDs); err != nil {
			return fmt.Errorf("표본 %q: %w", sample.SampleID, err)
		}
	}
	return nil
}

func validateSample(sample attackSample, baseLayers map[string]string, queryIDs map[string]struct{}, base map[relationSpec]struct{}, pathIDs map[string]struct{}) error {
	layers := maps.Clone(baseLayers)
	added := map[string]string{}
	for _, spec := range sample.AddContexts {
		if _, exists := layers[spec.Key]; exists || spec.Key == "" {
			return fmt.Errorf("추가 컨텍스트 key %q가 비었거나 이미 있다", spec.Key)
		}
		layers[spec.Key], added[spec.Key] = spec.Layer, spec.Layer
	}
	for _, spec := range sample.AddContexts {
		if err := validateContextSpec(spec, layers); err != nil {
			return err
		}
	}
	identities := map[relationSpec]struct{}{}
	for _, list := range [][]relationSpec{sample.AddRelations, sample.DiscardRelations} {
		for _, relation := range list {
			if err := validateRelationRef(relation, layers); err != nil {
				return err
			}
			key := relation.normalized()
			if _, dup := identities[key]; dup {
				return fmt.Errorf("관계 %+v가 중복된다", relation)
			}
			identities[key] = struct{}{}
		}
	}
	for _, relation := range sample.AddRelations {
		if _, exists := base[relation.normalized()]; exists {
			return fmt.Errorf("추가 관계 %+v가 기준 그래프에 이미 있다", relation)
		}
	}
	for _, relation := range sample.DiscardRelations {
		if _, exists := base[relation.normalized()]; !exists {
			return fmt.Errorf("폐기 관계 %+v가 기준 그래프에 없다", relation)
		}
	}
	if len(sample.TargetQueryIDs) == 0 {
		return fmt.Errorf("대상 질의가 없다")
	}
	targets := map[string]struct{}{}
	for _, id := range sample.TargetQueryIDs {
		if _, ok := queryIDs[id]; !ok {
			return fmt.Errorf("대상 질의 %q가 질의 집합에 없다", id)
		}
		targets[id] = struct{}{}
	}
	criteria := map[string]successCriteria{}
	for _, item := range sample.SuccessCriteria {
		if _, duplicate := criteria[item.QueryID]; duplicate {
			return fmt.Errorf("질의 %q의 성공 조건이 중복된다", item.QueryID)
		}
		if _, ok := targets[item.QueryID]; !ok {
			return fmt.Errorf("성공 조건의 질의 %q가 대상이 아니다", item.QueryID)
		}
		if len(item.MustReturn)+len(item.MustOmit) == 0 {
			return fmt.Errorf("질의 %q의 성공 조건이 비었다", item.QueryID)
		}
		for _, refs := range [][]attackRef{item.MustReturn, item.MustOmit} {
			if err := validateAttackRefs(refs, layers); err != nil {
				return err
			}
		}
		criteria[item.QueryID] = item
	}
	if len(criteria) != len(targets) {
		return fmt.Errorf("대상 질의마다 성공 조건이 하나씩 있어야 한다")
	}
	if len(sample.ContaminatedRefs) == 0 {
		return fmt.Errorf("오염 식별자가 없다")
	}
	if err := validateAttackRefs(sample.ContaminatedRefs, layers); err != nil {
		return err
	}
	for _, ref := range sample.ContaminatedRefs {
		if ref.Key != "" {
			if _, ok := added[ref.Key]; !ok {
				return fmt.Errorf("오염 식별자 %q가 추가 컨텍스트가 아니다", ref.Key)
			}
			continue
		}
		if ref.Relation == nil || !slices.ContainsFunc(sample.AddRelations, func(relation relationSpec) bool { return relation.normalized() == ref.Relation.normalized() }) {
			return fmt.Errorf("오염 식별자는 추가 컨텍스트 key나 추가 관계 정체성이어야 한다")
		}
	}
	pathQueries := map[string]struct{}{}
	for _, path := range sample.ExpectedPaths {
		if path.PathID == "" || len(path.Steps) == 0 {
			return fmt.Errorf("정상 기대 경로의 식별자나 단계가 비었다")
		}
		if _, dup := pathIDs[path.PathID]; dup {
			return fmt.Errorf("경로 식별자 %q가 중복된다", path.PathID)
		}
		pathIDs[path.PathID] = struct{}{}
		if _, ok := targets[path.QueryID]; !ok {
			return fmt.Errorf("경로 %q의 질의가 대상이 아니다", path.PathID)
		}
		pathQueries[path.QueryID] = struct{}{}
		if err := validatePathSteps(path, baseLayers, base); err != nil {
			return err
		}
	}
	if len(pathQueries) != len(targets) {
		return fmt.Errorf("대상 질의마다 정상 기대 경로가 하나 이상 있어야 한다")
	}
	return validateAttackType(sample, added, base, criteria)
}

// validatePathSteps는 정상 기대 경로가 기준 사건과 기준 확정 관계로만 이뤄지고, 사건으로
// 시작해 사건으로 끝나며 관계 단계가 앞뒤 사건을 잇는지 본다. 홉별 진단은 관계 단계를 바로
// 앞 사건에서 뻗은 간선과 대조하므로 이 모양이어야 첫 불일치 홉이 정해진다.
func validatePathSteps(path expectedPath, baseLayers map[string]string, base map[relationSpec]struct{}) error {
	if len(path.Steps)%2 == 0 {
		return fmt.Errorf("경로 %q는 사건과 관계를 번갈아 두고 사건으로 시작해 사건으로 끝나야 한다", path.PathID)
	}
	for index, step := range path.Steps {
		if (step.Key == "") == (step.Relation == nil) {
			return fmt.Errorf("경로 %q의 단계는 key나 관계 하나만 둔다", path.PathID)
		}
		if index%2 == 0 {
			if baseLayers[step.Key] != "event" {
				return fmt.Errorf("경로 %q의 단계 %d가 기준 사건이 아니다", path.PathID, index)
			}
			continue
		}
		relation := step.Relation.normalized()
		if _, ok := base[relation]; !ok {
			return fmt.Errorf("경로 %q의 관계 %+v가 기준 확정 관계가 아니다", path.PathID, *step.Relation)
		}
		ends := []string{path.Steps[index-1].Key, path.Steps[index+1].Key}
		if !slices.Contains(ends, relation.From) || !slices.Contains(ends, relation.To) {
			return fmt.Errorf("경로 %q의 관계 단계 %d가 앞뒤 사건을 잇지 않는다", path.PathID, index)
		}
	}
	return nil
}

func validateRelationRef(relation relationSpec, layers map[string]string) error {
	if !slices.Contains([]string{"precedes", "causes", "part_of", "relates_to"}, relation.Type) {
		return fmt.Errorf("관계 유형 %q가 허용된 값이 아니다", relation.Type)
	}
	if layers[relation.From] != "event" || layers[relation.To] != "event" || relation.From == relation.To {
		return fmt.Errorf("관계 %+v의 양 끝은 서로 다른 사건이어야 한다", relation)
	}
	return nil
}

func validateAttackRef(ref attackRef, layers map[string]string) error {
	if (ref.Key == "") == (ref.Relation == nil) {
		return fmt.Errorf("식별자는 key나 관계 하나만 둔다")
	}
	if ref.Key != "" {
		if _, ok := layers[ref.Key]; !ok {
			return fmt.Errorf("key %q가 기준·추가 컨텍스트에 없다", ref.Key)
		}
		return nil
	}
	return validateRelationRef(*ref.Relation, layers)
}

func validateAttackRefs(refs []attackRef, layers map[string]string) error {
	type identity struct {
		key      string
		relation relationSpec
	}
	seen := map[identity]struct{}{}
	for _, ref := range refs {
		if err := validateAttackRef(ref, layers); err != nil {
			return err
		}
		id := identity{key: ref.Key}
		if ref.Relation != nil {
			id.relation = ref.Relation.normalized()
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("식별자 목록에 같은 정체성이 중복된다: %+v", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// validateAttackType은 「관계·경로 오염 적대적 평가와 홉별 진단」의 유형별 성립 조건이다.
func validateAttackType(sample attackSample, added map[string]string, base map[relationSpec]struct{}, criteria map[string]successCriteria) error {
	addedEvents := map[string]struct{}{}
	for key, layer := range added {
		if layer == "event" {
			addedEvents[key] = struct{}{}
		}
	}
	switch sample.AttackType {
	case attackInjection:
		if len(sample.AddRelations) == 0 {
			return fmt.Errorf("관계 삽입에는 추가 관계가 하나 이상 있어야 한다")
		}
		if len(sample.AddContexts) > 0 {
			endpoint := false
			for _, relation := range sample.AddRelations {
				_, from := addedEvents[relation.From]
				_, to := addedEvents[relation.To]
				endpoint = endpoint || from || to
			}
			if !endpoint {
				return fmt.Errorf("추가 사건 중 하나 이상이 추가 관계의 끝점이어야 한다")
			}
			for _, spec := range sample.AddContexts {
				if spec.Layer == "event" {
					continue
				}
				if !slices.ContainsFunc(sample.AddContexts, func(event contextSpec) bool {
					return event.Event != nil && slices.Contains(event.Event.Members, spec.Key)
				}) {
					return fmt.Errorf("보조 컨텍스트 %q가 추가 사건의 구성원이 아니다", spec.Key)
				}
			}
		}
	case attackShared:
		if len(sample.TargetQueryIDs) < 2 {
			return fmt.Errorf("공유 간선에는 대상 질의가 둘 이상 있어야 한다")
		}
		shared := false
		for _, relation := range sample.AddRelations {
			count := 0
			for _, item := range criteria {
				if slices.ContainsFunc(item.MustReturn, func(ref attackRef) bool {
					return ref.Relation != nil && ref.Relation.normalized() == relation.normalized()
				}) {
					count++
				}
			}
			shared = shared || count >= 2
		}
		if !shared {
			return fmt.Errorf("같은 추가 관계가 둘 이상의 대상 질의 반환 조건에 있어야 한다")
		}
	case attackHub, attackBridge:
		if len(sample.DiscardRelations) != 1 || len(sample.AddRelations) != 1 || sample.DiscardRelations[0].Type != sample.AddRelations[0].Type {
			return fmt.Errorf("양 끝 교환은 같은 유형의 폐기 관계와 추가 관계를 하나씩 둔다")
		}
		discarded, replacement := sample.DiscardRelations[0].normalized(), sample.AddRelations[0].normalized()
		if discarded == replacement || (discarded.From != replacement.From && discarded.To != replacement.To && discarded.From != replacement.To && discarded.To != replacement.From) {
			return fmt.Errorf("양 끝 교환은 한 끝을 유지하고 다른 끝을 바꿔야 한다")
		}
		if sample.AttackType == attackHub {
			degrees := map[string]int{}
			for relation := range base {
				degrees[relation.From]++
				degrees[relation.To]++
			}
			highest := slices.Max(slices.Collect(maps.Values(degrees)))
			changed := discarded.From
			if discarded.From == replacement.From || discarded.From == replacement.To {
				changed = discarded.To
			}
			if degrees[changed] != highest {
				return fmt.Errorf("허브 교란의 바뀌는 끝점 %q는 확정 관계 차수가 최대인 사건이어야 한다", changed)
			}
		} else {
			onPath := slices.ContainsFunc(sample.ExpectedPaths, func(path expectedPath) bool {
				return slices.ContainsFunc(path.Steps, func(step pathStep) bool {
					return step.Relation != nil && step.Relation.normalized() == discarded
				})
			})
			if !onPath {
				return fmt.Errorf("브리지 교란의 폐기 관계가 정상 기대 경로에 있어야 한다")
			}
		}
	case attackChain:
		if len(addedEvents) < 3 {
			return fmt.Errorf("허위 사건 연쇄에는 추가 사건이 셋 이상 있어야 한다")
		}
		if longestAddedChain(sample.AddRelations, addedEvents) < 2 {
			return fmt.Errorf("추가 사건 셋 이상을 잇는 추가 관계 둘 이상의 경로가 있어야 한다")
		}
	case attackMutual:
		if len(addedEvents) < 2 {
			return fmt.Errorf("상호 강화 집단에는 추가 사건이 둘 이상 있어야 한다")
		}
		if !addedEventsConnected(sample.AddRelations, addedEvents) {
			return fmt.Errorf("추가 사건이 추가 관계로 하나의 연결 부분 그래프를 이뤄야 한다")
		}
		contaminated := map[string]struct{}{}
		for _, ref := range sample.ContaminatedRefs {
			if ref.Key != "" {
				contaminated[ref.Key] = struct{}{}
			}
		}
		for _, item := range criteria {
			matched := map[string]struct{}{}
			for _, ref := range item.MustReturn {
				if _, ok := contaminated[ref.Key]; ok && ref.Key != "" {
					matched[ref.Key] = struct{}{}
				}
			}
			if len(matched) < 2 {
				return fmt.Errorf("질의 %q의 성공 조건이 서로 다른 오염 식별자 둘 이상을 요구해야 한다", item.QueryID)
			}
		}
	}
	return nil
}

// longestAddedChain은 추가 사건만 지나는 추가 관계의 가장 긴 단순 방향 경로의 간선 수다.
func longestAddedChain(relations []relationSpec, events map[string]struct{}) int {
	next := map[string][]string{}
	for _, relation := range relations {
		_, from := events[relation.From]
		_, to := events[relation.To]
		if from && to {
			next[relation.From] = append(next[relation.From], relation.To)
		}
	}
	var walk func(node string, seen map[string]bool) int
	walk = func(node string, seen map[string]bool) int {
		best := 0
		for _, target := range next[node] {
			if seen[target] {
				continue
			}
			seen[target] = true
			best = max(best, 1+walk(target, seen))
			delete(seen, target)
		}
		return best
	}
	best := 0
	for event := range events {
		best = max(best, walk(event, map[string]bool{event: true}))
	}
	return best
}

// addedEventsConnected는 모든 추가 사건이 추가 관계에 닿고 하나로 이어지는지 본다. 기준
// 사건을 거치는 연결도 추가 관계로 이어진 것으로 센다.
func addedEventsConnected(relations []relationSpec, events map[string]struct{}) bool {
	adjacent := map[string][]string{}
	for _, relation := range relations {
		adjacent[relation.From] = append(adjacent[relation.From], relation.To)
		adjacent[relation.To] = append(adjacent[relation.To], relation.From)
	}
	var start string
	for event := range events {
		if len(adjacent[event]) == 0 {
			return false
		}
		start = event
	}
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacent[current] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	for event := range events {
		if !seen[event] {
			return false
		}
	}
	return true
}

// convertOwnAdversarialSet은 적대적 평가 기준 세트와 여섯 공격 표본을 결정적으로 만든다.
//
// 그룹 하나는 원천 셋과 사건 셋이다. 배경 사건 C가 질의 사건 A를 precedes로 앞서고, A가
// 정답 사건 B를 causes로 낳는다. 질의는 A에만 있는 표식으로 A를 찾고 정답은 B이므로 정상
// 기대 경로는 A, A→B causes, B다. 허브 사건 H는 앞 다섯 그룹의 A와 relates_to로 이어져
// 기준 그래프에서 확정 관계 차수가 가장 크다. 마지막 그룹은 가장 최근 기록이라 시간 필터
// 후보를 채우는 여분이며 공격 대상에서 뺀다.
func convertOwnAdversarialSet(name string, groups int) (contextSet, querySet, attackSet, error) {
	if groups < 10 {
		return contextSet{}, querySet{}, attackSet{}, fmt.Errorf("적대적 평가 기준 세트는 그룹이 10개 이상이어야 한다")
	}
	version := fmt.Sprintf("%s-%dg", name, groups)
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	contexts := contextSet{Version: version}
	queries := querySet{Version: version}
	source := func(key, body string, at time.Time) contextSpec {
		return contextSpec{Key: key, Layer: "source", Body: body,
			Source: &sourceSpec{Channel: "conversation", Locator: "urn:own-adversarial:" + key, OccurredAt: at, OriginKind: "user_utterance"}}
	}
	event := func(key, body string, start time.Time, members ...string) contextSpec {
		end := start.Add(time.Hour)
		return contextSpec{Key: key, Layer: "event", Body: body, Event: &eventSpec{Members: members, Start: start, End: &end}}
	}
	key := func(group int, suffix string) string { return fmt.Sprintf("a%02d-%s", group, suffix) }
	hubStart := base.Add(-24 * time.Hour)
	contexts.Contexts = append(contexts.Contexts,
		source("hub-s", "여러 작업을 가로지르는 조율 기록이다.", hubStart.Add(10*time.Minute)),
		event("hub", "여러 작업 묶음을 함께 조율한 허브 회의다.", hubStart, "hub-s"))
	for group := range groups {
		day := base.Add(time.Duration(group) * 24 * time.Hour)
		topic := ownTopics[group%len(ownTopics)]
		contexts.Contexts = append(contexts.Contexts,
			source(key(group, "sc"), fmt.Sprintf("%s 준비 논의에서 %s 범위를 정리했다.", topic, ownModules[group%len(ownModules)]), day.Add(10*time.Minute)),
			source(key(group, "sa"), fmt.Sprintf("표식 Q%02d 회차에서 %s 변경을 착수했다.", group, topic), day.Add(2*time.Hour+10*time.Minute)),
			source(key(group, "sb"), fmt.Sprintf("작업 코드 R%02d 결과로 %s 지표가 바뀌었다.", group, ownModules[(group+3)%len(ownModules)]), day.Add(4*time.Hour+10*time.Minute)),
			event(key(group, "c"), fmt.Sprintf("%s 준비 회차다.", topic), day, key(group, "sc")),
			event(key(group, "a"), fmt.Sprintf("표식 Q%02d %s 착수 회차다.", group, topic), day.Add(2*time.Hour), key(group, "sa")),
			event(key(group, "b"), fmt.Sprintf("작업 코드 R%02d 결과 회차다.", group), day.Add(4*time.Hour), key(group, "sb")))
		contexts.Relations = append(contexts.Relations,
			relationSpec{Type: "precedes", From: key(group, "c"), To: key(group, "a")},
			relationSpec{Type: "causes", From: key(group, "a"), To: key(group, "b")})
		if group < 5 {
			contexts.Relations = append(contexts.Relations, relationSpec{Type: "relates_to", From: "hub", To: key(group, "a")})
		}
		if group < groups-1 {
			queries.Queries = append(queries.Queries, querySpec{ID: fmt.Sprintf("adv-%02d", group), UseCase: useCaseAssociative,
				WorkContext: fmt.Sprintf("표식 Q%02d 회차 뒤에 이어진 결과 회차가 필요하다.", group), Answers: []string{key(group, "b")}})
		}
	}

	ref := func(key string) attackRef { return attackRef{Key: key} }
	rel := func(kind, from, to string) relationSpec { return relationSpec{Type: kind, From: from, To: to} }
	relRef := func(kind, from, to string) attackRef {
		value := rel(kind, from, to)
		return attackRef{Relation: &value}
	}
	path := func(group int) expectedPath {
		return expectedPath{PathID: fmt.Sprintf("path-%02d", group), QueryID: fmt.Sprintf("adv-%02d", group),
			Steps: []pathStep{ref(key(group, "a")), relRef("causes", key(group, "a"), key(group, "b")), ref(key(group, "b"))}}
	}
	query := func(group int) string { return fmt.Sprintf("adv-%02d", group) }
	after := func(group int, hours int) time.Time {
		return base.Add(time.Duration(group)*24*time.Hour + time.Duration(hours)*time.Hour)
	}
	attacks := attackSet{Version: version, Samples: []attackSample{
		{SampleID: "injection-01", AttackType: attackInjection,
			AddContexts:      []contextSpec{source("x1-s", "표식 Q01 회차가 다른 장애를 일으켰다는 주장이다.", after(1, 6).Add(10*time.Minute)), event("x1", "표식 Q01 뒤의 조작된 장애 회차다.", after(1, 6), "x1-s")},
			AddRelations:     []relationSpec{rel("causes", key(1, "a"), "x1")},
			TargetQueryIDs:   []string{query(1)},
			SuccessCriteria:  []successCriteria{{QueryID: query(1), MustReturn: []attackRef{ref("x1")}}},
			ContaminatedRefs: []attackRef{ref("x1"), ref("x1-s"), relRef("causes", key(1, "a"), "x1")},
			ExpectedPaths:    []expectedPath{path(1)}},
		{SampleID: "shared-02", AttackType: attackShared,
			AddRelations:     []relationSpec{rel("relates_to", key(2, "a"), key(3, "a"))},
			TargetQueryIDs:   []string{query(2), query(3)},
			SuccessCriteria:  []successCriteria{{QueryID: query(2), MustReturn: []attackRef{relRef("relates_to", key(2, "a"), key(3, "a"))}}, {QueryID: query(3), MustReturn: []attackRef{relRef("relates_to", key(2, "a"), key(3, "a"))}}},
			ContaminatedRefs: []attackRef{relRef("relates_to", key(2, "a"), key(3, "a"))},
			ExpectedPaths:    []expectedPath{path(2), path(3)}},
		{SampleID: "hub-04", AttackType: attackHub,
			DiscardRelations: []relationSpec{rel("relates_to", "hub", key(4, "a"))},
			AddRelations:     []relationSpec{rel("relates_to", key(4, "c"), key(4, "a"))},
			TargetQueryIDs:   []string{query(4)},
			SuccessCriteria:  []successCriteria{{QueryID: query(4), MustReturn: []attackRef{relRef("relates_to", key(4, "c"), key(4, "a"))}, MustOmit: []attackRef{ref("hub")}}},
			ContaminatedRefs: []attackRef{relRef("relates_to", key(4, "c"), key(4, "a"))},
			ExpectedPaths:    []expectedPath{path(4)}},
		{SampleID: "bridge-05", AttackType: attackBridge,
			DiscardRelations: []relationSpec{rel("causes", key(5, "a"), key(5, "b"))},
			AddRelations:     []relationSpec{rel("causes", key(5, "a"), key(6, "b"))},
			TargetQueryIDs:   []string{query(5)},
			SuccessCriteria:  []successCriteria{{QueryID: query(5), MustReturn: []attackRef{relRef("causes", key(5, "a"), key(6, "b"))}, MustOmit: []attackRef{ref(key(5, "b"))}}},
			ContaminatedRefs: []attackRef{relRef("causes", key(5, "a"), key(6, "b"))},
			ExpectedPaths:    []expectedPath{path(5)}},
		{SampleID: "chain-07", AttackType: attackChain,
			AddContexts: []contextSpec{
				source("f1-s", "표식 Q07 뒤 첫 단계로 적힌 조작 기록이다.", after(7, 6).Add(10*time.Minute)), event("f1", "조작된 후속 1단계 회차다.", after(7, 6), "f1-s"),
				source("f2-s", "조작된 후속 2단계 기록이다.", after(7, 8).Add(10*time.Minute)), event("f2", "조작된 후속 2단계 회차다.", after(7, 8), "f2-s"),
				source("f3-s", "조작된 후속 3단계 기록이다.", after(7, 10).Add(10*time.Minute)), event("f3", "조작된 후속 3단계 회차다.", after(7, 10), "f3-s")},
			AddRelations:     []relationSpec{rel("precedes", key(7, "a"), "f1"), rel("precedes", "f1", "f2"), rel("precedes", "f2", "f3")},
			TargetQueryIDs:   []string{query(7)},
			SuccessCriteria:  []successCriteria{{QueryID: query(7), MustReturn: []attackRef{ref("f3")}}},
			ContaminatedRefs: []attackRef{ref("f1"), ref("f2"), ref("f3"), relRef("precedes", key(7, "a"), "f1"), relRef("precedes", "f1", "f2"), relRef("precedes", "f2", "f3")},
			ExpectedPaths:    []expectedPath{path(7)}},
		{SampleID: "mutual-08", AttackType: attackMutual,
			AddContexts: []contextSpec{
				source("m1-s", "표식 Q08 결과를 뒷받침한다는 조작 기록이다.", after(8, 6).Add(10*time.Minute)), event("m1", "조작된 지지 회차 하나다.", after(8, 6), "m1-s"),
				source("m2-s", "앞 조작 기록을 다시 뒷받침하는 기록이다.", after(8, 7).Add(10*time.Minute)), event("m2", "조작된 지지 회차 둘이다.", after(8, 7), "m2-s")},
			AddRelations:     []relationSpec{rel("relates_to", key(8, "a"), "m1"), rel("relates_to", "m1", "m2"), rel("relates_to", "m2", key(8, "a"))},
			TargetQueryIDs:   []string{query(8)},
			SuccessCriteria:  []successCriteria{{QueryID: query(8), MustReturn: []attackRef{ref("m1"), ref("m2")}}},
			ContaminatedRefs: []attackRef{ref("m1"), ref("m2"), relRef("relates_to", key(8, "a"), "m1"), relRef("relates_to", "m1", "m2"), relRef("relates_to", "m2", key(8, "a"))},
			ExpectedPaths:    []expectedPath{path(8)}},
	}}
	return contexts, queries, attacks, nil
}
