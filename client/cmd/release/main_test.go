package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

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
	info := &debug.BuildInfo{GoVersion: "go1.27.1", Settings: []debug.BuildSetting{{Key: "CGO_ENABLED", Value: "0"}, {Key: "-trimpath", Value: "true"}, {Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "arm64"}, {Key: "vcs.revision", Value: "commit"}}}
	if err := validateBuild(info, "linux-arm64", "commit"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"linux-amd64", "darwin-arm64"} {
		if validateBuild(info, target, "commit") == nil {
			t.Fatal("다른 target 출처 허용")
		}
	}
	if validateBuild(info, "linux-arm64", "other") == nil {
		t.Fatal("다른 revision 허용")
	}
	info.GoVersion = "go1.26.0"
	if validateBuild(info, "linux-arm64", "commit") == nil {
		t.Fatal("다른 Go 판 허용")
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
