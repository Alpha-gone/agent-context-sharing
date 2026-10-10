package authz

import (
	"context"
	"errors"
)

const passwordWorkLimit = 4

// ErrPasswordBusy는 비밀번호 작업 자리가 없어 검증·생성을 시작하지 않았음을 나타낸다.
var ErrPasswordBusy = errors.New("password_work_busy")

// bcrypt는 context로 중단할 수 없으므로 반환할 때까지 같은 자리를 유지한다.
// 함수 경계는 실제 해시와 격리된 자원 상한 시험이 같은 실행 경로를 사용하게 한다.
type passwordWork struct {
	slots    chan struct{}
	generate func(string, int) (string, error)
	compare  func(string, string) (bool, error)
}

func newPasswordWork() *passwordWork {
	return &passwordWork{slots: make(chan struct{}, passwordWorkLimit), generate: hashPassword, compare: comparePassword}
}

func (work *passwordWork) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case work.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			work.release()
			return err
		}
		return nil
	default:
		return ErrPasswordBusy
	}
}

func (work *passwordWork) release() { <-work.slots }
