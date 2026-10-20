package main

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/web"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestWebSessionClosedStoreHTTPIntegration(t *testing.T) {
	backend := newPublicAuthenticationStore(t)
	service, err := authz.New(backend, authz.Config{Issuer: "https://service.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	id, err := service.Register(t.Context(), "sessionfault", "correct password")
	if err != nil {
		t.Fatal("시험 계정 생성 실패")
	}
	token, err := service.WebSession(t.Context(), id, "web")
	if err != nil {
		t.Fatal("시험 세션 발급 실패")
	}
	backend.Close()
	service, err = authz.New(backend, authz.Config{Issuer: "https://service.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	adapter := webAuthentication{service}
	if result, err := adapter.WebSession(t.Context(), id, "web"); !errors.Is(err, web.ErrAuthenticationUnavailable) || result.Raw != "" {
		t.Fatal("실제 저장소 장애의 발급 오류 사상 실패")
	}
	if err := adapter.Revoke(t.Context(), web.Session{ID: token.ID, ExpiresAt: token.ExpiresAt}); !errors.Is(err, web.ErrAuthenticationUnavailable) {
		t.Fatal("실제 저장소 장애의 폐기 오류 사상 실패")
	}
	security := transportSecurity{directTLS: true}
	screen, err := web.New(adapter, backend, web.Config{SecureCookie: security.isTLSRequest})
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(backend, nil, security)
	app.web = screen
	for _, path := range []string{"/access", "/logout"} {
		t.Run(path, func(t *testing.T) {
			method := http.MethodGet
			if path == "/logout" {
				method = http.MethodPost
			}
			request := httptest.NewRequest(method, "https://service.test"+path, nil)
			request.AddCookie(&http.Cookie{Name: "agent_context_session", Value: token.Raw})
			response := httptest.NewRecorder()
			app.handler().ServeHTTP(response, request)
			if response.Code != 500 || response.Header().Get("Location") != "" || len(response.Result().Cookies()) != 0 || !strings.Contains(response.Body.String(), "인증 상태를 확인할 수 없습니다.") {
				t.Fatal("실제 검증 저장소 장애가 재로그인·로그아웃 성공으로 바뀌었습니다")
			}
		})
	}
}

func TestWebSessionHTTPContractIntegration(t *testing.T) {
	backend := newPublicAuthenticationStore(t)
	service, err := authz.New(backend, authz.Config{Issuer: "https://service.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	id, err := service.Register(t.Context(), "sessionhttp", "correct password")
	if err != nil {
		t.Fatal("시험 계정 생성 실패")
	}
	security := transportSecurity{directTLS: true}
	screen, err := web.New(webAuthentication{service}, backend, web.Config{SecureCookie: security.isTLSRequest, ClientAddress: security.clientAddress})
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(backend, nil, security)
	app.web = screen
	server := httptest.NewTLSServer(app.handler())
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(client.CloseIdleConnections)
	do := func(method, path, body string, cookie *http.Cookie, headers http.Header) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal("시험 요청 준비 실패")
		}
		request.Header = headers.Clone()
		if request.Header == nil {
			request.Header = make(http.Header)
		}
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("TLS 요청 교환 실패")
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal("응답 본문 읽기 실패")
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal("응답 본문 반환 실패")
		}
		return response
	}
	response := do(http.MethodPost, "/login", url.Values{"login_id": {"sessionhttp"}, "password": {"correct password"}}.Encode(), nil, nil)
	cookies := response.Cookies()
	if response.StatusCode != 303 || response.Header.Get("Cache-Control") != "no-store" || len(cookies) != 1 {
		t.Fatal("로그인 세션 쿠키 발급 실패")
	}
	cookie := cookies[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge != 0 {
		t.Fatal("세션 쿠키 보호 속성 불일치")
	}
	account, token, err := service.Verify(t.Context(), cookie.Value, "web")
	if err != nil || account != id || !token.ExpiresAt.Equal(cookie.Expires) {
		t.Fatal("서명 세션과 쿠키의 만료 불일치")
	}
	key, err := backend.ActiveSigningKey(t.Context())
	if err != nil {
		t.Fatal("시험 서명 키 조회 실패")
	}
	var public, private jose.JSONWebKey
	if json.Unmarshal([]byte(key.PublicKey), &public) != nil || json.Unmarshal([]byte(key.PrivateKey), &private) != nil {
		t.Fatal("시험 키 해석 실패")
	}
	parsed, err := jwt.ParseSigned(cookie.Value, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal("발급 JWT 해석 실패")
	}
	var claims jwt.Claims
	if parsed.Claims(public.Key, &claims) != nil || claims.Expiry.Time().Sub(claims.IssuedAt.Time()) != 12*time.Hour || claims.ID != token.ID {
		t.Fatal("세션 필수 클레임·12시간 수명 불일치")
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", key.ID))
	if err != nil {
		t.Fatal("교차 채널 표본 준비 실패")
	}
	claims.Audience = jwt.Audience{"https://service.test/mcp"}
	otherChannel, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal("교차 채널 표본 서명 실패")
	}
	for _, test := range []struct {
		name, path, bearer string
		cookie             *http.Cookie
		status             int
	}{
		{"cookie", "/access", "", cookie, 200},
		{"Bearer only", "/access", cookie.Value, nil, 303},
		{"query only", "/access?token=" + url.QueryEscape(cookie.Value), "", nil, 303},
		{"wrong audience cookie", "/access", "", &http.Cookie{Name: cookie.Name, Value: otherChannel}, 303},
		{"no Bearer fallback", "/access", cookie.Value, &http.Cookie{Name: cookie.Name, Value: otherChannel}, 303},
		{"cookie independent of Bearer", "/access", otherChannel, cookie, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := make(http.Header)
			if test.bearer != "" {
				headers.Set("Authorization", "Bearer "+test.bearer)
			}
			response := do(http.MethodGet, test.path, "", test.cookie, headers)
			if response.StatusCode != test.status || len(response.Cookies()) != 0 {
				t.Fatal("웹 전달·대상 경계 또는 갱신 없음 위반")
			}
		})
	}
	t.Run("confirmation and origin do not logout", func(t *testing.T) {
		if response := do(http.MethodGet, "/logout", "", cookie, nil); response.StatusCode != 200 || len(response.Cookies()) != 0 {
			t.Fatal("로그아웃 GET이 상태를 바꿨습니다")
		}
		if response := do(http.MethodPost, "/logout", "", cookie, http.Header{"Sec-Fetch-Site": {"cross-site"}}); response.StatusCode != 403 || len(response.Cookies()) != 0 {
			t.Fatal("교차 출처 로그아웃 허용")
		}
		if response := do(http.MethodGet, "/access", "", cookie, nil); response.StatusCode != 200 {
			t.Fatal("선행 거부가 세션을 폐기했습니다")
		}
	})
	t.Run("logout revokes only current session", func(t *testing.T) {
		independent, err := service.WebSession(t.Context(), id, "web")
		if err != nil {
			t.Fatal("독립 세션 발급 실패")
		}
		response := do(http.MethodPost, "/logout", "", cookie, nil)
		deleted := response.Cookies()
		if response.StatusCode != 303 || response.Header.Get("Location") != "/login" || response.Header.Get("Cache-Control") != "no-store" || len(deleted) != 1 || deleted[0].MaxAge != -1 || deleted[0].Value != "" || !deleted[0].Secure || !deleted[0].HttpOnly || deleted[0].SameSite != http.SameSiteLaxMode || deleted[0].Path != "/" {
			t.Fatal("로그아웃 쿠키 삭제 계약 불일치")
		}
		if response := do(http.MethodGet, "/access", "", cookie, nil); response.StatusCode != 303 {
			t.Fatal("로그아웃한 쿠키 재사용 허용")
		}
		if response := do(http.MethodGet, "/access", "", &http.Cookie{Name: cookie.Name, Value: independent.Raw}, nil); response.StatusCode != 200 {
			t.Fatal("로그아웃이 같은 계정의 다른 세션도 폐기했습니다")
		}
		if response := do(http.MethodPost, "/logout", "", cookie, nil); response.StatusCode != 303 {
			t.Fatal("반복 로그아웃 처리 실패")
		}
	})
}
