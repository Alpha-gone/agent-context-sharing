package store

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent_context_sharing/internal/model"
	"github.com/jackc/pgx/v5"
)

// ContextRevision은 검색 정점과 분리해 보존한 파생 판과 고정된 이전 판 연결이다.
type ContextRevision struct {
	Context           model.Context
	PreviousVersion   *int64
	SupersedesVersion *int64
}

// ContextRevision은 지정 그래프의 불변 파생 판을 읽고 식별자·판을 재검사한다.
func (s *Store) ContextRevision(ctx context.Context, graphID, contextID model.ID, version int64) (result ContextRevision, err error) {
	if !graphID.IsV7() || !contextID.IsV7() || version < 1 {
		return result, model.FieldError{Field: "version", Message: "파생 판 조회 인자가 올바르지 않다"}
	}
	ctx, release, err := s.enterReadScope(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := release(); closeErr != nil {
			result, err = ContextRevision{}, errors.Join(err, closeErr)
		}
	}()
	var raw []byte
	var supersedesID *string
	err = s.reader(ctx).QueryRow(ctx, `SELECT snapshot, previous_version, supersedes_context_id::text, supersedes_version
		FROM public.context_revision WHERE graph_id=$1 AND context_id=$2 AND version=$3`, graphID.String(), contextID.String(), version).Scan(&raw, &result.PreviousVersion, &supersedesID, &result.SupersedesVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContextRevision{}, ErrNotFound
	}
	if err != nil {
		return ContextRevision{}, storageError("revision_read", err)
	}
	if err := json.Unmarshal(raw, &result.Context); err != nil {
		return ContextRevision{}, storageError("revision_decode", err)
	}
	value := result.Context
	if err := value.Validate(); err != nil {
		return ContextRevision{}, storageError("revision_context", err)
	}
	if value.GraphID != graphID || value.ID != contextID || value.Version != version || value.Derived == nil ||
		(supersedesID != nil && (value.Derived.Supersedes == nil || value.Derived.Supersedes.String() != *supersedesID)) {
		return ContextRevision{}, storageError("revision_identity", ErrInvariantViolation)
	}
	return result, nil
}

// recordDerivedRevision은 관찰한 이전 판을 먼저 보존하고 결과 판의 연결을 기록한다.
// 기준 판의 ON CONFLICT는 기존 이력을 변경하지 않는다. 새 결과 판의 충돌은 오류다.
func (s *Store) recordDerivedRevision(ctx context.Context, tx pgx.Tx, previous *model.Context, current model.Context, superseded *model.Context) error {
	if current.Derived == nil {
		return nil
	}
	var previousVersion, supersedesVersion *int64
	var supersedesID *string
	if previous != nil {
		if err := s.insertDerivedRevision(ctx, tx, *previous, nil, nil, nil, true); err != nil {
			return err
		}
		previousVersion = new(previous.Version)
		if err := tx.QueryRow(ctx, `SELECT supersedes_context_id::text, supersedes_version FROM public.context_revision
			WHERE graph_id=$1 AND context_id=$2 AND version=$3`, current.GraphID.String(), current.ID.String(), previous.Version).Scan(&supersedesID, &supersedesVersion); err != nil {
			return fmt.Errorf("이전 파생 판 연결 조회: %w", err)
		}
	}
	if superseded != nil {
		if err := s.insertDerivedRevision(ctx, tx, *superseded, nil, nil, nil, true); err != nil {
			return err
		}
		supersedesID, supersedesVersion = new(superseded.ID.String()), new(superseded.Version)
	}
	return s.insertDerivedRevision(ctx, tx, current, previousVersion, supersedesID, supersedesVersion, false)
}

func (s *Store) insertDerivedRevision(ctx context.Context, tx pgx.Tx, value model.Context, previousVersion *int64, supersedesID *string, supersedesVersion *int64, baseline bool) error {
	if err := value.Validate(); err != nil {
		return fmt.Errorf("파생 판 스냅숏 검증: %w", err)
	}
	value = attachReferences(value, value.Derived.DerivedFrom, nil)
	slices.SortFunc(value.Derived.DerivedFrom, func(a, b model.ID) int { return strings.Compare(a.String(), b.String()) })
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("파생 판 스냅숏 인코딩: %w", err)
	}
	query := `INSERT INTO public.context_revision (graph_id,context_id,version,previous_version,supersedes_context_id,supersedes_version,snapshot)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	if baseline {
		query += ` ON CONFLICT (graph_id,context_id,version) DO NOTHING`
	}
	if _, err := tx.Exec(ctx, query, value.GraphID.String(), value.ID.String(), value.Version, previousVersion, supersedesID, supersedesVersion, raw); err != nil {
		return fmt.Errorf("파생 판 이력 저장: %w", err)
	}
	return nil
}

// lockReferenceChanges는 대상 조회와 새 참조 추가 사이의 팬텀을 막는다.
// 멱등성의 bigint 자문 키와 다른 2정수 키 공간이며 충돌은 추가 대기만 만든다.
func lockReferenceChanges(ctx context.Context, tx pgx.Tx, graphID model.ID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1094931276, hashtext($1))`, graphID.String()); err != nil {
		return fmt.Errorf("그래프 참조 전이 잠금: %w", err)
	}
	return nil
}
