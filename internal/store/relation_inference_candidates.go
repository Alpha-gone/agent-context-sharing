package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// InferenceCandidateRequest는 추론 후보 읽기 설정이다.
type InferenceCandidateRequest = model.InferenceCandidateRequest

// InferenceCandidates는 같은 그래프의 활성 사건 후보만 읽는다. 접근 시각은 갱신하지 않는다.
func (s *Store) InferenceCandidates(ctx context.Context, request InferenceCandidateRequest) (result model.InferenceSnapshot, err error) {
	task := request.Task
	if !task.GraphID.IsV7() || !task.ContextID.IsV7() || task.TargetVersion < 1 || task.Generation.IsZero() || request.ProfileFingerprint == "" || request.EmbeddingModel == "" || request.Limit < 1 || request.Limit > 20 || request.IndexWait <= 0 || request.AdjacencyWindow <= 0 {
		return result, fmt.Errorf("추론 후보 조회 인자가 올바르지 않다")
	}
	seenChannels := make(map[string]bool, len(request.Channels))
	for _, channel := range request.Channels {
		if !slices.Contains([]string{"semantic", "keyword", "member", "rule", "conversation"}, channel) || seenChannels[channel] {
			return result, fmt.Errorf("추론 후보 채널이 올바르지 않다")
		}
		seenChannels[channel] = true
	}
	if len(seenChannels) == 0 {
		return result, fmt.Errorf("추론 후보 채널이 없다")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, fmt.Errorf("추론 후보 스냅숏 시작: %w", err)
	}
	defer func() {
		err = errors.Join(err, rollbackTransaction(ctx, tx))
		if err != nil {
			result = model.InferenceSnapshot{}
		}
	}()
	ctx = context.WithValue(ctx, readTxKey{}, tx)
	target, err := s.context(ctx, tx, task.GraphID, task.ContextID)
	if err != nil {
		return result, err
	}
	if target.Layer != model.LayerEvent || target.Event == nil || target.DeletedAt != nil || target.Version != task.TargetVersion {
		return result, fmt.Errorf("추론 대상 사건이 변경됐거나 활성 상태가 아니다")
	}
	result.Target = target
	var state *string
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT (SELECT state FROM public.index_task WHERE graph_id=$1 AND context_id=$2), statement_timestamp()`, task.GraphID.String(), task.ContextID.String()).Scan(&state, &now); err != nil {
		return result, fmt.Errorf("추론 대상 색인 상태: %w", err)
	}
	if seenChannels["semantic"] && state != nil {
		result.SemanticUnavailable = true
		until := task.Generation.Add(request.IndexWait)
		if *state == "pending" && now.Before(until) {
			result.IndexWaitUntil = &until
			return result, nil
		}
	}
	// 판정 완료·정체성 제외 뒤 상한을 채우도록 채널을 작은 페이지로 읽는다.
	scanLimit := min(request.Limit*5, 100)
	channels := make(map[string][]model.ID, 5)
	var semanticSQL string
	if seenChannels["semantic"] && !result.SemanticUnavailable {
		if err := enableIterativeScan(ctx, tx); err != nil {
			return result, err
		}
		semanticSQL = `SELECT c.context_id::text FROM public.context_embedding c
			JOIN public.context_embedding target ON target.context_id=$2 AND target.graph_id=$1 AND target.model_id=$3
			JOIN ` + s.contextTable() + ` node ON node.properties ->> 'context_id'::text=c.context_id::text
			WHERE c.graph_id=$1 AND c.model_id=$3 AND c.context_id<>$2
			AND node.properties ->> 'graph_id'::text=$1::text AND node.properties ->> 'layer'::text='event'
			AND node.properties ->> 'deleted_at'::text IS NULL
			ORDER BY c.embedding <=> target.embedding,c.context_id LIMIT $4 OFFSET $5`
		rows, queryErr := tx.Query(ctx, semanticSQL, task.GraphID.String(), task.ContextID.String(), request.EmbeddingModel, scanLimit, 0)
		if queryErr != nil {
			return result, fmt.Errorf("추론 의미 후보 조회: %w", queryErr)
		}
		channels["semantic"], err = searchCandidateIDs(rows)
		if err != nil {
			return result, err
		}
		var present bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.context_embedding WHERE graph_id=$1 AND context_id=$2 AND model_id=$3)`, task.GraphID.String(), task.ContextID.String(), request.EmbeddingModel).Scan(&present); err != nil {
			return result, err
		}
		if !present {
			result.SemanticUnavailable = true
			delete(channels, "semantic")
		}
	}
	positive, negative := websearchOrQuery(target.Body)
	keywordSQL := `WITH q AS (SELECT CASE WHEN $3::text='' THEN websearch_to_tsquery('simple',$2)
		WHEN $2::text='' THEN websearch_to_tsquery('simple',$3) ELSE websearch_to_tsquery('simple',$2) && websearch_to_tsquery('simple',$3) END AS value)
		SELECT properties ->> 'context_id'::text FROM ` + s.contextTable() + ` CROSS JOIN q
		WHERE properties ->> 'graph_id'::text=$1 AND properties ->> 'layer'::text='event' AND properties ->> 'deleted_at'::text IS NULL
		AND properties ->> 'context_id'::text<>$4 AND to_tsvector('simple',properties ->> 'body'::text) @@ q.value
		ORDER BY ts_rank_cd(to_tsvector('simple',properties ->> 'body'::text),q.value) DESC,
		(properties ->> 'recorded_at'::text)::timestamptz DESC,properties ->> 'context_id'::text ASC LIMIT $5 OFFSET $6`
	var rows pgx.Rows
	if seenChannels["keyword"] {
		rows, err = tx.Query(ctx, keywordSQL, task.GraphID.String(), positive, negative, task.ContextID.String(), scanLimit, 0)
		if err != nil {
			return result, fmt.Errorf("추론 키워드 후보 조회: %w", err)
		}
		channels["keyword"], err = searchCandidateIDs(rows)
		if err != nil {
			return result, err
		}
	}
	memberTable := pgx.Identifier{s.graphName, "HAS_MEMBER"}.Sanitize()
	memberSQL := `SELECT n.properties ->> 'context_id'::text FROM ` + memberTable + ` e
		JOIN ` + memberTable + ` own ON own.end_id=e.end_id
		JOIN ` + s.contextTable() + ` n ON n.id=e.start_id
		JOIN ` + s.contextTable() + ` target ON target.id=own.start_id
		WHERE target.properties ->> 'graph_id'::text=$1 AND target.properties ->> 'context_id'::text=$2
		AND own.properties ->> 'graph_id'::text=$1 AND e.properties ->> 'graph_id'::text=$1
		AND n.properties ->> 'graph_id'::text=$1 AND n.properties ->> 'layer'::text='event'
		AND n.properties ->> 'deleted_at'::text IS NULL AND n.properties ->> 'context_id'::text<>$2
		GROUP BY n.properties ->> 'context_id'::text ORDER BY count(DISTINCT e.end_id) DESC,n.properties ->> 'context_id'::text LIMIT $3 OFFSET $4`
	if seenChannels["member"] {
		rows, err = tx.Query(ctx, memberSQL, task.GraphID.String(), task.ContextID.String(), scanLimit, 0)
		if err != nil {
			return result, fmt.Errorf("추론 구성원 후보 조회: %w", err)
		}
		channels["member"], err = searchCandidateIDs(rows)
		if err != nil {
			return result, err
		}
	}
	if seenChannels["rule"] {
		events, err := s.activeEventContexts(ctx, task.GraphID, task.ContextID)
		if err != nil {
			return result, err
		}
		for _, candidate := range events {
			_, _, part := memberSubsetCandidate(task.GraphID, target, candidate)
			_, _, adjacent := adjacentPrecedesCandidate(task.GraphID, target, candidate, request.AdjacencyWindow)
			if part || adjacent {
				channels["rule"] = append(channels["rule"], candidate.ID)
			}
		}
	}
	if seenChannels["conversation"] && target.Event.ConversationID != "" {
		neighbors, err := s.nearestConversationEvents(ctx, tx, task.GraphID, target)
		if err != nil {
			return result, err
		}
		slices.SortFunc(neighbors, func(a, b model.Context) int { return cmp.Or(a.Event.Start.Compare(b.Event.Start), a.ID.Compare(b.ID)) })
		for _, candidate := range neighbors {
			channels["conversation"] = append(channels["conversation"], candidate.ID)
		}
	}
	byID := make(map[model.ID]model.InferenceCandidate)
	checked := make(map[model.ID]bool)
	load := func(ids []model.ID) error {
		pending := make([]model.ID, 0, len(ids))
		for _, id := range ids {
			if !checked[id] {
				checked[id] = true
				pending = append(pending, id)
			}
		}
		values, err := s.contextsByIDs(ctx, tx, task.GraphID, pending)
		if err != nil {
			return err
		}
		for _, candidate := range values {
			if candidate.GraphID != task.GraphID || candidate.Layer != model.LayerEvent || candidate.Event == nil || candidate.DeletedAt != nil || candidate.ID == target.ID {
				return storageError("inference_candidate_scope", ErrInvariantViolation)
			}
			allowed, err := s.inferenceAllowed(ctx, tx, request, target, candidate)
			if err != nil {
				return err
			}
			if len(allowed.Allowed) != 0 {
				byID[candidate.ID] = allowed
			}
		}
		return nil
	}
	for _, name := range []string{"semantic", "keyword", "member", "rule", "conversation"} {
		if !seenChannels[name] {
			continue
		}
		if name == "semantic" && result.SemanticUnavailable {
			continue
		}
		channel := model.InferenceChannel{Name: name}
		page := channels[name]
		if name == "rule" && len(page) > scanLimit {
			page = page[:scanLimit]
		}
		offset := 0
		channelSeen := make(map[model.ID]bool)
		for len(page) > 0 && len(channel.Candidates) < request.Limit {
			if err := load(page); err != nil {
				return result, err
			}
			for _, id := range page {
				if value, ok := byID[id]; ok && !channelSeen[id] {
					channelSeen[id] = true
					channel.Candidates = append(channel.Candidates, value)
				}
				if len(channel.Candidates) == request.Limit {
					break
				}
			}
			offset += len(page)
			if len(page) < scanLimit || len(channel.Candidates) == request.Limit {
				break
			}
			var next pgx.Rows
			switch name {
			case "semantic":
				next, err = tx.Query(ctx, semanticSQL, task.GraphID.String(), task.ContextID.String(), request.EmbeddingModel, scanLimit, offset)
			case "keyword":
				next, err = tx.Query(ctx, keywordSQL, task.GraphID.String(), positive, negative, task.ContextID.String(), scanLimit, offset)
			case "member":
				next, err = tx.Query(ctx, memberSQL, task.GraphID.String(), task.ContextID.String(), scanLimit, offset)
			case "rule":
				if offset >= len(channels[name]) {
					page = nil
				} else {
					page = channels[name][offset:min(offset+scanLimit, len(channels[name]))]
				}
				continue
			default:
				page = nil
				continue
			}
			if err != nil {
				return result, fmt.Errorf("추론 후보 페이지 조회: %w", err)
			}
			page, err = searchCandidateIDs(next)
			if err != nil {
				return result, err
			}
		}
		result.Channels = append(result.Channels, channel)
	}
	return result, nil
}

