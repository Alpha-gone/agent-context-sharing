//go:build embedding_live

package index

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestGeminiLiveEmbeddingContract은 DB 없이 합성 문서·질의를 최대 두 번 전송한다.
func TestGeminiLiveEmbeddingContract(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if key == "" {
		t.Fatal("실연결 시험에는 외부 전송·비용 허용과 GEMINI_API_KEY 주입이 필요하다")
	}
	address, _ := url.Parse("https://generativelanguage.googleapis.com")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	exerciseLiveEmbeddingContract(t, key, address, transport)
}
