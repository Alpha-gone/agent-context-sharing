package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/ingest"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

type fakeServer struct {
	account, agent, graph model.ID
	saved                 map[int64]model.Context
}

func (s *fakeServer) ValidateIngestCredential(_ context.Context, token string) (model.ID, model.ID, error) {
	if token != "synthetic-token" {
		return model.ID{}, model.ID{}, model.ErrNotFound
	}
	return s.account, s.agent, nil
}
func (s *fakeServer) EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error) {
	return model.GraphGradeEditor, true, nil
}
func (s *fakeServer) Graph(context.Context, model.ID) (model.Graph, error) {
	return model.Graph{ID: s.graph}, nil
}
func (s *fakeServer) CreateIngestSource(_ context.Context, _ model.ID, value model.Context, _ store.WriteLimits, _ time.Duration, _ string) (model.Context, bool, error) {
	idx := *value.Source.TurnIndex
	if old, ok := s.saved[idx]; ok {
		if old.Body != value.Body {
			return model.Context{}, false, store.ErrIngestBodyMismatch
		}
		return old, true, nil
	}
	s.saved[idx] = value
	return value, false, nil
}
func setup(t *testing.T, server string, graph model.ID) (string, string) {
	t.Helper()
	project := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(filepath.Join(project, projectFile), []byte(fmt.Sprintf(`{"enabled":true,"graph_id":%q}`, graph.String())), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INGEST_STATE_DIR", state)
	t.Setenv("INGEST_SERVER_URL", server)
	t.Setenv("INGEST_CREDENTIAL", "synthetic-token")
	t.Setenv("INGEST_CREDENTIAL_FILE", "")
	return project, state
}
func loadState(t *testing.T, dir, project, host, session string) sessionState {
	t.Helper()
	sum := sha256Name(project, host, session)
	var out sessionState
	if err := withState(dir, sum, func(s *sessionState, _ func() error) error { out = *s; return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}
func claudeEvent(project, session, body string) string {
	return fmt.Sprintf(`{"hook_event_name":"UserPromptSubmit","session_id":%q,"cwd":%q,"prompt":%q}`, session, project, body)
}

func testRun(ctx context.Context, args []string, input io.Reader) error {
	return runWithDispatcher(ctx, args, input, func(string, string, string) error { return nil })
}

func TestHookRetryAndPause(t *testing.T) {
	account, _ := model.NewID()
	agent, _ := model.NewID()
	graph, _ := model.NewID()
	backend := &fakeServer{account: account, agent: agent, graph: graph, saved: make(map[int64]model.Context)}
	handler := ingest.Handler{Backend: backend, Gap: time.Minute}
	first := true
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	project, state := setup(t, server.URL, graph)
	body := strings.Repeat("가", 8001)
	if err := testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(project, "session", body))); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	if err := testRun(t.Context(), []string{"flush", "--host", "claude", "--session", "session"}, nil); err == nil {
		t.Fatal("전송 실패 누락")
	}
	before := loadState(t, state, project, "claude", "session")
	if len(before.Pending) != 2 || before.Next != 2 || len(backend.saved) != 2 {
		t.Fatalf("전송 실패 보존: %+v %d", before, len(backend.saved))
	}
	if err := testRun(t.Context(), []string{"pause", "--host", "claude", "--session", "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(project, "session", "비공개 제외"))); err != nil {
		t.Fatal(err)
	}
	paused := loadState(t, state, project, "claude", "session")
	if len(paused.Pending) != 2 || paused.Next != 2 {
		t.Fatal("중지 중 턴 기록")
	}
	if err := testRun(t.Context(), []string{"resume", "--host", "claude", "--session", "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"flush", "--host", "claude", "--session", "session"}, nil); err != nil {
		t.Fatal(err)
	}
	after := loadState(t, state, project, "claude", "session")
	if len(after.Pending) != 0 || len(backend.saved) != 2 || backend.saved[0].Body+backend.saved[1].Body != body {
		t.Fatalf("재전송 불일치: %d %d", len(after.Pending), len(backend.saved))
	}
	for _, saved := range backend.saved {
		if strings.Contains(saved.Body, "비공개 제외") {
			t.Fatal("제외 턴 전송")
		}
	}
}

func TestConcurrentHooksPreserveSequence(t *testing.T) {
	graph, _ := model.NewID()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })
	project, state := setup(t, server.URL, graph)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_ = testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(project, "parallel", "합성")))
		})
	}
	wg.Wait()
	s := loadState(t, state, project, "claude", "parallel")
	if s.Next != 12 || len(s.Pending) != 12 {
		t.Fatalf("동시 수집 손실: %d %d", s.Next, len(s.Pending))
	}
	for i, q := range s.Pending {
		if q.Turn.Index != int64(i) {
			t.Fatal("순번 중복")
		}
	}
}

