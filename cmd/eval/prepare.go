package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const preparationVersion = "eval-data-v1"
const maxPublicDataBytes = 512 << 20

type publicDataPin struct {
	Dataset     string `json:"dataset"`
	Commit      string `json:"repository_commit"`
	SHA256      string `json:"sha256"`
	URL         string `json:"source_url"`
	License     string `json:"license"`
	Attribution string `json:"attribution"`
}

// 배포 파일의 이름과 내용 판을 구분한다. 원본은 실행 시 자동으로 내려받지 않는다.
func publicPin(format string) (publicDataPin, bool) {
	switch format {
	case "longmemeval":
		return publicDataPin{Dataset: "longmemeval-s", Commit: "2ec2a557f339b6c0369619b1ed5793734cc87533", SHA256: "08d8dad4be43ee2049a22ff5674eb86725d0ce5ff434cde2627e5e8e7e117894", URL: "https://huggingface.co/datasets/xiaowu0162/longmemeval/resolve/2ec2a557f339b6c0369619b1ed5793734cc87533/longmemeval_s", License: "MIT", Attribution: "Di Wu et al., LongMemEval (ICLR 2025)"}, true
	case "locomo":
		return publicDataPin{Dataset: "locomo10-text", Commit: "3eb6f2c585f5e1699204e3c3bdf7adc5c28cb376", SHA256: "79fa87e90f04081343b8c8debecb80a9a6842b76a7aa537dc9fdf651ea698ff4", URL: "https://github.com/snap-research/locomo/blob/3eb6f2c585f5e1699204e3c3bdf7adc5c28cb376/data/locomo10.json", License: "CC-BY-NC-4.0", Attribution: "Maharana et al., Evaluating Very Long-Term Conversational Memory of LLM Agents (ACL 2024)"}, true
	}
	return publicDataPin{}, false
}

type preparedMemory struct {
	Version string          `json:"version"`
	Groups  []preparedGroup `json:"groups"`
}

type preparedGroup struct {
	Code     string     `json:"unit_code"`
	Contexts contextSet `json:"context_set"`
}

type preparedQuery struct {
	Code           string            `json:"query_code"`
	Group          string            `json:"graph_code"`
	Unit           string            `json:"unit_code"`
	Type           string            `json:"type"`
	Question       string            `json:"question"`
	Answer         jsontext.Value    `json:"answer,omitzero"`
	AnswerStatus   string            `json:"answer_status"`
	EvidenceStatus string            `json:"evidence_status"`
	EvidenceReason string            `json:"evidence_reason,omitzero"`
	Evidence       []string          `json:"evidence_keys,omitzero"`
	AsOf           time.Time         `json:"as_of"`
	Steps          []supplementStep  `json:"steps,omitzero"`
	Actors         []supplementActor `json:"actors,omitzero"`
}

type preparedEvaluation struct {
	Version string          `json:"version"`
	Queries []preparedQuery `json:"queries"`
}

type privateMapping struct {
	Code              string         `json:"code"`
	Unit              string         `json:"unit_code"`
	Original          string         `json:"original_id"`
	Session           string         `json:"session_id,omitzero"`
	Timestamp         string         `json:"original_timestamp,omitzero"`
	Turn              string         `json:"turn_id,omitzero"`
	Speaker           string         `json:"speaker,omitzero"`
	UnindexedText     *string        `json:"unindexed_text,omitzero"`
	HasAnswer         *bool          `json:"has_answer,omitzero"`
	EvidenceRefs      []string       `json:"original_evidence_refs,omitzero"`
	OriginalAnswer    jsontext.Value `json:"original_answer,omitzero"`
	AdversarialAnswer jsontext.Value `json:"adversarial_answer,omitzero"`
}

type preparedMappings struct {
	Version string           `json:"version"`
	Items   []privateMapping `json:"items"`
}

type preparationReport struct {
	Version         string             `json:"version"`
	Dataset         string             `json:"dataset"`
	Pin             *publicDataPin     `json:"source,omitzero"`
	Mapping         manifestMapping    `json:"mapping"`
	Units           []manifestUnit     `json:"units"`
	Exclusions      manifestExclusions `json:"exclusions"`
	Included        int                `json:"included_queries"`
	Graphs          int                `json:"graph_groups"`
	Independent     int                `json:"independent_units"`
	Contexts        int                `json:"contexts"`
	Types           map[string]int     `json:"query_types"`
	Evidence        map[string]int     `json:"evidence_status_counts"`
	EvidenceReasons map[string]int     `json:"evidence_reason_counts"`
	SourceCounts    map[string]int     `json:"source_counts"`
	Files           map[string]string  `json:"private_file_sha256"`
}

type preparedData struct {
	Memory     preparedMemory
	Evaluation preparedEvaluation
	Mappings   preparedMappings
	Report     preparationReport
}

func newPreparedData(dataset string, pin *publicDataPin, total int) preparedData {
	return preparedData{Memory: preparedMemory{Version: preparationVersion}, Evaluation: preparedEvaluation{Version: preparationVersion}, Mappings: preparedMappings{Version: preparationVersion}, Report: preparationReport{Version: preparationVersion, Dataset: dataset, Pin: pin,
		Mapping:    manifestMapping{Preprocessing: preparationVersion, Split: preparationVersion, Answers: preparationVersion, Evidence: preparationVersion, AnswerPolicy: "evaluation-only-separated"},
		Exclusions: manifestExclusions{Policy: preparationVersion, Total: total, Items: []manifestExclusion{}}, Types: map[string]int{}, Evidence: map[string]int{}, EvidenceReasons: map[string]int{}, SourceCounts: map[string]int{}, Files: map[string]string{}}}
}

