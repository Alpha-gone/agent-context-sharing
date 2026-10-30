package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPStoredFieldErrorIsInternalIntegration(t *testing.T) {
	for _, tool := range []string{"node_update", "node_discard", "node_restore"} {
		t.Run(tool, func(t *testing.T) {
			f := newMCPWriteFixture(t)
			args := f.args(tool)
			args["context_id"] = f.derived.String()
			if tool == "node_update" {
				args["expected_version"], args["body"] = 1, "수정한 본문"
			}
			if tool == "node_restore" {
				f.call(t, "node_discard", args, "")
			}
			args["request_id"] = "stored_decode_request_148"
			var original string
			if err := f.pool.QueryRow(t.Context(), `SELECT properties::text FROM agent_context."Context"
				WHERE properties ->> 'context_id'::text=$1`, f.derived.String()).Scan(&original); err != nil {
				t.Fatal(err)
			}
			var properties map[string]any
			if err := json.Unmarshal([]byte(original), &properties); err != nil {
				t.Fatal(err)
			}
			properties["version"] = 0 // JSON 해석은 성공하고 저장 값의 모델 검증이 실패해야 한다.
			broken, err := json.Marshal(properties)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(t.Context(), `UPDATE agent_context."Context" SET properties=$2::ag_catalog.agtype
				WHERE properties ->> 'context_id'::text=$1`, f.derived.String(), string(broken)); err != nil {
				t.Fatal(err)
			}
			before := mcpWriteFingerprint(t, f.pool)
			value := f.call(t, tool, args, "internal")
			if len(value) != 2 || value["field"] != nil {
				t.Fatalf("저장 값의 검증 필드 노출: %+v", value)
			}
			if before != mcpWriteFingerprint(t, f.pool) {
				t.Fatal("저장 해석 장애가 업무·거부 감사·재생 결과·한도를 커밋했다")
			}
			_, decodeErr := f.database.Context(t.Context(), f.graph, f.derived)
			_, stored := errors.AsType[*store.StorageError](decodeErr)
			field, invalid := errors.AsType[model.FieldError](decodeErr)
			if !stored || !invalid || field.Field != "version" {
				t.Fatalf("모델 필드 오류의 저장 장애 표식: %v", decodeErr)
			}
			var replayCount int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.idempotency_record WHERE idempotency_key=$1`, args["request_id"]).Scan(&replayCount); err != nil || replayCount != 0 {
				t.Fatalf("내부 장애의 재생 기록=%d: %v", replayCount, err)
			}
			// 저장 값만 복구한 뒤 같은 요청 키가 새 실행과 정상 재생에 사용돼야 한다.
			if _, err := f.pool.Exec(t.Context(), `UPDATE agent_context."Context" SET properties=$2::ag_catalog.agtype
				WHERE properties ->> 'context_id'::text=$1`, f.derived.String(), original); err != nil {
				t.Fatal(err)
			}
			first, err := callMCPWrite(t.Context(), f.handlers, f.principal, tool, args)
			writeContent(t, first, err, "")
			after := mcpWriteFingerprint(t, f.pool)
			replay, err := callMCPWrite(t.Context(), f.other, f.principal, tool, args)
			writeContent(t, replay, err, "")
			if first.Content[0].(*sdk.TextContent).Text != replay.Content[0].(*sdk.TextContent).Text || after != mcpWriteFingerprint(t, f.pool) {
				t.Fatal("복구 뒤 같은 키의 재생이 결과·업무·감사를 중복 적용했다")
			}
		})
	}
}

func TestMCPInputFieldErrorRemainsReplayableIntegration(t *testing.T) {
	f := newMCPWriteFixture(t)
	args := f.args("node_update")
	args["context_id"], args["expected_version"], args["request_id"] = f.derived.String(), 1, "input_field_request_148"
	args["valid_from"], args["valid_to"] = "2026-10-29T12:00:00Z", "2026-10-28T12:00:00Z"
	before := mcpWriteFingerprint(t, f.pool)
	value := f.call(t, "node_update", args, "invalid_argument")
	if value["field"] != "valid_to" {
		t.Fatalf("실제 입력 필드 오류=%+v", value)
	}
	after := mcpWriteFingerprint(t, f.pool)
	if before == after {
		t.Fatal("결정적 입력 거부의 감사·재생 결과가 없다")
	}
	var rejected, replays int
	if err := f.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM public.operation_log WHERE context_id=$1 AND result='rejected'),
		(SELECT count(*) FROM public.idempotency_record WHERE idempotency_key=$2)`, f.derived.String(), args["request_id"]).Scan(&rejected, &replays); err != nil || rejected != 1 || replays != 1 {
		t.Fatalf("입력 거부 감사=%d/재생=%d: %v", rejected, replays, err)
	}
	r, err := callMCPWrite(t.Context(), f.other, f.principal, "node_update", args)
	replayed := writeContent(t, r, err, "invalid_argument")
	firstJSON, _ := json.Marshal(value, json.Deterministic(true))
	replayJSON, _ := json.Marshal(replayed, json.Deterministic(true))
	if !bytes.Equal(firstJSON, replayJSON) || after != mcpWriteFingerprint(t, f.pool) {
		t.Fatal("결정적 입력 거부의 재생 결과·감사가 바뀌었다")
	}
}
