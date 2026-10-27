//go:build codex_expiry_live

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 실제 1시간을 기다리는 시험은 명시한 태그·75분 상한으로 실행한다.
// 자격·클라이언트 만료 정보·서버/호스트 시계·DB 시각을 바꾸지 않는다.
func TestTEST08CodexCLINaturalAccessExpiryIntegration(t *testing.T) {
	test08CodexOAuth(t, "natural_expiry", func(t *testing.T, session test08OAuthSession) {
		parts := strings.Split(session.initialToken.AccessToken, ".")
		if len(parts) != 3 || session.initialToken.ExpiresIn != 3600 {
			t.Fatal("실제 접근 자격의 수명 형식 불일치")
		}
		payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
		var clock struct {
			Issued  int64 `json:"iat"`
			Expires int64 `json:"exp"`
		}
		if err != nil || json.Unmarshal(payload, &clock) != nil || clock.Expires-clock.Issued != 3600 {
			t.Fatal("실제 접근 JWT의 1시간 수명 불일치")
		}
		// 클레임은 대기 시각만 정한다. 자격의 실제 검증은 TLS 서버가 한다.
		expires := time.Unix(clock.Expires, 0)
		if remaining := time.Until(expires); remaining < 59*time.Minute || remaining > time.Hour {
			t.Fatal("자연 만료 표본이 최초 발급 직후가 아닙니다")
		}
		original, err := os.ReadFile(session.credentials)
		if err != nil {
			t.Fatal("시험 자격 스냅숏 실패")
		}
		t.Logf("TEST-08 config=test08-natural-expiry-v1 stage=waiting correlation=natural-expiry-1 expires=%s unchanged_clock=true unchanged_credentials=true", expires.UTC().Format(time.RFC3339))
		timer := time.NewTimer(time.Until(expires.Add(2 * time.Second)))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-session.ctx.Done():
			t.Fatal("실제 접근 만료 대기 취소")
		}
		unchanged, err := os.ReadFile(session.credentials)
		if err != nil || !bytes.Equal(original, unchanged) || session.exchanges.Load() != 1 {
			t.Fatal("대기 중 저장 자격이 변경되거나 갱신되었습니다")
		}
		request, err := http.NewRequestWithContext(session.ctx, http.MethodPost, session.server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"natural-expiry","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
		if err != nil {
			t.Fatal("만료 자격 대조 요청 준비 실패")
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("MCP-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", "server/discover")
		request.Header.Set("Authorization", "Bearer "+session.initialToken.AccessToken)
		response, err := session.server.Client().Do(request)
		if err != nil {
			t.Fatal("만료 자격의 실제 TLS 요청 실패")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), "invalid_token") {
			t.Fatal("실제로 만료한 최초 접근 자격이 401로 거부되지 않았습니다")
		}
		before := session.calls.Load()
		_ = session.command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Return TEST08.").Run()
		if session.calls.Load() < before+2 || session.exchanges.Load() != 2 || session.tokenSucceeded.Load() != 2 {
			t.Fatal("실제 접근 만료 뒤 CLI의 단일 갱신·보호 요청 실패")
		}
		count, consumed := session.refreshCounts()
		after, err := os.ReadFile(session.credentials)
		info, statErr := os.Stat(session.credentials)
		if count != 2 || consumed != 1 || err != nil || bytes.Equal(original, after) || statErr != nil || info.Mode().Perm() != 0600 {
			t.Fatal("자연 만료 뒤 단일 소비·새 자격의 안전 저장 불일치")
		}
		before = session.calls.Load()
		_ = session.command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Return TEST08.").Run()
		if session.calls.Load() < before+2 || session.exchanges.Load() != 2 {
			t.Fatal("자연 만료 뒤 새 CLI 프로세스의 후속 자격 재사용 실패")
		}
		t.Logf("TEST-08 config=test08-natural-expiry-v1 stage=accepted correlation=natural-expiry-1 time=%s expired_access_status=401 refresh_success=1 refresh_consumed=1 restored_in_new_process=true", time.Now().UTC().Format(time.RFC3339))
	})
}
