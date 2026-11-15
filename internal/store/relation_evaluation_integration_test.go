package store

import (
	"agent_context_sharing/internal/relinfer"
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"os"

	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestRelationEvaluationCandidates는 명시한 합성 평가 파일을 별도 DB에서 검색한다.
func TestRelationEvaluationCandidates(t *testing.T) {
	if os.Getenv("RINF_JUDGE_EVALUATE") == "1" && os.Getenv("RINF_GENERATED_INPUTS") == "" {
		t.Fatal("RINF_GENERATED_INPUTS required before provider calls")
	}
	path := os.Getenv("RINF_EVALUATION_FILE")
	if path == "" {
		t.Skip("RINF_EVALUATION_FILE 미설정")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type event struct {
		Body  string
		Start time.Time
		End   *time.Time
	}
	var input struct {
		Samples []struct {
			ID, Split string
			Input     struct {
				Data struct {
					Target     event
					Candidates []struct {
						event
						Allowed []struct{ Type string }
					}
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &input, json.MatchCaseInsensitiveNames(true)); err != nil {
		t.Fatal(err)
	}
	if len(input.Samples) == 0 {
		t.Fatal("empty evaluation samples")
	}
	db := newIsolatedIntegrationStore(t)
	var vectors struct{ Items map[string][]float64 }
	if vpath := os.Getenv("RINF_VECTOR_FILE"); vpath != "" {
		raw, e := os.ReadFile(vpath)
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(raw, &vectors, json.MatchCaseInsensitiveNames(true)); e != nil {
			t.Fatal(e)
		}
	}
	var rows []map[string]any
	var judgments []map[string]any
	generatedInputs := map[string]string{}
	for _, sample := range input.Samples {
		if sample.Split != "eval" {
			continue
		}
		graph, actor, member := lifeProposalFixture(t, db)
		convert := func(e event) model.Context {
			c := testEventContext(t, graph, actor, member.ID)
			c.Body = e.Body
			c.Event.Start = e.Start
			c.Event.End = e.End
			return c
		}
		candidateInput := convert(sample.Input.Data.Candidates[0].event)
		if sample.ID == "n03" {
			candidateInput.Event.Start = candidateInput.Event.Start.Add(time.Hour)
		}
		if sample.Input.Data.Candidates[0].Allowed[0].Type == "part_of" {
			extra := testSourceContext(t, graph, actor, "urn:rinf:evaluation:member")
			extra.Body = "합성 평가의 상위 사건 추가 구성원"
			extra.Source.OccurredAt = candidateInput.Event.Start
			added, err := db.CreateContext(t.Context(), graph, extra, nil)
			if err != nil {
				t.Fatal(err)
			}
			candidateInput.Event.MemberIDs = append(candidateInput.Event.MemberIDs, added.ID)
		}
		candidate, err := db.CreateContext(t.Context(), graph, candidateInput, nil)
		if err != nil {
			t.Fatal(err)
		}
		target, err := db.CreateContext(t.Context(), graph, convert(sample.Input.Data.Target), nil)
		if err != nil {
			t.Fatal(err)
		}
		distractor := testEventContext(t, graph, actor, member.ID)
		distractor.Body = "천문대에서 토성 관측 사진을 정리했다."
		other, err := db.CreateContext(t.Context(), graph, distractor, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(vectors.Items) > 0 {
			for _, c := range []model.Context{target, candidate, other} {
				v := vectors.Items[fmt.Sprintf("%x", sha256.Sum256([]byte(c.Body)))]
				if len(v) != 768 {
					t.Fatal("missing fixed embedding")
				}
				claimExec(t, db, `INSERT INTO public.context_embedding(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier) VALUES($1,$2,$3,$4,now(),now(),'hot')`, c.ID.String(), graph.String(), vectorText(v), "gemini-embedding-2")
			}
			if _, e := db.pool.Exec(t.Context(), `DELETE FROM public.index_task WHERE graph_id=$1`, graph.String()); e != nil {
				t.Fatal(e)
			}
		}
		req := model.InferenceCandidateRequest{Task: model.RelationInferenceTask{GraphID: graph, ContextID: target.ID, TargetVersion: target.Version, Generation: time.Now().Add(-time.Hour)}, ProfileFingerprint: testInferenceProfile, EmbeddingModel: "gemini-embedding-2", Channels: []string{"keyword", "member", "rule", "conversation"}, Limit: 8, IndexWait: time.Minute, AdjacencyWindow: time.Hour}
		if len(vectors.Items) > 0 {
			req.Channels = append([]string{"semantic"}, req.Channels...)
		}
		snapshot, err := db.InferenceCandidates(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("RINF_JUDGE_EVALUATE") == "1" && slices.Contains([]string{"n01", "n03", "n05", "n06"}, sample.ID) {
			profile := relinfer.Profile{Provider: "openai", Model: "gpt-6-luna", PromptVersion: "2", SchemaVersion: "1", EmbeddingModel: "gemini-embedding-2", IndexWait: time.Minute, AdjacencyWindow: time.Hour, Channels: req.Channels, CandidateLimit: 8, TargetChars: 2000, CandidateChars: 800, InputChars: 9000}
			built, e := relinfer.BuildInput(profile, snapshot.Target, snapshot.Channels)
			if e != nil {
				t.Fatal(e)
			}
			judge, e := relinfer.NewOpenAIJudgeWithPrompt(&http.Client{Timeout: 60 * time.Second}, "https://api.openai.com/v1", os.Getenv("RELATION_INFERENCE_API_KEY"), "gpt-6-luna", "2")
			if e != nil {
				t.Fatal(e)
			}
			generatedInputs[sample.ID] = string(built.Data)
			for round := range 10 {
				started := time.Now()
				r, e := judge.Judge(t.Context(), built.Data)
				if e != nil {
					t.Fatal(e)
				}
				checked, e := relinfer.ValidateOutput(built, r.Output)
				valid, match := false, false
				if e == nil {
					for _, c := range checked {
						if c.CandidateID == candidate.ID && c.Judgment.RelationType == model.RelationType(sample.Input.Data.Candidates[0].Allowed[0].Type) {
							valid = c.InvalidReason == ""
							if sample.ID == "n06" {
								match = valid && (c.Judgment.Decision == "none" || c.Judgment.Decision == "abstain")
							} else {
								match = valid && c.Judgment.Decision == "relation"
								if sample.ID == "n03" || sample.ID == "n05" {
									match = match && c.Judgment.Direction == "T_to_C"
								}
							}
						}
					}
				}
				judgments = append(judgments, map[string]any{"sample_id": sample.ID, "repeat": round + 1, "input_sha256": fmt.Sprintf("%x", sha256.Sum256(built.Data)), "profile_fingerprint": built.ProfileFingerprint, "valid": valid, "expected_pair_match": match, "input_tokens": r.InputTokens, "output_tokens": r.OutputTokens, "latency_ms": time.Since(started).Milliseconds()})
			}
		}
		for _, ch := range snapshot.Channels {
			row := map[string]any{"sample_id": sample.ID, "channel": ch.Name, "candidates": len(ch.Candidates), "pair_found": false, "type_eligible": false, "distractor_found": false}
			for _, c := range ch.Candidates {
				if c.Event.ID == candidate.ID {
					row["pair_found"] = true
					row["type_eligible"] = slices.Contains(c.Allowed, model.RelationType(sample.Input.Data.Candidates[0].Allowed[0].Type))
				}
				if c.Event.ID == other.ID {
					row["distractor_found"] = true
				}
			}
			rows = append(rows, row)
		}
	}
	if os.Getenv("RINF_JUDGE_EVALUATE") == "1" {
		if len(judgments) != 40 {
			t.Fatalf("expected 40 judgments, got %d", len(judgments))
		}
		p := os.Getenv("RINF_GENERATED_INPUTS")
		if p == "" {
			t.Fatal("RINF_GENERATED_INPUTS required")
		}
		raw, e := json.Marshal(generatedInputs)
		if e != nil {
			t.Fatal(e)
		}
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(raw); e != nil {
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
	}
	want := 24
	semantic := "unmeasured"
	if len(vectors.Items) > 0 {
		want = 30
		semantic = "fixed_real_gemini_vectors"
	}
	if len(rows) != want {
		t.Fatalf("unexpected channel row count %d", len(rows))
	}
	result, err := json.Marshal(map[string]any{"rows": rows, "judgments": judgments, "semantic": semantic, "scope": "synthetic_candidate_search", "default_activation": false})
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("RINF_CANDIDATE_REPORT")
	if out == "" {
		t.Fatal("report path required")
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(result); err != nil {
		t.Fatal(err)
	}
}
