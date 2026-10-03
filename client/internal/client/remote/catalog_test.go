package remote

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

type catalogSource struct {
	identity      string
	discovery     jsontext.Value
	tools         jsontext.Value
	result        jsontext.Value
	discoverError error
	listError     error
	callError     error
	discoverHook  func(context.Context) error
	listHook      func()
	discovers     atomic.Int32
	lists         atomic.Int32
	calls         atomic.Int32
}

func (s *catalogSource) Identity(context.Context) (string, error) { return s.identity, nil }
func (s *catalogSource) Discover(ctx context.Context) (jsontext.Value, error) {
	s.discovers.Add(1)
	if s.discoverHook != nil {
		if err := s.discoverHook(ctx); err != nil {
			return nil, err
		}
	}
	return bytes.Clone(s.discovery), s.discoverError
}
func (s *catalogSource) ListTools(context.Context) (jsontext.Value, error) {
	s.lists.Add(1)
	if s.listHook != nil {
		s.listHook()
	}
	return bytes.Clone(s.tools), s.listError
}
func (s *catalogSource) CallTool(context.Context, string, jsontext.Value) (jsontext.Value, error) {
	s.calls.Add(1)
	return bytes.Clone(s.result), s.callError
}

func newCatalogSource(t *testing.T) *catalogSource {
	t.Helper()
	return &catalogSource{
		identity:  "account-a",
		discovery: jsontext.Value(`{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{},"extensions":{"io.github.alpha-gone/write-idempotency":{"retentionMs":86400000}}},"ttlMs":1000,"cacheScope":"public","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"agent-context","version":"test"}}}`),
		tools:     jsontext.Value(`{"tools":` + string(contract.Manifest()) + `,"ttlMs":1000,"cacheScope":"public"}`),
		result:    jsontext.Value(`{"structuredContent":{"cursor":"opaque+/=","version":9007199254740993,"truncated":true},"content":[],"isError":false}`),
	}
}

func changeField(t *testing.T, raw jsontext.Value, field string, value any) jsontext.Value {
	t.Helper()
	var object map[string]jsontext.Value
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if value == nil {
		delete(object, field)
	} else {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		object[field] = encoded
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestDiscoveryContractAndIdempotencyNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		valid       bool
		negotiated  bool
	}{
		{"valid", "", nil, true, true},
		{"old_revision", "supportedVersions", []string{"2025-11-25"}, false, false},
		{"missing_tools", "capabilities", map[string]any{}, false, false},
		{"null_tools", "capabilities", map[string]any{"tools": nil}, false, false},
		{"invalid_tools", "capabilities", map[string]any{"tools": true}, false, false},
		{"missing_info", "_meta", nil, false, false},
		{"empty_info", "_meta", map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": " ", "version": "test"}}, false, false},
		{"no_extension", "capabilities", map[string]any{"tools": map[string]any{}}, true, false},
		{"wrong_retention", "capabilities", map[string]any{"tools": map[string]any{}, "extensions": map[string]any{idempotencyExtension: map[string]any{"retentionMs": 1}}}, true, false},
		{"missing_retention", "capabilities", map[string]any{"tools": map[string]any{}, "extensions": map[string]any{idempotencyExtension: map[string]any{}}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := newCatalogSource(t)
			if tc.field != "" {
				source.discovery = changeField(t, source.discovery, tc.field, tc.value)
			}
			client := New(source, nil)
			result, err := client.Discover(t.Context())
			if !tc.valid {
				if !errors.Is(err, contract.ErrProtocol) {
					t.Fatalf("계약을 위반한 발견 응답을 허용했습니다: %v", err)
				}
				if tools, err := client.ListTools(t.Context()); tools != nil || !errors.Is(err, contract.ErrProtocol) || source.lists.Load() != 0 {
					t.Fatal("발견 실패 뒤 도구가 공개되었습니다")
				}
				return
			}
			if err != nil || result.WriteIdempotency != tc.negotiated || !bytes.Equal(result.Result, source.discovery) {
				t.Fatalf("확장 협상 결과가 다릅니다: %+v, %v", result, err)
			}
			result.Result[0] = ' '
			next, err := client.Discover(t.Context())
			if err != nil || !bytes.Equal(next.Result, source.discovery) {
				t.Fatal("발견 결과의 내부 캐시가 노출되었습니다")
			}
		})
	}
}

func TestCatalogCacheTTLAndScope(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ttl            any
		scope          string
		identityChange bool
		advance        time.Duration
		want           int32
	}{
		{"fresh", 1000, "public", false, 999 * time.Millisecond, 1},
		{"expired", 1000, "public", false, time.Second, 2},
		{"zero", 0, "public", false, 0, 2},
		{"absent", nil, "public", false, 0, 2},
		{"private_same", 1000, "private", false, 0, 1},
		{"private_changed", 1000, "private", true, 0, 2},
		{"public_changed", 1000, "public", true, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := newCatalogSource(t)
			source.discovery = changeField(t, changeField(t, source.discovery, "ttlMs", tc.ttl), "cacheScope", tc.scope)
			source.tools = changeField(t, changeField(t, source.tools, "ttlMs", tc.ttl), "cacheScope", tc.scope)
			now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
			client := New(source, func() time.Time { return now })
			if _, err := client.ListTools(t.Context()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(tc.advance)
			if tc.identityChange {
				source.identity = "account-b"
			}
			if _, err := client.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); err != nil {
				t.Fatal(err)
			}
			if source.discovers.Load() != tc.want || source.lists.Load() != tc.want || source.calls.Load() != 1 {
				t.Fatalf("캐시 요청 수: 발견 %d, 목록 %d, 호출 %d", source.discovers.Load(), source.lists.Load(), source.calls.Load())
			}
		})
	}
}

