package main

import (
	"bytes"
	"encoding/json/v2"
	"testing"
)

func TestMCPDiscardedEventMemberRejectionIntegration(t *testing.T) {
	for _, change := range []string{"create", "members", "body", "time"} {
		t.Run(change, func(t *testing.T) {
			f := newMCPWriteFixture(t)
			discard := f.args("node_discard")
			discard["context_id"] = f.source.String()
			f.call(t, "node_discard", discard, "")
			before, err := f.database.Context(t.Context(), f.graph, f.firstEvent)
			if err != nil || before.DeletedAt != nil {
				t.Fatalf("구성원 폐기 후 사건은 활성 상태여야 한다: %+v, %v", before, err)
			}
			tool := "node_update"
			args := f.args(tool)
			args["context_id"], args["expected_version"] = f.firstEvent.String(), before.Version
			switch change {
			case "create":
				tool = "node_create"
				args = f.args(tool)
				args["layer"], args["body"] = "event", "폐기된 구성원 사건"
				args["member_refs"] = []string{f.source.String(), f.derived.String()}
				args["event_start"], args["event_end"] = "2026-10-20T11:00:00Z", "2026-10-20T13:00:00Z"
			case "members":
				args["member_refs"] = []string{f.source.String(), f.derived.String()}
			case "body":
				args["body"] = "구성원을 생략한 본문 수정"
			case "time":
				args["event_end"] = "2026-10-20T14:00:00Z"
			}
			args["request_id"], args["judgment_input"] = "discarded_event_member_173", "폐기된 사건 구성원 거부"
			var nodesBefore int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_context."Context"`).Scan(&nodesBefore); err != nil {
				t.Fatal(err)
			}
			value := f.call(t, tool, args, "invalid_argument")
			if value["field"] != "member_refs" || value["retryable"] == true {
				t.Fatalf("사건 구성원 입력 오류=%+v", value)
			}
			var rejected, replays, nodesAfter int
			if err := f.pool.QueryRow(t.Context(), `SELECT
				(SELECT count(*) FROM public.operation_log WHERE graph_id=$1 AND judgment_input=$2 AND result='rejected' AND reject_reason='invalid_argument'),
				(SELECT count(*) FROM public.idempotency_record WHERE idempotency_key=$3),
				(SELECT count(*) FROM agent_context."Context")`, f.graph.String(), args["judgment_input"], args["request_id"]).Scan(&rejected, &replays, &nodesAfter); err != nil || rejected != 1 || replays != 1 || nodesAfter != nodesBefore {
				t.Fatalf("거부=%d/재생=%d/노드=%d→%d: %v", rejected, replays, nodesBefore, nodesAfter, err)
			}
			after, err := f.database.Context(t.Context(), f.graph, f.firstEvent)
			if err != nil {
				t.Fatal(err)
			}
			beforeJSON, _ := json.Marshal(before, json.Deterministic(true))
			afterJSON, _ := json.Marshal(after, json.Deterministic(true))
			if !bytes.Equal(beforeJSON, afterJSON) {
				t.Fatal("거부된 쓰기가 기존 사건을 변경했다")
			}
			// 참조를 고쳐도 이전 요청 키는 저장된 거부 결과를 재생해야 한다.
			repair := f.args("node_update")
			repair["context_id"], repair["expected_version"], repair["member_refs"] = f.firstEvent.String(), before.Version, []string{f.derived.String()}
			f.call(t, "node_update", repair, "")
			fingerprint := mcpWriteFingerprint(t, f.pool)
			r, err := callMCPWrite(t.Context(), f.other, f.principal, tool, args)
			replayed := writeContent(t, r, err, "invalid_argument")
			firstJSON, _ := json.Marshal(value, json.Deterministic(true))
			replayJSON, _ := json.Marshal(replayed, json.Deterministic(true))
			if !bytes.Equal(firstJSON, replayJSON) || fingerprint != mcpWriteFingerprint(t, f.pool) {
				t.Fatal("거부 재생이 결과를 바꾸거나 업무·감사·재생 기록을 중복 적용했다")
			}
		})
	}
}
