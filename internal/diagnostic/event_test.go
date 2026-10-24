package diagnostic

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"
)

type eventSink struct {
	events []Event
	accept bool
}

func (sink *eventSink) Emit(_ context.Context, event Event) bool {
	sink.events = append(sink.events, event)
	return sink.accept
}

func TestReporterKindsAndIndependentLevel(t *testing.T) {
	for _, kind := range []Kind{Request, Auth, Search, Index, Management, System} {
		t.Run(string(kind), func(t *testing.T) {
			sink := &eventSink{accept: true}
			reporter, err := NewReporter(kind, sink, testSource(t))
			if err != nil {
				t.Fatal(err)
			}
			if !reporter.RecordDiagnostic(testContext(t), "ERROR", "write", "rolled_back", "constraint_violation") {
				t.Fatal("유효한 이벤트가 거부됐다")
			}
			event := sink.events[0]
			if event.Kind != kind || event.Level != "ERROR" || event.Time.Location() != time.UTC || event.Validate() != nil {
				t.Fatalf("종류·수준·UTC 보존: %+v", event)
			}
		})
	}
}

func TestReporterRejectsUnstructuredValuesAndReportsDrop(t *testing.T) {
	sink := &eventSink{}
	reporter, err := NewReporter(Request, sink, testSource(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "https://example.test/?token=secret", "database error: secret", "body\ntext", "자격", string(make([]byte, 65))} {
		if reporter.RecordDiagnostic(testContext(t), "WARN", "validation", "failed", value) {
			t.Fatalf("임의 문자열을 수락했다: %q", value)
		}
	}
	if len(sink.events) != 0 {
		t.Fatal("거부된 이벤트가 수신기에 전달됐다")
	}
	if reporter.RecordDiagnostic(testContext(t), "WARN", "validation", "failed", "invalid_argument") || len(sink.events) != 1 {
		t.Fatal("수신기 누락 결과를 보존하지 않았다")
	}
	if _, err := NewReporter("other", sink, testSource(t)); err == nil {
		t.Fatal("미지원 종류를 수락했다")
	}
	if _, err := NewReporter(Request, nil, testSource(t)); err == nil {
		t.Fatal("수신기 누락을 수락했다")
	}
}

func TestEventRequiresUTCAndKnownLevel(t *testing.T) {
	event := validEvent(t, Management)
	event.Stage, event.Result, event.ReasonCode = "commit", "unknown", "commit_unknown"
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	event.Time = event.Time.In(time.FixedZone("UTC offset", 0))
	if event.Validate() == nil {
		t.Fatal("UTC 외의 Location을 허용했다")
	}
	event.Time = time.Now().UTC()
	event.Level = "FATAL"
	if event.Validate() == nil {
		t.Fatal("미지원 수준을 허용했다")
	}
}

func testSource(t *testing.T) Source {
	t.Helper()
	source, err := NewSource("test_slot")
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	return WithTrace(t.Context(), NewRequestTrace())
}

func validEvent(t *testing.T, kind Kind) Event {
	t.Helper()
	source := testSource(t)
	return Event{Time: time.Now().UTC(), Kind: kind, Level: "INFO", InstanceID: source.InstanceID(), BootID: source.BootID(), CorrelationID: NewRequestTrace().CorrelationID(), Stage: "write", Result: "completed", ReasonCode: "completed"}
}

func TestOBS01KindsFieldsAndIdentity(t *testing.T) {
	id := uuid.NewV7().String()
	cases := []struct {
		kind   Kind
		fields Fields
	}{
		{Request, Fields{Identity: IdentityFields{AccountID: id, GraphID: id}, Request: RequestFields{Route: "mcp", Method: "POST", Tool: "node_create", Status: 200, RPCCode: -32602, ValidationField: "body", LatencyMS: 3}}},
		{Auth, Fields{Identity: IdentityFields{AccountID: id, ClientID: "codex-cli", GrantID: id}, Auth: AuthFields{ValidationField: "password"}}},
		{Search, Fields{Identity: IdentityFields{GraphID: id}, Search: SearchFields{Channel: "time", Path: "direct", SelectionReason: "event_anchor", Candidates: 2, Nodes: 1, Edges: 1, Characters: 10, LatencyMS: 3, Truncated: true}}},
		{Index, Fields{Identity: IdentityFields{GraphID: id, ContextID: id}, Index: IndexFields{TaskID: id, Generation: 2, Attempts: 1, LeaseResult: "acquired", ProviderStatus: 200, LatencyMS: 10}}},
		{Management, Fields{Identity: IdentityFields{AccountID: id, GraphID: id, ContextID: id, RelationID: id}, Management: ManagementFields{Operation: "relation_confirm", ValidationField: "relation_type", RelationType: "causes", ExpectedLayer: "event", ActualLayer: "source", CommitState: "rolled_back"}}},
		{System, Fields{System: SystemFields{Job: "migration", MigrationNumber: 8, ConfigurationVersion: 1, Processed: 4}}},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			event := validEvent(t, tc.kind)
			event.Fields, event.IdentityVerified = tc.fields, true
			if err := event.Validate(); err != nil {
				t.Fatal(err)
			}
			name, err := tc.kind.Filename()
			if err != nil || name != string(tc.kind)+".jsonl" {
				t.Fatal("종류와 파일 대응")
			}
			for _, other := range []Kind{Request, Auth, Search, Index, Management, System} {
				if other == tc.kind {
					continue
				}
				event.Kind = other
				if event.Validate() == nil {
					t.Fatalf("%s 필드를 %s에서 수락했다", tc.kind, other)
				}
			}
		})
	}
	for _, field := range []IdentityFields{{AccountID: id}, {GraphID: id}, {ContextID: id}, {RelationID: id}, {ClientID: "codex-cli"}, {GrantID: id}} {
		event := validEvent(t, Auth)
		event.Fields.Identity = field
		if event.Validate() == nil {
			t.Fatal("미확인 ID를 수락했다")
		}
	}
}

