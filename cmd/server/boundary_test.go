package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// packageBoundaries는 정본의 기존·목표 패키지 표를 직접 읽는다.
// 테스트에 별도 허용표를 두면 구현과 테스트가 함께 설계에서 벗어날 수 있다.
func packageBoundaries(t *testing.T) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "spec", "service", "agent-context", "sdd", "architecture.md"))
	if err != nil {
		t.Fatal(err)
	}
	boundaries := make(map[string][]string)
	active := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "## ") {
			active = line == "## 패키지 경계" || line == "## 목표 추가 패키지와 조립"
		}
		if !active || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 5 {
			continue
		}
		name := strings.TrimSpace(cells[1])
		if !strings.HasPrefix(name, "`") {
			continue
		}
		name = strings.Trim(name, "`")
		if _, duplicate := boundaries[name]; duplicate {
			t.Fatalf("중복 패키지: %s", name)
		}
		var dependencies []string
		for i, token := range strings.Split(cells[3], "`") {
			if i%2 == 1 {
				dependencies = append(dependencies, token)
			}
		}
		boundaries[name] = dependencies
	}
	if len(boundaries) != 11 {
		t.Fatalf("설계 업무 패키지 수: %d, want 11", len(boundaries))
	}
	return boundaries
}

// boundaryExemptPackages는 「패키지 경계」가 업무 패키지 밖으로 둔 것이다.
// 마이그레이션 실행기는 `store`를 거치지 않고 데이터베이스에 직접 닿는 도구다.
var boundaryExemptPackages = []string{"migrate"}

// TestPackageDependenciesMatchDesign은 「패키지 경계」 표와 실제 import를 양방향으로 대조한다.
//
// 한쪽 방향만 보면 표가 낡는다. 표에 없는 의존이 생기는 것뿐 아니라 표에 적힌 의존이
// 사라지는 것도 문서와 구현을 어긋나게 하므로 둘 다 실패로 다룬다.
func TestPackageDependenciesMatchDesign(t *testing.T) {
	expected := packageBoundaries(t)
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

	for _, violation := range dependencyViolations(expected, found) {
		t.Error(violation)
	}
	if cycle := dependencyCycle(found); len(cycle) > 0 {
		t.Errorf("패키지 순환: %v", cycle)
	}

}

// TestDatabaseHandleStaysInStore는 서버 모듈의 데이터베이스 핸들이 `store` 밖으로 나가지 않는지 본다.
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
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
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

func dependencyViolations(expected, actual map[string][]string) []string {
	var violations []string
	for name := range actual {
		if _, exists := expected[name]; !exists {
			violations = append(violations, "미선언 패키지: "+name)
		}
	}
	for name, dependencies := range expected {
		imported, exists := actual[name]
		if !exists {
			violations = append(violations, "누락 패키지: "+name)
			continue
		}
		for _, dependency := range imported {
			if !slices.Contains(dependencies, dependency) {
				violations = append(violations, fmt.Sprintf("추가 의존: %s -> %s", name, dependency))
			}
		}
		for _, dependency := range dependencies {
			if !slices.Contains(imported, dependency) {
				violations = append(violations, fmt.Sprintf("누락 의존: %s -> %s", name, dependency))
			}
		}
	}
	slices.Sort(violations)
	return violations
}

func dependencyCycle(dependencies map[string][]string) []string {
	states := map[string]int{}
	var path, cycle []string
	var visit func(string) bool
	visit = func(name string) bool {
		if states[name] == 1 {
			cycle = append(slices.Clone(path), name)
			return true
		}
		if states[name] == 2 {
			return false
		}
		states[name] = 1
		path = append(path, name)
		for _, dependency := range dependencies[name] {
			if visit(dependency) {
				return true
			}
		}
		path = path[:len(path)-1]
		states[name] = 2
		return false
	}
	for name := range dependencies {
		if visit(name) {
			return cycle
		}
	}
	return nil
}

