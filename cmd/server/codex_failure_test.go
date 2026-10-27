package main

import (
	"bytes"
	"net/http"
	"os"
	"testing"
)

// 403과 내부 오류는 호스트의 자격 갱신/재인가를 유발하지 않아야 한다.
// 서버의 오류 사상은 TEST-07이, 이 시험은 실제 CLI의 반응을 검증한다.
func TestTEST08CodexCLIHTTPFailuresIntegration(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			test08CodexOAuth(t, "http_failure", func(t *testing.T, session test08OAuthSession) {
				original, err := os.ReadFile(session.credentials)
				if err != nil {
					t.Fatal("시험 자격 스냅숏 실패")
				}
				before := session.calls.Load()
				session.forcedStatus.Store(int64(status))
				_ = session.command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Return TEST08.").Run()
				after, err := os.ReadFile(session.credentials)
				if err != nil || !bytes.Equal(original, after) || session.forcedRequests.Load() == 0 || session.forcedRequests.Load() > 8 || session.calls.Load() != before || session.exchanges.Load() != 1 {
					t.Fatal("403/500 뒤 유한 오류 종료·자격 보존·재인가 없음의 경계 불일치")
				}
				session.forcedStatus.Store(0)
				_ = session.command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Return TEST08.").Run()
				if session.calls.Load() < before+2 || session.exchanges.Load() != 1 {
					t.Fatal("HTTP 오류 해제 뒤 기존 자격 재사용 실패")
				}
				t.Logf("TEST-08 stage=http_failure_recovered status=%d token_attempts=1 credential_preserved=true requests=%d", status, session.forcedRequests.Load())
			})
		})
	}
}
