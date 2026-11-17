package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// ErrIngestBodyMismatch는 같은 대화 턴의 저장 본문이 다를 때 반환한다.
var ErrIngestBodyMismatch = errors.New("수집 턴 본문 불일치")

type ingestSourceOptions struct {
	gap           time.Duration
	correlationID string
}

// CreateIngestSource는 수집 원천과 구간화 작업을 같은 트랜잭션에서 저장한다.
func (s *Store) CreateIngestSource(ctx context.Context, graph model.ID, value model.Context, limits WriteLimits, gap time.Duration, correlationID string) (model.Context, bool, error) {
	if value.Layer != model.LayerSource || value.Source == nil || value.Source.TurnIndex == nil || value.Source.Reference.Channel != model.SourceChannelConversation || gap < time.Minute || gap > 24*time.Hour {
		return model.Context{}, false, model.FieldError{Field: "source", Message: "수집 원천을 확인해 주십시오."}
	}
	if err := validateContextInput(graph, value, nil); err != nil {
		return model.Context{}, false, err
	}
	operation := &OperationRecord{Kind: OperationAdd, GraphID: graph, ContextID: value.ID, AccountID: value.CreatedBy, AgentID: value.CreatedByAgent, JudgmentInput: "conversation ingestion", JudgmentSource: model.ConfirmationSourceIngest}
	result, err := s.createContext(ctx, graph, value, nil, model.ID{}, operation, limits, &ingestSourceOptions{gap: gap, correlationID: correlationID})
	if err != nil {
		return model.Context{}, false, err
	}
	return result, result.ID != value.ID, nil
}

func (s *Store) findConversationSourceWith(ctx context.Context, queryer cypherQueryer, graph model.ID, conversation string, turn int64) (model.Context, error) {
	var properties string
	err := queryer.QueryRow(ctx, `SELECT properties::text FROM `+s.contextTable()+` WHERE properties ->> 'graph_id'::text=$1 AND properties ->> 'layer'::text='source' AND properties ->> 'conversation_id'::text=$2 AND properties ->> 'turn_index'::text=$3`, graph.String(), conversation, fmt.Sprint(turn)).Scan(&properties)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Context{}, ErrNotFound
	}
	if err != nil {
		return model.Context{}, storageError("ingest_source_lookup", err)
	}
	return parseContext(`{"label":"Context","properties":`+properties+`}`, graph)
}
