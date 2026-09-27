// 평가 컨텍스트 집합을 그래프 하나에 적재하고 색인이 끝날 때까지 기다린다.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent_context_sharing/internal/index"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// loadedGraph는 적재 결과다. Keys는 데이터셋의 key를 실제 컨텍스트 식별자로 옮긴
// 표이며 정답 판정이 이 표를 쓴다.
type loadedGraph struct {
	GraphID   model.ID
	AccountID model.ID
	Keys      map[string]model.ID
}

// loadGraph는 평가 전용 계정과 그래프를 새로 만들고 컨텍스트 집합을 올린다.
//
// 그래프를 회차마다 새로 만드는 이유는 개발 데이터베이스를 다른 작업과 공유하기
// 때문이다. 「격리 강제」가 모든 접근을 graph_id로 가르므로 평가가 자기 그래프만
// 쓰면 다른 그래프의 내용이 후보로 올라오지 않는다.
func loadGraph(ctx context.Context, database *store.Store, set contextSet) (loadedGraph, error) {
	accountID, err := model.NewID()
	if err != nil {
		return loadedGraph{}, fmt.Errorf("평가 계정 식별자 생성: %w", err)
	}
	now := time.Now().UTC()
	// 평가 계정은 로그인하지 않는다. 자격 증명 열은 비울 수 없으므로 검증을 통과할 수
	// 없는 고정 표식을 넣어 이 계정으로는 인증이 성립하지 않게 둔다. 로그인 아이디는
	// 「입력 검증 세부」가 영문 소문자·숫자·밑줄 32자로 제한하므로 식별자의 하이픈을
	// 빼고 앞부분만 쓴다.
	if err := database.CreateAccount(ctx, store.Account{
		ID:           accountID,
		LoginID:      "eval_" + strings.ReplaceAll(accountID.String(), "-", "")[:27],
		PasswordHash: "eval-no-login",
		CreatedAt:    now,
	}); err != nil {
		return loadedGraph{}, err
	}
	graphID, err := model.NewID()
	if err != nil {
		return loadedGraph{}, fmt.Errorf("평가 그래프 식별자 생성: %w", err)
	}
	graph, err := database.CreateGraphWithOwner(ctx, model.Graph{
		ID:             graphID,
		Name:           "eval " + set.Version,
		Description:    "검색 품질 평가 전용 그래프",
		CreatedBy:      accountID,
		CreatedAt:      now,
		LastActivityAt: now,
		Version:        1,
	}, store.WriteLimits{})
	if err != nil {
		return loadedGraph{}, fmt.Errorf("평가 그래프 생성: %w", err)
	}
	keys := make(map[string]model.ID, len(set.Contexts))
	if err := createContexts(ctx, database, graph.ID, accountID, set.Contexts, keys); err != nil {
		// 절반만 찬 그래프를 남기지 않는다. 적재가 실패하면 호출자가 그래프 식별자를
		// 받지 못하므로 여기에서 지우지 않으면 정리할 방법이 남지 않는다.
		if deleteErr := database.SetGraphDeleted(ctx, graph.ID, accountID, true); deleteErr != nil {
			return loadedGraph{}, errors.Join(err, fmt.Errorf("적재 실패 뒤 평가 그래프 정리: %w", deleteErr))
		}
		return loadedGraph{}, err
	}
	if err := confirmRelations(ctx, database, graph.ID, accountID, set, keys); err != nil {
		if deleteErr := database.SetGraphDeleted(ctx, graph.ID, accountID, true); deleteErr != nil {
			return loadedGraph{}, errors.Join(err, fmt.Errorf("관계 확정 실패 뒤 평가 그래프 정리: %w", deleteErr))
		}
		return loadedGraph{}, err
	}
	return loadedGraph{GraphID: graph.ID, AccountID: accountID, Keys: keys}, nil
}

// confirmRelations는 데이터셋의 사건 관계를 확정 상태로 올린다.
//
// 평가는 확정 관계가 그래프 경로 확장을 여는 것을 재야 하므로 제안 상태로 두지 않는다.
// 기록은 남기지 않는다. 평가 그래프는 측정 뒤 지워지므로 감사 행은 판정에 쓰이지 않는다.
func confirmRelations(ctx context.Context, database *store.Store, graphID, accountID model.ID, set contextSet, keys map[string]model.ID) error {
	agentID, err := model.NewID()
	if err != nil {
		return fmt.Errorf("평가 에이전트 식별자 생성: %w", err)
	}
	for _, spec := range set.Relations {
		relationID, err := model.NewID()
		if err != nil {
			return fmt.Errorf("관계 식별자 생성: %w", err)
		}
		now := time.Now().UTC()
		relation := model.Relation{ID: relationID, GraphID: graphID, Type: model.RelationType(spec.Type), FromContextID: keys[spec.From], ToContextID: keys[spec.To], State: model.RelationStateConfirmed, ProposedBy: model.ProposalSourceAgent, ProposedAt: now, ConfirmedBy: accountID, ConfirmedByAgent: agentID, ConfirmedAt: &now}
		if _, err := database.ConfirmRelation(ctx, graphID, relation, nil); err != nil {
			return fmt.Errorf("관계 %s(%s→%s) 확정: %w", spec.Type, spec.From, spec.To, err)
		}
	}
	return nil
}

