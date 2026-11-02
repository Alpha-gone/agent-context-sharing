package authz

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"agent_context_sharing/internal/store"
)

// ClientRegistration는 RFC 7591 공개 클라이언트 등록 요청이다.
// 알 수 없는 확장 필드는 무시하며 URI 메타데이터를 외부에서 조회하지 않는다.
type ClientRegistration struct {
	RedirectURIs            []string       `json:"redirect_uris"`
	ClientName              string         `json:"client_name,omitempty"`
	TokenEndpointAuthMethod string         `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string       `json:"grant_types,omitempty"`
	ResponseTypes           []string       `json:"response_types,omitempty"`
	Scope                   string         `json:"scope,omitempty"`
	ClientSecret            jsontext.Value `json:"client_secret,omitempty"`
	SoftwareStatement       jsontext.Value `json:"software_statement,omitempty"`
}

// RegisteredClient는 발급한 ID와 서버가 수락한 메타데이터만 반환한다.
type RegisteredClient struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope"`
}

// RegisterClient는 자격을 발급하지 않고 고정 리소스의 공개 client 등록만 저장한다.
func (s *Service) RegisterClient(ctx context.Context, input ClientRegistration) (RegisteredClient, error) {
	if err := ctx.Err(); err != nil {
		return RegisteredClient{}, err
	}
	if s.oauth == nil {
		return RegisteredClient{}, ErrUnavailable
	}
	if len(input.RedirectURIs) == 0 || len(input.RedirectURIs) > 8 {
		return RegisteredClient{}, &OAuthError{Code: "invalid_redirect_uri"}
	}
	for i, callback := range input.RedirectURIs {
		if !validRegistrationCallback(callback) || slices.Contains(input.RedirectURIs[:i], callback) {
			return RegisteredClient{}, &OAuthError{Code: "invalid_redirect_uri"}
		}
	}
	if !utf8.ValidString(input.ClientName) || utf8.RuneCountInString(input.ClientName) > 128 || strings.ContainsFunc(input.ClientName, unicode.IsControl) ||
		(input.TokenEndpointAuthMethod != "" && input.TokenEndpointAuthMethod != "none") ||
		(input.Scope != "" && input.Scope != "agent-context") || len(input.ClientSecret) != 0 || len(input.SoftwareStatement) != 0 {
		return RegisteredClient{}, &OAuthError{Code: "invalid_client_metadata"}
	}
	if input.GrantTypes != nil && (len(input.GrantTypes) == 0 || !slices.Contains(input.GrantTypes, "authorization_code")) {
		return RegisteredClient{}, &OAuthError{Code: "invalid_client_metadata"}
	}
	for i, grant := range input.GrantTypes {
		if (grant != "authorization_code" && grant != "refresh_token") || slices.Contains(input.GrantTypes[:i], grant) {
			return RegisteredClient{}, &OAuthError{Code: "invalid_client_metadata"}
		}
	}
	if input.ResponseTypes != nil && (len(input.ResponseTypes) != 1 || input.ResponseTypes[0] != "code") {
		return RegisteredClient{}, &OAuthError{Code: "invalid_client_metadata"}
	}
	random, err := secret()
	if err != nil {
		return RegisteredClient{}, &credentialFailure{ErrUnavailable, err}
	}
	client := OAuthClient{ID: "dcr_" + random, Name: input.ClientName, RedirectURIs: slices.Clone(input.RedirectURIs), Resources: []string{s.oauth.Resource}, LoopbackPortPolicy: "same_ip", CreatedAt: s.now().UTC().Truncate(time.Second)}
	if err := s.oauth.Clients.RegisterOAuthClient(ctx, client); err != nil {
		if errors.Is(err, store.ErrOAuthClientCapacity) {
			return RegisteredClient{}, err
		}
		return RegisteredClient{}, &credentialFailure{ErrUnavailable, err}
	}
	if err := ctx.Err(); err != nil {
		return RegisteredClient{}, err
	}
	return RegisteredClient{ClientID: client.ID, ClientIDIssuedAt: client.CreatedAt.Unix(), ClientName: client.Name, RedirectURIs: client.RedirectURIs, TokenEndpointAuthMethod: "none", GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, Scope: "agent-context"}, nil
}

