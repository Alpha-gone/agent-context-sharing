package relinfer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"encoding/json/jsontext"
	json "encoding/json/v2"
)

// ErrInvalidOutput는 전체 출력이 판정 계약을 만족하지 못했음을 뜻한다.
var ErrInvalidOutput = errors.New("추론 출력 계약 위반")

// Judge는 제공자별 호출을 공통 JSON 판정 출력으로 바꾸는 경계다.
type Judge interface {
	Judge(context.Context, []byte) (JudgeResult, error)
}

// JudgeResult는 원시 응답이 아닌 판정 JSON과 제공자가 측정한 사용량을 담는다.
type JudgeResult struct {
	Output                    []byte
	InputTokens, OutputTokens *int64
	ProviderStatus            *string
}

// Evidence는 실제 전송한 두 발췌의 인용이다. 진단 출력에 사용하지 않는다.
type Evidence struct {
	Target    string `json:"target"`
	Candidate string `json:"candidate"`
}

// Judgment는 하나의 쌍과 유형에 대한 모델 판정이다.
type Judgment struct {
	Candidate    string             `json:"candidate"`
	RelationType model.RelationType `json:"relation_type"`
	Decision     string             `json:"decision"`
	Direction    string             `json:"direction"`
	Evidence     Evidence           `json:"evidence"`
	Reason       string             `json:"reason"`
}

// CheckedJudgment는 서버의 검증 결과와 내부 후보 식별자를 담는다.
type CheckedJudgment struct {
	Judgment         Judgment
	CandidateID      model.ID
	CandidateVersion int64
	InvalidReason    string
}

// ValidateOutput은 전체 JSON 형식 오류와 개별 판정 위반을 구분한다.
func ValidateOutput(input PreparedInput, raw []byte) ([]CheckedJudgment, error) {
	var sent inputPayload
	if err := json.Unmarshal(input.Data, &sent, json.RejectUnknownMembers(true)); err != nil || sent.SchemaVersion == "" || sent.Data.Target.Alias != "T" {
		return nil, fmt.Errorf("판정 입력이 올바르지 않다: %w", ErrInvalidOutput)
	}
	var envelope struct {
		SchemaVersion string           `json:"schema_version"`
		Decisions     []jsontext.Value `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &envelope, json.RejectUnknownMembers(true)); err != nil || envelope.SchemaVersion != sent.SchemaVersion || envelope.Decisions == nil {
		return nil, ErrInvalidOutput
	}
	// 각 후보×유형은 최대 한 번만 판정한다. 초과 출력은 전체 형식 위반이다.
	if len(envelope.Decisions) > len(sent.Data.Candidates)*3 {
		return nil, ErrInvalidOutput
	}
	type entry struct {
		input    inputCandidate
		selected Selected
	}
	candidates := make(map[string]entry, len(sent.Data.Candidates))
	if len(sent.Data.Candidates) != len(input.Candidates) {
		return nil, ErrInvalidOutput
	}
	for i, candidate := range sent.Data.Candidates {
		selected := input.Candidates[i]
		if selected.Alias != candidate.Alias || !selected.ID.IsV7() || selected.Version < 1 {
			return nil, ErrInvalidOutput
		}
		candidates[candidate.Alias] = entry{candidate, selected}
	}
	result := make([]CheckedJudgment, 0, len(envelope.Decisions))
	seen := make(map[string]int)
	for _, value := range envelope.Decisions {
		var judgment Judgment
		checked := CheckedJudgment{}
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(value, &fields); err != nil || fields == nil || fields["candidate"] == nil || fields["relation_type"] == nil || fields["decision"] == nil {
			checked.InvalidReason = "output_invalid"
			result = append(result, checked)
			continue
		}
		if err := json.Unmarshal(value, &judgment, json.RejectUnknownMembers(true)); err != nil {
			checked.InvalidReason = "output_invalid"
			result = append(result, checked)
			continue
		}
		checked.Judgment = judgment
		candidate, ok := candidates[judgment.Candidate]
		if !ok {
			checked.InvalidReason = "output_invalid"
			result = append(result, checked)
			continue
		}
		checked.CandidateID, checked.CandidateVersion = candidate.selected.ID, candidate.selected.Version
		key := judgment.Candidate + "/" + string(judgment.RelationType)
		allowed, found := allowedFor(candidate.input.Allowed, judgment.RelationType)
		switch {
		case !found || !slices.Contains([]string{"relation", "none", "abstain"}, judgment.Decision):
			checked.InvalidReason = "output_invalid"
		case utf8.RuneCountInString(judgment.Reason) > 500:
			checked.InvalidReason = "output_invalid"
		case judgment.RelationType == model.RelationTypeRelatesTo && judgment.Direction != "":
			checked.InvalidReason = "output_invalid"
		case judgment.RelationType == model.RelationTypePartOf && judgment.Direction != allowed.Direction:
			checked.InvalidReason = "output_invalid"
		case judgment.RelationType == model.RelationTypeCauses && !slices.Contains(allowed.Directions, judgment.Direction):
			checked.InvalidReason = "output_invalid"
		case judgment.Decision != "relation" && (judgment.Evidence.Target != "" || judgment.Evidence.Candidate != ""):
			checked.InvalidReason = "output_invalid"
		case judgment.Decision == "relation" && (strings.TrimSpace(judgment.Evidence.Target) == "" || strings.TrimSpace(judgment.Evidence.Candidate) == "" || utf8.RuneCountInString(judgment.Evidence.Target) > 500 || utf8.RuneCountInString(judgment.Evidence.Candidate) > 500 || !strings.Contains(sent.Data.Target.Body, judgment.Evidence.Target) || !strings.Contains(candidate.input.Body, judgment.Evidence.Candidate)):
			checked.InvalidReason = "evidence_unverified"
		}
		if previous, duplicate := seen[key]; duplicate {
			checked.InvalidReason = "output_invalid"
			result[previous].InvalidReason = "output_invalid"
		} else {
			seen[key] = len(result)
		}
		result = append(result, checked)
	}
	for _, candidate := range sent.Data.Candidates {
		selected := candidates[candidate.Alias].selected
		for _, allowed := range candidate.Allowed {
			key := candidate.Alias + "/" + string(allowed.Type)
			if _, exists := seen[key]; !exists {
				result = append(result, CheckedJudgment{Judgment: Judgment{Candidate: candidate.Alias, RelationType: allowed.Type, Decision: "invalid"}, CandidateID: selected.ID, CandidateVersion: selected.Version, InvalidReason: "output_invalid"})
			}
		}
	}
	return result, nil
}

func allowedFor(values []allowedRelation, kind model.RelationType) (allowedRelation, bool) {
	for _, value := range values {
		if value.Type == kind {
			return value, true
		}
	}
	return allowedRelation{}, false
}

// StaticJudge는 외부 호출 없이 같은 결과를 반환하는 결정적 판정 대역이다.
type StaticJudge struct{ Result JudgeResult }

func (s StaticJudge) Judge(ctx context.Context, _ []byte) (JudgeResult, error) {
	if err := ctx.Err(); err != nil {
		return JudgeResult{}, err
	}
	result := s.Result
	result.Output = slices.Clone(s.Result.Output)
	return result, nil
}
