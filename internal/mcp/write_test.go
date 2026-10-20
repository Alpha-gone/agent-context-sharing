package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

type writeExecutorFunc func(context.Context, model.ID, string, string, []byte, func(context.Context) error, func(context.Context) ([]byte, error)) ([]byte, error)

func (f writeExecutorFunc) ExecuteWrite(ctx context.Context, account model.ID, key, tool string, input []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error) {
	return f(ctx, account, key, tool, input, check, apply)
}

func TestWriteRunnerNormalization(t *testing.T) {
	account, _ := model.NewID()
	principal := Principal{AccountID: account, ClientID: "verified-client"}
	var fingerprints [][]byte
	executor := writeExecutorFunc(func(ctx context.Context, account model.ID, key, tool string, normalized []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error) {
		if key != "request_key_00001" && key != "request_key_00002" {
			t.Fatal("키 추출", key)
		}
		if err := check(ctx); err != nil {
			return nil, err
		}
		fingerprints = append(fingerprints, bytes.Clone(normalized))
		return apply(ctx)
	})
	registry := agentRegistryStub{validate: func(context.Context, model.ID, string, model.ID) error { return nil }}
	runner, err := NewWriteRunner(executor, registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{
		`{"graph_id":"0199aabb-ccdd-7000-8000-000000000001","expected_version":1,"name":"한","request_id":"request_key_00001"}`,
		`{"request_id":"request_key_00002","name":"\ud55c","expected_version":1.0e0,"graph_id":"0199AABBCCDD70008000000000000001"}`,
	} {
		if _, err := runner.Execute(t.Context(), principal, "graph_update", []byte(arguments), func(context.Context) error { return nil }, func(context.Context) ([]byte, error) { return []byte(`{"version":2}`), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(fingerprints) != 2 || !bytes.Equal(fingerprints[0], fingerprints[1]) {
		t.Fatalf("동일 의미 정규화 실패: %q", fingerprints)
	}
	if bytes.Contains(fingerprints[0], []byte("request_id")) || bytes.Contains(fingerprints[0], []byte("client")) {
		t.Fatal("지문에 외피/자격 포함")
	}
}

func TestWriteRunnerToolsAndGuards(t *testing.T) {
	for _, tool := range []string{"graph_create", "graph_update", "node_create", "node_update", "node_discard", "node_restore", "relation_confirm", "relation_discard"} {
		for _, mode := range []string{"keyed", "keyless", "permission denied", "agent revoked", "invalid input", "cancelled"} {
			t.Run(tool+"/"+mode, func(t *testing.T) {
				account, _ := model.NewID()
				principal := Principal{AccountID: account, ClientID: "verified-client"}
				checks, bindings, runs, execs := 0, 0, 0, 0
				registry := agentRegistryStub{validate: func(_ context.Context, a model.ID, c string, agent model.ID) error {
					bindings++
					if checks != 1 || a != account || c != principal.ClientID || agent.String() != catalogID {
						t.Fatal("행위자 결합 순서")
					}
					if mode == "agent revoked" {
						return model.FieldError{Field: "agent_id", Message: "등록 폐기"}
					}
					return nil
				}}
				executor := writeExecutorFunc(func(ctx context.Context, a model.ID, key, name string, normalized []byte, check func(context.Context) error, apply func(context.Context) ([]byte, error)) ([]byte, error) {
					execs++
					if a != account || name != tool {
						t.Fatal("실행 경계 신원")
					}
					if err := check(ctx); err != nil {
						return nil, err
					}
					return apply(ctx)
				})
				runner, err := NewWriteRunner(executor, registry)
				if err != nil {
					t.Fatal(err)
				}
				arguments := catalogSample(tool)
				if mode != "keyless" {
					arguments["request_id"] = "request_key_00001"
				}
				if mode == "invalid input" {
					arguments["request_id"] = "short"
				}
				data, _ := json.Marshal(arguments)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "cancelled" {
					cancel()
				}
				_, err = runner.Execute(ctx, principal, tool, data, func(context.Context) error {
					checks++
					if mode == "permission denied" {
						return model.ErrNotFound
					}
					return nil
				}, func(context.Context) ([]byte, error) { runs++; return []byte(`{"ok":true}`), nil })
				wantRuns := 1
				if mode == "permission denied" || mode == "invalid input" || mode == "cancelled" || mode == "agent revoked" && !strings.HasPrefix(tool, "graph_") {
					wantRuns = 0
				}
				if runs != wantRuns || (wantRuns == 0) != (err != nil) {
					t.Fatalf("적용=%d err=%v", runs, err)
				}
				if (mode == "invalid input" || mode == "cancelled") && execs != 0 {
					t.Fatal("선행 입력 거부가 실행기를 호출")
				}
				if strings.HasPrefix(tool, "graph_") && bindings != 0 {
					t.Fatal("그래프 쓰기에 agent 강제")
				}
			})
		}
	}
}

func TestWriteRunnerBoundaryAndFingerprintFields(t *testing.T) {
	account, _ := model.NewID()
	principal := Principal{AccountID: account, ClientID: "client"}
	var fingerprints [][]byte
	executor := writeExecutorFunc(func(ctx context.Context, a model.ID, key, tool string, n []byte, c func(context.Context) error, r func(context.Context) ([]byte, error)) ([]byte, error) {
		fingerprints = append(fingerprints, bytes.Clone(n))
		if err := c(ctx); err != nil {
			return nil, err
		}
		return r(ctx)
	})
	registry := agentRegistryStub{validate: func(context.Context, model.ID, string, model.ID) error { return nil }}
	runner, err := NewWriteRunner(executor, registry)
	if err != nil {
		t.Fatal(err)
	}
	base := catalogSample("node_update")
	base["judgment_input"] = "판단 입력"
	for _, mode := range []string{"original", "body", "agent", "version", "judgment", "keep"} {
		value := map[string]any{}
		for k, v := range base {
			value[k] = v
		}
		switch mode {
		case "body":
			value["body"] = "변경 본문"
		case "agent":
			id, _ := model.NewID()
			value["agent_id"] = id.String()
		case "version":
			value["expected_version"] = 2
		case "judgment":
			value["judgment_input"] = "다른 판단"
		case "keep":
			delete(value, "body")
			value["management_action"] = "keep"
		}
		data, _ := json.Marshal(value)
		if _, err := runner.Execute(t.Context(), principal, "node_update", data, func(context.Context) error { return nil }, func(context.Context) ([]byte, error) { return []byte(`{"ok":true}`), nil }); err != nil {
			t.Fatal(err)
		}
	}
	for i := range fingerprints {
		for j := range i {
			if bytes.Equal(fingerprints[i], fingerprints[j]) {
				t.Fatal("지문이 변경 필드를 누락")
			}
		}
	}
	if _, err := runner.Execute(t.Context(), principal, "agent_register", []byte(`{"registration_key":"registration_key"}`), func(context.Context) error { return nil }, func(context.Context) ([]byte, error) { t.Fatal("등록이 재생 경계에 포함"); return nil, nil }); err == nil {
		t.Fatal("등록 도구 수락")
	}
	for _, field := range []string{"_meta", "access_token", "client_id"} {
		input := catalogSample("graph_create")
		input[field] = "synthetic-secret"
		data, _ := json.Marshal(input)
		if _, err := runner.Execute(t.Context(), principal, "graph_create", data, func(context.Context) error { return nil }, func(context.Context) ([]byte, error) { t.Fatal("외피가 인자로 수락"); return nil, nil }); err == nil {
			t.Fatal("알 수 없는 외피 필드")
		}
	}
	if _, err := NewWriteRunner(nil, registry); err == nil {
		t.Fatal("실행기 누락 수락")
	}
	if _, err := NewWriteRunner(executor, nil); err == nil {
		t.Fatal("등록 저장소 누락 수락")
	}
	_, err = runner.Execute(t.Context(), Principal{}, "graph_create", []byte(`{"name":"이름"}`), nil, nil)
	if err == nil {
		t.Fatal("미검증 주체 수락")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runner.Execute(ctx, principal, "graph_create", nil, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("취소 누락")
	}
}
