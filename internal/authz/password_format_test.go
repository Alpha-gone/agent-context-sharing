package authz

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestRegisteredPasswordUsesDocumentedV2Format(t *testing.T) {
	password := strings.Repeat("한", 128)
	backend := newMemoryStore()
	service := testService(t, backend)
	if _, err := service.Register(t.Context(), "format", password); err != nil {
		t.Fatal(err)
	}
	account, err := backend.AccountByLoginID(t.Context(), "format")
	if err != nil {
		t.Fatal(err)
	}
	hash, ok := strings.CutPrefix(account.PasswordHash, "bcrypt-sha256-b64-v2:")
	if !ok {
		t.Fatal("SDD가 정의한 v2 접두사가 아니다")
	}
	digest := sha256.Sum256([]byte(password))
	material := base64.RawStdEncoding.EncodeToString(digest[:])
	if len(material) != 43 || strings.ContainsAny(material, "\x00=") {
		t.Fatal("패딩 없는 표준 Base64 형식이 아니다")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(material)); err != nil {
		t.Fatalf("SDD의 v2 입력으로 대조 실패: %v", err)
	}
}
