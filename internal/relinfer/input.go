package relinfer

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
)

// Profile은 재현과 자동 확정 판정에 필요한 비밀 없는 추론 구성이다.
type Profile struct {
	Provider, Model, PromptVersion, SchemaVersion           string
	EmbeddingModel                                          string
	IndexWait, AdjacencyWindow                              time.Duration
	Channels                                                []string
	CandidateLimit, TargetChars, CandidateChars, InputChars int
	Parameters                                              map[string]string
}

// Fingerprint는 채널과 매개변수 순서에 영향받지 않는 SHA-256 지문을 반환한다.
func (p Profile) Fingerprint() (string, error) {
	if p.Provider == "" || p.Model == "" || p.PromptVersion == "" || p.SchemaVersion == "" ||
		p.EmbeddingModel == "" || p.IndexWait <= 0 || p.AdjacencyWindow <= 0 || p.CandidateLimit < 1 || p.CandidateLimit > 20 || p.TargetChars < 1 || p.CandidateChars < 1 || p.InputChars < p.TargetChars {
		return "", fmt.Errorf("추론 프로필 구성이 올바르지 않다")
	}
	channels := slices.Clone(p.Channels)
	slices.Sort(channels)
	if len(channels) == 0 || len(slices.Compact(slices.Clone(channels))) != len(channels) {
		return "", fmt.Errorf("추론 채널 구성이 올바르지 않다")
	}
	for _, channel := range channels {
		if !slices.Contains([]string{"semantic", "keyword", "member", "rule", "conversation"}, channel) {
			return "", fmt.Errorf("알 수 없는 추론 채널이다")
		}
	}
	// 정렬된 쌍은 map의 순회 순서와 JSON map 표현에 의존하지 않는다.
	keys := make([]string, 0, len(p.Parameters))
	for key := range p.Parameters {
		if key == "" {
			return "", fmt.Errorf("빈 추론 매개변수 이름이다")
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	type parameter struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	parameters := make([]parameter, 0, len(keys))
	for _, key := range keys {
		parameters = append(parameters, parameter{key, p.Parameters[key]})
	}
	canonical := struct {
		Provider        string      `json:"provider"`
		Model           string      `json:"model"`
		PromptVersion   string      `json:"prompt_version"`
		SchemaVersion   string      `json:"schema_version"`
		Channels        []string    `json:"channels"`
		EmbeddingModel  string      `json:"embedding_model"`
		IndexWait       int64       `json:"index_wait_ns"`
		AdjacencyWindow int64       `json:"adjacency_window_ns"`
		CandidateLimit  int         `json:"candidate_limit"`
		TargetChars     int         `json:"target_chars"`
		CandidateChars  int         `json:"candidate_chars"`
		InputChars      int         `json:"input_chars"`
		Parameters      []parameter `json:"parameters"`
	}{p.Provider, p.Model, p.PromptVersion, p.SchemaVersion, channels, p.EmbeddingModel, int64(p.IndexWait), int64(p.AdjacencyWindow), p.CandidateLimit, p.TargetChars, p.CandidateChars, p.InputChars, parameters}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("추론 프로필 직렬화: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Candidate는 저장소가 같은 스냅숏에서 확인한 활성 사건과 판정 가능 유형이다.
type Candidate = model.InferenceCandidate

// Channel은 독립적으로 순위를 매긴 후보 목록이다. 배열의 첫 항목이 1위다.
type Channel = model.InferenceChannel

// Contribution은 후보 기록에 남기는 채널별 순위다.
type Contribution struct {
	Channel string `json:"channel"`
	Rank    int    `json:"rank"`
}

// Selected는 모델에 보내지 않는 식별자·판과 채널 근거다.
type Selected struct {
	ID            model.ID
	Version       int64
	Alias         string
	Contributions []Contribution
}

// PreparedInput은 모델 전송 데이터와 서버 내부 별칭 대응을 분리한다.
type PreparedInput struct {
	Data                []byte
	TargetID            model.ID
	TargetVersion       int64
	Candidates          []Selected
	ProfileFingerprint  string
	SemanticUnavailable bool
}

type allowedRelation struct {
	Type       model.RelationType `json:"type"`
	Direction  string             `json:"direction,omitempty"`
	Directions []string           `json:"directions,omitempty"`
}
type inputEvent struct {
	Alias        string     `json:"alias"`
	Start        time.Time  `json:"start"`
	End          *time.Time `json:"end,omitzero"`
	Conversation bool       `json:"conversation"`
	Body         string     `json:"body"`
}
type inputCandidate struct {
	inputEvent
	Allowed []allowedRelation `json:"allowed"`
}
type inputPayload struct {
	SchemaVersion string `json:"schema_version"`
	Data          struct {
		Target     inputEvent       `json:"target"`
		Candidates []inputCandidate `json:"candidates"`
	} `json:"data"`
}
type scoredCandidate struct {
	candidate     Candidate
	score         *big.Rat
	contributions []Contribution
}

// BuildInput은 사건만 결합해 순위를 정하고 발췌와 전체 입력 문자 상한을 적용한다.
func BuildInput(profile Profile, target model.Context, channels []Channel) (PreparedInput, error) {
	fingerprint, err := profile.Fingerprint()
	if err != nil {
		return PreparedInput{}, err
	}
	if err := validateEvent(target, target.GraphID); err != nil {
		return PreparedInput{}, err
	}
	byID := map[model.ID]*scoredCandidate{}
	seenChannels := map[string]bool{}
	for _, channel := range channels {
		if !slices.Contains(profile.Channels, channel.Name) || seenChannels[channel.Name] {
			return PreparedInput{}, fmt.Errorf("추론 채널이 중복되거나 프로필 밖이다")
		}
		seenChannels[channel.Name] = true
		seen := map[model.ID]bool{}
		for index, candidate := range channel.Candidates {
			if err := validateEvent(candidate.Event, target.GraphID); err != nil {
				return PreparedInput{}, err
			}
			if candidate.Event.ID == target.ID || seen[candidate.Event.ID] {
				return PreparedInput{}, fmt.Errorf("추론 후보가 대상과 같거나 중복됐다")
			}
			seen[candidate.Event.ID] = true
			allowed, err := normalizeAllowed(candidate)
			if err != nil {
				return PreparedInput{}, err
			}
			if len(allowed) == 0 {
				continue
			}
			candidate.Allowed = allowed
			candidate.CauseDirections = slices.Clone(candidate.CauseDirections)
			slices.Sort(candidate.CauseDirections)
			found := byID[candidate.Event.ID]
			if found == nil {
				found = &scoredCandidate{candidate: candidate, score: new(big.Rat)}
				byID[candidate.Event.ID] = found
			} else if !reflect.DeepEqual(found.candidate.Event, candidate.Event) || !slices.Equal(found.candidate.Allowed, allowed) || found.candidate.PartOfDirection != candidate.PartOfDirection || !slices.Equal(found.candidate.CauseDirections, candidate.CauseDirections) {
				return PreparedInput{}, fmt.Errorf("채널의 같은 사건 판정 입력이 일치하지 않는다")
			}
			found.score.Add(found.score, big.NewRat(1, int64(index+1)))
			found.contributions = append(found.contributions, Contribution{channel.Name, index + 1})
		}
	}
	ranked := make([]*scoredCandidate, 0, len(byID))
	for _, candidate := range byID {
		ranked = append(ranked, candidate)
	}
	slices.SortFunc(ranked, func(a, b *scoredCandidate) int {
		if c := b.score.Cmp(a.score); c != 0 {
			return c
		}
		if c := b.candidate.Event.RecordedAt.Compare(a.candidate.Event.RecordedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.candidate.Event.ID.String(), b.candidate.Event.ID.String())
	})
	if len(ranked) > profile.CandidateLimit {
		ranked = ranked[:profile.CandidateLimit]
	}
	payload := inputPayload{SchemaVersion: profile.SchemaVersion}
	payload.Data.Target = eventExcerpt(target, "T", profile.TargetChars)
	payload.Data.Candidates = []inputCandidate{}
	result := PreparedInput{TargetID: target.ID, TargetVersion: target.Version, ProfileFingerprint: fingerprint}
	for _, candidate := range ranked {
		alias := fmt.Sprintf("C%d", len(result.Candidates)+1)
		item := inputCandidate{inputEvent: eventExcerpt(candidate.candidate.Event, alias, profile.CandidateChars)}
		for _, kind := range candidate.candidate.Allowed {
			direction := ""
			if kind == model.RelationTypePartOf {
				direction = candidate.candidate.PartOfDirection
			}
			entry := allowedRelation{Type: kind, Direction: direction}
			if kind == model.RelationTypeCauses {
				entry.Directions = slices.Clone(candidate.candidate.CauseDirections)
			}
			item.Allowed = append(item.Allowed, entry)
		}
		payload.Data.Candidates = append(payload.Data.Candidates, item)
		raw, err := json.Marshal(payload)
		if err != nil {
			return PreparedInput{}, err
		}
		if utf8.RuneCount(raw) > profile.InputChars {
			payload.Data.Candidates = payload.Data.Candidates[:len(payload.Data.Candidates)-1]
			break
		}
		slices.SortFunc(candidate.contributions, func(a, b Contribution) int { return strings.Compare(a.Channel, b.Channel) })
		result.Candidates = append(result.Candidates, Selected{candidate.candidate.Event.ID, candidate.candidate.Event.Version, alias, candidate.contributions})
	}
	result.Data, err = json.Marshal(payload)
	if err != nil {
		return PreparedInput{}, err
	}
	if utf8.RuneCount(result.Data) > profile.InputChars {
		return PreparedInput{}, fmt.Errorf("대상과 입력 구조가 전체 문자 상한을 초과한다")
	}
	return result, nil
}

func validateEvent(value model.Context, graph model.ID) error {
	if !graph.IsV7() || value.GraphID != graph || !value.ID.IsV7() || value.Version < 1 || value.Layer != model.LayerEvent || value.Event == nil || value.DeletedAt != nil || !utf8.ValidString(value.Body) {
		return fmt.Errorf("추론 대상은 같은 그래프의 활성 사건이어야 한다")
	}
	return nil
}

func normalizeAllowed(candidate Candidate) ([]model.RelationType, error) {
	allowed := slices.Clone(candidate.Allowed)
	slices.Sort(allowed)
	if len(slices.Compact(slices.Clone(allowed))) != len(allowed) {
		return nil, fmt.Errorf("판정 유형이 중복됐다")
	}
	for _, kind := range allowed {
		if !slices.Contains([]model.RelationType{model.RelationTypeCauses, model.RelationTypePartOf, model.RelationTypeRelatesTo}, kind) {
			return nil, fmt.Errorf("허용되지 않은 판정 유형이다")
		}
		if kind == model.RelationTypePartOf && candidate.PartOfDirection != "T_to_C" && candidate.PartOfDirection != "C_to_T" {
			return nil, fmt.Errorf("부분 관계 방향이 없다")
		}
		if kind == model.RelationTypeCauses {
			if len(candidate.CauseDirections) == 0 || len(candidate.CauseDirections) > 2 {
				return nil, fmt.Errorf("인과 관계 방향이 없다")
			}
			for _, direction := range candidate.CauseDirections {
				if direction != "T_to_C" && direction != "C_to_T" {
					return nil, fmt.Errorf("인과 관계 방향이 올바르지 않다")
				}
			}
			if len(candidate.CauseDirections) == 2 && candidate.CauseDirections[0] == candidate.CauseDirections[1] {
				return nil, fmt.Errorf("인과 관계 방향이 중복됐다")
			}
		}
	}
	return allowed, nil
}

func eventExcerpt(event model.Context, alias string, maxChars int) inputEvent {
	text := []rune(event.Body)
	if len(text) > maxChars {
		text = text[:maxChars]
	}
	return inputEvent{Alias: alias, Start: event.Event.Start, End: event.Event.End, Conversation: event.Event.ConversationID != "", Body: string(text)}
}
