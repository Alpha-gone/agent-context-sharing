package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestCredentialComparisonContract(t *testing.T) {
	for _, mode := range []string{"missing", "empty login", "invalid login", "wrong password", "correct password"} {
		t.Run(mode, func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			id, err := service.Register(t.Context(), "existing", "synthetic password")
			if err != nil {
				t.Fatal(err)
			}
			login, password := "existing", "synthetic password"
			dummy := false
			switch mode {
			case "missing":
				login, dummy = "missing", true
			case "empty login":
				login, password, dummy = "", "", true
			case "invalid login":
				login, dummy = "INVALID", true
			case "wrong password":
				password = "wrong synthetic password"
			}
			var compared int
			service.passwords.compare = func(stored, password string) (bool, error) {
				compared++
				if (stored == service.dummyPasswordHash) != dummy || len(service.passwords.slots) != 1 {
					t.Fatal("더미 대조 또는 비밀번호 작업 자리의 계약이 다릅니다")
				}
				hash := strings.TrimPrefix(stored, passwordHashPrefix)
				if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost != service.config.BcryptCost {
					t.Fatal("대조 비용이 구성한 bcrypt 비용과 다릅니다")
				}
				return comparePassword(stored, password)
			}
			service.passwords.generate = func(string, int) (string, error) {
				t.Fatal("현재 형식 대조에서 불필요하게 해시를 생성했습니다")
				return "", errors.New("unexpected generation")
			}
			got, err := service.Authenticate(t.Context(), login, password)
			if mode == "correct password" {
				if err != nil || got != id {
					t.Fatal("정상 자격 대조에 실패했습니다")
				}
			} else if !errors.Is(err, ErrInvalidCredential) || !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) || err.Error() != ErrInvalidCredential.Error() || got != (model.ID{}) {
				t.Fatal("없는 계정과 잘못된 비밀번호의 응답이 다릅니다")
			}
			if compared != 1 || len(service.passwords.slots) != 0 {
				t.Fatal("대조 횟수 또는 작업 자리 반환이 다릅니다")
			}
		})
	}
}

func TestDuplicateRegistrationHashesBeforeStorage(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	var generated int
	service.passwords.generate = func(password string, cost int) (string, error) {
		generated++
		if cost != service.config.BcryptCost || len(service.passwords.slots) != 1 {
			t.Fatal("가입의 해시 비용 또는 자리 점유가 다릅니다")
		}
		return hashPassword(password, cost)
	}
	service.store = credentialStoreHooks{authStore: backend, lookup: func(context.Context, string) (store.Account, error) {
		t.Fatal("가입이 해시 전에 계정 존재를 조회했습니다")
		return store.Account{}, store.ErrNotFound
	}, create: func(ctx context.Context, account store.Account) error {
		if generated == 0 {
			t.Fatal("해시를 생성하기 전에 계정을 저장했습니다")
		}
		return backend.CreateAccount(ctx, account)
	}}
	first, err := service.Register(t.Context(), "duplicate", "first synthetic password")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := service.Register(t.Context(), "duplicate", "second synthetic password"); !errors.Is(err, ErrLoginIDExists) || got != (model.ID{}) {
		t.Fatal("동일 아이디 가입을 중복으로 분류하지 않았습니다")
	}
	account, err := backend.AccountByLoginID(t.Context(), "duplicate")
	if err != nil || account.ID != first || generated != 2 || len(backend.accounts) != 1 || len(service.passwords.slots) != 0 {
		t.Fatal("중복 가입의 해시 비용·원래 계정 보존·자리 반환이 다릅니다")
	}
	if _, err := comparePassword(account.PasswordHash, "first synthetic password"); err != nil {
		t.Fatal("중복 가입이 기존 비밀번호를 변경했습니다")
	}
}

