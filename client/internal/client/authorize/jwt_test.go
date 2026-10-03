package authorize

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/client/internal/client/contract"
)

func decodeProof(t *testing.T, raw string) (jwk, map[string]any) {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatal("compact JWT가 아닙니다")
	}
	h, _ := encoding.DecodeString(parts[0])
	c, _ := encoding.DecodeString(parts[1])
	sig, _ := encoding.DecodeString(parts[2])
	var header struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		JWK jwk    `json:"jwk"`
	}
	var claims map[string]any
	if json.Unmarshal(h, &header) != nil || json.Unmarshal(c, &claims) != nil || header.Typ != "dpop+jwt" || header.Alg != "ES256" || len(header.JWK.D) != 0 || len(sig) != 64 {
		t.Fatal("proof JOSE 계약이 다릅니다")
	}
	key, err := header.JWK.publicKey()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, hash[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("proof 서명이 다릅니다")
	}
	return header.JWK, claims
}

func TestProofBindingAndFreshness(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	seen := make(map[string]bool)
	for range 100 {
		proof, err := makeProof(key, now, "https://RESOURCE.test:443/%6Dcp?ignored=1", "secret-token")
		if err != nil {
			t.Fatal(err)
		}
		jwk, claims := decodeProof(t, proof)
		if thumbprint(jwk) != thumbprint(publicJWK(key)) || claims["htm"] != "POST" || claims["htu"] != resourceURL || claims["iat"] != float64(now.Unix()) || claims["ath"] != tokenHash("secret-token") {
			t.Fatal("요청·토큰·시간 결합이 다릅니다")
		}
		id, ok := claims["jti"].(string)
		parsed, err := uuid.Parse(id)
		if !ok || err != nil || parsed[6]>>4 != 7 || seen[id] {
			t.Fatal("jti가 없거나 재사용됐습니다")
		}
		seen[id] = true
	}
	proof, err := makeProof(key, now, issuerURL+"/token", "")
	if err != nil {
		t.Fatal(err)
	}
	_, claims := decodeProof(t, proof)
	if _, present := claims["ath"]; present {
		t.Fatal("토큰 교환 proof에 ath를 보냈습니다")
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if thumbprint(publicJWK(other)) == thumbprint(publicJWK(key)) {
		t.Fatal("새 프로세스가 같은 키를 썼습니다")
	}
}

func TestTokenVerificationFailures(t *testing.T) {
	f := newFixture(t)
	key := publicJWK(f.signer)
	key.Kid = "key"
	for _, field := range []string{"iss", "sub", "aud", "exp", "cnf", "nbf", "signature", "alg", "kid", "crit", "duplicate"} {
		t.Run(field, func(t *testing.T) {
			header := map[string]any{"alg": "ES256", "kid": "key"}
			claims := map[string]any{"iss": issuerURL, "sub": "account-a", "aud": resourceURL, "exp": f.now.Unix() + 3600, "cnf": map[string]string{"jkt": "process"}}
			switch field {
			case "iss":
				claims[field] = "other"
			case "sub":
				claims[field] = ""
			case "aud":
				claims[field] = []string{"other"}
			case "exp":
				claims[field] = f.now.Unix()
			case "cnf":
				claims[field] = map[string]string{"jkt": "other"}
			case "nbf":
				claims[field] = f.now.Unix() + 100
			case "alg":
				header[field] = "HS256"
			case "kid":
				header[field] = "missing"
			case "crit":
				header[field] = []string{"extension"}
			}
			var raw string
			var err error
			if field == "duplicate" {
				raw, err = signJWT(f.signer, header, jsontext.Value(`{"iss":"first","iss":"second"}`))
				if err == nil {
					t.Fatal("중복 JSON 속성을 서명했습니다")
				}
				return
			}
			raw, err = signJWT(f.signer, header, claims)
			if err != nil {
				t.Fatal(err)
			}
			if field == "signature" {
				parts := strings.Split(raw, ".")
				sig, _ := encoding.DecodeString(parts[2])
				sig[0] ^= 1
				parts[2] = encoding.EncodeToString(sig)
				raw = strings.Join(parts, ".")
			}
			if _, err := verifyToken(raw, []jwk{key}, issuerURL, resourceURL, "process", f.now); !errors.Is(err, contract.ErrProtocol) {
				t.Fatal("잘못된 JWT를 저장했습니다")
			}
		})
	}
	valid := f.token("process", "account-a", f.now.Unix()+3600)
	if _, err := verifyToken(valid, []jwk{key, key}, issuerURL, resourceURL, "process", f.now); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("중복 kid를 허용했습니다")
	}
	key.D = jsontext.Value(`"private-key"`)
	if _, err := key.publicKey(); !errors.Is(err, contract.ErrProtocol) {
		t.Fatal("개인 JWK를 허용했습니다")
	}
}

func TestIncomingJWTRejectsDuplicateProperties(t *testing.T) {
	f := newFixture(t)
	key := publicJWK(f.signer)
	key.Kid = "key"
	valid, err := signJWT(f.signer, map[string]string{"alg": "ES256", "kid": "key"}, map[string]any{"iss": issuerURL, "sub": "account-a", "aud": resourceURL, "exp": f.now.Unix() + 3600, "cnf": map[string]string{"jkt": "process"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []int{0, 1} {
		parts := strings.Split(valid, ".")
		raw, _ := encoding.DecodeString(parts[part])
		duplicate := `"kid":"key",`
		if part == 1 {
			duplicate = `"iss":"` + issuerURL + `",`
		}
		parts[part] = encoding.EncodeToString([]byte("{" + duplicate + string(raw[1:])))
		// 외부 발급자가 서명한 중복 속성을 검증한다. signer의 JSON 검사에 의존하지 않는다.
		hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		r, s, err := ecdsa.Sign(rand.Reader, f.signer, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		signature := make([]byte, 64)
		r.FillBytes(signature[:32])
		s.FillBytes(signature[32:])
		parts[2] = encoding.EncodeToString(signature)
		if _, err := verifyToken(strings.Join(parts, "."), []jwk{key}, issuerURL, resourceURL, "process", f.now); !errors.Is(err, contract.ErrProtocol) {
			t.Fatal("유효한 서명으로 중복 JSON 속성 검사를 우회했습니다")
		}
	}
}
