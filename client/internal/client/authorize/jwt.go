// Package authorize는 브라우저 인가와 메모리 전용 DPoP 자격 증명을 소유한다.
package authorize

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
)

var encoding = base64.RawURLEncoding.Strict()

var errUnknownSigningKey = fmt.Errorf("알 수 없는 서명 키: %w", contract.ErrProtocol)

type jwk struct {
	Kty string         `json:"kty"`
	Crv string         `json:"crv"`
	X   string         `json:"x"`
	Y   string         `json:"y"`
	Kid string         `json:"kid,omitempty"`
	Alg string         `json:"alg,omitempty"`
	Use string         `json:"use,omitempty"`
	Ops []string       `json:"key_ops,omitempty"`
	D   jsontext.Value `json:"d,omitempty"`
}

func publicJWK(key *ecdsa.PrivateKey) jwk {
	return jwk{Kty: "EC", Crv: "P-256", X: encoding.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: encoding.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}
}

func thumbprint(key jwk) string {
	// RFC 7638은 필수 공개 속성의 사전순 JSON을 해시한다.
	canonical := `{"crv":"P-256","kty":"EC","x":"` + key.X + `","y":"` + key.Y + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return encoding.EncodeToString(sum[:])
}

func (key jwk) publicKey() (*ecdsa.PublicKey, error) {
	x, ex := encoding.DecodeString(key.X)
	y, ey := encoding.DecodeString(key.Y)
	if key.Kty != "EC" || key.Crv != "P-256" || len(key.D) != 0 || ex != nil || ey != nil || len(x) != 32 || len(y) != 32 || key.Alg != "" && key.Alg != "ES256" || key.Use != "" && key.Use != "sig" || len(key.Ops) != 0 && !slices.Contains(key.Ops, "verify") {
		return nil, contract.ErrProtocol
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, contract.ErrProtocol
	}
	return pub, nil
}

func signJWT(key *ecdsa.PrivateKey, header, claims any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", contract.ErrProtocol
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", contract.ErrProtocol
	}
	payload := encoding.EncodeToString(h) + "." + encoding.EncodeToString(c)
	hash := sha256.Sum256([]byte(payload))
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		return "", contract.ErrProtocol
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return payload + "." + encoding.EncodeToString(sig), nil
}

func makeProof(key *ecdsa.PrivateKey, now time.Time, target, token string) (string, error) {
	target, err := normalizedURL(target, true)
	if err != nil {
		return "", err
	}
	header := struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		JWK jwk    `json:"jwk"`
	}{"dpop+jwt", "ES256", publicJWK(key)}
	claims := struct {
		Method string `json:"htm"`
		Target string `json:"htu"`
		Issued int64  `json:"iat"`
		ID     string `json:"jti"`
		Hash   string `json:"ath,omitempty"`
	}{Method: "POST", Target: target, Issued: now.UTC().Unix(), ID: uuid.NewV7().String()}
	if token != "" {
		hash := sha256.Sum256([]byte(token))
		claims.Hash = encoding.EncodeToString(hash[:])
	}
	return signJWT(key, header, claims)
}

type tokenClaims struct {
	Issuer       string         `json:"iss"`
	Subject      string         `json:"sub"`
	Audience     jsontext.Value `json:"aud"`
	Expires      int64          `json:"exp"`
	NotBefore    *int64         `json:"nbf,omitempty"`
	Confirmation struct {
		Thumbprint string `json:"jkt"`
	} `json:"cnf"`
}

func verifyToken(raw string, keys []jwk, issuer, resource, jkt string, now time.Time) (tokenClaims, error) {
	var claims tokenClaims
	if len(raw) > 64<<10 {
		return claims, contract.ErrProtocol
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return claims, contract.ErrProtocol
	}
	headerBytes, eh := encoding.DecodeString(parts[0])
	body, eb := encoding.DecodeString(parts[1])
	signature, es := encoding.DecodeString(parts[2])
	var header struct {
		Alg  string         `json:"alg"`
		Kid  string         `json:"kid"`
		Crit jsontext.Value `json:"crit"`
		B64  jsontext.Value `json:"b64"`
	}
	if eh != nil || eb != nil || es != nil || len(signature) != 64 || json.Unmarshal(headerBytes, &header) != nil || header.Alg != "ES256" || header.Kid == "" || len(header.Crit) != 0 || len(header.B64) != 0 || jsontext.Value(body).Kind() != '{' || json.Unmarshal(body, &claims) != nil {
		return claims, contract.ErrProtocol
	}
	var key *ecdsa.PublicKey
	for _, candidate := range keys {
		if candidate.Kid == header.Kid {
			if key != nil {
				return claims, contract.ErrProtocol
			}
			var err error
			key, err = candidate.publicKey()
			if err != nil {
				return claims, err
			}
		}
	}
	if key == nil {
		return claims, errUnknownSigningKey
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, hash[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return claims, contract.ErrProtocol
	}
	var audiences []string
	if claims.Audience.Kind() == '"' {
		var audience string
		if json.Unmarshal(claims.Audience, &audience) != nil {
			return claims, contract.ErrProtocol
		}
		audiences = []string{audience}
	} else if json.Unmarshal(claims.Audience, &audiences) != nil {
		return claims, contract.ErrProtocol
	}
	if claims.Issuer != issuer || claims.Subject == "" || !slices.Contains(audiences, resource) || claims.Expires <= now.Unix() || claims.NotBefore != nil && *claims.NotBefore > now.Unix() || claims.Confirmation.Thumbprint != jkt {
		return claims, contract.ErrProtocol
	}
	return claims, nil
}

func httpsURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#") {
		return nil, contract.ErrProtocol
	}
	return u, nil
}

// normalizedURL은 scheme·host·기본 포트·비예약 문자를 정규화한다.
func normalizedURL(raw string, proof bool) (string, error) {
	u, err := httpsURL(raw)
	if err != nil {
		return "", err
	}
	if !proof && (u.RawQuery != "" || u.ForceQuery) {
		return "", contract.ErrProtocol
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "443" {
		port = ""
	}
	if port != "" || strings.Contains(host, ":") {
		host = strings.TrimSuffix(net.JoinHostPort(host, port), ":")
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	var normalized strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] != '%' {
			normalized.WriteByte(path[i])
			continue
		}
		if i+2 >= len(path) {
			return "", contract.ErrProtocol
		}
		hex := path[i+1 : i+3]
		b, err := url.PathUnescape("%" + hex)
		if err != nil {
			return "", contract.ErrProtocol
		}
		c := b[0]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.ContainsRune("-._~", rune(c)) {
			normalized.WriteByte(c)
		} else {
			normalized.WriteString("%" + strings.ToUpper(hex))
		}
		i += 2
	}
	return "https://" + host + normalized.String(), nil
}