func validRegistrationCallback(raw string) bool {
	if raw == "" || len(raw) > 1024 || strings.TrimSpace(raw) != raw || strings.Contains(raw, "#") || strings.ContainsFunc(raw, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || u.User != nil || strings.ContainsAny(u.Hostname(), "*%\\") {
		return false
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return false
	}
	if port := u.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" || u.Port() == "" {
		return false
	}
	// localhost는 등록 원문의 완전 일치만 허용한다. 가변 포트 대조는
	// sameLoopbackCallback이 literal IP에만 적용하므로 DNS 조회가 필요 없다.
	if u.Hostname() == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(u.Hostname())
	return err == nil && address.IsLoopback() && !address.Is4In6() && address.Zone() == ""
}

type registrationWindow struct {
	until time.Time
	count int
}
type registrationHandler struct {
	service *Service
	address func(*http.Request) netip.Addr
	mu      sync.Mutex
	sources map[netip.Prefix]registrationWindow
}

// NewRegistrationHandler는 전송 계층이 검증한 주소로 독립 등록 예산을 적용한다.
func NewRegistrationHandler(service *Service, address func(*http.Request) netip.Addr) http.Handler {
	return &registrationHandler{service: service, address: address, sources: make(map[netip.Prefix]registrationWindow)}
}

func (h *registrationHandler) allow(request *http.Request) time.Duration {
	var address netip.Addr
	if h.address != nil {
		address = h.address(request)
	} else {
		host, _, err := net.SplitHostPort(request.RemoteAddr)
		if err == nil {
			address, _ = netip.ParseAddr(host)
		}
	}
	key := netip.Prefix{}
	if address.IsValid() && address.Zone() == "" {
		address = address.Unmap()
		bits := 64
		if address.Is4() {
			bits = 32
		}
		key = netip.PrefixFrom(address, bits).Masked()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.service.now()
	window, exists := h.sources[key]
	if !exists || !now.Before(window.until) {
		var remaining time.Duration
		for source, state := range h.sources {
			if !now.Before(state.until) {
				delete(h.sources, source)
			} else {
				remaining = max(remaining, state.until.Sub(now))
			}
		}
		if len(h.sources) >= 4096 {
			return remaining
		}
		window = registrationWindow{until: now.Add(time.Minute)}
	}
	if window.count >= 10 {
		return window.until.Sub(now)
	}
	window.count++
	h.sources[key] = window
	return 0
}

func (h *registrationHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", "POST")
		writeJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request"})
		return
	}
	if request.Context().Err() != nil {
		return
	}
	if wait := h.allow(request); wait > 0 {
		writer.Header().Set("Retry-After", strconv.FormatInt(max(1, int64((wait+time.Second-1)/time.Second)), 10))
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	types := request.Header.Values("Content-Type")
	media, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if len(types) != 1 || err != nil || media != "application/json" || request.URL.RawQuery != "" || len(request.Header.Values("Authorization")) != 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 8<<10))
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(writer, status, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	var input ClientRegistration
	if len(raw) == 0 || raw[0] == 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	// null·배열을 객체로 수락하지 않고 중복 JSON 이름도 거부한다.
	var object map[string]jsontext.Value
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	for _, key := range []string{"client_name", "token_endpoint_auth_method", "scope"} {
		if value, present := object[key]; present && value.Kind() != jsontext.KindString {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
			return
		}
	}
	for _, key := range []string{"redirect_uris", "grant_types", "response_types"} {
		if value, present := object[key]; present && value.Kind() != jsontext.KindBeginArray {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
			return
		}
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	registered, err := h.service.RegisterClient(request.Context(), input)
	if request.Context().Err() != nil {
		return
	}
	if failure, ok := errors.AsType[*OAuthError](err); ok {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": failure.Code})
		return
	}
	if errors.Is(err, store.ErrOAuthClientCapacity) {
		writer.Header().Set("Retry-After", "60")
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(writer, http.StatusCreated, registered)
}
