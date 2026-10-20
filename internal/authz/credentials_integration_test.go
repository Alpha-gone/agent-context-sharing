package authz

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

func TestConcurrentAccountRegistrationIntegration(t *testing.T) {
	firstStore, secondStore := newAuthenticationContractStores(t)
	services := []*Service{testService(t, firstStore), testService(t, secondStore)}
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var workers sync.WaitGroup
	t.Cleanup(func() { unblock(); workers.Wait() })
	var generated atomic.Int32
	for _, service := range services {
		service.passwords.generate = func(password string, cost int) (string, error) {
			hash, err := hashPassword(password, cost)
			generated.Add(1)
			arrived <- struct{}{}
			<-release
			return hash, err
		}
	}
	type result struct {
		id    model.ID
		err   error
		index int
	}
	passwords := []string{"first synthetic password", "second synthetic password"}
	results := make(chan result, 2)
	for index, service := range services {
		workers.Go(func() {
			id, err := service.Register(t.Context(), "concurrent", passwords[index])
			results <- result{id, err, index}
		})
	}
	awaitSigningKeyLoad(t, arrived)
	awaitSigningKeyLoad(t, arrived)
	unblock()
	workers.Wait()
	close(results)
	var succeeded, duplicate, winner int
	var accountID model.ID
	for result := range results {
		switch {
		case result.err == nil:
			succeeded++
			accountID, winner = result.id, result.index
		case errors.Is(result.err, ErrLoginIDExists):
			duplicate++
		default:
			t.Fatal("실제 동시 가입이 성공 또는 아이디 중복으로 분류되지 않았습니다")
		}
	}
	if succeeded != 1 || duplicate != 1 || generated.Load() != 2 {
		t.Fatal("동시 가입의 1성공·1중복 또는 같은 해시 비용을 확인하지 못했습니다")
	}
	for _, service := range services {
		service.passwords.generate = hashPassword
		if got, err := service.Authenticate(t.Context(), "concurrent", passwords[winner]); err != nil || got != accountID {
			t.Fatal("다른 인스턴스에서 성공한 계정의 자격을 확인하지 못했습니다")
		}
		if _, err := service.Authenticate(t.Context(), "concurrent", passwords[1-winner]); !errors.Is(err, ErrInvalidCredential) {
			t.Fatal("패한 가입의 비밀번호로 인증했습니다")
		}
		if len(service.passwords.slots) != 0 {
			t.Fatal("동시 가입 뒤 비밀번호 작업 자리가 남았습니다")
		}
	}
	account, err := firstStore.AccountByLoginID(t.Context(), "concurrent")
	if err != nil || account.ID != accountID {
		t.Fatal("실제 저장된 계정이 가입 성공 결과와 다릅니다")
	}
	// 같은 UUID의 충돌은 아이디 중복이 아닌 내부 무결성 장애다.
	err = secondStore.CreateAccount(t.Context(), store.Account{ID: accountID, LoginID: "different", PasswordHash: account.PasswordHash, CreatedAt: time.Now().UTC()})
	failure, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || failure.Code != "23505" || failure.ConstraintName != "account_pkey" || errors.Is(err, store.ErrLoginIDExists) {
		t.Fatal("UUID 기본 키 충돌을 아이디 중복으로 잘못 분류했습니다")
	}
	if _, ok := errors.AsType[*store.StorageError](err); !ok {
		t.Fatal("기본 키 충돌이 안전한 내부 저장 오류가 아닙니다")
	}
	secondStore.Close()
	if _, err := services[1].Authenticate(t.Context(), "concurrent", passwords[winner]); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalidCredential) || err.Error() != ErrUnavailable.Error() {
		t.Fatal("실제 저장소 종료를 자격 불일치로 처리하거나 원문을 노출했습니다")
	}
}

func TestConcurrentCredentialRehashIntegration(t *testing.T) {
	firstStore, secondStore := newAuthenticationContractStores(t)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	const password = "legacy synthetic password"
	legacy := syntheticLegacyPasswordHash(t, "direct", password)
	if err := firstStore.CreateAccount(t.Context(), store.Account{ID: id, LoginID: "legacy", PasswordHash: legacy, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal("레거시 합성 계정 저장에 실패했습니다")
	}
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var workers sync.WaitGroup
	t.Cleanup(func() { unblock(); workers.Wait() })
	var updated atomic.Int32
	results := make(chan error, 2)
	for _, backend := range []*store.Store{firstStore, secondStore} {
		service := testService(t, credentialStoreHooks{authStore: backend, update: func(ctx context.Context, accountID model.ID, previous, next string) (bool, error) {
			arrived <- struct{}{}
			<-release
			changed, err := backend.UpdatePasswordHashIfMatches(ctx, accountID, previous, next)
			if changed {
				updated.Add(1)
			}
			return changed, err
		}})
		workers.Go(func() {
			got, err := service.Authenticate(t.Context(), "legacy", password)
			if err == nil && (got != id || len(service.passwords.slots) != 0) {
				err = errors.New("인증 계정 또는 자리 반환이 다릅니다")
			}
			results <- err
		})
	}
	awaitSigningKeyLoad(t, arrived)
	awaitSigningKeyLoad(t, arrived)
	unblock()
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("동시 레거시 인증에 실패했습니다")
		}
	}
	account, err := secondStore.AccountByLoginID(t.Context(), "legacy")
	if err != nil || updated.Load() != 1 || account.ID != id {
		t.Fatal("조건부 재해시가 한 번만 적용되지 않았습니다")
	}
	if legacy, err := comparePassword(account.PasswordHash, password); err != nil || legacy {
		t.Fatal("최종 현재 형식 해시로 대조하지 못했습니다")
	}
	if _, err := comparePassword(account.PasswordHash, "wrong synthetic password"); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatal("재해시 뒤 잘못된 비밀번호를 구분하지 못했습니다")
	}
}