func TestRejectInvalidCacheHintsAndList(t *testing.T) {
	for _, target := range []string{"discover", "list"} {
		for _, tc := range []struct {
			name, field string
			value       any
		}{
			{"negative_ttl", "ttlMs", -1}, {"fraction_ttl", "ttlMs", 1.5},
			{"string_ttl", "ttlMs", "1000"}, {"null_ttl", "ttlMs", jsontext.Value(`null`)},
			{"overflow_ttl", "ttlMs", int64(9223372036854775807)},
			{"invalid_scope", "cacheScope", "shared"}, {"null_scope", "cacheScope", jsontext.Value(`null`)},
		} {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				source := newCatalogSource(t)
				if target == "discover" {
					source.discovery = changeField(t, source.discovery, tc.field, tc.value)
				} else {
					source.tools = changeField(t, source.tools, tc.field, tc.value)
				}
				tools, err := New(source, nil).ListTools(t.Context())
				if tools != nil || !errors.Is(err, contract.ErrProtocol) {
					t.Fatalf("잘못된 캐시 힌트를 허용했습니다: %v", err)
				}
			})
		}
	}
	for _, invalid := range []jsontext.Value{
		jsontext.Value(`{"tools":[]}`), jsontext.Value(`{"tools":null}`),
		jsontext.Value(`{"tools":` + string(contract.Manifest()) + `,"nextCursor":"more"}`),
		jsontext.Value(`{"tools":` + string(contract.Manifest()) + `,"nextCursor":null}`),
	} {
		source := newCatalogSource(t)
		source.tools = invalid
		catalog, err := New(source, nil).ListTools(t.Context())
		if catalog != nil || !errors.Is(err, contract.ErrProtocol) {
			t.Fatalf("불완전한 도구 목록을 허용했습니다: %v", err)
		}
	}
}

func TestFingerprintChangeStopsPublicationAndCalls(t *testing.T) {
	source := newCatalogSource(t)
	now := time.Now()
	client := New(source, func() time.Time { return now })
	if _, err := client.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	var tools []contract.Tool
	if err := json.Unmarshal(contract.Manifest(), &tools); err != nil {
		t.Fatal(err)
	}
	tools[0].Description = "변경된 설명"
	source.tools = changeField(t, source.tools, "tools", tools)
	now = now.Add(time.Second)
	if catalog, err := client.ListTools(t.Context()); catalog != nil || !errors.Is(err, contract.ErrProtocol) {
		t.Fatalf("변경된 목록이 공개되었습니다: %v", err)
	}
	source.tools = newCatalogSource(t).tools
	if result, err := client.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); result != nil || !errors.Is(err, contract.ErrProtocol) || source.calls.Load() != 0 {
		t.Fatal("최초 지문 변경 뒤 호출이 재개되었습니다")
	}
}

