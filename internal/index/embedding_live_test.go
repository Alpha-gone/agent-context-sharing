//go:build embedding_live

package index

import (
	"context"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestGeminiLiveEmbeddingContract는 명시적으로 선택한 경우에만 합성 입력을 외부로 보낸다.
// DB는 사용하지 않으며 호출·인증 수용만 검사하고 모델 품질을 판정하지 않는다.
func TestGeminiLiveEmbeddingContract(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if key == "" {
		t.Fatal("실연결 시험에는 외부 전송·비용 허용과 GEMINI_API_KEY 주입이 필요하다")
	}
	address, err := url.Parse("https://generativelanguage.googleapis.com")
	if err != nil {
		t.Fatal("공식 임베딩 주소 해석 실패")
	}
	worker, err := New(testStore(t), Config{
		Provider: "gemini", APIKey: key, BaseURL: address,
		Model: "gemini-embedding-2", VectorType: "vector", Dimension: 768,
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 65*time.Second)
	defer cancel()
	for _, call := range []struct {
		name  string
		input string
		embed func(context.Context, string) ([]float64, error)
	}{
		{"document", "합성 시험 문서입니다. 파란색 상자는 선반 위에 있습니다.", worker.EmbedDocument},
		{"query", "합성 시험의 파란색 상자는 어디에 있습니까?", worker.Embed},
	} {
		started := time.Now()
		vector, err := call.embed(ctx, call.input)
		if err != nil {
			t.Fatalf("%s 실연결 실패: %v", call.name, err)
		}
		if len(vector) != 768 {
			t.Fatalf("%s 벡터 차원 = %d, 기대값 768", call.name, len(vector))
		}
		nonzero := false
		for _, value := range vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				t.Fatalf("%s 벡터가 유한하지 않다", call.name)
			}
			nonzero = nonzero || value != 0
		}
		if !nonzero {
			t.Fatalf("%s 응답이 영벡터다", call.name)
		}
		t.Logf("%s: dimension=%d elapsed_ms=%d", call.name, len(vector), time.Since(started).Milliseconds())
	}
}
