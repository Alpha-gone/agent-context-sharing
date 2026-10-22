package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAgentHandlerValidationResultAndFailure(t *testing.T) {
	account, _ := model.NewID()
	agent, _ := model.NewID()
	for _, mode := range []string{"success", "name", "unknown_field", "key", "principal", "cancel", "dependency"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			handler, err := NewAgentHandler(agentRegistryStub{register: func(_ context.Context, got model.ID, client, key string) (model.ID, error) {
				calls++
				if got != account || client != "first" || key != "registration_key_1" {
					t.Fatal("검증한 등록 결합 유실")
				}
				if mode == "dependency" {
					return model.ID{}, errors.New("private-token-uri")
				}
				return agent, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			principal := Principal{AccountID: account, ClientID: "first"}
			name, arguments := "agent_register", `{"registration_key":"registration_key_1"}`
			switch mode {
			case "name":
				name = "graph_create"
			case "unknown_field":
				arguments = `{"registration_key":"registration_key_1","account_id":"forged"}`
			case "key":
				arguments = `{"registration_key":"bad"}`
			case "principal":
				principal.AccountID = model.ID{}
			case "cancel":
				cancel()
			}
			result, err := handler(ctx, principal, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: name, Arguments: []byte(arguments)}})
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) || result != nil || calls != 0 {
					t.Fatal("취소 뒤 등록 실행")
				}
				return
			}
			if err != nil || result == nil {
				t.Fatal(result, err)
			}
			data, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "private-token-uri") {
				t.Fatal("저장 오류 원문 노출")
			}
			if mode == "success" {
				if result.IsError || calls != 1 || !strings.Contains(string(data), agent.String()) || len(result.Content) != 1 || result.Content[0].(*sdk.TextContent).Text != string(data) {
					t.Fatal("등록 결과 외피 불일치")
				}
			} else {
				want := 0
				if mode == "dependency" {
					want = 1
				}
				if !result.IsError || calls != want {
					t.Fatal("등록 거부·장애 사상 불일치")
				}
			}
		})
	}
	if handler, err := NewAgentHandler(nil); handler != nil || err == nil {
		t.Fatal("조립 누락 수락")
	}
}
