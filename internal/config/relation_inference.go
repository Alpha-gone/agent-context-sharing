package config

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RelationInferenceConfig는 작업 등록과 판정에 사용할 검증된 구성이다.
type RelationInferenceConfig struct {
	Mode, Provider, Model, APIKey                           string
	BaseURL                                                 *url.URL
	Timeout, IndexWait                                      time.Duration
	CandidateLimit, TargetChars, CandidateChars, InputChars int
	AutoConfirmTypes                                        []string
	AcceptedProfile                                         string
}

var inferenceFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

func loadRelationInference(cfg *Config, env Environment) error {
	value := RelationInferenceConfig{Mode: strings.TrimSpace(env("RELATION_INFERENCE_MODE"))}
	if value.Mode == "" {
		value.Mode = "off"
	}
	if !slices.Contains([]string{"off", "shadow", "propose", "apply"}, value.Mode) {
		return invalid("RELATION_INFERENCE_MODE", "허용된 추론 모드가 아니다", nil)
	}
	value.Provider = strings.TrimSpace(env("RELATION_INFERENCE_PROVIDER"))
	value.Model = strings.TrimSpace(env("RELATION_INFERENCE_MODEL"))
	value.APIKey = strings.TrimSpace(env("RELATION_INFERENCE_API_KEY"))
	if value.Mode != "off" {
		if value.Provider != "openai" || hasControl(value.Provider) {
			return invalid("RELATION_INFERENCE_PROVIDER", "지원하는 제공자는 openai다", nil)
		}
		if value.Model == "" || hasControl(value.Model) {
			return invalid("RELATION_INFERENCE_MODEL", "모델이 필요하다", nil)
		}
		if value.APIKey == "" || hasControl(env("RELATION_INFERENCE_API_KEY")) {
			return invalid("RELATION_INFERENCE_API_KEY", "API 키가 필요하다", nil)
		}
		base, err := parseHTTPURL("RELATION_INFERENCE_BASE_URL", env("RELATION_INFERENCE_BASE_URL"))
		if err != nil {
			return err
		}
		if base.Scheme != "https" || base.User != nil || base.RawQuery != "" || base.ForceQuery || strings.Contains(env("RELATION_INFERENCE_BASE_URL"), "#") {
			return invalid("RELATION_INFERENCE_BASE_URL", "사용자 정보·query·fragment 없는 HTTPS URL이 필요하다", nil)
		}
		value.BaseURL = base
	}
	for _, item := range []struct {
		key      string
		dst      *time.Duration
		fallback time.Duration
	}{
		{"RELATION_INFERENCE_TIMEOUT", &value.Timeout, 30 * time.Second},
		{"RELATION_INFERENCE_INDEX_WAIT", &value.IndexWait, 10 * time.Minute},
	} {
		*item.dst = item.fallback
		if raw := strings.TrimSpace(env(item.key)); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil || parsed <= 0 || (item.key == "RELATION_INFERENCE_TIMEOUT" && parsed > time.Duration(1<<62-1)) {
				return invalid(item.key, "허용 범위의 양수 기간이 필요하다", err)
			}
			*item.dst = parsed
		}
	}
	for _, item := range []struct {
		key           string
		dst           *int
		fallback, max int
	}{
		{"RELATION_INFERENCE_CANDIDATE_LIMIT", &value.CandidateLimit, 8, 20},
		{"RELATION_INFERENCE_TARGET_CHARS", &value.TargetChars, 2000, int(^uint(0) >> 1)},
		{"RELATION_INFERENCE_CANDIDATE_CHARS", &value.CandidateChars, 800, int(^uint(0) >> 1)},
		{"RELATION_INFERENCE_INPUT_CHARS", &value.InputChars, 9000, int(^uint(0) >> 1)},
	} {
		*item.dst = item.fallback
		if raw := strings.TrimSpace(env(item.key)); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > item.max {
				return invalid(item.key, "허용 범위의 양의 정수가 필요하다", err)
			}
			*item.dst = parsed
		}
	}
	if value.InputChars < value.TargetChars {
		return invalid("RELATION_INFERENCE_INPUT_CHARS", "대상 문자 상한 이상이어야 한다", nil)
	}
	if raw := strings.TrimSpace(env("RELATION_INFERENCE_AUTO_CONFIRM_TYPES")); raw != "" {
		values, err := parseList("RELATION_INFERENCE_AUTO_CONFIRM_TYPES", raw)
		if err != nil {
			return err
		}
		for _, kind := range values {
			if !slices.Contains([]string{"relates_to", "part_of"}, kind) {
				return invalid("RELATION_INFERENCE_AUTO_CONFIRM_TYPES", "허용되지 않은 관계 유형이다", nil)
			}
		}
		value.AutoConfirmTypes = values
	}
	value.AcceptedProfile = strings.TrimSpace(env("RELATION_INFERENCE_ACCEPTED_PROFILE"))
	if value.AcceptedProfile != "" && !inferenceFingerprint.MatchString(value.AcceptedProfile) {
		return invalid("RELATION_INFERENCE_ACCEPTED_PROFILE", "SHA-256 소문자 지문이 필요하다", nil)
	}
	cfg.RelationInference = value
	return nil
}
