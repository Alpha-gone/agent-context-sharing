package authorize

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"agent_context_sharing/client/internal/client/contract"
)

const responseLimit = 32 << 20

type statusError struct{ status int }

func (statusError) Error() string { return "인가 HTTP 응답이 성공하지 않았습니다." }

type metadata struct {
	Issuer         string   `json:"issuer"`
	Authorization  string   `json:"authorization_endpoint"`
	Token          string   `json:"token_endpoint"`
	JWKS           string   `json:"jwks_uri"`
	Challenges     []string `json:"code_challenge_methods_supported"`
	Algorithms     []string `json:"dpop_signing_alg_values_supported"`
	Scopes         []string `json:"scopes_supported"`
	AuthMethods    []string `json:"token_endpoint_auth_methods_supported"`
	Responses      []string `json:"response_types_supported"`
	Grants         []string `json:"grant_types_supported"`
	IssuerResponse bool     `json:"authorization_response_iss_parameter_supported"`
	keys           []jwk
	resource       string
}

func wellKnown(raw, suffix string) (string, error) {
	u, err := httpsURL(raw)
	if err != nil || u.RawQuery != "" || u.ForceQuery {
		return "", contract.ErrProtocol
	}
	path := "/.well-known/" + suffix + strings.TrimSuffix(u.EscapedPath(), "/")
	u.Path, err = url.PathUnescape(path)
	if err != nil {
		return "", contract.ErrProtocol
	}
	u.RawPath = path
	return u.String(), nil
}

func (m *Manager) httpClient(discovery bool) *http.Client {
	return &http.Client{Transport: m.transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !discovery {
			return http.ErrUseLastResponse
		}
		if len(via) > 3 {
			return contract.ErrProtocol
		}
		if _, err := httpsURL(req.URL.String()); err != nil {
			return err
		}
		// discovery에는 자격 증명을 싣지 않으며 redirect마다 헤더를 재구성한다.
		req.Header = make(http.Header)
		req.Header.Set("Accept", "application/json")
		req.Host = ""
		return nil
	}}
}

func readJSON(ctx context.Context, client *http.Client, request *http.Request, out any) error {
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, contract.ErrProtocol) {
			return contract.ErrProtocol
		}
		return contract.ErrTransport
	}
	defer response.Body.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return contract.ErrProtocol
		}
		return statusError{response.StatusCode}
	}
	content, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || content != "application/json" && content != "application/jwk-set+json" {
		return contract.ErrProtocol
	}
	if response.ContentLength > responseLimit {
		return contract.ErrProtocol
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return contract.ErrTransport
	}
	if len(body) > responseLimit || jsontext.Value(body).Kind() != '{' || json.Unmarshal(body, out) != nil {
		return contract.ErrProtocol
	}
	return nil
}

func (m *Manager) getJSON(ctx context.Context, target string, out any) error {
	err := m.getJSONStatus(ctx, target, out)
	if _, ok := errors.AsType[statusError](err); ok {
		return contract.ErrProtocol
	}
	return err
}

func (m *Manager) getJSONStatus(ctx context.Context, target string, out any) error {
	if _, err := httpsURL(target); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return contract.ErrProtocol
	}
	req.Header.Set("Accept", "application/json")
	return readJSON(ctx, m.httpClient(true), req, out)
}

func (m *Manager) discover(ctx context.Context, challenge Challenge) (metadata, error) {
	resource, err := m.protectedResource(ctx, challenge.metadata)
	if err != nil {
		return metadata{}, err
	}
	return m.authorizationServer(ctx, resource)
}

// ResourceMetadata는 검증된 발견 단계 사이에서만 전달하는 불투명 값이다.
type ResourceMetadata struct{ resource, issuer string }

// String은 발견 URL을 출력하지 않는다.
func (ResourceMetadata) String() string { return "[발견 정보 비공개]" }

// GoString은 상세 디버그 표현에서도 URL을 출력하지 않는다.
func (r ResourceMetadata) GoString() string { return r.String() }

// CheckProtectedResource는 보호 리소스의 DPoP·scope·리소스 결합을 진단한다.
func (m *Manager) CheckProtectedResource(ctx context.Context) (ResourceMetadata, error) {
	return m.protectedResource(ctx, "")
}

