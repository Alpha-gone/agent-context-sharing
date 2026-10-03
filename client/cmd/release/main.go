// Command release는 네 플랫폼의 오프라인 배포 후보와 무결성 검증 자료를 만든다.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
	"uuid"
)

var targets = []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"}
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("사용법: release build|verify [옵션]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	directory := flags.String("dir", "", "새 산출물 디렉터리 또는 검증 대상")
	goBin := flags.String("go", "go", "Go 1.27.1 실행 파일")
	version := flags.String("version", "", "배포 후보 판 번호")
	key := flags.String("key", "", "서명용 Ed25519 PKCS#8 PEM 개인 키")
	publicKey := flags.String("public-key", "", "별도 경로로 신뢰한 Ed25519 PKIX PEM 공개 키")
	unsigned := flags.Bool("allow-unsigned", false, "후보 checksum만 검사하며 공식 검증으로 보지 않음")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *directory == "" {
		return errors.New("산출물 디렉터리가 필요하다")
	}
	switch args[0] {
	case "build":
		if !versionPattern.MatchString(*version) {
			return errors.New("단일 토큰 판 번호가 필요하다")
		}
		return build(*directory, *goBin, *version, *key)
	case "verify":
		return verify(*directory, *publicKey, *unsigned)
	default:
		return errors.New("build 또는 verify만 지원한다")
	}
}

func command(name string, args ...string) (string, error) {
	output, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("배포 명령 %s 실패: %w", filepath.Base(name), err)
	}
	return strings.TrimSpace(string(output)), nil
}

