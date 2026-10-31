package authz

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestDeletedSigningKeyExpiresFromCache(t *testing.T) {
	backend := newMemoryStore()
	service := testService(t, backend)
	id, err := service.Register(t.Context(), "removed", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(t.Context(), id, "web")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.keys = nil
	backend.mu.Unlock()
	service.cache.mu.Lock()
	service.cache.keysLoadedAt = time.Now().Add(-publicKeyTTL)
	service.cache.mu.Unlock()
	if _, _, err := service.Verify(t.Context(), token.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("deleted key remained trusted: %v", err)
	}
}

func TestExpiredKeyCacheFailsClosed(t *testing.T) {
	backend := &failingStore{memoryStore: newMemoryStore()}
	service := testService(t, backend)
	key, err := service.activeKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.cache.publicKey(t.Context(), backend, key.ID); err != nil {
		t.Fatal(err)
	}
	backend.signingKeyFails = true
	service.cache.keysLoadedAt = time.Now().Add(-publicKeyTTL)
	if _, err := service.cache.publicKey(t.Context(), backend, key.ID); !errors.Is(err, errStoreUnavailable) {
		t.Fatalf("stale key used after reload failure: %v", err)
	}
}

func TestCurrentPasswordCostUpgradeOnlyAfterSuccess(t *testing.T) {
	for _, storedCost := range []int{4, 6} {
		t.Run(string(rune('0'+storedCost)), func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			service.config.BcryptCost = storedCost
			_, err := service.Register(t.Context(), "cost", "correct horse battery staple")
			if err != nil {
				t.Fatal(err)
			}
			before, err := backend.AccountByLoginID(t.Context(), "cost")
			if err != nil {
				t.Fatal(err)
			}
			service.config.BcryptCost = 5
			if _, err := service.Authenticate(t.Context(), "cost", "wrong password"); !errors.Is(err, ErrInvalidCredential) {
				t.Fatal(err)
			}
			failed, err := backend.AccountByLoginID(t.Context(), "cost")
			if err != nil {
				t.Fatal(err)
			}
			if failed.PasswordHash != before.PasswordHash {
				t.Fatal("failed login changed hash")
			}
			if _, err := service.Authenticate(t.Context(), "cost", "correct horse battery staple"); err != nil {
				t.Fatal(err)
			}
			after, err := backend.AccountByLoginID(t.Context(), "cost")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := strings.CutPrefix(after.PasswordHash, passwordHashPrefix)
			cost, err := bcrypt.Cost([]byte(raw))
			if err != nil || cost != max(5, storedCost) {
				t.Fatalf("cost = %d: %v", cost, err)
			}
			if storedCost == 6 && after.PasswordHash != before.PasswordHash {
				t.Fatal("stronger hash changed")
			}
		})
	}
}
