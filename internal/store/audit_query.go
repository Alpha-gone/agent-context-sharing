package store

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"agent_context_sharing/internal/model"
)

type auditCursor struct {
	at     time.Time
	id     model.ID
	source string
}

func encodeAuditCursor(entry model.AuditEntry) string {
	return base64.RawURLEncoding.EncodeToString([]byte(entry.OccurredAt.UTC().Format(time.RFC3339Nano) + "|" + entry.ID.String() + "|" + entry.Source))
}

func decodeAuditCursor(raw string) (auditCursor, error) {
	if len(raw) > 512 {
		return auditCursor{}, ErrInvalidCursor
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return auditCursor{}, ErrInvalidCursor
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 3 || (parts[2] != "operation" && parts[2] != "web") {
		return auditCursor{}, ErrInvalidCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil || at.IsZero() || at.Location() != time.UTC {
		return auditCursor{}, ErrInvalidCursor
	}
	id, err := model.ParseID(parts[1])
	if err != nil {
		return auditCursor{}, ErrInvalidCursor
	}
	return auditCursor{at: at, id: id, source: parts[2]}, nil
}

// ListAuditEntries는 출처별로 유한한 기록을 읽고 소유자용 한 페이지로 합친다.
// limit는 호출 경계에서 요청 계정의 GraphPage로 결정한 값을 받는다.
func (s *Store) ListAuditEntries(ctx context.Context, graphID, actorID model.ID, filter model.AuditFilter, cursor string, limit int) (entries []model.AuditEntry, next string, err error) {
	if !graphID.IsV7() || !actorID.IsV7() || limit <= 0 || limit >= math.MaxInt32 || (!filter.Target.IsZero() && !filter.Target.IsV7()) {
		return nil, "", model.FieldError{Field: "audit", Message: "감사 범위의 식별자와 페이지 크기가 올바르지 않다"}
	}
	for _, at := range []*time.Time{filter.From, filter.Until} {
		if at != nil && (at.IsZero() || at.Location() != time.UTC) {
			return nil, "", model.FieldError{Field: "period", Message: "기간은 UTC로 지정해야 한다"}
		}
	}
	if filter.From != nil && filter.Until != nil && !filter.From.Before(*filter.Until) {
		return nil, "", model.FieldError{Field: "period", Message: "기간 시작은 끝보다 이전이어야 한다"}
	}
	var position auditCursor
	if cursor != "" {
		position, err = decodeAuditCursor(cursor)
		if err != nil {
			return nil, "", err
		}
	}
	ctx, release, err := s.enterHopReadScope(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if closeErr := release(); closeErr != nil {
			entries, next, err = nil, "", errors.Join(err, closeErr)
		}
		err = connectionError(ctx, "감사 기록 조회", err)
	}()
	grade, found, err := effectiveGrade(ctx, s.reader(ctx), graphID, actorID)
	if err != nil {
		return nil, "", err
	}
	if !found || grade != model.GraphGradeOwner {
		return nil, "", ErrNotFound
	}
	var after, afterID any
	if cursor != "" {
		after, afterID = position.at, position.id.String()
	}
	var target any
	if !filter.Target.IsZero() {
		target = filter.Target.String()
	}
	rank := 0
	if position.source == "web" {
		rank = 1
	}
	args := []any{graphID.String(), filter.From, filter.Until, target, after, afterID, rank, limit + 1}
	for _, source := range []string{"operation", "web"} {
		query := auditOperationSQL
		if source == "web" {
			query = auditWebSQL
		}
		rows, err := s.reader(ctx).Query(ctx, query, args...)
		if err != nil {
			return nil, "", storageError("audit_query", err)
		}
		for rows.Next() {
			entry := model.AuditEntry{Source: source}
			var rawID, rawGraph, rawActor string
			if err := rows.Scan(&rawID, &rawGraph, &entry.Kind, &entry.Action, &rawActor, &entry.OccurredAt, &entry.Target, &entry.Detail); err != nil {
				rows.Close()
				return nil, "", storageError("audit_scan", err)
			}
			entry.ID, err = model.ParseID(rawID)
			if err == nil {
				entry.ActorID, err = model.ParseID(rawActor)
			}
			if err != nil || rawGraph != graphID.String() {
				rows.Close()
				return nil, "", storageError("audit_scope", errors.Join(ErrInvariantViolation, err))
			}
			entry.OccurredAt = entry.OccurredAt.UTC()
			entries = append(entries, entry)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, "", storageError("audit_rows", err)
		}
	}
	slices.SortFunc(entries, func(a, b model.AuditEntry) int {
		if c := b.OccurredAt.Compare(a.OccurredAt); c != 0 {
			return c
		}
		if c := b.ID.Compare(a.ID); c != 0 {
			return c
		}
		return strings.Compare(b.Source, a.Source)
	})
	if len(entries) > limit {
		entries = entries[:limit]
		next = encodeAuditCursor(entries[len(entries)-1])
	}
	return entries, next, nil
}

const auditOperationSQL = `SELECT operation_id::text, graph_id::text, 'operation', operation_kind,
	actor_account_id::text, applied_at, COALESCE(context_id::text, relation_id::text, ''),
	result || COALESCE(': ' || reject_reason, '')
	FROM public.operation_log WHERE graph_id=$1
	AND ($2::timestamptz IS NULL OR applied_at >= $2) AND ($3::timestamptz IS NULL OR applied_at < $3)
	AND ($4::uuid IS NULL OR context_id=$4 OR relation_id=$4)
	AND ($5::timestamptz IS NULL OR (applied_at, operation_id, 0) < ($5, $6::uuid, $7::int))
	ORDER BY applied_at DESC, operation_id DESC LIMIT $8`

const auditWebSQL = `SELECT audit_id::text, graph_id::text, target_kind, action, actor_account_id::text,
	occurred_at, COALESCE(target_context_id::text, subject_id::text, team_id::text, graph_id::text, ''),
	concat_ws(' / ',
		CASE WHEN before_grade IS NOT NULL OR after_grade IS NOT NULL
			THEN COALESCE(before_grade, '-') || ' → ' || COALESCE(after_grade, '-') END,
		CASE WHEN member_account_id IS NOT NULL THEN '구성원: ' || member_account_id::text END)
	FROM public.web_audit_log WHERE graph_id=$1
	AND ($2::timestamptz IS NULL OR occurred_at >= $2) AND ($3::timestamptz IS NULL OR occurred_at < $3)
	AND ($4::uuid IS NULL OR target_context_id=$4 OR subject_id=$4 OR team_id=$4 OR graph_id=$4 OR member_account_id=$4 OR requester_account_id=$4)
	AND ($5::timestamptz IS NULL OR (occurred_at, audit_id, 1) < ($5, $6::uuid, $7::int))
	ORDER BY occurred_at DESC, audit_id DESC LIMIT $8`