func build(directory, goBin, version, keyPath string) error {
	toolVersion, err := command(goBin, "version")
	if err != nil || !strings.Contains(toolVersion, "go1.27.1 ") {
		return errors.New("Go 1.27.1이 필요하다")
	}
	if _, err := os.Stat("go.mod"); err != nil {
		return errors.New("client 모듈 루트에서 실행해야 한다")
	}
	module, err := command(goBin, "list", "-m")
	if err != nil || module != "agent_context_sharing/client" {
		return errors.New("client 모듈 루트에서 실행해야 한다")
	}
	var private ed25519.PrivateKey
	if keyPath != "" {
		encoded, err := os.ReadFile(keyPath)
		if err != nil {
			return errors.New("서명 키를 읽지 못했다")
		}
		block, rest := pem.Decode(encoded)
		if block == nil || len(bytes.TrimSpace(rest)) != 0 {
			return errors.New("서명 키 PEM 형식 오류")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return errors.New("PKCS#8 서명 키 형식 오류")
		}
		var ok bool
		private, ok = parsed.(ed25519.PrivateKey)
		if !ok {
			return errors.New("Ed25519 서명 키가 필요하다")
		}
	}
	revision, err := command("git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	status, err := command("git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0o755); err != nil {
		return errors.New("새 산출물 디렉터리만 사용할 수 있다")
	}
	var checksums strings.Builder
	for _, target := range targets {
		goos, goarch, _ := strings.Cut(target, "-")
		name := "agent-context-client-" + target
		path := filepath.Join(directory, name)
		cmd := exec.Command(goBin, "build", "-mod=readonly", "-trimpath", "-buildvcs=true", "-ldflags=-X main.releaseVersion="+version, "-o", path, "./cmd/client")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch, "GOTOOLCHAIN=local")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s 빌드 실패: %w", target, err)
		}
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			return err
		}
		if err := validateBuild(info, target, revision); err != nil {
			return err
		}
		digest, err := fileDigest(path)
		if err != nil {
			return err
		}
		provenance := map[string]any{"format": "agent-context-client-local-build-v1", "candidate": true,
			"version": version, "revision": revision, "dirty": status != "", "target": target, "sha256": digest,
			"goVersion": info.GoVersion, "settings": info.Settings, "modules": info.Deps, "created": time.Now().UTC().Format(time.RFC3339)}
		if err := writeJSON(path+".provenance.json", provenance); err != nil {
			return err
		}
		if err := writeJSON(path+".spdx.json", sbom(info, name, version, digest)); err != nil {
			return err
		}
		for _, suffix := range []string{"", ".provenance.json", ".spdx.json"} {
			hash, err := fileDigest(path + suffix)
			if err != nil {
				return err
			}
			fmt.Fprintf(&checksums, "%s  %s\n", hash, name+suffix)
		}
	}
	manifest := []byte(checksums.String())
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), manifest, 0o644); err != nil {
		return err
	}
	if private == nil {
		fmt.Println("네 플랫폼의 서명 없는 배포 후보를 생성했다. 공식 출시 또는 서명 검증 완료가 아니다.")
		return verify(directory, "", true)
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS.sig"), ed25519.Sign(private, manifest), 0o644); err != nil {
		return err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(private.Public())
	if err != nil {
		return err
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := os.WriteFile(filepath.Join(directory, "signing-public.pem"), public, 0o644); err != nil {
		return err
	}
	fmt.Println("서명된 후보를 생성했다. 발행자 신뢰와 플랫폼별 출시 판정은 별도로 필요하다.")
	return nil
}

func validateBuild(info *debug.BuildInfo, target, revision string) error {
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	goos, goarch, _ := strings.Cut(target, "-")
	if info.GoVersion != "go1.27.1" || settings["CGO_ENABLED"] != "0" || settings["-trimpath"] != "true" || settings["GOOS"] != goos || settings["GOARCH"] != goarch || settings["vcs.revision"] != revision {
		return errors.New("실행 파일의 빌드 출처·target·CGO·trimpath가 맞지 않는다")
	}
	return nil
}

func fileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("일반 산출물 파일이 필요하다")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func writeJSON(path string, value any) error {
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

func sbom(info *debug.BuildInfo, name, version, digest string) map[string]any {
	packageEntry := func(id, name, version string) map[string]any {
		return map[string]any{"SPDXID": id, "name": name, "versionInfo": version, "downloadLocation": "NOASSERTION", "filesAnalyzed": false,
			"licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION", "copyrightText": "NOASSERTION"}
	}
	root := packageEntry("SPDXRef-Binary", name, version)
	root["checksums"] = []any{map[string]string{"algorithm": "SHA256", "checksumValue": digest}}
	packages := []any{root, packageEntry("SPDXRef-Go", "Go", info.GoVersion)}
	relationships := []any{map[string]string{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": "SPDXRef-Binary"},
		map[string]string{"spdxElementId": "SPDXRef-Binary", "relationshipType": "DEPENDS_ON", "relatedSpdxElement": "SPDXRef-Go"}}
	modules := append([]*debug.Module{&info.Main}, info.Deps...)
	for i, module := range modules {
		if module.Replace != nil {
			module = module.Replace
		}
		id := fmt.Sprintf("SPDXRef-Module-%d", i)
		packages = append(packages, packageEntry(id, module.Path, module.Version))
		relationships = append(relationships, map[string]string{"spdxElementId": "SPDXRef-Binary", "relationshipType": "DEPENDS_ON", "relatedSpdxElement": id})
	}
	return map[string]any{"spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT", "name": name,
		"documentNamespace": "https://spdx.org/spdxdocs/agent-context-client-" + uuid.NewV7().String(),
		"creationInfo":      map[string]any{"created": time.Now().UTC().Format(time.RFC3339), "creators": []string{"Tool: agent-context-client-release"}},
		"packages":          packages, "relationships": relationships}
}

func verify(directory, publicPath string, allowUnsigned bool) error {
	manifest, err := os.ReadFile(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return errors.New("checksum 목록을 읽지 못했다")
	}
	if !allowUnsigned {
		if publicPath == "" {
			return errors.New("별도 경로로 신뢰한 공개 키가 필요하다")
		}
		publicPEM, err := os.ReadFile(publicPath)
		if err != nil {
			return errors.New("신뢰 공개 키를 읽지 못했다")
		}
		block, rest := pem.Decode(publicPEM)
		if block == nil || len(bytes.TrimSpace(rest)) != 0 {
			return errors.New("공개 키 PEM 형식 오류")
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return errors.New("공개 키 형식 오류")
		}
		public, ok := parsed.(ed25519.PublicKey)
		signature, err := os.ReadFile(filepath.Join(directory, "SHA256SUMS.sig"))
		if !ok || err != nil || !ed25519.Verify(public, manifest, signature) {
			return errors.New("checksum 목록 서명 검증 실패")
		}
	}
	wanted := make(map[string]bool)
	for _, target := range targets {
		for _, suffix := range []string{"", ".provenance.json", ".spdx.json"} {
			wanted["agent-context-client-"+target+suffix] = true
		}
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(manifest), "\n"), "\n") {
		hash, name, ok := strings.Cut(line, "  ")
		if !ok || len(hash) != 64 || !wanted[name] {
			return errors.New("checksum 목록의 누락·중복·경로 오류")
		}
		delete(wanted, name)
		actual, err := fileDigest(filepath.Join(directory, name))
		if err != nil || hash != actual {
			return fmt.Errorf("%s checksum 검증 실패", name)
		}
	}
	if len(wanted) != 0 {
		return errors.New("네 플랫폼 산출물 또는 SBOM·출처 기록 누락")
	}
	for _, target := range targets {
		path := filepath.Join(directory, "agent-context-client-"+target)
		var provenance struct {
			Revision string `json:"revision"`
			SHA256   string `json:"sha256"`
			Target   string `json:"target"`
		}
		encoded, err := os.ReadFile(path + ".provenance.json")
		if err != nil || json.Unmarshal(encoded, &provenance) != nil || provenance.Target != target {
			return errors.New("빌드 출처 형식 오류")
		}
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			return errors.New("실행 파일 빌드 정보를 읽지 못했다")
		}
		if err := validateBuild(info, target, provenance.Revision); err != nil {
			return err
		}
		digest, err := fileDigest(path)
		if err != nil || digest != provenance.SHA256 {
			return errors.New("출처의 실행 파일 digest 불일치")
		}
	}
	fmt.Println("네 플랫폼 checksum·실행 파일 빌드 출처 검증 통과. 운영체제·호스트 출시 판정은 별도다.")
	return nil
}
