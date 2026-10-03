package authorize

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"agent_context_sharing/client/internal/client/contract"
)

func TestProtectedResourceFallsBackOnlyOnImplicitPathNotFound(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		explicit  bool
		wantCalls int
		want      error
	}{
		{"root-only", http.StatusNotFound, false, 2, nil},
		{"explicit-not-found", http.StatusNotFound, true, 1, contract.ErrProtocol},
		{"server-error", http.StatusInternalServerError, false, 1, contract.ErrProtocol},
		{"invalid-document", http.StatusOK, false, 1, contract.ErrProtocol},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			manager := fixture.manager("5s")
			var paths []string
			manager.transport = roundTripper(func(request *http.Request) (*http.Response, error) {
				paths = append(paths, request.URL.Path)
				if strings.HasSuffix(request.URL.Path, "/mcp") {
					response, err := jsonResponse(request, map[string]any{})
					if err != nil {
						return nil, err
					}
					response.StatusCode = test.status
					return response, nil
				}
				return fixture.transport(request)
			})
			location := ""
			if test.explicit {
				location, _ = wellKnown(manager.cfg.RemoteURL(), "oauth-protected-resource")
			}
			_, err := manager.protectedResource(t.Context(), location)
			if !errors.Is(err, test.want) || len(paths) != test.wantCalls {
				t.Fatalf("발견 오류=%v, 조회 수=%d", err, len(paths))
			}
			if len(paths) == 2 && paths[1] != "/.well-known/oauth-protected-resource" {
				t.Fatal("루트 대체 위치 불일치")
			}
			if fixture.opens.Load() != 0 {
				t.Fatal("발견 진단이 브라우저를 실행했다")
			}
		})
	}
}
