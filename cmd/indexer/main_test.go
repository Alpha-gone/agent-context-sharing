package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"agent_context_sharing/internal/config"
)

func TestIndexerConfigurationBeforeStartup(t *testing.T) {
	data, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "embedded", "invalid_provider", "invalid_placement"} {
		t.Run(mode, func(t *testing.T) {
			values := map[string]string{}
			for line := range strings.SplitSeq(string(data), "\n") {
				if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
					values[key] = strings.Trim(value, "'")
				}
			}
			values["GEMINI_API_KEY"], values["INDEX_WORKER_PLACEMENT"] = "synthetic-key", "external"
			switch mode {
			case "embedded":
				values["INDEX_WORKER_PLACEMENT"] = "embedded"
			case "invalid_provider":
				values["EMBEDDING_PROVIDER"] = "private-invalid-provider"
			case "invalid_placement":
				values["INDEX_WORKER_PLACEMENT"] = "private-invalid-placement"
			}
			calls := 0
			expected := errors.New("synthetic startup failure")
			err := configureAndRun(t.Context(), func(key string) string { return values[key] }, func(ctx context.Context, cfg config.Config) error {
				calls++
				if cfg.IndexWorkerPlacement != config.PlacementExternal || cfg.EmbeddingProvider != "gemini" || ctx != t.Context() {
					t.Error("검증한 구성·컨텍스트가 보존되지 않았다")
				}
				return expected
			})
			if mode == "valid" {
				if calls != 1 || !errors.Is(err, expected) {
					t.Fatalf("조립 호출·원인 보존=%d: %v", calls, err)
				}
			} else if calls != 0 || err == nil || strings.Contains(err.Error(), "private-invalid") {
				t.Fatalf("잘못된 구성의 자원 접근·원문 노출=%d: %v", calls, err)
			}
		})
	}
}
