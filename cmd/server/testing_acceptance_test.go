package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTEST02DependencyScannerUsesProductionSources(t *testing.T) {
	root := t.TempDir()
	for name, source := range map[string]string{
		"normal.go": `package fixture
import _ "agent_context_sharing/internal/model"`,
		"alias.go": `//go:build linux

package fixture
import injected "agent_context_sharing/internal/plan"`,
		"duplicate.go": `package fixture
import m "agent_context_sharing/internal/model"`,
		"fixture_test.go": `package fixture
import "agent_context_sharing/internal/store"`,
		"notes.txt": `import "agent_context_sharing/internal/migrate"`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	actual := internalImports(t, root)
	if !slices.Equal(actual, []string{"model", "plan"}) {
		t.Fatalf("제품 소스의 중복 제거·별칭·플랫폼 의존 = %v", actual)
	}
	// 실제 소스 해석 결과를 대조기에 넣어 금지 의존과 낡은 허용표를 함께 탐지한다.
	violations := dependencyViolations(map[string][]string{"fixture": {"model", "perm"}}, map[string][]string{"fixture": actual})
	if !slices.Equal(violations, []string{"누락 의존: fixture -> perm", "추가 의존: fixture -> plan"}) {
		t.Fatalf("양방향 위반 = %v", violations)
	}
	if !slices.Equal(boundaryExemptPackages, []string{"migrate"}) {
		t.Fatalf("업무 패키지와 분리한 도구 예외 = %v", boundaryExemptPackages)
	}
}