func TestCredentialFailuresHideDetailsAndPreserveCause(t *testing.T) {
	for _, mode := range []string{"create", "lookup", "compare", "dummy", "generate", "rehash generate", "rehash update", "corrupt stored hash"} {
		t.Run(mode, func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			cause := errors.New("synthetic-private-error-marker")
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			backend.accounts["existing"] = store.Account{ID: id, LoginID: "existing", PasswordHash: "synthetic-corrupt-hash", CreatedAt: time.Now().UTC()}
			hooks := credentialStoreHooks{authStore: backend}
			switch mode {
			case "create":
				hooks.create = func(context.Context, store.Account) error { return cause }
			case "lookup":
				hooks.lookup = func(context.Context, string) (store.Account, error) { return store.Account{}, cause }
			case "compare", "dummy":
				service.passwords.compare = func(string, string) (bool, error) { return false, cause }
			case "generate":
				service.passwords.generate = func(string, int) (string, error) { return "", cause }
			case "rehash generate":
				service.passwords.compare = func(string, string) (bool, error) { return true, nil }
				service.passwords.generate = func(string, int) (string, error) { return "", cause }
			case "rehash update":
				service.passwords.compare = func(string, string) (bool, error) { return true, nil }
				hooks.update = func(context.Context, model.ID, string, string) (bool, error) { return false, cause }
			}
			service.store = hooks
			var got model.ID
			if mode == "create" || mode == "generate" {
				got, err = service.Register(t.Context(), "newuser", "synthetic password")
			} else {
				login := "existing"
				if mode == "dummy" {
					login = "missing"
				}
				got, err = service.Authenticate(t.Context(), login, "synthetic password")
			}
			if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalidCredential) || err.Error() != ErrUnavailable.Error() || got != (model.ID{}) || len(service.passwords.slots) != 0 {
				t.Fatal("내부 장애의 분류·원문 비노출·자리 반환이 다릅니다")
			}
			if mode != "corrupt stored hash" && !errors.Is(err, cause) {
				t.Fatal("내부 원인이 오류 체인에서 사라졌습니다")
			}
			if len(backend.accounts) != 1 || backend.accounts["existing"].PasswordHash != "synthetic-corrupt-hash" {
				t.Fatal("실패한 자격 처리가 저장 상태를 변경했습니다")
			}
		})
	}
}

func TestCredentialCancellationAtReturnBoundaries(t *testing.T) {
	for _, mode := range []string{"lookup", "compare", "generate", "create", "rehash update"} {
		t.Run(mode, func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			backend.accounts["existing"] = store.Account{ID: id, LoginID: "existing", PasswordHash: "old synthetic hash"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hooks := credentialStoreHooks{authStore: backend}
			service.passwords.compare = func(string, string) (bool, error) {
				if mode == "compare" {
					cancel()
				}
				return mode == "rehash update", nil
			}
			switch mode {
			case "lookup":
				hooks.lookup = func(ctx context.Context, login string) (store.Account, error) {
					cancel()
					return backend.AccountByLoginID(ctx, login)
				}
			case "generate":
				service.passwords.generate = func(string, int) (string, error) { cancel(); return "", errors.New("synthetic generation error") }
			case "create":
				hooks.create = func(context.Context, store.Account) error { cancel(); return nil }
			case "rehash update":
				hooks.update = func(context.Context, model.ID, string, string) (bool, error) { cancel(); return false, nil }
			}
			service.store = hooks
			var got model.ID
			if mode == "generate" || mode == "create" {
				got, err = service.Register(ctx, "newuser", "synthetic password")
			} else {
				got, err = service.Authenticate(ctx, "existing", "synthetic password")
			}
			if !errors.Is(err, context.Canceled) || got != (model.ID{}) || len(service.passwords.slots) != 0 || len(backend.accounts) != 1 || backend.accounts["existing"].PasswordHash != "old synthetic hash" {
				t.Fatal("취소 뒤 성공·저장 부작용 또는 자리 누수가 있습니다")
			}
		})
	}
}

type credentialStoreHooks struct {
	authStore
	create func(context.Context, store.Account) error
	lookup func(context.Context, string) (store.Account, error)
	update func(context.Context, model.ID, string, string) (bool, error)
}

func (source credentialStoreHooks) CreateAccount(ctx context.Context, account store.Account) error {
	if source.create != nil {
		return source.create(ctx, account)
	}
	return source.authStore.CreateAccount(ctx, account)
}

func (source credentialStoreHooks) AccountByLoginID(ctx context.Context, login string) (store.Account, error) {
	if source.lookup != nil {
		return source.lookup(ctx, login)
	}
	return source.authStore.AccountByLoginID(ctx, login)
}

func (source credentialStoreHooks) UpdatePasswordHashIfMatches(ctx context.Context, id model.ID, previous, next string) (bool, error) {
	if source.update != nil {
		return source.update(ctx, id, previous, next)
	}
	return source.authStore.UpdatePasswordHashIfMatches(ctx, id, previous, next)
}
