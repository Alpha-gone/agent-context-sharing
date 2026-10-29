package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// 실제 추론 없이 CLI의 네이티브 MCP 도구 실행을 구동하는 Responses 대역이다.
// 도구 이름은 CLI가 보낸 목록에서 찾고 식별자는 실제 서버 결과에서만 얻는다.
type test08Model struct {
	mu             sync.Mutex
	enabled        bool
	replay         bool
	replayError    string
	parallelReplay bool
	replayRejected bool
	beforeUpdate   func(string) error
	beforeReplay   func(string) error
	updateVersion  float64
	step           int
	catalog        int
	names          []string
	failure        string
	agent          string
	graph          string
	node           string
	version        float64
	observed       []string
}

func (script *test08Model) observe(tool string, data []byte) {
	if tool == "" {
		return
	}
	script.mu.Lock()
	defer script.mu.Unlock()
	var result struct {
		Result struct {
			IsError bool           `json:"isError"`
			Value   map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &result) != nil || result.Result.Value == nil {
		script.failure = "tool_result"
		return
	}
	if result.Result.IsError {
		if script.replayError != "" && script.step == 8 && tool == "graph_update" && result.Result.Value["code"] == script.replayError {
			if script.replayError == "version_conflict" && fmt.Sprint(result.Result.Value["current_version"]) != "2" {
				script.failure = "conflict_version"
				return
			}
			script.replayRejected = true
			script.observed = append(script.observed, tool)
			return
		}
		script.failure = "tool_result"
		return
	}
	script.observed = append(script.observed, tool)
	value := result.Result.Value
	if agent, ok := value["agent_id"].(string); ok {
		script.agent = agent
	}
	if graph, ok := value["graph"].(map[string]any); ok {
		script.graph, _ = graph["graph_id"].(string)
		script.version, _ = graph["version"].(float64)
	}
	if node, ok := value["node"].(map[string]any); ok {
		script.node, _ = node["context_id"].(string)
	}
}

func (script *test08Model) serve(w http.ResponseWriter, r *http.Request) {
	script.mu.Lock()
	defer script.mu.Unlock()
	if !script.enabled {
		http.Error(w, "local inference guard", http.StatusBadRequest)
		return
	}
	var request struct {
		Tools []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Tools []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"tools"`
		} `json:"tools"`
	}
	if json.UnmarshalRead(io.LimitReader(r.Body, 8<<20), &request) != nil {
		script.failure = "model_request"
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	script.catalog = len(request.Tools)
	script.names = nil
	for _, tool := range request.Tools {
		script.names = append(script.names, tool.Type+":"+tool.Name)
		for _, nested := range tool.Tools {
			script.names = append(script.names, nested.Type+":"+nested.Name)
		}
	}
	steps := []string{"agent_register", "graph_list", "graph_create", "node_create", "node_get", "graph_get", "graph_update", "graph_get"}
	if script.replay {
		steps = []string{"agent_register", "graph_list", "graph_create", "node_create", "node_get", "graph_get", "graph_update", "graph_update", "graph_get"}
	}
	if script.replayError != "" {
		steps = steps[:8]
	}
	if script.step > len(steps) || script.failure != "" {
		http.Error(w, "fixture stopped", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(kind string, fields map[string]any) {
		fields["type"] = kind
		data, err := json.Marshal(fields)
		if err != nil {
			script.failure = "model_encoding"
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	}
	responseID := fmt.Sprintf("resp_test08_%d", script.step)
	emit("response.created", map[string]any{"response": map[string]any{"id": responseID, "status": "in_progress", "output": []any{}}})
	var item map[string]any
	if script.step < len(steps) {
		tool := steps[script.step]
		name := ""
		namespace := ""
		for _, candidate := range request.Tools {
			if candidate.Name == tool || strings.HasSuffix(candidate.Name, "__"+tool) {
				name = candidate.Name
			}
			if candidate.Name == "mcp__test08" {
				for _, nested := range candidate.Tools {
					if nested.Name == tool {
						name, namespace = nested.Name, candidate.Name
					}
				}
			}
		}
		if name == "" {
			script.failure = "missing_tool"
			return
		}
		args := map[string]any{}
		switch script.step {
		case 0:
			args["registration_key"] = "test08-registration-001"
		case 2:
			args["name"], args["request_id"] = "TEST08 graph", "test08-graph-create-001"
		case 3:
			args = map[string]any{"graph_id": script.graph, "agent_id": script.agent, "layer": "source", "body": "TEST08 synthetic source", "source_ref": map[string]any{"channel": "other", "locator": "urn:test08:source"}, "occurred_at": "2026-10-26T00:00:00Z", "origin_kind": "agent_output", "request_id": "test08-node-create-001"}
		case 4:
			args["graph_id"], args["context_id"], args["hops"] = script.graph, script.node, 0
		case 5, 8:
			args["graph_id"] = script.graph
		case 6:
			if script.beforeUpdate != nil && script.beforeUpdate(script.graph) != nil {
				script.failure = "before_update"
				return
			}
			script.updateVersion = script.version
			args["graph_id"], args["expected_version"], args["name"], args["request_id"] = script.graph, script.version, "TEST08 changed", "test08-graph-update-001"
		case 7:
			args["graph_id"] = script.graph
			if script.replay {
				if script.beforeReplay != nil && script.beforeReplay(script.graph) != nil {
					script.failure = "before_replay"
					return
				}
				args["expected_version"], args["name"], args["request_id"] = script.updateVersion, "TEST08 changed", "test08-graph-update-001"
				if script.replayError == "invalid_argument" {
					args["name"] = "TEST08 conflicting fingerprint"
				}
				if script.replayError == "version_conflict" {
					args["request_id"] = "test08-graph-update-other"
				}
			}
		}
		encoded, _ := json.Marshal(args)
		item = map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_test08_%d", script.step), "call_id": fmt.Sprintf("call_test08_%d", script.step), "name": name, "arguments": string(encoded)}
		if namespace != "" {
			item["namespace"] = namespace
		}
		if script.parallelReplay && script.step == 6 {
			second := map[string]any{"type": "function_call", "id": "fc_test08_parallel", "call_id": "call_test08_parallel", "name": name, "arguments": string(encoded)}
			if namespace != "" {
				second["namespace"] = namespace
			}
			for index, call := range []map[string]any{item, second} {
				emit("response.output_item.added", map[string]any{"output_index": index, "item": call})
				emit("response.output_item.done", map[string]any{"output_index": index, "item": call})
			}
			emit("response.completed", map[string]any{"response": map[string]any{"id": responseID, "status": "completed", "output": []any{item, second}}})
			script.step += 2
			return
		}
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": item})
	} else {
		item = map[string]any{"type": "message", "id": "msg_test08", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "TEST08 complete", "annotations": []any{}}}}
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": item})
		emit("response.output_text.delta", map[string]any{"item_id": "msg_test08", "output_index": 0, "content_index": 0, "delta": "TEST08 complete"})
	}
	emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	emit("response.completed", map[string]any{"response": map[string]any{"id": responseID, "status": "completed", "output": []any{item}}})
	script.step++
}
