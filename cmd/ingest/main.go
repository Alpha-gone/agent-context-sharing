// Command ingest는 문서화된 호스트 훅의 입력을 로컬에 보관하고 수집 경로로 전송한다.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
)

const projectFile = ".agent-context-ingest.json"
const maxInput = 4 << 20

var errCodexResume = errors.New("codex session cannot resume")

type projectConfig struct {
	Enabled bool   `json:"enabled"`
	GraphID string `json:"graph_id"`
}

type hookInput struct {
	Type          string   `json:"type"`
	Event         string   `json:"hook_event_name"`
	Session       string   `json:"session_id"`
	Thread        string   `json:"thread-id"`
	CWD           string   `json:"cwd"`
	Prompt        string   `json:"prompt"`
	Assistant     string   `json:"last_assistant_message"`
	Inputs        []string `json:"input-messages"`
	LastAssistant string   `json:"last-assistant-message"`
}

type message struct{ speaker, body string }

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin); err != nil {
		// 오류의 원문은 서버 응답, 경로 또는 사용자 데이터를 포함할 수 있다.
		if errors.Is(err, errCodexResume) {
			fmt.Fprintln(os.Stderr, "중지한 Codex 세션은 재개할 수 없습니다. 새 세션을 사용하십시오.")
		} else {
			fmt.Fprintln(os.Stderr, "대화 수집 처리 실패: 설정과 대기 상태를 확인하십시오")
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader) error {
	return runWithDispatcher(ctx, args, input, startDelivery)
}

func startDelivery(project, host, session string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	process, err := os.StartProcess(executable, []string{executable, "flush", "--host", host, "--session", session}, &os.ProcAttr{Dir: project, Files: []*os.File{null, null, null}, Sys: &syscall.SysProcAttr{Setsid: true}})
	if err != nil {
		return err
	}
	return process.Release()
}

func runWithDispatcher(ctx context.Context, args []string, input io.Reader, dispatch func(string, string, string) error) error {
	started := time.Now().UTC()
	if len(args) < 2 {
		return errors.New("command")
	}
	command := args[0]
	var host, session, cwd string
	var messages []message
	if command == "hook" {
		host = args[1]
		var raw []byte
		var err error
		switch host {
		case "claude":
			if len(args) != 2 {
				return errors.New("arguments")
			}
			raw, err = io.ReadAll(io.LimitReader(input, maxInput+1))
		case "codex":
			if len(args) != 3 {
				return errors.New("arguments")
			}
			raw = []byte(args[2])
		default:
			return errors.New("host")
		}
		if err != nil || len(raw) > maxInput {
			return errors.New("input")
		}
		var event hookInput
		if err := json.Unmarshal(raw, &event); err != nil {
			return errors.New("input")
		}
		cwd = event.CWD
		if host == "claude" {
			session = event.Session
			switch event.Event {
			case "UserPromptSubmit":
				messages = append(messages, message{"user", event.Prompt})
			case "Stop":
				messages = append(messages, message{"agent", event.Assistant})
			default:
				return nil
			}
		} else {
			if event.Type != "agent-turn-complete" {
				return nil
			}
			session = event.Thread
			for _, body := range event.Inputs {
				messages = append(messages, message{"user", body})
			}
			messages = append(messages, message{"agent", event.LastAssistant})
		}
	} else {
		if command != "pause" && command != "resume" && command != "flush" {
			return errors.New("command")
		}
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&host, "host", "", "")
		flags.StringVar(&session, "session", "", "")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("arguments")
		}
		cwd, _ = os.Getwd()
	}
	if (host != "claude" && host != "codex") || session == "" || !utf8.ValidString(session) || strings.ContainsRune(session, 0) || utf8.RuneCountInString(host+":"+session) > 128 || cwd == "" {
		return errors.New("session")
	}
	project, cfg, err := findProject(cwd)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !cfg.Enabled || cfg.GraphID == "" {
		return nil
	}
	if _, err := model.ParseID(cfg.GraphID); err != nil {
		return errors.New("graph")
	}
	stateDir := os.Getenv("INGEST_STATE_DIR")
	if stateDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		stateDir = filepath.Join(base, "agent-context", "ingest")
	}
	name := sha256Name(project, host, session)
	queued := false
	err = withState(stateDir, name, func(state *sessionState, save func() error) error {
		now := time.Now().UTC()
		switch command {
		case "pause":
			if !state.Paused {
				state.PauseIntervals = append(state.PauseIntervals, pauseInterval{Start: now})
				state.Paused = true
			}
			return save()
		case "resume":
			if host == "codex" && (state.Paused || len(state.PauseIntervals) > 0) {
				return errCodexResume
			}
			if state.Paused {
				if len(state.PauseIntervals) == 0 || state.PauseIntervals[len(state.PauseIntervals)-1].End != nil {
					state.PauseIntervals = append(state.PauseIntervals, pauseInterval{Start: time.Time{}})
				}
				if now.Before(state.PauseIntervals[len(state.PauseIntervals)-1].Start) {
					state.PauseIntervals[len(state.PauseIntervals)-1].Start = time.Time{}
				}
				state.PauseIntervals[len(state.PauseIntervals)-1].End = new(now)
				state.Paused = false
			}
			queued = len(state.Pending) > 0
			return save()
		}
		if command != "hook" || state.wasPausedAt(started) || (host == "codex" && len(state.PauseIntervals) > 0) {
			return nil
		}
		endpoint, err := parseEndpoint(os.Getenv("INGEST_SERVER_URL"))
		if err != nil {
			return err
		}
		conversation := host + ":" + name
		for _, m := range messages {
			if !utf8.ValidString(m.body) || strings.ContainsRune(m.body, 0) {
				return errors.New("body")
			}
			runes := []rune(m.body)
			for len(runes) > 0 {
				n := min(8000, len(runes))
				state.Pending = append(state.Pending, queuedTurn{Graph: cfg.GraphID, Endpoint: endpoint, Conversation: conversation, Turn: turn{Index: state.Next, Speaker: m.speaker, OccurredAt: started, Body: string(runes[:n])}})
				state.Next++
				runes = runes[n:]
			}
		}
		if err := save(); err != nil {
			return err
		}
		queued = len(state.Pending) > 0
		return nil
	})
	if err != nil {
		return err
	}
	if (command == "hook" || command == "resume") && queued {
		return dispatch(project, host, session)
	}
	if command == "flush" {
		return deliver(ctx, stateDir, name, cfg.GraphID)
	}
	return nil

}

func findProject(cwd string) (string, projectConfig, error) {
	var cfg projectConfig
	path, err := filepath.Abs(cwd)
	if err != nil {
		return "", cfg, err
	}
	for {
		data, err := os.ReadFile(filepath.Join(path, projectFile))
		if err == nil {
			if len(data) > 4096 {
				return "", cfg, errors.New("project")
			}
			if err := json.Unmarshal(data, &cfg, json.RejectUnknownMembers(true)); err != nil {
				return "", cfg, errors.New("project")
			}
			return path, cfg, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", cfg, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", cfg, os.ErrNotExist
		}
		path = parent
	}
}

func parseEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("server")
	}
	u.Path = "/ingest/conversation"
	return u.String(), nil
}

func readCredential() (string, error) {
	value := os.Getenv("INGEST_CREDENTIAL")
	file := os.Getenv("INGEST_CREDENTIAL_FILE")
	if value != "" && file != "" {
		return "", errors.New("credential")
	}
	if file != "" {
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
			return "", errors.New("credential")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", errors.New("credential")
		}
		value = strings.TrimSpace(string(data))
	}
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("credential")
	}
	return value, nil
}

func sha256Name(project, host, session string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + host + "\x00" + session))
	return hex.EncodeToString(sum[:])
}
