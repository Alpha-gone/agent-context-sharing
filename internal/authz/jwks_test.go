package authz

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestJWKSBeforeFirstTokenInitializesPublicKeyWithoutRotation(t *testing.T) {
	service := testService(t, newMemoryStore())
	first, err := service.JWKS(t.Context())
	if err != nil || len(first.Keys) != 1 {
		t.Fatalf("첫 토큰 전 공개 키 수=%d, 오류=%v", len(first.Keys), err)
	}
	second, err := service.JWKS(t.Context())
	if err != nil || len(second.Keys) != 1 || first.Keys[0].KeyID != second.Keys[0].KeyID {
		t.Fatal("JWKS 조회가 키를 회전했다")
	}
	encoded, err := json.Marshal(first)
	if err != nil || strings.Contains(string(encoded), `"d":`) || !first.Keys[0].IsPublic() {
		t.Fatal("JWKS에 개인 키가 노출됐다")
	}
}
