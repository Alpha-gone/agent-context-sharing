package config

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// DiagnosticConfig는 기록기 준비에 전달할 경로·배포 슬롯·수준이다.
// 실제 파일 권한·영속성·심볼릭 링크·단일 기록자 잠금은 기록기가 확인한다.
type DiagnosticConfig struct {
	Directory  string
	InstanceID string
	Level      string
}

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func loadDiagnostic(cfg *Config, env Environment) error {
	for _, key := range []string{"DIAGNOSTIC_LOG_DIR", "DIAGNOSTIC_INSTANCE_ID", "DIAGNOSTIC_LOG_LEVEL"} {
		if hasControl(env(key)) {
			return invalid(key, "제어 문자를 포함할 수 없다", nil)
		}
	}
	directory := strings.TrimSpace(env("DIAGNOSTIC_LOG_DIR"))
	if directory == "" {
		directory = "/var/log/agent-context"
	}
	if !filepath.IsAbs(directory) || directory == string(filepath.Separator) || filepath.Clean(directory) != directory || hasControl(directory) || strings.Contains(directory, "\\") {
		return invalid("DIAGNOSTIC_LOG_DIR", "정규 절대 디렉터리 경로가 아니다", nil)
	}
	for _, root := range []string{"/tmp", "/var/tmp", "/private/tmp", "/private/var/tmp", "/var/folders", "/private/var/folders", "/dev/shm"} {
		if directory == root || strings.HasPrefix(directory, root+"/") {
			return invalid("DIAGNOSTIC_LOG_DIR", "임시 디렉터리를 사용할 수 없다", nil)
		}
	}
	for component := range strings.SplitSeq(filepath.ToSlash(directory), "/") {
		if slices.Contains([]string{"assets", "public", "static"}, strings.ToLower(component)) {
			return invalid("DIAGNOSTIC_LOG_DIR", "공개 자산 경로를 사용할 수 없다", nil)
		}
	}
	instance := strings.TrimSpace(env("DIAGNOSTIC_INSTANCE_ID"))
	if !instanceIDPattern.MatchString(instance) {
		return invalid("DIAGNOSTIC_INSTANCE_ID", "1~64자 ASCII 영문·숫자·밑줄·하이픈이 필요하다", nil)
	}
	level := strings.TrimSpace(env("DIAGNOSTIC_LOG_LEVEL"))
	if level == "" {
		level = "INFO"
	}
	if !slices.Contains([]string{"DEBUG", "INFO", "WARN", "ERROR"}, level) {
		return invalid("DIAGNOSTIC_LOG_LEVEL", "DEBUG·INFO·WARN·ERROR 중 하나여야 한다", nil)
	}
	cfg.Diagnostic = DiagnosticConfig{Directory: directory, InstanceID: instance, Level: level}
	return nil
}