func TestResponseAndDestinationSafety(t *testing.T) {
	state := sessionState{Pending: []queuedTurn{{Graph: "g", Endpoint: "https://example.test/ingest/conversation", Conversation: "c", Turn: turn{Index: 0, Body: "private"}}}}
	if err := flush(t.Context(), http.DefaultClient, &state, "other", state.Pending[0].Endpoint, "secret", func() error { return nil }); err == nil || len(state.Pending) != 1 {
		t.Fatal("목적지 변경 허용")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"turns":[{"turn_index":9,"status":"stored","context_id":"x"}]}`)
	}))
	defer server.Close()
	state.Pending[0].Endpoint = server.URL
	if err := flush(t.Context(), server.Client(), &state, "g", server.URL, "secret", func() error { return nil }); err == nil || len(state.Pending) != 1 {
		t.Fatal("잘못된 응답에서 삭제")
	}
}

func TestInvalidCredentialFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte("synthetic-token"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INGEST_CREDENTIAL", "")
	t.Setenv("INGEST_CREDENTIAL_FILE", file)
	if _, err := readCredential(); err == nil {
		t.Fatal("공개 자격 파일 허용")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readCredential(); err != nil || got != "synthetic-token" {
		t.Fatal("자격 파일 거부")
	}
	if _, err := parseEndpoint("http://example.test"); err == nil {
		t.Fatal("비보안 전송 허용")
	}
	if _, _, err := findProject(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("미설정 프로젝트: %v", err)
	}
}

func TestCodexHookRejectedTurn(t *testing.T) {
	graph, _ := model.NewID()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("자격 전달")
		}
		_, _ = io.WriteString(w, `{"turns":[{"turn_index":0,"status":"rejected","reason":"limit_exceeded"},{"turn_index":1,"status":"stored","context_id":"synthetic"}]}`)
	}))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })
	project, state := setup(t, server.URL, graph)
	input := fmt.Sprintf(`{"type":"agent-turn-complete","thread-id":"thread","cwd":%q,"input-messages":["입력"],"last-assistant-message":"응답"}`, project)
	if err := testRun(t.Context(), []string{"hook", "codex", input}, nil); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	if err := testRun(t.Context(), []string{"flush", "--host", "codex", "--session", "thread"}, nil); err != nil {
		t.Fatal(err)
	}
	s := loadState(t, state, project, "codex", "thread")
	if s.Next != 2 || len(s.Pending) != 0 || len(s.Rejected) != 1 || s.Rejected[0].Reason != "limit_exceeded" {
		t.Fatalf("거부 처리: %+v", s)
	}
	files, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(state, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "synthetic-token") || strings.Contains(string(data), "입력") || strings.Contains(string(data), "응답") {
			t.Fatal("자격 또는 본문이 처리 기록에 남음")
		}
	}
}

func TestHookErrorDoesNotPrintSecrets(t *testing.T) {
	if os.Getenv("INGEST_TEST_CHILD") == "1" {
		os.Args = []string{"ingest", "hook", "claude"}
		main()
		return
	}
	graph, _ := model.NewID()
	project, _ := setup(t, "https://127.0.0.1:1", graph)
	t.Setenv("INGEST_SERVER_URL", "http://invalid.test")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestHookErrorDoesNotPrintSecrets$")
	cmd.Env = append(os.Environ(), "INGEST_TEST_CHILD=1")
	cmd.Stdin = strings.NewReader(claudeEvent(project, "test", "private-prompt-marker"))
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("실패 상태 누락")
	}
	if strings.Contains(string(output), "private-prompt-marker") || strings.Contains(string(output), "synthetic-token") {
		t.Fatal("민감 정보 출력")
	}
}

func TestDisabledProjectDoesNotCreateQueue(t *testing.T) {
	graph, _ := model.NewID()
	project, stateDir := setup(t, "https://127.0.0.1:1", graph)
	if err := os.WriteFile(filepath.Join(project, projectFile), []byte(`{"enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(project, "disabled", "제외"))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("비활성 프로젝트에 대기 파일 생성")
	}
}

