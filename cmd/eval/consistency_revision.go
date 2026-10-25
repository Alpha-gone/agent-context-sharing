package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/search"
)

const revisionCandidate search.Consistency = "evaluation_revision"
const consistencyRetryLimit = 3
const consistencyRequestTimeout = 30 * time.Second

var errRevisionExhausted = errors.New("내용 판 재시도 상한 소진")

type consistencyAttempt struct {
	Retries   int
	Exhausted bool
}

type consistencyRequest struct {
	graph   model.ID
	attempt *consistencyAttempt
}

type consistencyRequestKey struct{}

// 후보 구현은 실행기에만 존재한다. 운영 검색의 Consistency 열거형을 늘리지 않는다.
type revisionEvaluationStore struct {
	search.Store
	revision func(context.Context, model.ID) (int64, error)
}

func (database revisionEvaluationStore) WithReadSnapshot(ctx context.Context, read func(context.Context) error) (model.ReadStats, error) {
	stats := model.ReadStats{Connections: -1, PeakConnections: -1}
	request, ok := ctx.Value(consistencyRequestKey{}).(consistencyRequest)
	if !ok || request.attempt == nil || read == nil {
		return stats, fmt.Errorf("내용 판 후보의 요청 경계가 없다")
	}
	return stats, readStableRevision(ctx, request.graph, database.revision, read, request.attempt)
}

func readStableRevision(ctx context.Context, graph model.ID, revision func(context.Context, model.ID) (int64, error), read func(context.Context) error, attempt *consistencyAttempt) error {
	for index := 0; index <= consistencyRetryLimit; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := revision(ctx, graph)
		if err != nil {
			return err
		}
		if index > 0 {
			attempt.Retries++
		}
		if err := read(ctx); err != nil {
			return err
		}
		after, err := revision(ctx, graph)
		if err != nil {
			return err
		}
		if after == before {
			return ctx.Err()
		}
	}
	attempt.Exhausted = true
	return errRevisionExhausted
}

// Flow의 외부 임베딩은 search의 기존 읽기 경계 앞에서 한 번 실행된다.
type consistencyReader struct {
	service *search.Service
}

func (reader *consistencyReader) Flow(ctx context.Context, input search.Input) (search.Flow, error) {
	flow, _, err := reader.measuredFlow(ctx, input)
	return flow, err
}

func (reader *consistencyReader) measuredFlow(ctx context.Context, input search.Input) (search.Flow, consistencyAttempt, error) {
	ctx, cancel := context.WithTimeout(ctx, consistencyRequestTimeout)
	defer cancel()
	if input.AsOf.IsZero() {
		input.AsOf = time.Now().UTC()
	}
	attempt := consistencyAttempt{}
	ctx = context.WithValue(ctx, consistencyRequestKey{}, consistencyRequest{graph: input.GraphID, attempt: &attempt})
	flow, err := reader.service.Flow(ctx, input)
	return flow, attempt, err
}