func (m *Manager) protectedResource(ctx context.Context, location string) (ResourceMetadata, error) {
	var md ResourceMetadata
	explicitLocation := location != ""
	if location == "" {
		var err error
		location, err = wellKnown(m.cfg.RemoteURL(), "oauth-protected-resource")
		if err != nil {
			return md, err
		}
	}
	var resource struct {
		Resource   string   `json:"resource"`
		Servers    []string `json:"authorization_servers"`
		Bound      bool     `json:"dpop_bound_access_tokens_required"`
		Algorithms []string `json:"dpop_signing_alg_values_supported"`
		Scopes     []string `json:"scopes_supported"`
	}
	err := m.getJSONStatus(ctx, location, &resource)
	if status, ok := errors.AsType[statusError](err); ok && status.status == http.StatusNotFound && !explicitLocation {
		root, parseErr := httpsURL(m.cfg.RemoteURL())
		if parseErr != nil {
			return md, parseErr
		}
		root.Path, root.RawPath = "", ""
		fallback, locationErr := wellKnown(root.String(), "oauth-protected-resource")
		if locationErr != nil {
			return md, locationErr
		}
		if fallback != location {
			err = m.getJSONStatus(ctx, fallback, &resource)
		}
	}
	if _, ok := errors.AsType[statusError](err); ok {
		err = contract.ErrProtocol
	}
	if err != nil {
		return md, err
	}
	actual, e1 := normalizedURL(resource.Resource, false)
	expected, e2 := normalizedURL(m.cfg.RemoteURL(), false)
	if e1 != nil || e2 != nil || actual != expected || len(resource.Servers) != 1 || !resource.Bound || !slices.Contains(resource.Algorithms, "ES256") || !slices.Contains(resource.Scopes, "agent-context") {
		return md, contract.ErrProtocol
	}
	issuer, err := httpsURL(resource.Servers[0])
	if err != nil || issuer.RawQuery != "" || issuer.ForceQuery {
		return md, contract.ErrProtocol
	}
	return ResourceMetadata{resource.Resource, resource.Servers[0]}, nil
}

// CheckAuthorizationServer는 인가 서버의 PKCE·DPoP·공개 클라이언트와 서명 키를 진단한다.
func (m *Manager) CheckAuthorizationServer(ctx context.Context, resource ResourceMetadata) error {
	_, err := m.authorizationServer(ctx, resource)
	return err
}

func (m *Manager) authorizationServer(ctx context.Context, resource ResourceMetadata) (metadata, error) {
	var md metadata
	location, err := wellKnown(resource.issuer, "oauth-authorization-server")
	if err != nil {
		return md, err
	}
	if err := m.getJSON(ctx, location, &md); err != nil {
		return md, err
	}
	if md.Issuer != resource.issuer || !md.IssuerResponse || !slices.Contains(md.Challenges, "S256") || !slices.Contains(md.Algorithms, "ES256") || !slices.Contains(md.Scopes, "agent-context") || !slices.Contains(md.AuthMethods, "none") || !slices.Contains(md.Responses, "code") || !slices.Contains(md.Grants, "authorization_code") {
		return md, contract.ErrProtocol
	}
	for _, endpoint := range []string{md.Authorization, md.Token, md.JWKS} {
		if _, err := httpsURL(endpoint); err != nil {
			return md, err
		}
	}
	keys, err := m.signingKeys(ctx, md.JWKS)
	if err != nil {
		return md, err
	}
	md.keys = keys
	md.resource = resource.resource
	return md, nil
}

func (m *Manager) signingKeys(ctx context.Context, target string) ([]jwk, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := m.getJSON(ctx, target, &set); err != nil {
		return nil, err
	}
	if len(set.Keys) == 0 {
		return nil, contract.ErrProtocol
	}
	seen := make(map[string]bool)
	for _, key := range set.Keys {
		if key.Kid == "" || seen[key.Kid] {
			return nil, contract.ErrProtocol
		}
		if _, err := key.publicKey(); err != nil {
			return nil, err
		}
		seen[key.Kid] = true
	}
	return slices.Clone(set.Keys), nil
}

// Challenge는 검증된 DPoP 발견 힌트다. 제로 값은 well-known 위치를 사용한다.
type Challenge struct{ metadata string }

