package authz

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestPasswordWorkBoundsAllCredentialPaths(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	id, err := service.Register(t.Context(), "existing", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	observed := &credentialLookupStore{authStore: backend}
	service.store = observed
	started := make(chan struct{}, passwordWorkLimit)
	finish := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	t.Cleanup(release)
	var active, peak, generated, compared atomic.Int32
	work := func() {
		count := active.Add(1)
		for old := peak.Load(); count > old; old = peak.Load() {
			if peak.CompareAndSwap(old, count) {
				break
			}
		}
		defer active.Add(-1)
		started <- struct{}{}
		<-finish
	}
	service.passwords.generate = func(string, int) (string, error) {
		generated.Add(1)
		work()
		return "generated", nil
	}
	service.passwords.compare = func(stored, _ string) (bool, error) {
		compared.Add(1)
		work()
		if stored == service.dummyPasswordHash {
			return false, bcrypt.ErrMismatchedHashAndPassword
		}
		return false, nil
	}
	results := make(chan error, passwordWorkLimit)
	var group sync.WaitGroup
	group.Go(func() { _, err := service.Register(t.Context(), "newuser", "correct password"); results <- err })
	group.Go(func() { _, err := service.Register(t.Context(), "existing", "correct password"); results <- err })
	group.Go(func() { _, err := service.Authenticate(t.Context(), "missing", "wrong password"); results <- err })
	group.Go(func() {
		got, err := service.Authenticate(t.Context(), "existing", "correct password")
		if got != id && err == nil {
			err = errors.New("wrong account")
		}
		results <- err
	})
	for range passwordWorkLimit {
		awaitPasswordWork(t, started)
	}
	if _, err := service.Authenticate(t.Context(), "existing", "correct password"); !errors.Is(err, ErrPasswordBusy) {
		t.Fatalf("상한 초과 로그인 = %v", err)
	}
	if _, err := service.Register(t.Context(), "overflow", "correct password"); !errors.Is(err, ErrPasswordBusy) {
		t.Fatalf("상한 초과 가입 = %v", err)
	}
	if peak.Load() != passwordWorkLimit || generated.Load() != 2 || compared.Load() != 2 {
		t.Fatal("공유 bcrypt 실행 상한 또는 더미·중복 경로가 다릅니다")
	}
	if observed.lookups.Load() != 2 {
		t.Fatal("자리 초과 요청에서 계정을 조회했습니다")
	}
	release()
	group.Wait()
	close(results)
	var success, duplicate, mismatch int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
			mismatch++
		case err.Error() == "duplicate":
			duplicate++
		default:
			t.Fatal(err)
		}
	}
	if success != 2 || duplicate != 1 || mismatch != 1 || len(service.passwords.slots) != 0 {
		t.Fatal("정상·중복·없는 계정 결과 또는 자리 반환이 다릅니다")
	}
	if _, err := service.Register(t.Context(), "recovered", "correct password"); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordWorkCancellationKeepsLegacyRehashSlot(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	id, _ := model.NewID()
	if err := backend.CreateAccount(t.Context(), store.Account{ID: id, LoginID: "legacy", PasswordHash: "oldhash"}); err != nil {
		t.Fatal(err)
	}
	service.passwords.compare = func(string, string) (bool, error) { return true, nil }
	started, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	t.Cleanup(release)
	service.passwords.generate = func(string, int) (string, error) { close(started); <-finish; return "newhash", nil }
	for range passwordWorkLimit - 1 {
		if err := service.passwords.acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer service.passwords.release()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := service.Authenticate(ctx, "legacy", "correct password"); result <- err }()
	awaitPasswordWork(t, started)
	cancel()
	if _, err := service.Authenticate(t.Context(), "missing", "wrong password"); !errors.Is(err, ErrPasswordBusy) {
		t.Fatalf("취소된 실행 중 해시의 자리 = %v", err)
	}
	release()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("취소 전파 = %v", err)
	}
	account, err := backend.AccountByLoginID(t.Context(), "legacy")
	if err != nil || account.PasswordHash != "oldhash" || len(service.passwords.slots) != passwordWorkLimit-1 {
		t.Fatal("취소 뒤 해시 갱신 또는 자리 누수")
	}
}

func TestPasswordWorkCancellationAndFailuresReleaseSlots(t *testing.T) {
	for _, mode := range []string{"canceled before", "canceled generation", "generation error", "lookup error", "comparison error", "canceled comparison"} {
		t.Run(mode, func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls int
			service.passwords.generate = func(string, int) (string, error) {
				calls++
				if mode == "canceled generation" {
					cancel()
					return "generated", nil
				}
				return "", errors.New("generation failed")
			}
			service.passwords.compare = func(string, string) (bool, error) {
				calls++
				if mode == "canceled comparison" {
					cancel()
				}
				return false, bcrypt.ErrMismatchedHashAndPassword
			}
			if mode == "canceled before" {
				cancel()
			}
			if mode == "lookup error" {
				service.store = failingCredentialStore{authStore: backend}
			}
			var err error
			if strings.Contains(mode, "comparison") || mode == "lookup error" {
				_, err = service.Authenticate(ctx, "missing", "wrong password")
			} else {
				_, err = service.Register(ctx, "unused", "correct password")
			}
			if err == nil || len(service.passwords.slots) != 0 || len(backend.accounts) != 0 {
				t.Fatal("오류·취소 뒤 부작용 또는 자리 누수")
			}
			if strings.HasPrefix(mode, "canceled") && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if (mode == "canceled before" || mode == "lookup error") && calls != 0 {
				t.Fatal("해시 실행 전에 거부하지 않았습니다")
			}
		})
	}
}

type failingCredentialStore struct{ authStore }

type credentialLookupStore struct {
	authStore
	lookups atomic.Int32
}

func (source *credentialLookupStore) AccountByLoginID(ctx context.Context, loginID string) (store.Account, error) {
	source.lookups.Add(1)
	return source.authStore.AccountByLoginID(ctx, loginID)
}

func (failingCredentialStore) AccountByLoginID(context.Context, string) (store.Account, error) {
	return store.Account{}, errors.New("lookup failed")
}

func TestMissingAccountNeverAuthenticatesEmptyPassword(t *testing.T) {
	service := testService(t, newMemoryStore())
	if _, err := service.Authenticate(t.Context(), "missing", ""); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatal(err)
	}
}

func awaitPasswordWork(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("비밀번호 시험 작업이 시작되지 않았습니다")
	}
}