func TestLargeEscapedBatchSplitsByEncodedSize(t *testing.T) {
	const endpoint = "https://example.test/ingest/conversation"
	state := sessionState{}
	for i := range 100 {
		state.Pending = append(state.Pending, queuedTurn{Graph: "g", Endpoint: endpoint, Conversation: "c", Turn: turn{Index: int64(i), Speaker: "user", OccurredAt: time.Now().UTC(), Body: strings.Repeat("\x01", 8000)}})
	}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if len(body) > 4<<20 {
			return nil, fmt.Errorf("oversized request: %d", len(body))
		}
		var batch request
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, err
		}
		items := make([]map[string]any, 0, len(batch.Turns))
		for _, item := range batch.Turns {
			items = append(items, map[string]any{"turn_index": item.Index, "status": "stored", "context_id": "synthetic"})
		}
		raw, err := json.Marshal(map[string]any{"turns": items})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})}
	if err := flush(t.Context(), client, &state, "g", endpoint, "synthetic", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 0 || requests < 2 {
		t.Fatalf("묶음 분할 실패: %d %d", len(state.Pending), requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPauseWhileDeliveryBlocked(t *testing.T) {
	graph, _ := model.NewID()
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"turns":[{"turn_index":0,"status":"stored","context_id":"synthetic"}]}`)
	}))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })
	project, stateDir := setup(t, server.URL, graph)
	t.Chdir(project)
	if err := testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(project, "race", "기존 발화"))); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- testRun(t.Context(), []string{"flush", "--host", "claude", "--session", "race"}, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("전송 시작 지연")
	}
	if err := testRun(t.Context(), []string{"pause", "--host", "claude", "--session", "race"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"hook", "claude"}, strings.NewReader(claudeEvent(project, "race", "제외 발화"))); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"resume", "--host", "claude", "--session", "race"}, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state := loadState(t, stateDir, project, "claude", "race")
	if state.Next != 1 || len(state.Pending) != 0 {
		t.Fatalf("중지 구간 수집 또는 대기: %d %d", state.Next, len(state.Pending))
	}
	if len(state.PauseIntervals) != 1 || state.PauseIntervals[0].End == nil || !state.wasPausedAt(state.PauseIntervals[0].Start) {
		t.Fatal("중지 이력 누락")
	}
}

func TestHookStartsDetachedDelivery(t *testing.T) {
	graph, _ := model.NewID()
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"turns":[{"turn_index":0,"status":"stored","context_id":"synthetic"}]}`)
	}))
	defer server.Close()
	project, stateDir := setup(t, server.URL, graph)
	certFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certFile)
	binary := filepath.Join(t.TempDir(), "ingest")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("수집 클라이언트 빌드: %v %s", err, output)
	}
	command := exec.CommandContext(t.Context(), binary, "hook", "claude")
	command.Dir = project
	command.Stdin = strings.NewReader(claudeEvent(project, "background", "합성 발화"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("훅: %v %s", err, output)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("분리 전송이 시작되지 않았다")
	}
	state := loadState(t, stateDir, project, "claude", "background")
	if len(state.Pending) != 1 {
		t.Fatal("서버 응답 전 대기 항목 손실")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state = loadState(t, stateDir, project, "claude", "background")
		if len(state.Pending) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("분리 전송 완료가 반영되지 않았다")
}

type startedReader struct {
	io.Reader
	started chan struct{}
	once    sync.Once
}

func (r *startedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.Reader.Read(p)
}

func TestPausedHookDelayedUntilAfterResume(t *testing.T) {
	graph, _ := model.NewID()
	project, stateDir := setup(t, "https://example.test", graph)
	name := sha256Name(project, "claude", "delayed")
	started := make(chan struct{})
	done := make(chan error, 1)
	err := withState(stateDir, name, func(state *sessionState, save func() error) error {
		state.Paused = true
		state.PauseIntervals = append(state.PauseIntervals, pauseInterval{Start: time.Now().UTC()})
		if err := save(); err != nil {
			return err
		}
		go func() {
			input := &startedReader{Reader: strings.NewReader(claudeEvent(project, "delayed", "제외 발화")), started: started}
			done <- testRun(t.Context(), []string{"hook", "claude"}, input)
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			return errors.New("훅 시작 지연")
		}
		end := time.Now().UTC()
		state.PauseIntervals[len(state.PauseIntervals)-1].End = &end
		state.Paused = false
		return save()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state := loadState(t, stateDir, project, "claude", "delayed")
	if state.Next != 0 || len(state.Pending) != 0 {
		t.Fatal("재개 뒤 중지 구간 훅이 기록됐다")
	}
}

func TestCodexPauseFailsClosedAfterResume(t *testing.T) {
	graph, _ := model.NewID()
	project, stateDir := setup(t, "https://example.test", graph)
	t.Chdir(project)
	if err := testRun(t.Context(), []string{"pause", "--host", "codex", "--session", "delayed"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := testRun(t.Context(), []string{"resume", "--host", "codex", "--session", "delayed"}, nil); !errors.Is(err, errCodexResume) {
		t.Fatal("Codex 재개 거부 누락", err)
	}
	input := fmt.Sprintf(`{"type":"agent-turn-complete","thread-id":"delayed","cwd":%q,"input-messages":["중지 구간의 지연 알림"],"last-assistant-message":"응답"}`, project)
	if err := testRun(t.Context(), []string{"hook", "codex", input}, nil); err != nil {
		t.Fatal(err)
	}
	state := loadState(t, stateDir, project, "codex", "delayed")
	if state.Next != 0 || len(state.Pending) != 0 {
		t.Fatal("중지 이력이 있는 Codex 세션의 지연 알림 기록")
	}
}

func TestPauseKeepsEarlierHook(t *testing.T) {
	start := time.Now().UTC()
	state := sessionState{Paused: true, PauseIntervals: []pauseInterval{{Start: start}}}
	if state.wasPausedAt(start.Add(-time.Second)) {
		t.Fatal("중지 전에 시작한 훅을 제외했다")
	}
	if !state.wasPausedAt(start.Add(time.Nanosecond)) {
		t.Fatal("중지 중 훅을 허용했다")
	}
}