func TestExpiryRevalidatesDiscoveryAndEntireList(t *testing.T) {
	for _, target := range []string{"discover", "list"} {
		t.Run(target, func(t *testing.T) {
			source := newCatalogSource(t)
			now := time.Now()
			client := New(source, func() time.Time { return now })
			if _, err := client.ListTools(t.Context()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Second)
			if target == "discover" {
				source.discovery = changeField(t, source.discovery, "supportedVersions", []string{"old"})
			} else {
				source.tools = changeField(t, source.tools, "tools", []contract.Tool{})
			}
			if result, err := client.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); result != nil || !errors.Is(err, contract.ErrProtocol) || source.calls.Load() != 0 {
				t.Fatal("만료 뒤 검증되지 않은 호출이 전송되었습니다")
			}
		})
	}
}

func TestRemoteValidationAndResultPreservation(t *testing.T) {
	source := newCatalogSource(t)
	client := New(source, nil)
	for _, tc := range []struct{ name, arguments string }{
		{"audit", `{}`}, {"graph_get", `{}`}, {"graph_list", `{"page_size":0}`},
	} {
		if result, err := client.CallTool(t.Context(), tc.name, jsontext.Value(tc.arguments)); result != nil || !errors.Is(err, contract.ErrProtocol) || source.calls.Load() != 0 {
			t.Fatal("잘못된 도구 입력이 전송되었습니다")
		}
	}
	result, err := client.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`))
	if err != nil || !bytes.Equal(result.Raw(), source.result) {
		t.Fatalf("원격 결과가 바뀌었습니다: %v", err)
	}
	source.listHook = func() { source.identity = "account-b" }
	source.tools = changeField(t, source.tools, "ttlMs", 0)
	fresh := New(source, nil)
	if catalog, err := fresh.ListTools(t.Context()); catalog != nil || !errors.Is(err, contract.ErrIdentityChanged) || errors.Is(err, contract.ErrProtocol) {
		t.Fatalf("조회 도중 인증 주체 변경을 허용했습니다: %v", err)
	}
}

func TestIdentityChangeDiscardsResponseAndAllowsNewRequest(t *testing.T) {
	for _, target := range []string{"discover", "list"} {
		t.Run(target, func(t *testing.T) {
			source := newCatalogSource(t)
			source.discovery = changeField(t, source.discovery, "cacheScope", "private")
			source.tools = changeField(t, source.tools, "cacheScope", "private")
			now := time.Now()
			client := New(source, func() time.Time { return now })
			if _, err := client.ListTools(t.Context()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Second)
			if target == "discover" {
				source.discoverHook = func(context.Context) error { source.identity = "account-b"; return nil }
				// 다른 주체의 계약 불일치는 프로토콜 오류나 지문 변경으로 확정하지 않는다.
				source.discovery = changeField(t, source.discovery, "supportedVersions", []string{"old"})
			} else {
				source.listHook = func() { source.identity = "account-b" }
				var tools []contract.Tool
				if err := json.Unmarshal(contract.Manifest(), &tools); err != nil {
					t.Fatal(err)
				}
				tools[0].Description = "다른 주체의 설명"
				source.tools = changeField(t, source.tools, "tools", tools)
			}
			before := source.calls.Load()
			result, err := client.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`))
			if result != nil || !errors.Is(err, contract.ErrIdentityChanged) || errors.Is(err, contract.ErrProtocol) || source.calls.Load() != before {
				t.Fatalf("주체 변경 응답이 처리되거나 전송됐습니다: %v", err)
			}
			if client.discoveryEntry.identity == "account-b" || client.catalogEntry.identity == "account-b" {
				t.Fatal("폐기할 응답이 새 주체의 캐시에 들어갔습니다")
			}
			if client.rejected {
				t.Fatal("정상 계정 전환이 영구 프로토콜 거부로 바뀌었습니다")
			}
			source.discoverHook, source.listHook = nil, nil
			source.discovery, source.tools = newCatalogSource(t).discovery, newCatalogSource(t).tools
			source.discovery = changeField(t, source.discovery, "cacheScope", "private")
			source.tools = changeField(t, source.tools, "cacheScope", "private")
			if _, err := client.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); err != nil {
				t.Fatalf("새 주체의 명시적인 재요청이 실패했습니다: %v", err)
			}
			if client.catalogEntry.identity != "account-b" || source.calls.Load() != before+1 {
				t.Fatal("새 주체의 요청이 재검증되지 않았습니다")
			}
		})
	}
}

