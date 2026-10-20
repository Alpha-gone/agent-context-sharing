package authz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestAccountInputContract(t *testing.T) {
	for _, test := range []struct {
		name, loginID, password string
		valid                   bool
	}{
		{"minimum", "a_0", strings.Repeat("가", 8), true},
		{"maximum", strings.Repeat("a", 32), strings.Repeat("😀", 128), true},
		{"login too short", "ab", "synthetic password", false},
		{"login too long", strings.Repeat("a", 33), "synthetic password", false},
		{"uppercase login", "Abc", "synthetic password", false},
		{"unicode login", "가나다", "synthetic password", false},
		{"punctuated login", "ab-c", "synthetic password", false},
		{"leading login space", " abc", "synthetic password", false},
		{"trailing login newline", "abc\n", "synthetic password", false},
		{"empty password", "abc", "", false},
		{"password too short", "abc", strings.Repeat("가", 7), false},
		{"password too long", "abc", strings.Repeat("😀", 129), false},
		{"invalid utf8 byte", "abc", strings.Repeat("a", 8) + "\xff", false},
		{"truncated utf8", "abc", strings.Repeat("a", 8) + "\xe2\x82", false},
		{"overlong utf8", "abc", strings.Repeat("a", 8) + "\xc0\xaf", false},
		{"encoded surrogate", "abc", strings.Repeat("a", 8) + "\xed\xa0\x80", false},
		{"replacement code point", "abc", strings.Repeat("\ufffd", 8), true},
		{"combining code points", "abc", strings.Repeat("e\u0301", 4), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newMemoryStore()
			service := testService(t, backend)
			var generated int
			service.passwords.generate = func(password string, cost int) (string, error) {
				generated++
				return hashPassword(password, cost)
			}
			before := time.Now().UTC()
			id, err := service.Register(t.Context(), test.loginID, test.password)
			if !test.valid {
				if err == nil || id != (model.ID{}) || generated != 0 || len(backend.accounts) != 0 {
					t.Fatal("잘못된 입력이 해시 생성 또는 계정 저장 경계를 통과했습니다")
				}
			} else {
				if err != nil {
					t.Fatal("유효한 계정 등록에 실패했습니다")
				}
				account, err := backend.AccountByLoginID(t.Context(), test.loginID)
				if err != nil || generated != 1 || account.ID != id || !id.IsV7() || account.LoginID != test.loginID {
					t.Fatal("계정의 식별자 또는 입력 보존 계약이 다릅니다")
				}
				if account.CreatedAt.Location() != time.UTC || account.CreatedAt.Before(before) || account.CreatedAt.After(time.Now().UTC()) {
					t.Fatal("계정 생성 시각이 현재 UTC 시각이 아닙니다")
				}
				hash, ok := strings.CutPrefix(account.PasswordHash, "bcrypt-sha256-b64-v2:")
				cost, err := bcrypt.Cost([]byte(hash))
				if !ok || err != nil || cost != service.config.BcryptCost {
					t.Fatal("저장 형식 또는 구성한 bcrypt 비용이 다릅니다")
				}
				if got, err := service.Authenticate(t.Context(), test.loginID, test.password); err != nil || got != id || generated != 1 {
					t.Fatal("현재 형식의 자격 대조가 실패하거나 불필요한 재해시를 수행했습니다")
				}
			}
			if len(service.passwords.slots) != 0 {
				t.Fatal("계정 입력 처리 뒤 비밀번호 작업 자리가 남았습니다")
			}
		})
	}
}

func TestV2PasswordPreservesLongInputAndEncodesDigest(t *testing.T) {
	for _, test := range []struct{ name, password, different string }{
		{"beyond bcrypt byte limit", strings.Repeat("a", 72) + "x", strings.Repeat("a", 72) + "y"},
		{"unicode maximum", strings.Repeat("😀", 128), strings.Repeat("😀", 127) + "😁"},
		{"nul digest", syntheticNULDigestPassword(t), "different synthetic password"},
		{"no unicode normalization", strings.Repeat("é", 8), strings.Repeat("e\u0301", 8)},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored, err := hashPassword(test.password, bcrypt.MinCost)
			if err != nil {
				t.Fatal("현재 형식의 합성 해시 생성에 실패했습니다")
			}
			digest := sha256.Sum256([]byte(test.password))
			material := base64.RawStdEncoding.EncodeToString(digest[:])
			hash, ok := strings.CutPrefix(stored, "bcrypt-sha256-b64-v2:")
			if !ok || len(material) != 43 || strings.ContainsAny(material, "\x00=") {
				t.Fatal("패딩 없는 표준 Base64 저장 형식이 아닙니다")
			}
			if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(material)); err != nil {
				t.Fatal("독립적으로 만든 표준 Base64 입력과 저장 해시가 다릅니다")
			}
			if legacy, err := comparePassword(stored, test.password); err != nil || legacy {
				t.Fatal("현재 형식의 올바른 비밀번호를 거부했습니다")
			}
			if _, err := comparePassword(stored, test.different); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
				t.Fatal("서로 다른 비밀번호를 구분하지 못했습니다")
			}
		})
	}
}