func TestOBS01RejectsSensitiveValuesBeforeSink(t *testing.T) {
	for _, kind := range []Kind{Request, Auth, Search, Index, Management, System} {
		t.Run(string(kind), func(t *testing.T) {
			for _, secret := range []string{"secret", "token_secret", "0123456789abcdef", "https://example.test/?token=secret", "Bearer secret", "body\nsecret", strings.Repeat("x", 17000)} {
				sink := &eventSink{accept: true}
				reporter, err := NewReporter(kind, sink, testSource(t))
				if err != nil {
					t.Fatal(err)
				}
				for _, codes := range [][3]string{{secret, "failed", "invalid_argument"}, {"validation", secret, "invalid_argument"}, {"validation", "failed", secret}} {
					if reporter.RecordDiagnostic(testContext(t), "WARN", codes[0], codes[1], codes[2]) {
						t.Fatal("임의 코드 수락")
					}
				}
				if len(sink.events) != 0 {
					t.Fatal("민감정보가 수신기에 전달됐다")
				}
			}
		})
	}
	for _, mutate := range []struct {
		name   string
		change func(*Event)
	}{
		{"route_uri", func(e *Event) { e.Fields.Request.Route = "/graphs/secret?token=secret" }},
		{"method", func(e *Event) { e.Fields.Request.Method = "secret" }},
		{"tool", func(e *Event) { e.Fields.Request.Tool = "secret" }},
		{"field", func(e *Event) { e.Fields.Request.ValidationField = "secret" }},
		{"status", func(e *Event) { e.Fields.Request.Status = 600 }},
		{"rpc", func(e *Event) { e.Fields.Request.RPCCode = 1234 }},
		{"id_secret", func(e *Event) { e.IdentityVerified = true; e.Fields.Identity.AccountID = "secret" }},
		{"id_v4", func(e *Event) { e.IdentityVerified = true; e.Fields.Identity.AccountID = uuid.NewV4().String() }},
		{"flow_unverified", func(e *Event) { e.FlowID = uuid.NewV7().String() }},
		{"missing_trace", func(e *Event) { e.CorrelationID = "" }},
		{"request_job_only", func(e *Event) { e.CorrelationID = ""; e.JobID = uuid.NewV7().String() }},
		{"boot_v4", func(e *Event) { e.BootID = uuid.NewV4().String() }},
		{"instance_path", func(e *Event) { e.InstanceID = "../secret" }},
		{"date_out_of_range", func(e *Event) { e.Time = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			e := validEvent(t, Request)
			mutate.change(&e)
			if e.Validate() == nil {
				t.Fatal("잘못된 이벤트 수락")
			}
		})
	}
}

