package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// packageBoundaries는 `SDD.md`의 「패키지 경계」 표를 그대로 옮긴 것이다.
// 값은 그 패키지가 부르는 내부 패키지이며, 비어 있으면 아무것도 부르지 않는다.
var packageBoundaries = map[string][]string{
	"model":  {},
	"plan":   {"model"},
	"store":  {"model", "plan"},
	"perm":   {"model"},
	"index":  {"model", "store"},
	"authz":  {"model", "store"},
	"search": {"model", "store"},
	"mcp":    {"model", "perm", "plan", "search", "store"},
	"web":    {"model", "perm", "plan", "store"},
	"config": {"plan"},
}

// boundaryExemptPackages는 「패키지 경계」가 업무 패키지 밖으로 둔 것이다.
// 마이그레이션 실행기는 `store`를 거치지 않고 데이터베이스에 직접 닿는 도구다.
var boundaryExemptPackages = []string{"migrate"}

// TestPackageDependenciesMatchDesign은 「패키지 경계」 표와 실제 import를 양방향으로 대조한다.
//
// 한쪽 방향만 보면 표가 낡는다. 표에 없는 의존이 생기는 것뿐 아니라 표에 적힌 의존이
// 사라지는 것도 문서와 구현을 어긋나게 하므로 둘 다 실패로 다룬다. 실제로 이 테스트를
// 만들기 전까지 표는 없는 패키지와 성립하지 않는 의존 네 건을 담고 있었다.
func TestPackageDependenciesMatchDesign(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("내부 패키지 목록: %v", err)
	}

	found := make(map[string][]string)
	for _, entry := range entries {
		if !entry.IsDir() || slices.Contains(boundaryExemptPackages, entry.Name()) {
			continue
		}
		found[entry.Name()] = internalImports(t, filepath.Join(root, entry.Name()))
	}

	for name := range found {
		if _, declared := packageBoundaries[name]; !declared {
			t.Errorf("패키지 %s가 「패키지 경계」 표에 없다", name)
		}
	}
	for name := range packageBoundaries {
		if _, exists := found[name]; !exists {
			t.Errorf("「패키지 경계」 표의 패키지 %s가 구현에 없다", name)
		}
	}

	for name, expected := range packageBoundaries {
		actual, exists := found[name]
		if !exists {
			continue
		}
		for _, dependency := range actual {
			if !slices.Contains(expected, dependency) {
				t.Errorf("%s가 표에 없는 %s를 의존한다", name, dependency)
			}
		}
		for _, dependency := range expected {
			if !slices.Contains(actual, dependency) {
				t.Errorf("%s가 표에 적힌 %s를 의존하지 않는다", name, dependency)
			}
		}
	}
}

// TestDatabaseHandleStaysInStore는 데이터베이스 핸들이 `store` 밖으로 나가지 않는지 본다.
//
// 「접근 계층」이 우회 경로를 막는 방법으로 연결을 내보내지 않는 것을 들었다. 드라이버를
// 직접 import할 수 있는 곳은 `store`와 업무 패키지 밖의 마이그레이션 실행기뿐이다.
// 테스트 파일은 보지 않는다. 통합 테스트가 데이터베이스를 직접 준비하는 것은 제품 경로가
// 아니기 때문이다.
func TestDatabaseHandleStaysInStore(t *testing.T) {
	allowed := []string{
		filepath.Join("..", "..", "internal", "store"),
		filepath.Join("..", "..", "internal", "migrate"),
		filepath.Join("..", "..", "cmd", "migrate"),
	}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if slices.Contains(allowed, filepath.Dir(path)) {
			return nil
		}
		for _, imported := range fileImports(t, path) {
			if strings.HasPrefix(imported, "github.com/jackc/pgx") {
				t.Errorf("%s가 데이터베이스 드라이버를 직접 의존한다", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("파일 훑기: %v", err)
	}
}

// internalImports는 한 패키지의 비테스트 파일이 부르는 내부 패키지 이름을 모은다.
func internalImports(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("패키지 %s 읽기: %v", directory, err)
	}
	names := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		for _, imported := range fileImports(t, filepath.Join(directory, entry.Name())) {
			name, found := strings.CutPrefix(imported, "agent_context_sharing/internal/")
			if found && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return names
}

// fileImports는 파일 하나의 import 경로를 읽는다. 본문은 해석하지 않는다.
func fileImports(t *testing.T, path string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("%s 해석: %v", path, err)
	}
	paths := make([]string, 0, len(parsed.Imports))
	for _, imported := range parsed.Imports {
		value, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatalf("%s의 import 경로 해석: %v", path, err)
		}
		paths = append(paths, value)
	}
	return paths
}
