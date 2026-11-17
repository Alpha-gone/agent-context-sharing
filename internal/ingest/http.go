// Package ingest는 수집 자격으로 인증한 대화 턴 요청을 처리한다.
package ingest

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

type Backend interface {
	ValidateIngestCredential(context.Context, string) (model.ID, model.ID, error)
	EffectiveGrade(context.Context, model.ID, model.ID) (model.GraphGrade, bool, error)
	Graph(context.Context, model.ID) (model.Graph, error)
	CreateIngestSource(context.Context, model.ID, model.Context, store.WriteLimits, time.Duration, string) (model.Context, bool, error)
}
type Handler struct {
	Backend       Backend
	Plans         plan.AccountPlans
	Gap           time.Duration
	CorrelationID func(context.Context) string
}
type requestBody struct {
	GraphID        *string `json:"graph_id"`
	ConversationID *string `json:"conversation_id"`
	Turns          []turn  `json:"turns"`
}
type turn struct {
	Index      *int64  `json:"turn_index"`
	Speaker    *string `json:"speaker"`
	OccurredAt *string `json:"occurred_at"`
	Body       *string `json:"body"`
}
type turnResult struct {
	Index     int64  `json:"turn_index"`
	Status    string `json:"status"`
	ContextID string `json:"context_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
type responseBody struct {
	Turns []turnResult `json:"turns"`
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if len(r.Header.Values("Origin")) != 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	scheme, token, ok := strings.Cut(headers[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	account, agent, err := h.Backend.ValidateIngestCredential(r.Context(), token)
	if err != nil {
		if _, failed := errors.AsType[*store.StorageError](err); failed {
			w.WriteHeader(http.StatusInternalServerError)
		} else if errors.Is(err, model.ErrNotFound) {
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	if len(r.Header.Values("Content-Type")) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(raw) > 4<<20 {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	var input requestBody
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)); err != nil || input.GraphID == nil || input.ConversationID == nil || len(input.Turns) < 1 || len(input.Turns) > 100 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	graph, err := model.ParseID(*input.GraphID)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	conversation := *input.ConversationID
	if strings.TrimSpace(conversation) == "" || strings.ContainsRune(conversation, 0) || !utf8.ValidString(conversation) || utf8.RuneCountInString(conversation) > 128 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	seen := make(map[int64]bool, len(input.Turns))
	for _, t := range input.Turns {
		if t.Index == nil || *t.Index < 0 || seen[*t.Index] || t.Speaker == nil || (*t.Speaker != "user" && *t.Speaker != "agent") || t.OccurredAt == nil || t.Body == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seen[*t.Index] = true
		at, err := time.Parse(time.RFC3339Nano, *t.OccurredAt)
		if err != nil || at.IsZero() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, offset := at.Zone()
		if offset != 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !utf8.ValidString(*t.Body) || strings.ContainsRune(*t.Body, 0) || len(*t.Body) == 0 || utf8.RuneCountInString(*t.Body) > 8000 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	grade, found, err := h.Backend.EffectiveGrade(r.Context(), graph, account)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !found || (grade != model.GraphGradeOwner && grade != model.GraphGradeEditor) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	g, err := h.Backend.Graph(r.Context(), graph)
	if isStorageFailure(err) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if errors.Is(err, model.ErrNotFound) || err == nil && g.DeletedAt != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil || g.ID != graph {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	sort.Slice(input.Turns, func(i, j int) bool { return *input.Turns[i].Index < *input.Turns[j].Index })
	limits := h.Plans.For(account)
	writeLimits := store.WriteLimits{ActorID: account, StoredCharsPerGraph: limits.StoredCharactersPerGraph, WritesPerMinute: int64(limits.WritesPerMinute)}
	response := responseBody{Turns: make([]turnResult, 0, len(input.Turns))}
	for _, t := range input.Turns {
		id, err := model.NewID()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		at, _ := time.Parse(time.RFC3339Nano, *t.OccurredAt)
		kind := model.OriginKindUserUtterance
		if *t.Speaker == "agent" {
			kind = model.OriginKindAgentOutput
		}
		node := model.Context{ID: id, GraphID: graph, Layer: model.LayerSource, Body: *t.Body, RecordedAt: time.Now().UTC(), CreatedBy: account, CreatedByAgent: agent, Version: 1, Source: &model.SourceAttributes{Reference: model.SourceReference{Channel: model.SourceChannelConversation, Locator: "conversation:///" + url.PathEscape(conversation) + "/" + strconv.FormatInt(*t.Index, 10)}, OccurredAt: at.UTC(), OriginKind: kind, ConversationID: conversation, TurnIndex: t.Index}}
		correlation := ""
		if h.CorrelationID != nil {
			correlation = h.CorrelationID(r.Context())
		}
		saved, existing, err := h.Backend.CreateIngestSource(r.Context(), graph, node, writeLimits, h.Gap, correlation)
		result := turnResult{Index: *t.Index}
		switch {
		case err == nil:
			result.Status = "stored"
			if existing {
				result.Status = "existing"
			}
			result.ContextID = saved.ID.String()
		case isStorageFailure(err):
			w.WriteHeader(http.StatusInternalServerError)
			return
		case errors.Is(err, store.ErrIngestBodyMismatch):
			result.Status = "rejected"
			result.Reason = "body_mismatch"
		case errors.Is(err, model.ErrNotFound):
			w.WriteHeader(http.StatusNotFound)
			return
		default:
			if limit, ok := errors.AsType[plan.LimitError](err); ok {
				if limit.Name == "writes_per_minute" {
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				result.Status = "rejected"
				result.Reason = "limit_exceeded"
			} else if _, ok := errors.AsType[model.FieldError](err); ok {
				result.Status = "rejected"
				result.Reason = "invalid_argument"
			} else {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		response.Turns = append(response.Turns, result)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

func isStorageFailure(err error) bool { _, ok := errors.AsType[*store.StorageError](err); return ok }