func (s *Store) inferenceAllowed(ctx context.Context, tx pgx.Tx, request InferenceCandidateRequest, target, candidate model.Context) (model.InferenceCandidate, error) {
	value := model.InferenceCandidate{Event: candidate}
	rows, err := tx.Query(ctx, `SELECT DISTINCT d.relation_type FROM public.relation_inference_decision d
		JOIN public.relation_inference_run r ON r.run_id=d.run_id WHERE r.graph_id=$1 AND r.context_id=$2 AND r.target_version=$3
		AND r.profile_fingerprint=$4 AND d.candidate_context_id=$5 AND d.candidate_version=$6
		AND d.validation_result='valid'`, target.GraphID.String(), target.ID.String(), target.Version, request.ProfileFingerprint, candidate.ID.String(), candidate.Version)
	if err != nil {
		return value, err
	}
	judged := map[string]bool{}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			return value, err
		}
		judged[kind] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return value, err
	}
	rows.Close()
	// 같은 판과 프로필에서 이미 판정한 쌍은 다시 모델에 보내지 않는다.
	if len(judged) != 0 {
		return value, nil
	}
	settled := func(kind model.RelationType, from, to model.ID) (bool, error) {
		if kind == model.RelationTypeRelatesTo && from.Compare(to) > 0 {
			from, to = to, from
		}
		relation, err := s.relationByIdentity(ctx, tx, target.GraphID, kind, from, to)
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return relation.State == model.RelationStateConfirmed || relation.State == model.RelationStateDiscarded, nil
	}
	if !judged[string(model.RelationTypeRelatesTo)] {
		blocked, err := settled(model.RelationTypeRelatesTo, target.ID, candidate.ID)
		if err != nil {
			return value, err
		}
		if !blocked {
			value.Allowed = append(value.Allowed, model.RelationTypeRelatesTo)
		}
	}
	if !judged[string(model.RelationTypeCauses)] {
		for _, direction := range []struct {
			name     string
			from, to model.ID
		}{{"T_to_C", target.ID, candidate.ID}, {"C_to_T", candidate.ID, target.ID}} {
			blocked, err := settled(model.RelationTypeCauses, direction.from, direction.to)
			if err != nil {
				return value, err
			}
			if !blocked {
				value.CauseDirections = append(value.CauseDirections, direction.name)
			}
		}
		if len(value.CauseDirections) > 0 {
			value.Allowed = append(value.Allowed, model.RelationTypeCauses)
		}
	}
	if !judged[string(model.RelationTypePartOf)] {
		if relation, _, ok := memberSubsetCandidate(target.GraphID, target, candidate); ok {
			blocked, err := settled(model.RelationTypePartOf, relation.FromContextID, relation.ToContextID)
			if err != nil {
				return value, err
			}
			if !blocked {
				value.Allowed = append(value.Allowed, model.RelationTypePartOf)
				value.PartOfDirection = "C_to_T"
				if relation.FromContextID == target.ID {
					value.PartOfDirection = "T_to_C"
				}
			}
		}
	}
	return value, nil
}
