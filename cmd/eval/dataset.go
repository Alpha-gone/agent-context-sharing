// 평가 데이터셋의 형식과 읽기를 담당한다.
//
// 형식을 컨텍스트 집합 파일과 질의 파일로 나눈 이유는 「검색 품질 평가」가 사용 사례
// 셋에 서로 다른 데이터셋을 붙였기 때문이다. 같은 컨텍스트 집합 위에서 질의 파일만
// 바꿔 사실 검색과 연상 검색을 재려면 둘이 분리되어 있어야 한다.
package main

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
)

// 사용 사례는 「검색 품질 평가」가 나눈 셋이다. 채널 구성이 사용 사례마다 다르게
// 작동하므로 합쳐서 재지 않고 이 값으로 갈라 집계한다.
const (
	useCaseFact        = "fact"
	useCaseAssociative = "associative"
	useCaseGlobal      = "global"
)

// useCases는 집계 순서를 고정하기 위한 목록이다.
var useCases = []string{useCaseFact, useCaseAssociative, useCaseGlobal}

// contextSpec은 컨텍스트 집합 파일의 한 항목이다.
//
// Key는 파일 안에서만 쓰는 이름이며 적재할 때 실제 UUIDv7로 바꾼다. 데이터셋이
// 식별자를 직접 갖지 않아야 같은 파일을 회차마다 새 그래프에 그대로 올릴 수 있다.
type contextSpec struct {
	Key     string       `json:"key"`
	Layer   string       `json:"layer"`
	Body    string       `json:"body"`
	Source  *sourceSpec  `json:"source,omitzero"`
	Derived *derivedSpec `json:"derived,omitzero"`
	Event   *eventSpec   `json:"event,omitzero"`
}

type sourceSpec struct {
	Channel    string    `json:"channel"`
	Locator    string    `json:"locator"`
	OccurredAt time.Time `json:"occurred_at"`
	OriginKind string    `json:"origin_kind"`
}

type derivedSpec struct {
	Kind            string     `json:"kind"`
	SummaryScope    string     `json:"summary_scope,omitzero"`
	DerivedFrom     []string   `json:"derived_from"`
	EvidenceState   string     `json:"evidence_state"`
	ConfidenceState string     `json:"confidence_state,omitzero"`
	ValidFrom       *time.Time `json:"valid_from,omitzero"`
	ValidTo         *time.Time `json:"valid_to,omitzero"`
}

type eventSpec struct {
	Members []string   `json:"members"`
	Start   time.Time  `json:"start"`
	End     *time.Time `json:"end,omitzero"`
}

// contextSet은 컨텍스트 집합 파일 전체다. Version은 「검증」이 기록하라고 한
// "사용한 데이터셋 판"이며 결과 파일에 그대로 싣는다.
type contextSet struct {
	Version  string        `json:"version"`
	Contexts []contextSpec `json:"contexts"`
}

// querySpec은 질의 파일의 한 항목이다. Answers는 컨텍스트 집합의 Key 목록이며
// 재현율과 순위 품질의 정답 집합이 된다.
type querySpec struct {
	ID           string     `json:"id"`
	UseCase      string     `json:"use_case"`
	WorkContext  string     `json:"work_context"`
	Answers      []string   `json:"answers"`
	Scope        string     `json:"scope,omitzero"`
	SummaryScope string     `json:"summary_scope,omitzero"`
	AsOf         *time.Time `json:"as_of,omitzero"`
}

type querySet struct {
	Version string      `json:"version"`
	Queries []querySpec `json:"queries"`
}