func TestCancellationPrecedesIdentityChange(t *testing.T) {
	source := newCatalogSource(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source.listHook = func() { source.identity = "account-b"; cancel() }
	if _, err := New(source, nil).ListTools(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, contract.ErrIdentityChanged) {
		t.Fatalf("취소보다 주체 변경을 먼저 분류했습니다: %v", err)
	}
}

func TestEmptyNextCursorIsAccepted(t *testing.T) {
	source := newCatalogSource(t)
	source.tools = changeField(t, source.tools, "nextCursor", "")
	if _, err := New(source, nil).ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCacheRefreshAndCancellation(t *testing.T) {
	source := newCatalogSource(t)
	client := New(source, nil)
	var workers sync.WaitGroup
	errorsFound := make(chan error, 100)
	for range 100 {
		workers.Go(func() {
			_, err := client.ListTools(t.Context())
			errorsFound <- err
		})
	}
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	if source.discovers.Load() != 1 || source.lists.Load() != 1 {
		t.Fatal("동시 요청이 발견·목록 조회를 중복 실행했습니다")
	}
	blocked := make(chan struct{})
	release := make(chan struct{})
	source = newCatalogSource(t)
	source.discoverHook = func(ctx context.Context) error {
		close(blocked)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	client = New(source, nil)
	done := make(chan error, 1)
	go func() { _, err := client.ListTools(t.Context()); done <- err }()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("발견 요청이 시작되지 않았습니다")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if _, err := client.ListTools(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("잠금 대기 취소가 전파되지 않았습니다: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(t.Context()); err != nil {
		t.Fatal("취소된 대기자가 캐시를 손상했습니다")
	}
}

func TestRemoteErrorsPropagateWithoutCaching(t *testing.T) {
	for _, target := range []string{"discover", "list", "call"} {
		t.Run(target, func(t *testing.T) {
			source := newCatalogSource(t)
			failure := errors.New("전송 대역 오류")
			switch target {
			case "discover":
				source.discoverError = failure
			case "list":
				source.listError = failure
			case "call":
				source.callError = failure
			}
			client := New(source, nil)
			if _, err := client.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); !errors.Is(err, failure) {
				t.Fatalf("원격 경계 오류가 사라졌습니다: %v", err)
			}
			source.discoverError, source.listError, source.callError = nil, nil, nil
			if _, err := client.CallTool(t.Context(), "graph_list", jsontext.Value(`{}`)); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := New(nil, nil).ListTools(t.Context()); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("원격 경계 부재를 허용했습니다")
	}
	source := newCatalogSource(t)
	source.identity = ""
	if _, err := New(source, nil).ListTools(t.Context()); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("인증 주체 부재를 허용했습니다")
	}
}

func TestFingerprintIgnoresRemoteToolOrder(t *testing.T) {
	source := newCatalogSource(t)
	source.tools = changeField(t, source.tools, "ttlMs", 0)
	client := New(source, nil)
	if _, err := client.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	var tools []contract.Tool
	if err := json.Unmarshal(contract.Manifest(), &tools); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(tools)
	source.tools = changeField(t, source.tools, "tools", tools)
	if _, err := client.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
}
