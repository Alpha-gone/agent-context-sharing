package authz

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const dpopProofLifetime = time.Minute

// DPoPRequest는 DPoP proof가 결합해야 하는 HTTP 요청의 인증 요소다.
// 토큰과 proof 원문은 이 구조를 벗어나 로그나 저장소로 전달하지 않는다.
type DPoPRequest struct {
	AccessToken string
	Proof       string
	Method      string
	Target      string
}

type dpopClaims struct {
	jwt.Claims
	Method          string `json:"htm"`
	Target          string `json:"htu"`
	AccessTokenHash string `json:"ath,omitempty"`
}

// verifyDPoPProof는 ES256 proof의 JOSE·요청 결합·토큰 결합을 확인하고, 모든 검증을
// 통과한 proof만 저장소에 단일 사용으로 예약한다.
func (s *Service) verifyDPoPProof(ctx context.Context, request DPoPRequest, expectedThumbprint string, requiresAccessTokenHash bool) (string, error) {
	parsed, err := jwt.ParseSigned(request.Proof, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(parsed.Headers) != 1 {
		return "", ErrInvalidDPoPProof
	}
	header := parsed.Headers[0]
	typ, ok := header.ExtraHeaders[jose.HeaderKey("typ")].(string)
	if !ok || typ != "dpop+jwt" || header.JSONWebKey == nil || !header.JSONWebKey.IsPublic() {
		return "", ErrInvalidDPoPProof
	}
	publicKey, ok := header.JSONWebKey.Key.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return "", ErrInvalidDPoPProof
	}
	var claims dpopClaims
	if err := parsed.Claims(publicKey, &claims); err != nil || claims.ID == "" || claims.IssuedAt == nil {
		return "", ErrInvalidDPoPProof
	}
	issuedAt := claims.IssuedAt.Time()
	now := time.Now().UTC()
	if issuedAt.Before(now.Add(-dpopProofLifetime)) || issuedAt.After(now.Add(dpopProofLifetime)) || claims.Method != request.Method || !sameDPoPTarget(claims.Target, request.Target) {
		return "", ErrInvalidDPoPProof
	}
	thumbprint, err := header.JSONWebKey.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", ErrInvalidDPoPProof
	}
	jkt := base64.RawURLEncoding.EncodeToString(thumbprint)
	if expectedThumbprint != "" && jkt != expectedThumbprint {
		return "", ErrInvalidDPoPProof
	}
	if requiresAccessTokenHash && claims.AccessTokenHash != dpopAccessTokenHash(request.AccessToken) {
		return "", ErrInvalidDPoPProof
	}
	proofIDHash := sha256.Sum256([]byte(claims.ID))
	reserved, err := s.store.ReserveDPoPProof(ctx, jkt, proofIDHash[:], issuedAt.Add(dpopProofLifetime))
	if err != nil {
		return "", errors.Join(ErrUnavailable, err)
	}
	if !reserved {
		return "", ErrInvalidDPoPProof
	}
	return jkt, nil
}

func dpopAccessTokenHash(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func sameDPoPTarget(raw, expected string) bool {
	actual, ok := normalizedDPoPTarget(raw)
	if !ok {
		return false
	}
	want, ok := normalizedDPoPTarget(expected)
	return ok && actual == want
}

// normalizedDPoPTarget은 RFC 9449가 htu 비교에 참조하는 RFC 3986의 구문·scheme 기반
// 정규화를 적용한다. 대소문자, 기본 포트, 빈 경로와 퍼센트 인코딩 표기 차이만 없애며
// query·fragment·사용자 정보가 있는 값은 정규 URL이 아니므로 거부한다.
func normalizedDPoPTarget(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host, port := strings.ToLower(parsed.Hostname()), parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	authority := host
	if port != "" || strings.Contains(host, ":") {
		// JoinHostPort가 IPv6 주소에 대괄호를 붙인다. 포트가 없으면 대괄호만 남긴다.
		authority = strings.TrimSuffix(net.JoinHostPort(host, port), ":")
	}
	path, ok := normalizedPercentEncoding(parsed.EscapedPath())
	if !ok {
		return "", false
	}
	if path == "" {
		path = "/"
	}
	return parsed.Scheme + "://" + authority + path, true
}

// normalizedPercentEncoding은 비예약 문자의 퍼센트 인코딩만 풀고 나머지 16진수를 대문자로
// 맞춘다. %2F처럼 예약 문자를 인코딩한 값은 `/`와 뜻이 달라 풀지 않는다.
func normalizedPercentEncoding(escaped string) (string, bool) {
	var normalized strings.Builder
	for i := 0; i < len(escaped); i++ {
		if escaped[i] != '%' {
			normalized.WriteByte(escaped[i])
			continue
		}
		if i+2 >= len(escaped) {
			return "", false
		}
		decoded, err := hex.DecodeString(escaped[i+1 : i+3])
		if err != nil {
			return "", false
		}
		if unreserved(decoded[0]) {
			normalized.WriteByte(decoded[0])
		} else {
			normalized.WriteString("%" + strings.ToUpper(escaped[i+1:i+3]))
		}
		i += 2
	}
	return normalized.String(), true
}

func unreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~", c) >= 0
}
