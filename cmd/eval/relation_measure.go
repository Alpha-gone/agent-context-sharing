package main

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/relinfer"
)

// measureRelation은 선택한 고정 표본을 반복 측정한다. 결과에는 본문과 원시 출력을 넣지 않는다.
func measureRelation(inputPath, goldPath, outPath, split, prompt string, repeats int) error {
	if (split != "tune" && split != "eval") || (prompt != "1" && prompt != "2") || repeats < 1 || repeats > 10 {
		return fmt.Errorf("관계 측정 분할·프롬프트 판·반복 횟수가 올바르지 않다")
	}
	check, err := checkRelationSamples(inputPath, goldPath)
	if err != nil {
		return err
	}
	var input relationSampleFile
	var gold relationGoldFile
	inputHash, err := readRelationSample(inputPath, &input)
	if err != nil {
		return err
	}
	goldHash, err := readRelationSample(goldPath, &gold)
	if err != nil {
		return err
	}
	if inputHash != check.InputSHA256 || goldHash != check.GoldSHA256 {
		return fmt.Errorf("검사 중 표본이 변경되었다")
	}
	key := os.Getenv("RELATION_INFERENCE_API_KEY")
	judge, err := relinfer.NewOpenAIJudgeWithPrompt(&http.Client{Timeout: 60 * time.Second}, "https://api.openai.com/v1", key, "gpt-6-luna", prompt)
	if err != nil {
		return err
	}
	profile := relinfer.Profile{Provider: "openai", Model: "gpt-6-luna", PromptVersion: prompt, SchemaVersion: "1", EmbeddingModel: "gemini-embedding-2", IndexWait: 10 * time.Minute, AdjacencyWindow: time.Hour, Channels: []string{"semantic", "keyword", "member", "rule", "conversation"}, CandidateLimit: 8, TargetChars: 2000, CandidateChars: 800, InputChars: 9000}
	if err := judge.ValidateProfile(profile); err != nil {
		return err
	}
	fingerprint, err := profile.Fingerprint()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("결과 파일 생성: %w", err)
	}
	defer f.Close()
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = f.Write(append(b, '\n'))
		if err != nil {
			return err
		}
		return f.Sync()
	}
	if err := write(map[string]any{"record": "manifest", "input_sha256": inputHash, "gold_sha256": goldHash, "profile_fingerprint": fingerprint, "prompt_version": prompt, "sample_split": split, "repeats": repeats, "default_activation": false, "status": "exploratory", "price": "unmeasured"}); err != nil {
		return err
	}
	id, err := model.ParseID("018cc251-f400-7000-8000-000000000003")
	if err != nil {
		return err
	}
	for round := 1; round <= repeats; round++ {
		for i, s := range input.Samples {
			if s.Split != split {
				continue
			}
			data, err := json.Marshal(s.Input)
			if err != nil {
				return err
			}
			start := time.Now()
			response, err := judge.Judge(context.Background(), data)
			if err != nil {
				if e := write(map[string]any{"record": "failure", "sample_id": s.ID, "repeat": round, "error": "provider_failure"}); e != nil {
					return e
				}
				return fmt.Errorf("표본 %s 제공자 호출: %w", s.ID, err)
			}
			checked, validationErr := relinfer.ValidateOutput(relinfer.PreparedInput{Data: data, Candidates: []relinfer.Selected{{ID: id, Version: 1, Alias: "C1"}}}, response.Output)
			row := map[string]any{"record": "sample", "sample_id": s.ID, "repeat": round, "latency_ms": time.Since(start).Milliseconds(), "input_tokens": response.InputTokens, "output_tokens": response.OutputTokens, "valid": false, "match": false}
			if validationErr == nil && len(checked) == 1 {
				c := checked[0]
				g := gold.Samples[i]
				match := c.InvalidReason == "" && (c.Judgment.Decision == g.Expected || g.Expected == "abstain_or_none" && (c.Judgment.Decision == "none" || c.Judgment.Decision == "abstain")) && (c.Judgment.Decision != "relation" || c.Judgment.Direction == g.Direction)
				row["valid"], row["match"], row["invalid_reason"], row["decision"] = c.InvalidReason == "", match, c.InvalidReason, c.Judgment.Decision
			}
			if err := write(row); err != nil {
				return err
			}
		}
	}
	return write(map[string]any{"record": "complete", "status": "deferred", "candidate_search": "unmeasured", "total_cost": "unmeasured", "default_activation": false})
}
