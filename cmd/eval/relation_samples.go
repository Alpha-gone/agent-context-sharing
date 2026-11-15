package main

import (
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/relinfer"
)

type relationSampleEvent struct {
	Alias        string     `json:"alias"`
	Start        time.Time  `json:"start"`
	End          *time.Time `json:"end,omitzero"`
	Conversation bool       `json:"conversation"`
	Body         string     `json:"body"`
}
type relationSampleAllowed struct {
	Type       model.RelationType `json:"type"`
	Direction  string             `json:"direction,omitempty"`
	Directions []string           `json:"directions,omitempty"`
}
type relationSampleCandidate struct {
	relationSampleEvent
	Allowed []relationSampleAllowed `json:"allowed"`
}
type relationSamplePayload struct {
	SchemaVersion string `json:"schema_version"`
	Data          struct {
		Target     relationSampleEvent       `json:"target"`
		Candidates []relationSampleCandidate `json:"candidates"`
	} `json:"data"`
}
type relationSampleFile struct {
	Version string `json:"version"`
	Samples []struct {
		ID    string                `json:"id"`
		Split string                `json:"split"`
		Input relationSamplePayload `json:"input"`
	} `json:"samples"`
}
type relationGoldFile struct {
	Version string `json:"version"`
	Samples []struct {
		ID           string             `json:"id"`
		Split        string             `json:"split"`
		Category     string             `json:"category"`
		Candidate    string             `json:"candidate"`
		RelationType model.RelationType `json:"relation_type"`
		Expected     string             `json:"expected"`
		Direction    string             `json:"direction"`
	} `json:"samples"`
}

type relationSampleCheck struct {
	Version           string `json:"version"`
	InputSHA256       string `json:"input_sha256"`
	GoldSHA256        string `json:"gold_sha256"`
	Tuning            int    `json:"tuning_samples"`
	Evaluation        int    `json:"evaluation_samples"`
	Status            string `json:"status"`
	DefaultActivation bool   `json:"default_activation"`
}

