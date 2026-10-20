package authz

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/json/v2"
	"fmt"

	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
)

// publicSigningKey는 저장 데이터가 공개·검증 경계를 통과하기 전에 ES256 결합을 확인한다.
// JSON 해석 오류도 원문을 포함할 수 있으므로 고정된 내부 오류만 반환한다.
func publicSigningKey(item store.SigningKey) (jose.JSONWebKey, error) {
	var key jose.JSONWebKey
	if json.Unmarshal([]byte(item.PublicKey), &key) != nil || !key.Valid() || !key.IsPublic() ||
		item.ID == "" || key.KeyID != item.ID || item.Algorithm != string(jose.ES256) ||
		key.Algorithm != string(jose.ES256) || key.Use != "sig" {
		return jose.JSONWebKey{}, fmt.Errorf("%w: 저장된 공개 키 형식이 올바르지 않다", ErrUnavailable)
	}
	public, ok := key.Key.(*ecdsa.PublicKey)
	if !ok || public.Curve != elliptic.P256() {
		return jose.JSONWebKey{}, fmt.Errorf("%w: 저장된 공개 키가 P-256이 아니다", ErrUnavailable)
	}
	return key, nil
}

func privateSigningKey(item store.SigningKey) (jose.JSONWebKey, error) {
	public, err := publicSigningKey(item)
	if err != nil {
		return jose.JSONWebKey{}, err
	}
	var key jose.JSONWebKey
	if json.Unmarshal([]byte(item.PrivateKey), &key) != nil || !key.Valid() ||
		key.KeyID != item.ID || key.Algorithm != string(jose.ES256) || key.Use != "sig" {
		return jose.JSONWebKey{}, fmt.Errorf("%w: 저장된 개인 키 형식이 올바르지 않다", ErrUnavailable)
	}
	private, ok := key.Key.(*ecdsa.PrivateKey)
	if !ok || !private.PublicKey.Equal(public.Key) {
		return jose.JSONWebKey{}, fmt.Errorf("%w: 저장된 서명 키 쌍이 일치하지 않는다", ErrUnavailable)
	}
	return key, nil
}