func TestOBS01FlowBootAndJSON(t *testing.T) {
	source := testSource(t)
	restarted := testSource(t)
	if source.InstanceID() != restarted.InstanceID() || source.BootID() == restarted.BootID() {
		t.Fatal("재기동 구분")
	}
	request := NewRequestTrace().WithTestRun(NewTestRun())
	flow := uuid.NewV7().String()
	trace, err := request.WithVerifiedFlow(flow)
	if err != nil {
		t.Fatal(err)
	}
	job := trace.NewJob()
	if job.JobID() == "" || job.CorrelationID() != request.CorrelationID() || job.TestRunID() != request.TestRunID() {
		t.Fatal("비동기 연결 손실")
	}
	if NewRequestTrace().CorrelationID() == request.CorrelationID() || job.NewJob().JobID() == job.JobID() {
		t.Fatal("회차 중복")
	}
	sink := &eventSink{accept: true}
	for _, kind := range []Kind{Request, Auth, Search, Index, Management, System} {
		r, err := NewReporter(kind, sink, source)
		if err != nil {
			t.Fatal(err)
		}
		if !r.RecordFields(WithTrace(t.Context(), job), "INFO", "write", "completed", "completed", true, Fields{}) {
			t.Fatal("흐름 전달 실패")
		}
	}
	for _, event := range sink.events {
		if event.BootID != source.BootID() || event.CorrelationID != request.CorrelationID() || event.FlowID != flow || event.JobID != job.JobID() || event.TestRunID != request.TestRunID() {
			t.Fatal("종류 사이 흐름 손실")
		}
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var restored Event
		if err := json.Unmarshal(data, &restored); err != nil || restored != event {
			t.Fatalf("JSON 왕복: %v", err)
		}
		if strings.Contains(string(data), "\n") || strings.Contains(string(data), `"fields"`) {
			t.Fatal("외피 인코딩")
		}
	}
	r, _ := NewReporter(System, sink, source)
	if !r.RecordDiagnostic(WithTrace(t.Context(), Trace{}.NewJob()), "INFO", "startup", "completed", "completed") {
		t.Fatal("요청 없는 작업 거부")
	}
	last := sink.events[len(sink.events)-1]
	data, _ := json.Marshal(last)
	if strings.Contains(string(data), `"correlation_id"`) || strings.Contains(string(data), `"flow_id"`) || !strings.Contains(string(data), `"identity_verified":false`) {
		t.Fatal("부재/미확인 표시")
	}
	if r.RecordDiagnostic(t.Context(), "INFO", "startup", "completed", "completed") {
		t.Fatal("상관 없는 이벤트 수락")
	}
}

func TestOBS01NarrowValueBoundary(t *testing.T) {
	var check func(reflect.Type)
	check = func(typ reflect.Type) {
		if typ == reflect.TypeFor[time.Time]() {
			return
		}
		switch typ.Kind() {
		case reflect.Struct:
			for index := range typ.NumField() {
				check(typ.Field(index).Type)
			}
		case reflect.String, reflect.Bool, reflect.Uint16, reflect.Uint64, reflect.Int32:
		default:
			t.Fatalf("임의 객체를 담는 타입: %s", typ)
		}
	}
	check(reflect.TypeFor[Event]())
	for _, bad := range []string{"", ".", "..", "a/b", "a\\b", "secret\n", strings.Repeat("x", 65)} {
		if _, err := NewSource(bad); err == nil {
			t.Fatal("잘못된 instance 수락")
		}
		if _, err := Kind(bad).Filename(); err == nil {
			t.Fatal("잘못된 basename 수락")
		}
	}
	if _, err := NewReporter(Request, &eventSink{}, Source{}); err == nil {
		t.Fatal("미설정 Source 수락")
	}
	if _, err := NewRequestTrace().WithVerifiedFlow("token_hash"); err == nil {
		t.Fatal("자격 추적 키 수락")
	}
}

func TestOBS01RejectsUnknownOptionalCodes(t *testing.T) {
	for _, tc := range []struct {
		kind  Kind
		group string
	}{
		{Request, "Request"}, {Auth, "Auth"}, {Search, "Search"}, {Index, "Index"}, {Management, "Management"}, {System, "System"},
	} {
		group := reflect.TypeFor[Fields]().FieldByName
		field, ok := group(tc.group)
		if !ok {
			t.Fatal(tc.group)
		}
		for index := range field.Type.NumField() {
			member := field.Type.Field(index)
			if member.Type.Kind() != reflect.String {
				continue
			}
			t.Run(tc.group+"/"+member.Name, func(t *testing.T) {
				event := validEvent(t, tc.kind)
				event.IdentityVerified = true
				reflect.ValueOf(&event.Fields).Elem().FieldByName(tc.group).FieldByName(member.Name).SetString("secret_input")
				if event.Validate() == nil {
					t.Fatal("임의 선택 문자열 수락")
				}
			})
		}
	}
}