func readRelationSample(path string, value any) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("관계 추론 표본 파일 열기 실패")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil || len(raw) > 16<<20 {
		return "", fmt.Errorf("관계 추론 표본 파일 크기 또는 읽기 오류")
	}
	if err := json.Unmarshal(raw, value, json.RejectUnknownMembers(true)); err != nil {
		return "", fmt.Errorf("관계 추론 표본 JSON 형식 오류")
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

// checkRelationSamples는 제공자나 DB를 호출하지 않고 모델 입력 생성 경로와 정답의 대응을 확인한다.
func checkRelationSamples(inputPath, goldPath string) (relationSampleCheck, error) {
	invalid := func() (relationSampleCheck, error) {
		return relationSampleCheck{}, fmt.Errorf("관계 추론 표본·정답·생성 입력이 일치하지 않는다")
	}
	var input relationSampleFile
	var gold relationGoldFile
	ih, err := readRelationSample(inputPath, &input)
	if err != nil {
		return relationSampleCheck{}, err
	}
	gh, err := readRelationSample(goldPath, &gold)
	if err != nil {
		return relationSampleCheck{}, err
	}
	if !manifestCode.MatchString(input.Version) || input.Version != gold.Version || len(input.Samples) == 0 || len(input.Samples) != len(gold.Samples) {
		return invalid()
	}
	graph, _ := model.ParseID("018cc251-f400-7000-8000-000000000001")
	targetID, _ := model.ParseID("018cc251-f400-7000-8000-000000000002")
	candidateID, _ := model.ParseID("018cc251-f400-7000-8000-000000000003")
	profile := relinfer.Profile{Provider: "openai", Model: "gpt-6-luna", PromptVersion: "1", SchemaVersion: "1", EmbeddingModel: "gemini-embedding-2", IndexWait: 10 * time.Minute, AdjacencyWindow: time.Hour, Channels: []string{"semantic", "keyword", "member", "rule", "conversation"}, CandidateLimit: 8, TargetChars: 2000, CandidateChars: 800, InputChars: 9000}
	result := relationSampleCheck{Version: input.Version, InputSHA256: ih, GoldSHA256: gh, Status: "sample_structure_only"}
	seen := map[string]bool{}
	seenInputs := map[[32]byte]bool{}
	for i, s := range input.Samples {
		g := gold.Samples[i]
		if !manifestCode.MatchString(s.ID) || seen[s.ID] || s.ID != g.ID || s.Split != g.Split || !slices.Contains([]string{"tune", "eval"}, s.Split) || !manifestCode.MatchString(g.Category) {
			return invalid()
		}
		seen[s.ID] = true
		if s.Split == "tune" {
			result.Tuning++
		} else {
			result.Evaluation++
		}
		p := s.Input
		if p.SchemaVersion != "1" || p.Data.Target.Alias != "T" || len(p.Data.Candidates) != 1 {
			return invalid()
		}
		c := p.Data.Candidates[0]
		if c.Alias != "C1" || g.Candidate != c.Alias || len(c.Allowed) != 1 || c.Allowed[0].Type != g.RelationType || p.Data.Target.Start.IsZero() || c.Start.IsZero() || p.Data.Target.Body == "" || c.Body == "" {
			return invalid()
		}
		if !slices.Contains([]string{"relation", "none", "abstain_or_none"}, g.Expected) {
			return invalid()
		}
		a := c.Allowed[0]
		switch a.Type {
		case model.RelationTypeRelatesTo:
			if a.Direction != "" || len(a.Directions) != 0 || g.Direction != "" {
				return invalid()
			}
		case model.RelationTypePartOf:
			if !slices.Contains([]string{"T_to_C", "C_to_T"}, a.Direction) || len(a.Directions) != 0 || (g.Expected == "relation" && g.Direction != a.Direction) || (g.Expected != "relation" && g.Direction != "") {
				return invalid()
			}
		case model.RelationTypeCauses:
			if a.Direction != "" || len(a.Directions) == 0 || (g.Expected == "relation" && !slices.Contains(a.Directions, g.Direction)) || (g.Expected != "relation" && g.Direction != "") {
				return invalid()
			}
		default:
			return invalid()
		}
		event := func(e relationSampleEvent, id model.ID) model.Context {
			conversation := ""
			if e.Conversation {
				conversation = "synthetic"
			}
			return model.Context{ID: id, GraphID: graph, Layer: model.LayerEvent, Body: e.Body, Version: 1, RecordedAt: e.Start, Event: &model.EventAttributes{Start: e.Start, End: e.End, ConversationID: conversation}}
		}
		candidate := relinfer.Candidate{Event: event(c.relationSampleEvent, candidateID), Allowed: []model.RelationType{a.Type}, PartOfDirection: a.Direction, CauseDirections: a.Directions}
		built, err := relinfer.BuildInput(profile, event(p.Data.Target, targetID), []relinfer.Channel{{Name: "keyword", Candidates: []relinfer.Candidate{candidate}}})
		if err != nil {
			return invalid()
		}
		// 정답 자체가 서버의 실제 출력·인용 검증 계약으로 표현 가능한지도 확인한다.
		decision := g.Expected
		if decision == "abstain_or_none" {
			decision = "abstain"
		}
		direction := g.Direction
		if a.Type == model.RelationTypePartOf {
			direction = a.Direction
		} else if a.Type == model.RelationTypeCauses && direction == "" {
			direction = a.Directions[0]
		}
		judgment := relinfer.Judgment{Candidate: c.Alias, RelationType: a.Type, Decision: decision, Direction: direction}
		if decision == "relation" {
			target := []rune(strings.TrimSpace(p.Data.Target.Body))
			candidate := []rune(strings.TrimSpace(c.Body))
			if len(target) == 0 || len(candidate) == 0 {
				return invalid()
			}
			judgment.Evidence = relinfer.Evidence{Target: string(target[0]), Candidate: string(candidate[0])}
		}
		output, err := json.Marshal(struct {
			SchemaVersion string              `json:"schema_version"`
			Decisions     []relinfer.Judgment `json:"decisions"`
		}{p.SchemaVersion, []relinfer.Judgment{judgment}})
		if err != nil {
			return invalid()
		}
		checked, err := relinfer.ValidateOutput(built, output)
		if err != nil || len(checked) != 1 || checked[0].InvalidReason != "" {
			return invalid()
		}
		expected, err := json.Marshal(p)
		if err != nil {
			return invalid()
		}
		sum := sha256.Sum256(expected)
		if seenInputs[sum] {
			return invalid()
		}
		seenInputs[sum] = true
		var left, right any
		if json.Unmarshal(expected, &left) != nil || json.Unmarshal(built.Data, &right) != nil || !reflect.DeepEqual(left, right) {
			return relationSampleCheck{}, fmt.Errorf("생성 입력 불일치: %s", s.ID)
		}
	}
	if result.Tuning == 0 || result.Evaluation == 0 {
		return invalid()
	}
	return result, nil
}