func TestBoundaryRulesRejectViolations(t *testing.T) {
	expected := map[string][]string{"model": {}, "search": {"model"}}
	for _, sample := range []struct {
		name   string
		actual map[string][]string
	}{
		{"missing_package", map[string][]string{"model": {}}},
		{"additional_package", map[string][]string{"model": {}, "search": {"model"}, "other": {}}},
		{"missing_dependency", map[string][]string{"model": {}, "search": {}}},
		{"additional_dependency", map[string][]string{"model": {}, "search": {"model", "store"}}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			if len(dependencyViolations(expected, sample.actual)) == 0 {
				t.Fatal("경계 위반을 놓쳤다")
			}
		})
	}
	if got := dependencyViolations(expected, expected); len(got) != 0 {
		t.Fatal(got)
	}
	if len(dependencyCycle(expected)) != 0 {
		t.Fatal("단방향 의존을 순환으로 판단했다")
	}
	if len(dependencyCycle(map[string][]string{"first": {"second"}, "second": {"first"}})) == 0 {
		t.Fatal("순환을 놓쳤다")
	}
}

// TestStoreExportsNoDatabaseHandle은 private 필드에 보관한 DB 자원이
// public 필드·별칭·함수 결과로 노출되는 우회 경로를 검사한다.
func TestStoreExportsNoDatabaseHandle(t *testing.T) {
	root := filepath.Join("..", "..", "internal", "store")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]ast.Expr{}
	var files []*ast.File
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		for _, decl := range file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range general.Specs {
				if spec, ok := spec.(*ast.TypeSpec); ok {
					types[spec.Name.Name] = spec.Type
				}
			}
		}
	}
	drivers := map[string]bool{}
	for _, file := range files {
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(path, "github.com/jackc/pgx") {
				continue
			}
			name := filepath.Base(path)
			if name == "v5" {
				name = "pgx"
			}
			if spec.Name != nil {
				name = spec.Name.Name
			}
			drivers[name] = true
		}
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Name.IsExported() && exposesDriver(decl.Type, drivers, types, map[string]bool{}) {
					t.Errorf("함수 %s가 DB 타입을 노출한다", decl.Name)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if spec, ok := spec.(*ast.TypeSpec); ok && spec.Name.IsExported() && exposesDriver(spec.Type, drivers, types, map[string]bool{}) {
						t.Errorf("타입 %s가 DB 타입을 노출한다", spec.Name)
					}
				}
			}
		}
	}
}

func exposesDriver(expression ast.Expr, drivers map[string]bool, types map[string]ast.Expr, visited map[string]bool) bool {
	if expression == nil {
		return false
	}
	fieldsExpose := func(fields *ast.FieldList, publicOnly bool) bool {
		if fields == nil {
			return false
		}
		for _, field := range fields.List {
			if publicOnly && len(field.Names) > 0 && !slices.ContainsFunc(field.Names, func(name *ast.Ident) bool { return name.IsExported() }) {
				continue
			}
			if exposesDriver(field.Type, drivers, types, visited) {
				return true
			}
		}
		return false
	}
	switch value := expression.(type) {
	case *ast.SelectorExpr:
		identifier, ok := value.X.(*ast.Ident)
		return ok && drivers[identifier.Name]
	case *ast.Ident:
		if visited[value.Name] {
			return false
		}
		visited[value.Name] = true
		return exposesDriver(types[value.Name], drivers, types, visited)
	case *ast.StarExpr:
		return exposesDriver(value.X, drivers, types, visited)
	case *ast.ArrayType:
		return exposesDriver(value.Elt, drivers, types, visited)
	case *ast.MapType:
		return exposesDriver(value.Key, drivers, types, visited) || exposesDriver(value.Value, drivers, types, visited)
	case *ast.ChanType:
		return exposesDriver(value.Value, drivers, types, visited)
	case *ast.StructType:
		return fieldsExpose(value.Fields, true)
	case *ast.InterfaceType:
		return fieldsExpose(value.Methods, false)
	case *ast.FuncType:
		return fieldsExpose(value.Params, false) || fieldsExpose(value.Results, false)
	}
	return false
}

func TestHandleDetectorRejectsExports(t *testing.T) {
	for _, sample := range []struct {
		declaration string
		exposed     bool
	}{
		{"*pgxpool.Pool", true}, {"struct { Pool *pgxpool.Pool }", true},
		{"func() pgx.Tx", true}, {"interface { Query() pgx.Rows }", true},
		{"struct { pool *pgxpool.Pool; Count int }", false},
	} {
		expression, err := parser.ParseExpr(sample.declaration)
		if err != nil {
			t.Fatal(err)
		}
		if got := exposesDriver(expression, map[string]bool{"pgx": true, "pgxpool": true}, nil, map[string]bool{}); got != sample.exposed {
			t.Errorf("%s: %t", sample.declaration, got)
		}
	}
}
