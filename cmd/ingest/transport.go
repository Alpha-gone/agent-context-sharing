package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
)

type request struct {
	Graph        string `json:"graph_id"`
	Conversation string `json:"conversation_id"`
	Turns        []turn `json:"turns"`
}
type response struct {
	Turns []struct {
		Index     int64  `json:"turn_index"`
		Status    string `json:"status"`
		ContextID string `json:"context_id"`
		Reason    string `json:"reason"`
	} `json:"turns"`
}

func flush(ctx context.Context, client *http.Client, state *sessionState, graph, endpoint, credential string, save func() error) error {
	for len(state.Pending) > 0 {
		first := state.Pending[0]
		if first.Graph != graph || first.Endpoint != endpoint {
			return errors.New("pending destination changed")
		}
		batch := request{Graph: graph, Conversation: first.Conversation, Turns: []turn{}}
		base, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		size := len(base)
		for _, item := range state.Pending {
			if len(batch.Turns) == 100 || item.Graph != graph || item.Endpoint != endpoint || item.Conversation != first.Conversation {
				break
			}
			encoded, err := json.Marshal(item.Turn)
			if err != nil {
				return err
			}
			extra := len(encoded)
			if len(batch.Turns) > 0 {
				extra++
			}
			if size+extra > 4<<20 {
				break
			}
			batch.Turns = append(batch.Turns, item.Turn)
			size += extra
		}
		if len(batch.Turns) == 0 {
			return errors.New("turn exceeds request limit")
		}
		data, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		result, err := client.Do(req)
		if err != nil {
			return errors.New("request")
		}
		raw, readErr := io.ReadAll(io.LimitReader(result.Body, 1<<20))
		closeErr := result.Body.Close()
		if readErr != nil || closeErr != nil || result.StatusCode != http.StatusOK || len(raw) >= 1<<20 {
			return errors.New("response")
		}
		var reply response
		if err := json.Unmarshal(raw, &reply); err != nil || len(reply.Turns) != len(batch.Turns) {
			return errors.New("response")
		}
		byIndex := make(map[int64]int, len(batch.Turns))
		for _, item := range batch.Turns {
			byIndex[item.Index] = 1
		}
		var rejected []rejection
		for _, item := range reply.Turns {
			if byIndex[item.Index] != 1 {
				return errors.New("response")
			}
			byIndex[item.Index] = 2
			switch item.Status {
			case "stored", "existing":
				if item.ContextID == "" || item.Reason != "" {
					return errors.New("response")
				}
			case "rejected":
				if item.Reason != "body_mismatch" && item.Reason != "invalid_argument" && item.Reason != "limit_exceeded" {
					return errors.New("response")
				}
				rejected = append(rejected, rejection{Index: item.Index, Reason: item.Reason})
			default:
				return errors.New("response")
			}
		}
		state.Pending = state.Pending[len(batch.Turns):]
		state.Rejected = append(state.Rejected, rejected...)
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}
