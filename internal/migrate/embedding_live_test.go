//go:build embedding_live

package migrate_test

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestGeminiLiveIndexSearchIntegration은 명시적으로 선택한 실연결 시험이다.
func TestGeminiLiveIndexSearchIntegration(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if key == "" {
		t.Fatal("실연결 시험에는 외부 전송 허용과 GEMINI_API_KEY 주입이 필요하다")
	}
	if err := validateLiveDatabaseAddress(os.Getenv("TEST_DATABASE_URL")); err != nil {
		t.Fatal(err)
	}
	address, _ := url.Parse("https://generativelanguage.googleapis.com")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	exerciseLiveIndexSearch(t, key, address, transport)
}