// ParseChallenge는 초기 도전·invalid_token만 재인증 대상으로 판정한다.
func ParseChallenge(status int, header http.Header, authenticated, retried bool) (Challenge, bool, error) {
	var challenge Challenge
	if status != http.StatusUnauthorized {
		return challenge, false, nil
	}
	params, err := dpopParameters(header.Values("WWW-Authenticate"))
	if err != nil {
		return challenge, false, err
	}
	if params["scope"] != "" && params["scope"] != "agent-context" || params["algs"] != "" && !slices.Contains(strings.Fields(params["algs"]), "ES256") {
		return challenge, false, contract.ErrProtocol
	}
	if params["resource_metadata"] != "" {
		if _, err := httpsURL(params["resource_metadata"]); err != nil {
			return challenge, false, err
		}
		challenge.metadata = params["resource_metadata"]
	}
	if params["error"] == "invalid_dpop_proof" || params["error"] == "use_dpop_nonce" {
		return challenge, false, contract.ErrProtocol
	}
	if retried || params["error"] != "invalid_token" && (authenticated || params["error"] != "") {
		return challenge, false, ErrAuthorization
	}
	return challenge, true, nil
}

func dpopParameters(headers []string) (map[string]string, error) {
	var selected map[string]string
	for _, header := range headers {
		if len(header) > 64<<10 || strings.ContainsAny(header, "\r\n") {
			return nil, contract.ErrProtocol
		}
		// 쉼표는 인용 문자열 밖에서만 매개변수를 구분한다.
		var segments []string
		start := 0
		quoted, escaped := false, false
		for i, c := range header {
			if escaped {
				escaped = false
				continue
			}
			if quoted && c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				quoted = !quoted
			}
			if c == ',' && !quoted {
				segments = append(segments, header[start:i])
				start = i + 1
			}
		}
		if quoted || escaped {
			return nil, contract.ErrProtocol
		}
		segments = append(segments, header[start:])
		var current map[string]string
		for _, segment := range segments {
			segment = strings.TrimSpace(segment)
			name, rest, ok := strings.Cut(segment, " ")
			if ok && !strings.Contains(name, "=") && !strings.HasPrefix(strings.TrimSpace(rest), "=") {
				current = nil
				if strings.EqualFold(name, "DPoP") {
					if selected != nil {
						return nil, contract.ErrProtocol
					}
					selected = make(map[string]string)
					current = selected
				}
				segment = strings.TrimSpace(rest)
			} else if strings.EqualFold(segment, "DPoP") {
				if selected != nil {
					return nil, contract.ErrProtocol
				}
				selected = make(map[string]string)
				current = selected
				continue
			}
			if current == nil {
				continue
			}
			key, value, ok := strings.Cut(segment, "=")
			key = strings.ToLower(strings.TrimSpace(key))
			value = strings.TrimSpace(value)
			if !ok || !httpToken(key) {
				return nil, contract.ErrProtocol
			}
			if _, exists := current[key]; exists {
				return nil, contract.ErrProtocol
			}
			if strings.HasPrefix(value, "\"") {
				if len(value) < 2 || value[len(value)-1] != '"' {
					return nil, contract.ErrProtocol
				}
				var decoded strings.Builder
				escape := false
				for _, c := range value[1 : len(value)-1] {
					if escape {
						decoded.WriteRune(c)
						escape = false
					} else if c == '\\' {
						escape = true
					} else if c == '"' || c < 32 || c == 127 {
						return nil, contract.ErrProtocol
					} else {
						decoded.WriteRune(c)
					}
				}
				if escape {
					return nil, contract.ErrProtocol
				}
				value = decoded.String()
			} else if !httpToken(value) {
				return nil, contract.ErrProtocol
			}
			current[key] = value
		}
	}
	if selected == nil {
		return nil, contract.ErrProtocol
	}
	return selected, nil
}

func httpToken(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c > 127 || !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}

func authorizationURL(md metadata, clientID, resource, redirect, state, verifier string) string {
	u, _ := url.Parse(md.Authorization)
	query := u.Query()
	for key, value := range map[string]string{"client_id": clientID, "response_type": "code", "redirect_uri": redirect, "resource": resource, "scope": "agent-context", "state": state, "code_challenge": tokenHash(verifier), "code_challenge_method": "S256"} {
		query.Set(key, value)
	}
	u.RawQuery = query.Encode()
	return u.String()
}
