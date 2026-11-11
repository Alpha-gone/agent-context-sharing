package relinfer

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"agent_context_sharing/internal/diagnostic"
	"agent_context_sharing/internal/model"
)

// RuntimeStore는 조립에 필요한 큐·후보·반영 경계를 합친다.
type RuntimeStore interface {
	Queue
	CandidateSource
	ResultSink
}

// RuntimeConfig는 실행기에서 검증한 관계 추론 구성이다.
type RuntimeConfig struct {
	Mode, Provider, Model, APIKey, BaseURL, EmbeddingModel, AcceptedProfile string
	Timeout, IndexWait, AdjacencyWindow                                     time.Duration
	CandidateLimit, TargetChars, CandidateChars, InputChars                 int
	AutoConfirmTypes                                                        []model.RelationType
}

// NewRuntime은 off에서는 제공자를 만들지 않고, 활성 모드에서는 작업자를 조립한다.
func NewRuntime(store RuntimeStore, cfg RuntimeConfig, client *http.Client, logger *slog.Logger) (*Worker, string, error) {
	if cfg.Mode == "" || cfg.Mode == "off" {
		return nil, "", nil
	}
	if store == nil || !slices.Contains([]string{"shadow", "propose", "apply"}, cfg.Mode) {
		return nil, "", fmt.Errorf("관계 추론 실행 구성이 올바르지 않다")
	}
	if cfg.AcceptedProfile != "" {
		value, err := hex.DecodeString(cfg.AcceptedProfile)
		if err != nil || len(value) != 32 || hex.EncodeToString(value) != cfg.AcceptedProfile {
			return nil, "", fmt.Errorf("승인된 추론 지문이 올바르지 않다")
		}
	}
	for _, kind := range cfg.AutoConfirmTypes {
		if kind != model.RelationTypeRelatesTo && kind != model.RelationTypePartOf {
			return nil, "", fmt.Errorf("자동 확정 유형이 올바르지 않다")
		}
	}
	if client == nil {
		client = &http.Client{}
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if transport, ok := copied.Transport.(*http.Transport); ok {
		if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
			return nil, "", fmt.Errorf("추론 제공자 TLS 검증을 비활성화할 수 없다")
		}
		copied.Transport = transport.Clone()
	} else if copied.Transport == nil {
		copied.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	judge, err := NewOpenAIJudge(&copied, cfg.BaseURL, cfg.APIKey, cfg.Model)
	if err != nil {
		return nil, "", err
	}
	profile := Profile{Provider: cfg.Provider, Model: cfg.Model, PromptVersion: "1", SchemaVersion: "1", EmbeddingModel: cfg.EmbeddingModel, IndexWait: cfg.IndexWait, AdjacencyWindow: cfg.AdjacencyWindow, Channels: []string{"semantic", "keyword", "member", "rule", "conversation"}, CandidateLimit: cfg.CandidateLimit, TargetChars: cfg.TargetChars, CandidateChars: cfg.CandidateChars, InputChars: cfg.InputChars}
	fingerprint, err := profile.Fingerprint()
	if err != nil {
		return nil, "", err
	}
	if err = judge.ValidateProfile(profile); err != nil {
		return nil, "", err
	}
	processor := &DecisionProcessor{Builder: InputBuilder{Source: store, Profile: profile}, Judge: judge, Sink: store, Mode: cfg.Mode, AcceptedProfile: cfg.AcceptedProfile, AutoConfirmTypes: slices.Clone(cfg.AutoConfirmTypes)}
	worker, err := New(store, processor, cfg.Timeout)
	if err != nil {
		return nil, "", err
	}
	worker.client = &copied
	if logger != nil {
		worker.logger = logger
	}
	return worker, fingerprint, nil
}

// SetDiagnostics는 작업 시작 전에 색인 진단 수신기를 주입한다.
func (w *Worker) SetDiagnostics(observer diagnostic.Observer) {
	w.diagnostics = observer
	if processor, ok := w.processor.(*DecisionProcessor); ok {
		processor.diagnostics = observer
	}
}

// Start는 독립된 작업 소비를 시작한다.
func (w *Worker) Start(parent context.Context) {
	w.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		w.cancel = cancel
		go func() { defer close(w.done); w.consume(diagnostic.ResumeJob(ctx, "")) }()
	})
}

func (w *Worker) consume(ctx context.Context) {
	interval := time.Second
	nextProgress := time.Time{}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for ctx.Err() == nil {
		if time.Now().After(nextProgress) {
			if source, ok := w.queue.(interface {
				RelationInferenceQueueCounts(context.Context) (int64, int64, error)
			}); ok {
				pending, failed, err := source.RelationInferenceQueueCounts(ctx)
				if err == nil && pending >= 0 && failed >= 0 {
					diagnostic.Observe(w.diagnostics, ctx, "INFO", "relation_inference_progress", "completed", "completed", false, diagnostic.Fields{Index: diagnostic.IndexFields{Pending: uint64(pending), Failed: uint64(failed), QueueCountsMeasured: true}})
				}
			}
			nextProgress = time.Now().Add(time.Minute)
		}
		processed, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && w.logger != nil {
			w.logger.ErrorContext(ctx, "관계 추론 작업 처리 실패", "reason_code", "relation_inference_cycle_failed")
		}
		if processed && err == nil {
			interval = 0
			continue
		}
		if processed {
			interval = time.Second
		} else {
			interval = max(time.Second, min(interval*2, 30*time.Second))
		}
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// Close는 회차를 취소하고 작업자와 전용 HTTP 연결 정리를 기다린다.
func (w *Worker) Close(ctx context.Context) error {
	w.startOnce.Do(func() { close(w.done) })
	w.stopOnce.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
	})
	select {
	case <-w.done:
		if w.client != nil {
			if transport, ok := w.client.Transport.(*http.Transport); ok {
				transport.CloseIdleConnections()
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
