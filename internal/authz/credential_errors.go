package authz

import (
	"errors"

	"agent_context_sharing/internal/store"
)

// ErrLoginIDExists는 가입 경로에서만 허용한 아이디 중복 안내다.
var ErrLoginIDExists = store.ErrLoginIDExists

// ErrInvalidAccountInput은 가입 아이디 또는 비밀번호의 형식 위반이다.
var ErrInvalidAccountInput = errors.New("invalid_account_input")

// credentialFailure는 오류 문자열을 고정하고 원인은 내부 오류 체인으로만 보존한다.
type credentialFailure struct{ kind, cause error }

func (failure *credentialFailure) Error() string   { return failure.kind.Error() }
func (failure *credentialFailure) Unwrap() []error { return []error{failure.kind, failure.cause} }