func TestLegacyPasswordRehashUsesSameWorkSlot(t *testing.T) {
	for _, format := range []string{"direct", "raw digest"} {
		t.Run(format, func(t *testing.T) {
			password := syntheticNULDigestPassword(t)
			oldHash := syntheticLegacyPasswordHash(t, format, password)
			backend := newMemoryStore()
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.CreateAccount(t.Context(), store.Account{ID: id, LoginID: "legacy", PasswordHash: oldHash}); err != nil {
				t.Fatal(err)
			}
			service := testService(t, backend)
			for range passwordWorkLimit - 1 {
				if err := service.passwords.acquire(t.Context()); err != nil {
					t.Fatal(err)
				}
				defer service.passwords.release()
			}
			checkSlot := func() {
				if len(service.passwords.slots) != passwordWorkLimit {
					t.Fatal("레거시 대조·생성·저장이 같은 비밀번호 작업 자리를 유지하지 않습니다")
				}
			}
			var compared, generated, updated int
			service.passwords.compare = func(stored, password string) (bool, error) {
				checkSlot()
				compared++
				return comparePassword(stored, password)
			}
			service.passwords.generate = func(password string, cost int) (string, error) {
				checkSlot()
				generated++
				return hashPassword(password, cost)
			}
			service.store = passwordUpdateObserver{authStore: backend, beforeUpdate: func() { checkSlot(); updated++ }}
			if _, err := service.Authenticate(t.Context(), "legacy", "wrong synthetic password"); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
				t.Fatal("레거시의 잘못된 비밀번호를 거부하지 않았습니다")
			}
			account, _ := backend.AccountByLoginID(t.Context(), "legacy")
			if generated != 0 || updated != 0 || account.PasswordHash != oldHash {
				t.Fatal("실패한 레거시 대조 뒤 해시가 바뀌었습니다")
			}
			if got, err := service.Authenticate(t.Context(), "legacy", password); err != nil || got != id {
				t.Fatal("여유 작업 자리 하나에서 레거시 인증·재해시에 실패했습니다")
			}
			account, _ = backend.AccountByLoginID(t.Context(), "legacy")
			legacy, err := comparePassword(account.PasswordHash, password)
			if err != nil || legacy || compared != 2 || generated != 1 || updated != 1 || len(service.passwords.slots) != passwordWorkLimit-1 {
				t.Fatal("성공한 레거시 인증의 현재 형식 전환 또는 자리 반환이 다릅니다")
			}
		})
	}
}

func TestMalformedPasswordHashesNeverRehash(t *testing.T) {
	for _, test := range []struct{ name, stored string }{
		{"empty", ""},
		{"direct truncated", "$2a$04$short"},
		{"current empty", "bcrypt-sha256-b64-v2:"},
		{"current malformed", "bcrypt-sha256-b64-v2:not-bcrypt"},
		{"legacy empty", "bcrypt-sha256-v1:"},
		{"legacy malformed", "bcrypt-sha256-v1:not-bcrypt"},
		{"unknown format", "bcrypt-sha256-b64-v3:not-bcrypt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newMemoryStore()
			id, err := model.NewID()
			if err != nil {
				t.Fatal(err)
			}
			// 저장소 손상 표본은 실제 SQL 제약을 통과하는 가입 경로와 분리한다.
			backend.accounts["malformed"] = store.Account{ID: id, LoginID: "malformed", PasswordHash: test.stored}
			service := testService(t, backend)
			service.passwords.generate = func(string, int) (string, error) {
				t.Fatal("잘못된 해시를 성공한 대조로 간주했습니다")
				return "", errors.New("unexpected generation")
			}
			service.store = passwordUpdateObserver{authStore: backend, beforeUpdate: func() { t.Fatal("잘못된 해시를 갱신했습니다") }}
			if got, err := service.Authenticate(t.Context(), "malformed", "synthetic password"); err == nil || got != (model.ID{}) {
				t.Fatal("잘못된 저장 형식을 인증했습니다")
			}
			account, _ := backend.AccountByLoginID(t.Context(), "malformed")
			if account.PasswordHash != test.stored || len(service.passwords.slots) != 0 {
				t.Fatal("잘못된 저장 형식의 처리 뒤 상태 변경 또는 자리 누수가 있습니다")
			}
		})
	}
}

type passwordUpdateObserver struct {
	authStore
	beforeUpdate func()
}

func (source passwordUpdateObserver) UpdatePasswordHashIfMatches(ctx context.Context, id model.ID, previous, next string) (bool, error) {
	source.beforeUpdate()
	return source.authStore.UpdatePasswordHashIfMatches(ctx, id, previous, next)
}

func syntheticNULDigestPassword(t *testing.T) string {
	t.Helper()
	for candidate := range 10_000 {
		password := fmt.Sprintf("synthetic-fixture-%d", candidate)
		digest := sha256.Sum256([]byte(password))
		if bytes.Contains(digest[:], []byte{0}) {
			return password
		}
	}
	t.Fatal("NUL 다이제스트 합성 표본을 만들지 못했습니다")
	return ""
}

func syntheticLegacyPasswordHash(t *testing.T, format, password string) string {
	t.Helper()
	material := []byte(password)
	prefix := ""
	if format == "raw digest" {
		digest := sha256.Sum256(material)
		material = digest[:]
		prefix = "bcrypt-sha256-v1:"
	}
	hash, err := bcrypt.GenerateFromPassword(material, bcrypt.MinCost)
	if err != nil {
		t.Fatal("레거시 합성 해시 생성에 실패했습니다")
	}
	return prefix + string(hash)
}
