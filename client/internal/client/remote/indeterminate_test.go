package remote

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"testing"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/contract"
)

func TestHTTPUnresolvedWriteKeepsIndeterminateAcrossTerminalErrors(t *testing.T) {
	for _, tool := range []string{"graph_create", "graph_list"} {
		for _, delivered := range []bool{false, true} {
			for _, ending := range []string{"authorization", "proof", "nonce", "malformed", "remote_error", "refresh", "success", "domain"} {
				t.Run(tool+"/"+map[bool]string{false: "before", true: "after"}[delivered]+"/"+ending, func(t *testing.T) {
					auth := readyAuthentication()
					auth.onAuth = func(_ context.Context) error { return authorize.ErrAuthorization }
					attempts := 0
					var key string
					result := jsontext.Value(`{"content":[],"isError":false}`)
					if ending == "domain" {
						result = jsontext.Value(`{"content":[],"isError":true,"structuredContent":{"error":{"code":"version_conflict"}}}`)
					}
					client := newHTTPTestClient(t, auth, transportFunc(func(r *http.Request) (*http.Response, error) {
						attempts++
						if attempts == 1 {
							key = r.Header.Get("Idempotency-Key")
							if !delivered {
								httptrace.ContextClientTrace(r.Context()).GetConn("resource.test:443")
								return nil, io.EOF
							}
							wireID(t, r)
							return nil, io.EOF
						}
						if r.Header.Get("Idempotency-Key") != key || tool == "graph_create" && key == "" {
							t.Fatal("논리적 쓰기의 키가 바뀌었다")
						}
						id := wireID(t, r)
						switch ending {
						case "authorization":
							return challengeResponse("invalid_token"), nil
						case "proof":
							return challengeResponse("invalid_dpop_proof"), nil
						case "nonce":
							return challengeResponse("use_dpop_nonce"), nil
						case "malformed":
							return rawResponse(http.StatusOK, []byte(`{}`)), nil
						case "remote_error":
							return rawResponse(http.StatusInternalServerError, []byte(`{"jsonrpc":"2.0","id":"`+id+`","error":{"code":-32603,"message":"remote error"}}`)), nil
						case "refresh":
							auth.onRefresh = func(_ context.Context) error { return contract.ErrBusy }
						}
						return rpcResponse(id, result), nil
					}), HTTPOptions{})
					client.source.(*httpSource).negotiated.Store(true)
					got, err := executeTest(t.Context(), client, tool)
					if attempts != 2 {
						t.Fatalf("완전한 종료 응답 뒤 재전송: %d회", attempts)
					}
					if ending == "success" || ending == "domain" {
						if err != nil || string(got) != string(result) {
							t.Fatalf("완료 결과를 잃었다: %s, %v", got, err)
						}
						return
					}
					if delivered && tool == "graph_create" {
						if !errors.Is(err, contract.ErrIndeterminate) || contract.ClassifyError(err).Retryable {
							t.Fatalf("불확정 쓰기의 최종 분류: %v", err)
						}
					} else {
						want := error(contract.ErrProtocol)
						if ending == "authorization" {
							want = authorize.ErrAuthorization
						} else if ending == "refresh" {
							want = contract.ErrBusy
						} else if ending == "remote_error" {
							if _, ok := errors.AsType[*ProtocolError](err); !ok {
								t.Fatalf("완전한 원격 오류를 잃었다: %v", err)
							}
							return
						}
						if !errors.Is(err, want) {
							t.Fatalf("읽기·전달 전 실패의 분류: %v, want %v", err, want)
						}
					}
				})
			}
		}
	}
}