// loadContextSet은 컨텍스트 집합을 읽고 적재 전에 형식을 검증한다. 적재 도중에
// 실패하면 그래프가 절반만 찬 상태로 남으므로 파일 단계에서 모두 거른다.
func loadContextSet(path string) (contextSet, error) {
	var set contextSet
	if err := readJSON(path, &set); err != nil {
		return contextSet{}, err
	}
	if set.Version == "" {
		return contextSet{}, fmt.Errorf("%s: 데이터셋 판을 비울 수 없다", path)
	}
	if len(set.Contexts) == 0 {
		return contextSet{}, fmt.Errorf("%s: 컨텍스트가 없다", path)
	}
	keys := make(map[string]string, len(set.Contexts))
	for _, spec := range set.Contexts {
		if spec.Key == "" || spec.Body == "" {
			return contextSet{}, fmt.Errorf("%s: key와 body를 비울 수 없다", path)
		}
		if _, duplicated := keys[spec.Key]; duplicated {
			return contextSet{}, fmt.Errorf("%s: key %q가 중복된다", path, spec.Key)
		}
		keys[spec.Key] = spec.Layer
	}
	for _, spec := range set.Contexts {
		if err := validateContextSpec(spec, keys); err != nil {
			return contextSet{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	return set, nil
}

// validateContextSpec은 계층이 요구하는 속성 묶음이 하나만 채워졌는지와 참조가
// 집합 안을 가리키는지 본다. 값 자체의 유효성은 model이 적재 시점에 다시 본다.
func validateContextSpec(spec contextSpec, keys map[string]string) error {
	filled := 0
	for _, present := range []bool{spec.Source != nil, spec.Derived != nil, spec.Event != nil} {
		if present {
			filled++
		}
	}
	if filled != 1 {
		return fmt.Errorf("컨텍스트 %q: 계층 속성 묶음을 하나만 채워야 한다", spec.Key)
	}
	switch model.Layer(spec.Layer) {
	case model.LayerSource:
		if spec.Source == nil {
			return fmt.Errorf("컨텍스트 %q: source 속성이 없다", spec.Key)
		}
	case model.LayerDerived:
		if spec.Derived == nil {
			return fmt.Errorf("컨텍스트 %q: derived 속성이 없다", spec.Key)
		}
		if len(spec.Derived.DerivedFrom) == 0 {
			return fmt.Errorf("컨텍스트 %q: 파생에는 근거가 하나 이상 있어야 한다", spec.Key)
		}
		if err := referencedKeys(spec.Key, "derived_from", spec.Derived.DerivedFrom, keys); err != nil {
			return err
		}
	case model.LayerEvent:
		if spec.Event == nil {
			return fmt.Errorf("컨텍스트 %q: event 속성이 없다", spec.Key)
		}
		if err := referencedKeys(spec.Key, "members", spec.Event.Members, keys); err != nil {
			return err
		}
	default:
		return fmt.Errorf("컨텍스트 %q: 계층 %q를 알 수 없다", spec.Key, spec.Layer)
	}
	return nil
}

func referencedKeys(owner, field string, references []string, keys map[string]string) error {
	for _, key := range references {
		if _, known := keys[key]; !known {
			return fmt.Errorf("컨텍스트 %q의 %s가 없는 key %q를 가리킨다", owner, field, key)
		}
	}
	return nil
}

// loadQuerySet은 질의 파일을 읽고 정답이 컨텍스트 집합 안을 가리키는지 확인한다.
func loadQuerySet(path string, contexts contextSet) (querySet, error) {
	var set querySet
	if err := readJSON(path, &set); err != nil {
		return querySet{}, err
	}
	if set.Version == "" {
		return querySet{}, fmt.Errorf("%s: 데이터셋 판을 비울 수 없다", path)
	}
	if len(set.Queries) == 0 {
		return querySet{}, fmt.Errorf("%s: 질의가 없다", path)
	}
	known := make(map[string]struct{}, len(contexts.Contexts))
	for _, spec := range contexts.Contexts {
		known[spec.Key] = struct{}{}
	}
	ids := make(map[string]struct{}, len(set.Queries))
	for _, query := range set.Queries {
		if query.ID == "" || query.WorkContext == "" {
			return querySet{}, fmt.Errorf("%s: id와 work_context를 비울 수 없다", path)
		}
		if _, duplicated := ids[query.ID]; duplicated {
			return querySet{}, fmt.Errorf("%s: 질의 id %q가 중복된다", path, query.ID)
		}
		ids[query.ID] = struct{}{}
		if !slices.Contains(useCases, query.UseCase) {
			return querySet{}, fmt.Errorf("%s: 질의 %q의 사용 사례 %q를 알 수 없다", path, query.ID, query.UseCase)
		}
		if len(query.Answers) == 0 {
			return querySet{}, fmt.Errorf("%s: 질의 %q에 정답이 없다", path, query.ID)
		}
		for _, answer := range query.Answers {
			if _, present := known[answer]; !present {
				return querySet{}, fmt.Errorf("%s: 질의 %q의 정답 %q가 컨텍스트 집합에 없다", path, query.ID, answer)
			}
		}
	}
	return set, nil
}

func readJSON(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("데이터셋 읽기: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: 데이터셋 해석: %w", path, err)
	}
	return nil
}
