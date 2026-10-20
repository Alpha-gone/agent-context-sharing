package authz

import (
	"context"
	"errors"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// ConnectionStore는 검증한 웹 세션의 계정을 공유 저장 상태와 다시 대조한다.
type ConnectionStore interface {
	ListConnections(context.Context, model.ID, model.ID) (store.ConnectionPage, error)
	RevokeOAuthGrant(context.Context, model.ID, model.ID) error
	RevokeAgent(context.Context, model.ID, model.ID) error
}

// ListConnections는 웹 세션에서 확인한 계정의 연결 상태만 반환한다.
func (s *Service) ListConnections(ctx context.Context, account, before model.ID) (store.ConnectionPage, error) {
	if err := ctx.Err(); err != nil {
		return store.ConnectionPage{}, err
	}
	if !account.IsV7() || (before != (model.ID{}) && !before.IsV7()) {
		return store.ConnectionPage{}, model.ErrNotFound
	}
	if s.config.Connections == nil {
		return store.ConnectionPage{}, ErrUnavailable
	}
	page, err := s.config.Connections.ListConnections(ctx, account, before)
	if ctx.Err() != nil {
		return store.ConnectionPage{}, ctx.Err()
	}
	if err != nil {
		s.connectionDiagnostic(ctx, "list", "store_unavailable")
		return store.ConnectionPage{}, &credentialFailure{ErrUnavailable, err}
	}
	return page, nil
}

// RevokeConnection은 저장소에서 소유자를 재확인하고 대상 연결만 폐기한다.
func (s *Service) RevokeConnection(ctx context.Context, account model.ID, action string, target model.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !account.IsV7() || !target.IsV7() {
		return model.ErrNotFound
	}
	if s.config.Connections == nil {
		return ErrUnavailable
	}
	var err error
	switch action {
	case "revoke_grant":
		err = s.config.Connections.RevokeOAuthGrant(ctx, account, target)
	case "revoke_agent":
		err = s.config.Connections.RevokeAgent(ctx, account, target)
	default:
		return model.ErrNotFound
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := errors.AsType[*store.StorageError](err); ok {
		s.connectionDiagnostic(ctx, action, "store_unavailable")
		return &credentialFailure{ErrUnavailable, err}
	}
	if errors.Is(err, model.ErrNotFound) {
		s.connectionDiagnostic(ctx, action, "target_unavailable")
		return model.ErrNotFound
	}
	if _, ok := errors.AsType[model.FieldError](err); ok {
		s.connectionDiagnostic(ctx, action, "target_unavailable")
		return model.ErrNotFound
	}
	if err != nil {
		s.connectionDiagnostic(ctx, action, "store_unavailable")
		return &credentialFailure{ErrUnavailable, err}
	}
	s.connectionDiagnostic(ctx, action, "completed")
	return nil
}

func (s *Service) connectionDiagnostic(ctx context.Context, stage, reason string) {
	if s.diagnostics != nil {
		level, result := "WARN", "failed"
		if reason == "completed" {
			level, result = "INFO", "completed"
		}
		s.diagnostics.RecordDiagnostic(ctx, level, "connections_"+stage, result, reason)
	}
}
