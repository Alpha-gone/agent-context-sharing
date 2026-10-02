package host

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
)

type toolRemote struct {
	catalog      *contract.Catalog
	result       *contract.Result
	lists, calls int
	arguments    jsontext.Value
	err          error
}

func (r *toolRemote) ListTools(context.Context) (*contract.Catalog, error) {
	r.lists++
	return r.catalog, r.err
}

func (r *toolRemote) CallTool(_ context.Context, name string, arguments jsontext.Value) (*contract.Result, error) {
	r.calls++
	r.arguments = bytes.Clone(arguments)
	if err := r.catalog.ValidateRemoteArguments(name, arguments); err != nil {
		return nil, err
	}
	return r.result, r.err
}

func newToolRemote(t *testing.T) *toolRemote {
	t.Helper()
	var tools []contract.Tool
	if err := json.Unmarshal(contract.Manifest(), &tools); err != nil {
		t.Fatal(err)
	}
	catalog, err := contract.ValidateTools(tools)
	if err != nil {
		t.Fatal(err)
	}
	result, err := contract.NewResult(jsontext.Value(`{"structuredContent":{"version":9007199254740993,"cursor":"opaque+/=","truncated":true},"content":[],"isError":false}`))
	if err != nil {
		t.Fatal(err)
	}
	return &toolRemote{catalog: catalog, result: result}
}

// 필수 속성만 채우며 선택 속성의 도메인 제약은 서버 판정에 남긴다.
func hostArguments(t *testing.T, tool contract.Tool, id string) jsontext.Value {
	t.Helper()
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type   string   `json:"type"`
			Format string   `json:"format"`
			Enum   []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	object := make(map[string]any)
	for _, name := range schema.Required {
		property := schema.Properties[name]
		switch {
		case property.Type == "integer":
			object[name] = 1
		case property.Format == "uuid":
			object[name] = id
		case len(property.Enum) > 0:
			object[name] = property.Enum[0]
		default:
			object[name] = "입력"
		}
	}
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestToolPoliciesMatchListingsAndCalls(t *testing.T) {
	agent := uuid.MustParse("019f0000-0000-7000-8000-000000000001")
	all, err := contract.NewPolicy("all", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode  string
		names []string
		count int
	}{
		{"all", nil, 13}, {"read_only", nil, 5},
		{"allowlist", []string{"graph_list", "node_create"}, 2},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			remote := newToolRemote(t)
			policy, err := contract.NewPolicy(tc.mode, tc.names)
			if err != nil {
				t.Fatal(err)
			}
			host := NewTools(remote, policy, agent)
			listed, err := host.ListTools(t.Context())
			if err != nil || len(listed) != tc.count {
				t.Fatalf("목록 수 = %d: %v", len(listed), err)
			}
			for _, tool := range remote.catalog.Tools(all) {
				beforeLists, beforeCalls := remote.lists, remote.calls
				arguments := hostArguments(t, tool, agent.String())
				result, err := host.CallTool(t.Context(), tool.Name, arguments)
				if !policy.Allows(tool.Name) {
					if !errors.Is(err, contract.ErrProtocol) || result != nil || remote.lists != beforeLists || remote.calls != beforeCalls {
						t.Fatalf("정책 밖 호출이 원격에 접근했습니다: %s", tool.Name)
					}
					continue
				}
				if err != nil || remote.calls != beforeCalls+1 || !bytes.Equal(result.Raw(), remote.result.Raw()) {
					t.Fatalf("허용 도구 중계 실패: %s, %v", tool.Name, err)
				}
				if contract.InjectsAgent(tool.Name) {
					var prepared map[string]jsontext.Value
					if err := json.Unmarshal(remote.arguments, &prepared); err != nil {
						t.Fatal(err)
					}
					if string(prepared["created_by_agent"]) != `"`+agent.String()+`"` {
						t.Fatal("행위 에이전트가 주입되지 않았습니다")
					}
					prepared["created_by_agent"] = jsontext.Value(`"019f0000-0000-7000-8000-000000000002"`)
					bypass, err := json.Marshal(prepared)
					if err != nil {
						t.Fatal(err)
					}
					before := remote.calls
					if _, err := host.CallTool(t.Context(), tool.Name, bypass); !errors.Is(err, contract.ErrProtocol) || remote.calls != before {
						t.Fatal("호스트의 우회 입력이 원격에 전송되었습니다")
					}
				}
			}
		})
	}
}

func TestToolBoundaryRejectsInvalidInputAndRemoteCatalog(t *testing.T) {
	remote := newToolRemote(t)
	policy, err := contract.NewPolicy("all", nil)
	if err != nil {
		t.Fatal(err)
	}
	host := NewTools(remote, policy, uuid.UUID{})
	if _, err := host.CallTool(t.Context(), "graph_list", jsontext.Value(`{"page_size":0}`)); !errors.Is(err, contract.ErrProtocol) || remote.calls != 0 {
		t.Fatal("호스트 입력 제약 위반이 전송되었습니다")
	}
	remote.catalog = nil
	if tools, err := host.ListTools(t.Context()); tools != nil || !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("nil 목록이 공개되었습니다")
	}
	if _, err := host.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("nil 목록으로 호출했습니다")
	}
	failure := errors.New("원격 대역 오류")
	remote.err = failure
	if _, err := host.ListTools(t.Context()); !errors.Is(err, failure) {
		t.Fatal("원격 오류가 사라졌습니다")
	}
	if _, err := host.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); !errors.Is(err, failure) {
		t.Fatal("원격 오류가 사라졌습니다")
	}
	host = NewTools(nil, policy, uuid.UUID{})
	if _, err := host.ListTools(t.Context()); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("원격 경계 부재를 허용했습니다")
	}
	if _, err := host.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("원격 경계 부재를 허용했습니다")
	}
}
