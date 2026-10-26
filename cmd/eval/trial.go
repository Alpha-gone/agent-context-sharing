package main

import (
	"context"
	"crypto/sha256"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type trialBindings map[string]string

func (b *trialBindings) String() string { return "" }
func (b *trialBindings) Set(value string) error {
	code, path, ok := strings.Cut(value, "=")
	if !ok || !manifestCode.MatchString(code) || path == "" {
		return fmt.Errorf("trial-input은 code=파일이어야 한다")
	}
	if *b == nil {
		*b = trialBindings{}
	}
	if _, ok := (*b)[code]; ok {
		return fmt.Errorf("trial-input 코드가 중복되었다")
	}
	(*b)[code] = path
	return nil
}

type trialCheck struct {
	Code    string `json:"code"`
	Role    string `json:"role"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}
type trialPreparation struct {
	Checks        []trialCheck                 `json:"checksum_checks"`
	Units         int                          `json:"queries"`
	Independent   int                          `json:"independent_units"`
	Contexts      int                          `json:"contexts"`
	Source        int                          `json:"source_contexts"`
	Derived       int                          `json:"derived_contexts"`
	Events        int                          `json:"events"`
	Progress      trialProgressSnapshot        `json:"execution"`
	Readiness     string                       `json:"measurement_index_readiness"`
	Pending       int                          `json:"measurement_index_pending"`
	Failed        int                          `json:"measurement_index_failed"`
	DocumentCalls int64                        `json:"document_embedding_http_attempts"`
	VectorRows    int64                        `json:"vector_rows"`
	VectorBytes   int64                        `json:"vector_column_bytes"`
	Costs         map[string]businessCostTotal `json:"costs_by_phase"`
}

type manifestTrial struct {
	Manifest                           executionManifest
	Root, Stage, Lock, Private, Bundle string
	Inputs                             map[string]string
	Preparation                        trialPreparation
	Progress                           *trialProgress
}

type trialError struct {
	Code  string
	Cause error
}

func (e *trialError) Error() string          { return "평가 실행 실패: " + e.Code }
func (e *trialError) Unwrap() error          { return e.Cause }
func failTrial(code string, err error) error { return &trialError{code, err} }

func runManifestTrial(manifestPath, root string, bindings trialBindings, prepared string) (err error) {
	m, err := loadManifest(manifestPath)
	if err != nil {
		return failTrial("manifest_invalid", err)
	}
	if m.Mode == "convert" {
		return failTrial("conversion_is_not_measurement", nil)
	}
	trial, err := newManifestTrial(m, root)
	if err != nil {
		return err
	}
	defer func() { err = trial.finish(err) }()
	trial.Progress.setStage("input")
	if err = trial.bindInputs(bindings, prepared); err != nil {
		return failTrial("input_mismatch", err)
	}
	trial.Progress.setStage("configuration")
	args, err := trial.arguments(prepared != "")
	if err != nil {
		return failTrial("control_mismatch", err)
	}
	trial.Progress.setStage("execution")
	if err = runEvaluation(args, trial.Progress); err != nil {
		return failTrial("execution_failed", err)
	}
	return nil
}

func newManifestTrial(m executionManifest, root string) (*manifestTrial, error) {
	if root == "" {
		return nil, failTrial("output_required", nil)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, failTrial("output_unavailable", err)
	}
	if _, err := os.Lstat(filepath.Join(root, m.TrialID)); !os.IsNotExist(err) {
		return nil, failTrial("trial_id_exists", err)
	}
	lock := filepath.Join(root, "."+m.TrialID+".lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return nil, failTrial("trial_id_locked", err)
	}
	stage, err := os.MkdirTemp(root, "."+m.TrialID+"-")
	if err != nil {
		_ = os.Remove(lock)
		return nil, failTrial("output_unavailable", err)
	}
	t := &manifestTrial{Manifest: m, Root: root, Stage: stage, Lock: lock, Private: filepath.Join(stage, "private"), Bundle: filepath.Join(stage, "bundle"), Inputs: map[string]string{}, Progress: newTrialProgress(m.Execution)}
	for _, path := range []string{t.Private, t.Bundle} {
		if err := os.Mkdir(path, 0700); err != nil {
			_ = os.RemoveAll(stage)
			_ = os.Remove(lock)
			return nil, failTrial("output_unavailable", err)
		}
	}
	return t, nil
}

func (t *manifestTrial) bindInputs(bindings trialBindings, prepared string) error {
	roles := map[string][]string{"basic": {"contexts", "queries"}, "adaptive": {"contexts", "queries"}, "consistency": {"contexts", "queries"}, "adversarial": {"contexts", "queries", "attacks"}, "continual": {"continual_scenarios"}, "business": {"business"}, "business_tasks": {"business_tasks"}, "units": {"unit_comparison"}, "embeddings": {"embedding_comparison"}, "originals": {"original_reproduction"}}[t.Manifest.Mode]
	if prepared != "" {
		if t.Manifest.Mode != "basic" {
			return fmt.Errorf("준비 입력 모드 불일치")
		}
		roles = []string{"contexts", "queries", "preparation"}
	}
	if len(t.Manifest.Inputs) != len(roles) {
		return fmt.Errorf("입력 역할 수 불일치")
	}
	seenRoles, codes := map[string]bool{}, map[string]bool{}
	for _, input := range t.Manifest.Inputs {
		if !slices.Contains(roles, input.Role) || seenRoles[input.Role] {
			return fmt.Errorf("입력 역할 불일치")
		}
		seenRoles[input.Role], codes[input.Code] = true, true
	}
	for code := range bindings {
		if !codes[code] {
			return fmt.Errorf("선언하지 않은 입력 코드")
		}
	}
	known := map[string]bool{}
	for _, input := range t.Manifest.Inputs {
		known[input.Code] = true
		path := bindings[input.Code]
		name := input.Code + ".json"
		if prepared != "" {
			switch input.Role {
			case "contexts":
				name = "memory.json"
			case "queries":
				name = "evaluation-private.json"
			case "preparation":
				name = "preparation.json"
			default:
				return fmt.Errorf("준비 입력 역할이 올바르지 않다")
			}
			if path != "" {
				return fmt.Errorf("준비 입력과 직접 입력을 혼합할 수 없다")
			}
			path = filepath.Join(prepared, name)
		}
		if path == "" {
			return fmt.Errorf("입력 연결이 누락되었다")
		}
		if _, duplicate := t.Inputs[input.Role]; duplicate {
			return fmt.Errorf("실행 역할이 중복되었다")
		}
		dest := filepath.Join(t.Private, name)
		if input.Role == "continual_scenarios" && path == "builtin:continual-v1" {
			raw, _ := json.Marshal(trialContinualInputs(), json.Deterministic(true))
			if fmt.Sprintf("%x", sha256.Sum256(raw)) != input.SHA256 {
				return fmt.Errorf("내장 시나리오 checksum 불일치")
			}
			if err := os.WriteFile(dest, raw, 0600); err != nil {
				return err
			}
		} else if err := snapshotTrialInput(path, dest, input.SHA256); err != nil {
			return err
		}
		t.Inputs[input.Role] = dest
		t.Preparation.Checks = append(t.Preparation.Checks, trialCheck{input.Code, input.Role, input.SHA256, input.Version})
		if input.Role == "original_reproduction" {
			if err := t.snapshotOriginals(path, dest); err != nil {
				return err
			}
		}
	}
	for code := range bindings {
		if !known[code] {
			return fmt.Errorf("선언하지 않은 입력 코드")
		}
	}
	return t.checkInputs(prepared != "")
}

func snapshotTrialInput(source, destination, checksum string) (err error) {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("입력은 일반 파일이어야 한다")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, output.Close()) }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, maxPublicDataBytes+1))
	if err != nil {
		return err
	}
	if n > maxPublicDataBytes || fmt.Sprintf("%x", hash.Sum(nil)) != checksum {
		return fmt.Errorf("입력 checksum 불일치 또는 크기 초과")
	}
	return output.Sync()
}

func (t *manifestTrial) input(role string) manifestInput {
	for _, input := range t.Manifest.Inputs {
		if input.Role == role {
			return input
		}
	}
	return manifestInput{}
}

func (t *manifestTrial) requireRoles(roles ...string) error {
	if len(t.Inputs) != len(roles) {
		return fmt.Errorf("모드와 실제 입력 역할이 맞지 않는다")
	}
	for _, role := range roles {
		if t.Inputs[role] == "" {
			return fmt.Errorf("필수 입력 역할이 없다")
		}
	}
	return nil
}

func (t *manifestTrial) checkVersion(role, version string) error {
	if t.input(role).Version != version {
		return fmt.Errorf("입력 판 불일치")
	}
	return nil
}

func (t *manifestTrial) checkUnits(units []manifestUnit) error {
	key := func(u manifestUnit) string { raw, _ := json.Marshal(u, json.Deterministic(true)); return string(raw) }
	actual, want := []string{}, []string{}
	independent := map[string]bool{}
	for _, u := range units {
		actual = append(actual, key(u))
		independent[u.Dataset+"/"+u.Independent] = true
	}
	for _, u := range t.Manifest.Units {
		want = append(want, key(u))
	}
	slices.Sort(actual)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		return fmt.Errorf("질의·유형·과업·독립 단위 매핑 불일치")
	}
	t.Preparation.Units, t.Preparation.Independent = len(units), len(independent)
	return nil
}

func (t *manifestTrial) finish(cause error) (err error) {
	defer func() { err = errors.Join(err, os.RemoveAll(t.Stage), os.Remove(t.Lock)) }()
	if cause == nil && t.Progress.snapshot().LimitExceeded {
		cause = failTrial("request_limit_exceeded", nil)
	}
	if cause == nil && time.Now().After(t.Progress.deadline) {
		cause = failTrial("deadline_exceeded", context.DeadlineExceeded)
	}
	if cause == nil {
		t.Progress.setStage("publication")
		cause = t.prepareBundle()
	}
	if cause != nil {
		if err := os.RemoveAll(t.Bundle); err != nil {
			return errors.Join(cause, failTrial("failure_report_unavailable", err))
		}
		if err := os.Mkdir(t.Bundle, 0700); err != nil {
			return errors.Join(cause, failTrial("failure_report_unavailable", err))
		}
		code := "execution_failed"
		for e, ok := errors.AsType[*trialError](cause); ok; {
			code = e.Code
			e, ok = errors.AsType[*trialError](e.Cause)
		}
		if errors.Is(cause, context.Canceled) {
			code = "cancelled"
		}
		if errors.Is(cause, context.DeadlineExceeded) {
			code = "deadline_exceeded"
		}
		failure := struct {
			Trial    string                `json:"trial_id"`
			Mode     string                `json:"mode"`
			Status   string                `json:"status"`
			Code     string                `json:"cause_code"`
			Progress trialProgressSnapshot `json:"progress"`
		}{t.Manifest.TrialID, t.Manifest.Mode, "failed", code, t.Progress.snapshot()}
		if e := writeTrialJSON(t.Bundle, "manifest.json", t.Manifest); e != nil {
			return errors.Join(cause, failTrial("failure_report_unavailable", e))
		}
		if e := writeTrialJSON(t.Bundle, "failure.json", failure); e != nil {
			return errors.Join(cause, failTrial("failure_report_unavailable", e))
		}
	}
	// private에는 원문/자격을 포함할 수 있는 입력 스냅숏이 있다. 공개 경로로 이동하지 않는다.
	if e := os.RemoveAll(t.Private); e != nil {
		return errors.Join(cause, failTrial("private_cleanup_failed", e))
	}
	if e := syncTrialDirectory(t.Bundle); e != nil {
		return errors.Join(cause, failTrial("publication_failed", e))
	}
	if e := renameTrialExclusive(t.Bundle, filepath.Join(t.Root, t.Manifest.TrialID)); e != nil {
		return errors.Join(cause, failTrial("publication_failed", e))
	}
	if e := syncTrialDirectory(t.Root); e != nil {
		return errors.Join(cause, failTrial("publication_sync_failed", e))
	}
	return cause
}

func writeTrialJSON(directory, name string, value any) (err error) {
	raw, err := json.Marshal(value, json.Deterministic(true), json.FormatNilSliceAsNull(false))
	if err != nil {
		return err
	}
	var check any
	if err := json.Unmarshal(raw, &check); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".json-")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(append(raw, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	verified, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(verified, &check); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(directory, name))
}

func syncTrialDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
