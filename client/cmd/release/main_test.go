package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestRepositoryLocalOutputKeepsPerBinaryDirtyProvenance(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "client"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":             "module agent_context_sharing/client\n\ngo 1.27.1\n",
		"cmd/client/main.go": "package main\nvar releaseVersion string\nfunc main() { println(releaseVersion) }\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(repo)
	t.Setenv("GOWORK", "off")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for _, args := range [][]string{
		{"init", "--quiet"}, {"add", "go.mod", "cmd/client/main.go"},
		{"-c", "user.name=Release fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull, "commit", "--quiet", "-m", "fixture"},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("시험 저장소 구성: %v: %s", err, output)
		}
	}
	if err := build("candidate", filepath.Join(runtime.GOROOT(), "bin", "go"), "test", ""); err != nil {
		t.Fatal(err)
	}
	for index, target := range targets {
		path := filepath.Join("candidate", "agent-context-client-"+target)
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := os.ReadFile(path + ".provenance.json")
		if err != nil {
			t.Fatal(err)
		}
		var provenance struct {
			Dirty bool `json:"dirty"`
		}
		if err := json.Unmarshal(encoded, &provenance); err != nil {
			t.Fatal(err)
		}
		modified := ""
		for _, setting := range info.Settings {
			if setting.Key == "vcs.modified" {
				modified = setting.Value
			}
		}
		if modified != fmt.Sprint(provenance.Dirty) || provenance.Dirty != (index != 0) {
			t.Errorf("%s: vcs.modified=%q, provenance dirty=%v", target, modified, provenance.Dirty)
		}
	}
	path := filepath.Join("candidate", "agent-context-client-"+targets[1]+".provenance.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{false, nil, "true", "missing"} {
		var provenance map[string]any
		if err := json.Unmarshal(original, &provenance); err != nil {
			t.Fatal(err)
		}
		provenance["dirty"] = value
		if value == "missing" {
			delete(provenance, "dirty")
		}
		if err := writeJSON(path, provenance); err != nil {
			t.Fatal(err)
		}
		// checksum까지 갱신하여 digest 실패가 출처 불일치를 가리지 않게 한다.
		var checksums strings.Builder
		for _, target := range targets {
			for _, suffix := range []string{"", ".provenance.json", ".spdx.json"} {
				name := "agent-context-client-" + target + suffix
				digest, err := fileDigest(filepath.Join("candidate", name))
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&checksums, "%s  %s\n", digest, name)
			}
		}
		if err := os.WriteFile(filepath.Join("candidate", "SHA256SUMS"), []byte(checksums.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verify("candidate", "", true); err == nil || strings.Contains(err.Error(), "checksum") {
			t.Fatalf("dirty 값 %v의 출처 오류를 거부하지 않았다: %v", value, err)
		}
	}
}

func TestSPDXIncludesLinkedModulesAndRuntime(t *testing.T) {
	info := &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Path: "client", Version: "(devel)"}, Deps: []*debug.Module{{Path: "sdk", Version: "v1.8.0"}}}
	document := sbom(info, "binary", "candidate", strings.Repeat("a", 64))
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(document["packages"].([]any)) != 4 || len(document["relationships"].([]any)) != 4 || !strings.Contains(string(encoded), `"spdxVersion":"SPDX-2.3"`) || !strings.Contains(string(encoded), "NOASSERTION") {
		t.Fatal("SPDX 패키지·관계·라이선스 미확인 표시 누락")
	}
}

func TestBuildProvenanceRejectsChangedTargetAndSettings(t *testing.T) {
	info := &debug.BuildInfo{GoVersion: "go1.27.1", Settings: []debug.BuildSetting{{Key: "CGO_ENABLED", Value: "0"}, {Key: "-trimpath", Value: "true"}, {Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "arm64"}, {Key: "vcs.revision", Value: "commit"}, {Key: "vcs.modified", Value: "false"}}}
	if err := validateBuild(info, "linux-arm64", "commit", false); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"linux-amd64", "darwin-arm64"} {
		if validateBuild(info, target, "commit", false) == nil {
			t.Fatal("다른 target 출처 허용")
		}
	}
	if validateBuild(info, "linux-arm64", "other", false) == nil {
		t.Fatal("다른 revision 허용")
	}
	info.GoVersion = "go1.26.0"
	if validateBuild(info, "linux-arm64", "commit", false) == nil {
		t.Fatal("다른 Go 판 허용")
	}
}

func TestBuildProvenanceRejectsMissingOrMismatchedDirty(t *testing.T) {
	info := &debug.BuildInfo{GoVersion: "go1.27.1", Settings: []debug.BuildSetting{{Key: "CGO_ENABLED", Value: "0"}, {Key: "-trimpath", Value: "true"}, {Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "arm64"}, {Key: "vcs.revision", Value: "commit"}}}
	if validateBuild(info, "linux-arm64", "commit", false) == nil {
		t.Fatal("vcs.modified 누락 허용")
	}
	for _, value := range []string{"false", "true", "", "invalid"} {
		candidate := *info
		candidate.Settings = append(append([]debug.BuildSetting(nil), info.Settings...), debug.BuildSetting{Key: "vcs.modified", Value: value})
		for _, dirty := range []bool{false, true} {
			if valid := validateBuild(&candidate, "linux-arm64", "commit", dirty) == nil; valid != (value == fmt.Sprint(dirty)) {
				t.Fatalf("vcs.modified=%q, dirty=%v: valid=%v", value, dirty, valid)
			}
		}
	}
}

func TestChecksumRejectsUnsignedWrongSignerTamperAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	var manifest strings.Builder
	for _, target := range targets {
		for _, suffix := range []string{"", ".provenance.json", ".spdx.json"} {
			name := "agent-context-client-" + target + suffix
			payload := []byte(name)
			if err := os.WriteFile(filepath.Join(dir, name), payload, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(payload)
			fmt.Fprintf(&manifest, "%s  %s\n", hex.EncodeToString(digest[:]), name)
		}
	}
	encoded := []byte(manifest.String())
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if verify(dir, "", false) == nil {
		t.Fatal("신뢰 키 없는 공식 검증 허용")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "trusted.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if verify(dir, keyPath, false) == nil {
		t.Fatal("서명 없는 공식 검증 허용")
	}
	_, wrong, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), ed25519.Sign(wrong, encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	if verify(dir, keyPath, false) == nil {
		t.Fatal("다른 발행자 서명 허용")
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), ed25519.Sign(private, encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent-context-client-darwin-amd64")
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(dir, keyPath, false); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal("서명 뒤 실행 파일 변조를 거부하지 않았다")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if verify(dir, keyPath, false) == nil {
		t.Fatal("산출물 누락 허용")
	}
}

func TestChecksumRejectsPathTraversal(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", "unknown"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("0", 64)+"  "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if verify(dir, "", true) == nil {
			t.Fatal("checksum 경로 오류 허용")
		}
	}
}

func TestChecksumRejectsDuplicateAndSymlink(t *testing.T) {
	dir := t.TempDir()
	name := "agent-context-client-darwin-amd64"
	payload := []byte("fixture")
	digest := sha256.Sum256(payload)
	line := fmt.Sprintf("%x  %s\n", digest, name)
	manifest := filepath.Join(dir, "SHA256SUMS")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(line+line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(dir, "", true); err == nil || !strings.Contains(err.Error(), "중복") {
		t.Fatal("checksum 중복을 거부하지 않았다")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(dir, "", true); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal("산출물 symlink를 거부하지 않았다")
	}
}