// createContexts는 참조가 이미 만들어진 항목부터 차례로 올린다. 파생은 근거를,
// 사건은 구성원을 식별자로 받으므로 순서를 맞추지 않으면 적재가 성립하지 않는다.
//
// keys에는 이미 만든 컨텍스트의 key를 담아 넘기고 새로 만든 항목도 같은 맵에 채운다. 공격
// 표본의 추가 컨텍스트는 기준 집합의 key를 참조하므로 두 집합이 한 맵을 공유해야 한다.
func createContexts(ctx context.Context, database *store.Store, graphID, accountID model.ID, specs []contextSpec, keys map[string]model.ID) error {
	pending := specs
	for len(pending) > 0 {
		remaining := make([]contextSpec, 0, len(pending))
		progressed := false
		for _, spec := range pending {
			ready, references := resolveReferences(spec, keys)
			if !ready {
				remaining = append(remaining, spec)
				continue
			}
			created, err := createContext(ctx, database, graphID, accountID, spec, references)
			if err != nil {
				return fmt.Errorf("컨텍스트 %q 적재: %w", spec.Key, err)
			}
			keys[spec.Key] = created
			progressed = true
		}
		if !progressed {
			// 참조가 순환하면 남은 항목이 영원히 준비되지 않는다. 파일 검증은 참조가
			// 집합 안을 가리키는지만 보므로 순환은 여기에서 드러난다.
			return fmt.Errorf("컨텍스트 참조가 순환한다: %d건이 남았다", len(remaining))
		}
		pending = remaining
	}
	return nil
}

// resolveReferences는 계층이 요구하는 참조를 식별자로 바꾼다. 아직 만들어지지 않은
// 참조가 하나라도 있으면 준비되지 않은 것으로 본다.
func resolveReferences(spec contextSpec, keys map[string]model.ID) (bool, []model.ID) {
	var wanted []string
	switch {
	case spec.Derived != nil:
		wanted = spec.Derived.DerivedFrom
	case spec.Event != nil:
		wanted = spec.Event.Members
	default:
		return true, nil
	}
	resolved := make([]model.ID, 0, len(wanted))
	for _, key := range wanted {
		id, ready := keys[key]
		if !ready {
			return false, nil
		}
		resolved = append(resolved, id)
	}
	return true, resolved
}

func createContext(ctx context.Context, database *store.Store, graphID, accountID model.ID, spec contextSpec, references []model.ID) (model.ID, error) {
	contextID, err := model.NewID()
	if err != nil {
		return model.ID{}, fmt.Errorf("컨텍스트 식별자 생성: %w", err)
	}
	value := model.Context{
		ID:             contextID,
		GraphID:        graphID,
		Layer:          model.Layer(spec.Layer),
		Body:           spec.Body,
		RecordedAt:     time.Now().UTC(),
		CreatedBy:      accountID,
		CreatedByAgent: accountID,
		Version:        1,
	}
	switch {
	case spec.Source != nil:
		value.Source = &model.SourceAttributes{
			Reference:  model.SourceReference{Channel: model.SourceChannel(spec.Source.Channel), Locator: spec.Source.Locator},
			OccurredAt: spec.Source.OccurredAt.UTC(),
			OriginKind: model.OriginKind(spec.Source.OriginKind),
		}
	case spec.Derived != nil:
		value.Derived = &model.DerivedAttributes{
			Kind:            model.DerivationKind(spec.Derived.Kind),
			SummaryScope:    model.SummaryScope(spec.Derived.SummaryScope),
			DerivedFrom:     references,
			EvidenceState:   model.EvidenceState(spec.Derived.EvidenceState),
			ConfidenceState: model.ConfidenceState(spec.Derived.ConfidenceState),
			ValidFrom:       utcOrNil(spec.Derived.ValidFrom),
			ValidTo:         utcOrNil(spec.Derived.ValidTo),
		}
	case spec.Event != nil:
		value.Event = &model.EventAttributes{
			MemberIDs: references,
			Start:     spec.Event.Start.UTC(),
			End:       utcOrNil(spec.Event.End),
		}
	}
	// derivedFrom 인자는 파생의 근거 간선만 만든다. 사건 구성원은 속성 묶음이 갖고
	// 있으므로 여기에 넘기면 계층 검증이 거부한다.
	var derivedFrom []model.ID
	if spec.Derived != nil {
		derivedFrom = references
	}
	created, err := database.CreateContext(ctx, graphID, value, derivedFrom)
	if err != nil {
		return model.ID{}, err
	}
	return created.ID, nil
}

func utcOrNil(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

// drainIndexQueue는 평가 그래프의 색인 대기 작업이 남지 않을 때까지 작업자를 돌린다.
//
// 의미 유사도 채널은 임베딩이 있어야 후보를 내므로 측정 전에 큐를 비워야 한다.
// 확보를 그래프로 좁히는 이유는 개발 데이터베이스를 공유하기 때문이다. 가리지 않고
// 비우면 다른 그래프가 남긴 대기 작업을 평가가 대신 처리하게 되고, 그만큼 측정 시작
// 상태가 회차마다 달라진다.
func drainIndexQueue(ctx context.Context, worker *index.Worker, graphID model.ID, limit int) error {
	for range limit {
		found, err := worker.RunOnceInGraph(ctx, graphID)
		if err != nil {
			return fmt.Errorf("색인 작업 처리: %w", err)
		}
		if !found {
			return nil
		}
	}
	return fmt.Errorf("색인 대기 작업이 %d회 안에 비워지지 않았다", limit)
}
