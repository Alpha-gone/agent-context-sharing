package store

import (
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/relinfer"
)

type acceptanceEvent struct {
	Body           string     `json:"body"`
	Start          time.Time  `json:"start"`
	End            *time.Time `json:"end"`
	Members        []string   `json:"members"`
	ConversationID string     `json:"conversation_id"`
}
type acceptanceCase struct {
	ID           string          `json:"id"`
	ScenarioID   string          `json:"scenario_id"`
	Category     string          `json:"category"`
	RelationType string          `json:"relation_type"`
	Target       acceptanceEvent `json:"target"`
	Candidate    acceptanceEvent `json:"candidate"`
	Distractor   acceptanceEvent `json:"distractor"`
}

// TestRelationAcceptancePrepare는 제공자 호출 없이 실제 DB 후보와 고정 판정 입력을 생성한다.
func TestRelationAcceptancePrepare(t *testing.T) {
	dir := os.Getenv("RINF_ACCEPTANCE_DIR")
	if dir == "" {
		t.Skip("RINF_ACCEPTANCE_DIR 미설정")
	}
	raw, e := os.ReadFile(dir + "/samples.json")
	if e != nil {
		t.Fatal(e)
	}
	var data struct {
		Version string           `json:"version"`
		Samples []acceptanceCase `json:"samples"`
	}
	if e = json.Unmarshal(raw, &data); e != nil {
		t.Fatal(e)
	}
	if len(data.Samples) != 400 {
		t.Fatalf("expected 400 independent cases, got %d", len(data.Samples))
	}
	db := newIsolatedIntegrationStore(t)
	f, e := os.OpenFile(dir+"/prepared.jsonl", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	var vectors struct {
		Items map[string][]float64 `json:"items"`
	}
	raw, e = os.ReadFile(dir + "/vectors.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &vectors); e != nil {
		t.Fatal(e)
	}
	for _, sample := range data.Samples {
		graph, actor, base := lifeProposalFixture(t, db)
		members := map[string]model.ID{}
		makeEvent := func(v acceptanceEvent) model.Context {
			c := testEventContext(t, graph, actor, base.ID)
			c.Body = v.Body
			c.Event.Start = v.Start
			c.Event.End = v.End
			c.Event.MemberIDs = nil
			c.Event.ConversationID = v.ConversationID
			for _, name := range v.Members {
				id, ok := members[name]
				if !ok {
					value := lifeDerived(t, db, graph, actor, base.ID)
					id = value.ID
					members[name] = id
				}
				c.Event.MemberIDs = append(c.Event.MemberIDs, id)
			}
			if len(c.Event.MemberIDs) == 0 {
				t.Fatal("empty member set")
			}
			stored, e := db.CreateContext(t.Context(), graph, c, nil)
			if e != nil {
				t.Fatal(e)
			}
			return stored
		}
		candidate := makeEvent(sample.Candidate)
		target := makeEvent(sample.Target)
		other := makeEvent(sample.Distractor)
		for _, c := range []model.Context{candidate, target, other} {
			v := vectors.Items[fmt.Sprintf("%x", sha256.Sum256([]byte(c.Body)))]
			if len(v) != 768 {
				t.Fatalf("missing embedding: %s", sample.ID)
			}
			claimExec(t, db, `INSERT INTO public.context_embedding(context_id,graph_id,embedding,model_id,indexed_at,last_accessed_at,storage_tier) VALUES($1,$2,$3,$4,now(),now(),'hot')`, c.ID.String(), graph.String(), vectorText(v), "gemini:gemini-embedding-2:retrieval-v1:vector:768")
		}
		if _, e = db.pool.Exec(t.Context(), `DELETE FROM public.index_task WHERE graph_id=$1`, graph.String()); e != nil {
			t.Fatal(e)
		}
		profile := relinfer.Profile{Provider: "openai", Model: "gpt-6-luna", PromptVersion: "2", SchemaVersion: "1", EmbeddingModel: "gemini:gemini-embedding-2:retrieval-v1:vector:768", IndexWait: time.Minute, AdjacencyWindow: time.Hour, Channels: []string{"semantic", "keyword", "member", "rule", "conversation"}, CandidateLimit: 8, TargetChars: 2000, CandidateChars: 800, InputChars: 9000, Parameters: map[string]string{"max_output_tokens": "1536", "service_tier": "default"}}
		adapter, e := relinfer.NewOpenAIJudgeWithOptions(&http.Client{}, "https://api.openai.com/v1", "configuration-probe", "gpt-6-luna", "2", 1536, "default")
		if e != nil {
			t.Fatal(e)
		}
		if e = adapter.ValidateProfile(profile); e != nil {
			t.Fatal(e)
		}
		fingerprint, e := profile.Fingerprint()
		if e != nil {
			t.Fatal(e)
		}
		snap, e := db.InferenceCandidates(t.Context(), model.InferenceCandidateRequest{Task: model.RelationInferenceTask{GraphID: graph, ContextID: target.ID, TargetVersion: target.Version, Generation: time.Now()}, ProfileFingerprint: fingerprint, EmbeddingModel: profile.EmbeddingModel, Channels: profile.Channels, Limit: 8, IndexWait: time.Minute, AdjacencyWindow: time.Hour})
		if e != nil {
			t.Fatal(e)
		}
		prepared, e := relinfer.BuildInput(profile, snap.Target, snap.Channels)
		if e != nil {
			t.Fatal(e)
		}
		channelRows := []map[string]any{}
		for _, ch := range snap.Channels {
			found, eligible, distractor := false, false, false
			for _, v := range ch.Candidates {
				if v.Event.ID == candidate.ID {
					found = true
					for _, a := range v.Allowed {
						if string(a) == sample.RelationType {
							eligible = true
						}
					}
				}
				if v.Event.ID == other.ID {
					distractor = true
				}
			}
			channelRows = append(channelRows, map[string]any{"channel": ch.Name, "pair_found": found, "type_eligible": eligible, "distractor_found": distractor, "count": len(ch.Candidates)})
		}
		row := map[string]any{"id": sample.ID, "scenario_id": sample.ScenarioID, "relation_type": sample.RelationType, "expected_candidate_id": candidate.ID.String(), "distractor_id": other.ID.String(), "input_sha256": fmt.Sprintf("%x", sha256.Sum256(prepared.Data)), "prepared": prepared, "channels": channelRows}
		b, e := json.Marshal(row)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(append(b, '\n')); e != nil {
			t.Fatal(e)
		}
		if e = f.Sync(); e != nil {
			t.Fatal(e)
		}
	}
}

// TestRelationAcceptanceValidate는 네트워크 없이 실제 서버 검증기로 안전한 판정 요약을 만든다.
func TestRelationAcceptanceValidate(t *testing.T) {
	path := os.Getenv("RINF_ACCEPTANCE_VALIDATE")
	if path == "" {
		t.Skip("RINF_ACCEPTANCE_VALIDATE 미설정")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var rows []struct {
		Key      string                 `json:"key"`
		Prepared relinfer.PreparedInput `json:"prepared"`
		Output   string                 `json:"output"`
	}
	if e = json.Unmarshal(raw, &rows); e != nil {
		t.Fatal(e)
	}
	result := []map[string]any{}
	for _, r := range rows {
		checked, e := relinfer.ValidateOutput(r.Prepared, []byte(r.Output))
		safe := []map[string]any{}
		if e == nil {
			for _, c := range checked {
				safe = append(safe, map[string]any{"candidate_id": c.CandidateID.String(), "relation_type": c.Judgment.RelationType, "decision": c.Judgment.Decision, "direction": c.Judgment.Direction, "invalid_reason": c.InvalidReason})
			}
		}
		result = append(result, map[string]any{"key": r.Key, "envelope_valid": e == nil, "decisions": safe})
	}
	raw, e = json.Marshal(result)
	if e != nil {
		t.Fatal(e)
	}
	f, e := os.OpenFile(path+".validated.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e = f.Write(raw); e != nil {
		t.Fatal(e)
	}
}
