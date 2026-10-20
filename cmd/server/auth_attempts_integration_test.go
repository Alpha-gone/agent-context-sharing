package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/migrate"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
	"agent_context_sharing/internal/web"
	"agent_context_sharing/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublicAuthenticationAttemptsIntegration(t *testing.T) {
	backend := newPublicAuthenticationStore(t)
	service, err := authz.New(backend, authz.Config{Issuer: "https://service.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	security := transportSecurity{trustedProxies: []netip.Prefix{netip.MustParsePrefix("10.203.0.1/32")}}
	server, err := web.New(webAuthentication{service: service}, backend, web.Config{SecureCookie: security.isTLSRequest, ClientAddress: security.clientAddress})
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(backend, nil, security)
	app.web = server
	handler := app.handler()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	login := "limit_" + strings.ReplaceAll(id.String(), "-", "")[:12]
	post := func(path, login, password, address string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"login_id": {login}, "password": {password}}.Encode()))
		request.RemoteAddr = "10.203.0.1:1234"
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("X-Forwarded-Proto", "https")
		request.Header.Set("CF-Connecting-IP", address)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := post("/register", login, "correct password", "198.51.100.1"); response.Code != 303 {
		t.Fatalf("정상 가입 = %d", response.Code)
	}
	account, err := backend.AccountByLoginID(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	response := post("/login", login, "correct password", "198.51.100.1")
	if response.Code != 303 || len(response.Result().Cookies()) != 1 {
		t.Fatalf("정상 로그인 = %d", response.Code)
	}
	if verified, _, err := service.Verify(t.Context(), response.Result().Cookies()[0].Value, web.SessionAudience); err != nil || verified != account.ID {
		t.Fatal("실제 로그인 세션 검증 실패", err)
	}
	if response := post("/register", login, "correct password", "198.51.100.1"); response.Code != 400 {
		t.Fatal("중복 가입 결과 변경")
	}
	var mismatch string
	for i := range 7 {
		name := login
		if i%2 == 0 {
			name = "absent_" + login
		}
		response := post("/login", name, "wrong password", "198.51.100.1")
		if response.Code != 401 {
			t.Fatalf("불일치 응답 = %d", response.Code)
		}
		// 아이디 반영을 제외한 안내는 존재 여부에 따라 달라지지 않는다.
		message := "로그인 아이디 또는 비밀번호가 올바르지 않습니다."
		if !strings.Contains(response.Body.String(), message) {
			t.Fatal("존재 여부별 안내 차이")
		}
		mismatch = message
	}
	response = post("/login", login, "correct password", "198.51.100.1")
	if response.Code != 429 || response.Header().Get("Retry-After") == "" || len(response.Result().Cookies()) != 0 || strings.Contains(response.Body.String(), mismatch) {
		t.Fatal("상한 초과에서 실제 인증·세션 발급")
	}
	if response := post("/login", login, "correct password", "198.51.100.2"); response.Code != 303 {
		t.Fatal("다른 출처에서 같은 계정이 잠겼습니다")
	}
}

func TestPublicAuthenticationAddressBudgetsIntegration(t *testing.T) {
	backend := newPublicAuthenticationStore(t)
	service, err := authz.New(backend, authz.Config{Issuer: "https://service.test", BcryptCost: 4})
	if err != nil {
		t.Fatal(err)
	}
	security := transportSecurity{trustedProxies: []netip.Prefix{netip.MustParsePrefix("10.203.0.1/32")}}
	for _, mode := range []string{"malformed visitor fallback", "IPv6 prefix"} {
		t.Run(mode, func(t *testing.T) {
			server, err := web.New(webAuthentication{service: service}, backend, web.Config{SecureCookie: security.isTLSRequest, ClientAddress: security.clientAddress})
			if err != nil {
				t.Fatal(err)
			}
			app := newApplication(backend, nil, security)
			app.web = server
			handler := app.handler()
			for i := range 12 {
				request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"login_id": {"absent"}, "password": {"wrong password"}}.Encode()))
				request.RemoteAddr = "10.203.0.1:1234"
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				request.Header.Set("X-Forwarded-Proto", "https")
				if mode == "IPv6 prefix" {
					request.Header.Set("CF-Connecting-IP", netip.AddrFrom16([16]byte{0x20, 1, 0xd, 0xb8, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(i + 1)}).String())
				} else {
					switch i % 4 {
					case 0:
						request.Header.Set("CF-Connecting-IP", "invalid")
					case 1:
						request.Header.Set("CF-Connecting-IP", "198.51.100.1, 198.51.100.2")
					case 2:
						request.Header.Add("CF-Connecting-IP", "198.51.100.3")
						request.Header.Add("CF-Connecting-IP", "198.51.100.4")
					}
				}
				if i == 11 {
					request.Header.Set("CF-Connecting-IP", "2001:db8:2::1")
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				expected := 401
				if i == 10 {
					expected = 429
				}
				if response.Code != expected || response.Header().Get("Cache-Control") != "no-store" || len(response.Result().Cookies()) != 0 {
					t.Fatal("실제 프록시·인증 조합의 출처 예산 또는 무세션 경계가 다릅니다")
				}
			}
		})
	}
}

// 공유 DB는 생성 권한 확인에만 사용하고 계정·서명 키는 정식 마이그레이션을 적용한 별도 DB에 둔다.
func newPublicAuthenticationStore(t *testing.T) *store.Store {
	t.Helper()
	backend, _, _ := newServerContractDatabase(t)
	return backend
}

func newServerContractDatabase(t *testing.T) (*store.Store, *pgxpool.Pool, string) {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_URL이 필요합니다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 공개 인증 DB 시험을 건너뜁니다")
	}
	isolated, err := url.Parse(address)
	if err != nil || (isolated.Scheme != "postgres" && isolated.Scheme != "postgresql") {
		t.Fatal("시험 DB는 PostgreSQL 연결 URL이어야 합니다")
	}
	config, err := pgxpool.ParseConfig(address)
	if err != nil {
		t.Fatal("시험 DB 연결 설정을 해석하지 못했습니다")
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("시험 DB 관리 연결을 준비하지 못했습니다")
	}
	t.Cleanup(admin.Close)
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "auth_http_" + strings.ReplaceAll(id.String(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal("시험 전용 DB를 만들지 못했습니다(DB 생성 권한 필요)")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Error("시험 전용 DB를 정리하지 못했습니다")
		}
	})
	isolated.Path, isolated.RawPath = "/"+name, ""
	pool, err := migrate.NewPool(t.Context(), isolated.String())
	if err != nil {
		t.Fatal("시험 전용 마이그레이션 연결을 준비하지 못했습니다")
	}
	t.Cleanup(pool.Close)
	var connected string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&connected); err != nil || connected != name {
		t.Fatal("실제 연결이 시험 전용 DB가 아니므로 중단합니다")
	}
	loaded, err := migrate.Load(migrations.FS, ".", migrate.Config{GraphName: "agent_context", VectorType: "vector", VectorDim: 768})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.EnsureHistory(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if err := migrate.Apply(t.Context(), pool, migration); err != nil {
			t.Fatal(err)
		}
	}
	query := isolated.Query()
	query.Set("pool_max_conns", "4")
	isolated.RawQuery = query.Encode()
	backend, err := store.New(t.Context(), isolated.String(), "agent_context", nil, nil, "")
	if err != nil {
		t.Fatal("시험 전용 저장소를 준비하지 못했습니다")
	}
	t.Cleanup(backend.Close)
	return backend, pool, isolated.String()
}