func (data *preparedData) addQuery(q preparedQuery, conversation, task string) {
	data.Evaluation.Queries = append(data.Evaluation.Queries, q)
	data.Report.Units = append(data.Report.Units, manifestUnit{Dataset: data.Report.Dataset, Query: q.Code, Conversation: conversation, Task: task, Type: q.Type, Independent: q.Unit})
	data.Report.Types[q.Type]++
	data.Report.Evidence[q.EvidenceStatus]++
	if q.EvidenceReason != "" {
		data.Report.EvidenceReasons[q.EvidenceReason]++
	}
}

func (data *preparedData) finish() error {
	data.Report.Included = len(data.Evaluation.Queries)
	data.Report.Graphs = len(data.Memory.Groups)
	units := map[string]bool{}
	keys := map[string]map[string]bool{}
	groups := map[string]bool{}
	for _, group := range data.Memory.Groups {
		if len(group.Contexts.Contexts) == 0 {
			return fmt.Errorf("준비 단위에 색인할 컨텍스트가 없다")
		}
		if groups[group.Code] {
			return fmt.Errorf("준비 단위 중복")
		}
		groups[group.Code] = true
		keys[group.Code] = map[string]bool{}
		for _, c := range group.Contexts.Contexts {
			if c.Key == "" || c.Body == "" || keys[group.Code][c.Key] {
				return fmt.Errorf("준비 컨텍스트 오류")
			}
			keys[group.Code][c.Key] = true
		}
		layers := map[string]string{}
		for _, c := range group.Contexts.Contexts {
			layers[c.Key] = c.Layer
		}
		for _, c := range group.Contexts.Contexts {
			if err := validateContextSpec(c, layers); err != nil {
				return fmt.Errorf("준비 컨텍스트 계약 오류")
			}
		}
		data.Report.Contexts += len(group.Contexts.Contexts)
	}
	queries := map[string]bool{}
	for _, q := range data.Evaluation.Queries {
		if queries[q.Code] || !groups[q.Group] || q.Question == "" {
			return fmt.Errorf("준비 질의 매핑 오류")
		}
		queries[q.Code] = true
		units[q.Unit] = true
		for _, k := range q.Evidence {
			if !keys[q.Group][k] {
				return fmt.Errorf("준비 근거 매핑 오류")
			}
		}
		if (q.EvidenceStatus == "annotated") != (len(q.Evidence) > 0) {
			return fmt.Errorf("준비 근거 상태 오류")
		}
	}
	data.Report.Independent = len(units)
	if data.Report.Included+len(data.Report.Exclusions.Items) != data.Report.Exclusions.Total {
		return fmt.Errorf("준비 문항 수 불일치")
	}
	return nil
}

func readPinnedPublic(path string, pin publicDataPin, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("공개 입력 열기 실패")
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maxPublicDataBytes+1))
	if err != nil || size > maxPublicDataBytes {
		return fmt.Errorf("공개 입력 크기/읽기 오류")
	}
	if hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return fmt.Errorf("공개 입력 checksum 불일치")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("공개 입력 되읽기 실패")
	}
	hash.Reset()
	// 파서 오류에 원문 값을 포함하지 않는다.
	if err = json.UnmarshalRead(io.TeeReader(io.LimitReader(file, maxPublicDataBytes+1), hash), out, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("공개 입력 JSON 형식 오류")
	}
	if hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return fmt.Errorf("공개 입력 checksum 불일치")
	}
	return nil
}

func runPrepareData(format, source, output string) error {
	pin, public := publicPin(format)
	if output == "" || (!public && format != "own-supplement") || (public && source == "") || (!public && source != "") {
		return fmt.Errorf("준비 형식·원본·출력 옵션 오류")
	}
	var data preparedData
	var err error
	switch format {
	case "longmemeval":
		var questions []longMemoryQuestion
		if err = readPinnedPublic(source, pin, &questions); err == nil {
			data, err = prepareLongMemory(questions, &pin, 500)
		}
	case "locomo":
		var conversations []locomoConversation
		if err = readPinnedPublic(source, pin, &conversations); err == nil {
			data, err = prepareLocomo(conversations, &pin, 10, 1986)
		}
	case "own-supplement":
		data, err = prepareSupplement()
	}
	if err != nil {
		return err
	}
	if err = data.finish(); err != nil {
		return err
	}
	return writePreparedData(output, data)
}

func writePreparedData(output string, data preparedData) (err error) {
	// 출력 경로를 독점 생성한 뒤에만 정리 소유권을 갖는다. 기존 경로·심볼릭 링크는 거부한다.
	if err = os.Mkdir(output, 0700); err != nil {
		return fmt.Errorf("새 준비 디렉터리 생성 실패")
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(output))
		}
	}()
	for _, item := range []struct {
		name  string
		value any
	}{
		{"memory.json", data.Memory}, {"evaluation-private.json", data.Evaluation}, {"mapping-private.json", data.Mappings},
	} {
		checksum, writeErr := writePreparedFile(filepath.Join(output, item.name), item.value)
		if writeErr != nil {
			return writeErr
		}
		data.Report.Files[item.name] = checksum
	}
	_, err = writePreparedFile(filepath.Join(output, "preparation.json"), data.Report)
	return err
}

func writePreparedFile(path string, value any) (string, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", fmt.Errorf("준비 파일 생성 실패")
	}
	hash := sha256.New()
	encodeErr := json.MarshalWrite(io.MultiWriter(file, hash), value, json.Deterministic(true))
	closeErr := file.Close()
	if encodeErr != nil || closeErr != nil {
		return "", fmt.Errorf("준비 파일 기록 실패")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
