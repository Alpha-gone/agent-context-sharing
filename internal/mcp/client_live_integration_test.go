//go:build client_live

package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/search"
	"agent_context_sharing/internal/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type clientLiveHeaderKey struct{}

// TestClientServiceLive는 별도 모듈의 클라이언트를 실제 인가·MCP·AGE 경계로 시험한다.
// 세션·브라우저 실행·embedding은 대체하며 시스템 브라우저나 로그인 UI 시험은 아니다.
func TestClientServiceLive(t *testing.T) {
	for _, mode := range []string{"issued_token", "renewal_window"} {
		t.Run(mode, func(t *testing.T) { runClientServiceLive(t, mode == "renewal_window") })
	}
}

func runClientServiceLive(t *testing.T, renewal bool) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("실제 클라이언트 시험에는 전용 TEST_DATABASE_URL이 필요하다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	database, err := store.New(t.Context(), databaseURL, graphName, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account := newHandlerID(t)
	createHandlerAccount(t, database, account)
	searcher, err := search.New(database, failingEmbedder{}, search.Config{
		Execution: search.ExecutionSequential, CandidateLimit: 10, FoldThreshold: 0.9, GraphStage: search.GraphStageGlobal,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := NewHandlerWithSearch(database, plan.AccountPlans{}, searcher, nil)
	cases := make(map[string]map[string]any)
	for _, c := range contractCases() {
		fixture := seedIsolationGraph(t, call, account, "클라이언트 실제 서비스 "+c.operation)
		if c.setup != nil {
			c.setup(t, call, account, fixture)
		}
		arguments := c.arguments(fixture)
		delete(arguments, "created_by_agent")
		cases[c.operation] = arguments
	}

	mux := http.NewServeMux()
	tlsServer := httptest.NewTLSServer(mux)
	defer tlsServer.Close()
	issuer, _ := url.Parse(tlsServer.URL)
	resource, _ := url.Parse(tlsServer.URL + "/mcp")
	callback, _ := url.Parse("http://127.0.0.1/callback")
	authConfig := authz.Config{Issuer: issuer.String(), Resource: resource.String(), Scope: Scope,
		Clients: map[string][]*url.URL{"client-live": {callback}}, BcryptCost: 4}
	service, err := authz.New(database, authConfig)
	if err != nil {
		t.Fatal(err)
	}
	otherInstance, err := authz.New(database, authConfig)
	if err != nil {
		t.Fatal(err)
	}
	authHandler, err := authz.NewHandler(service, func(*http.Request) (model.ID, bool) { return account, true }, "/login")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := make(map[string]int)
	keys := make(map[string]string)
	proofs := make(map[string]bool)
	ids := make(map[string]bool)
	tokens := make(map[string]bool)
	var violations []string
	var dropped, replayChecked bool
	var lostResult []byte
	var verifyAttempts atomic.Int64
	injected := make(map[string]bool)
	verify := func(ctx context.Context, a Authentication) (model.ID, error) {
		request := authz.DPoPRequest{AccessToken: a.AccessToken, Proof: a.Proof, Method: a.Method, Target: a.Target}
		instance := service
		if verifyAttempts.Add(1)%2 == 0 {
			instance = otherInstance
		}
		id, renewed, err := instance.VerifyAndRenew(ctx, request)
		if err != nil {
			if errors.Is(err, authz.ErrInvalidCredential) {
				return model.ID{}, ErrInvalidToken
			}
			if errors.Is(err, authz.ErrInvalidDPoPProof) {
				return model.ID{}, ErrInvalidDPoPProof
			}
			return model.ID{}, err
		}
		mu.Lock()
		defer mu.Unlock()
		tokens[a.AccessToken] = true
		if renewed.Raw != "" {
			header := ctx.Value(clientLiveHeaderKey{}).(http.Header)
			header.Set("Mcp-Access-Token", renewed.Raw)
			header.Set("Mcp-Access-Token-Expires-At", strconv.FormatInt(renewed.ExpiresAt.Unix(), 10))
		}
		if !replayChecked {
			_, _, replayErr := otherInstance.VerifyAndRenew(ctx, request)
			if !errors.Is(replayErr, authz.ErrInvalidDPoPProof) {
				violations = append(violations, "교차 인스턴스 proof 재생 허용")
			}
			replayChecked = true
		}
		return id, nil
	}
	wrappedCall := func(ctx context.Context, id model.ID, name string, arguments map[string]any) (ToolResult, error) {
		mu.Lock()
		seen[name]++
		if value, ok := arguments["created_by_agent"]; ok && value != isolationAgent {
			violations = append(violations, "행위 에이전트 주입 불일치")
		} else if ok {
			injected[name] = true
		}
		mu.Unlock()
		result, err := call(ctx, id, name, arguments)
		if name == "graph_create" && err == nil {
			encoded, encodeErr := json.Marshal(result)
			canonical := jsontext.Value(encoded)
			canonicalErr := canonical.Canonicalize()
			mu.Lock()
			if encodeErr != nil || canonicalErr != nil || (lostResult != nil && !bytes.Equal(lostResult, canonical)) {
				violations = append(violations, "유실된 최초 쓰기와 재생 결과 불일치")
			}
			lostResult = canonical
			mu.Unlock()
		}
		return result, err
	}
	server, err := New(Config{ResourceURL: resource, AuthorizationServerURL: issuer, AllowedOrigins: []*url.URL{issuer},
		ServerInfo: Implementation{Name: "agent-context-live", Version: "test"}}, verify, wrappedCall)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", server.ProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", server.AuthorizationServerMetadata)
	mux.HandleFunc("GET /authorize", authHandler.Authorize)
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if !renewal {
			authHandler.Token(w, r)
			return
		}
		// 한 시간 대기 대신 실제 DB 키로 갱신 구간의 토큰 fixture를 서명한다.
		// 인가 코드·PKCE·교환 자체는 실제 처리기를 통과한다.
		recorded := httptest.NewRecorder()
		authHandler.Token(recorded, r)
		if recorded.Code != http.StatusOK {
			http.Error(w, "교환 실패", recorded.Code)
			return
		}
		var response map[string]any
		if err := json.Unmarshal(recorded.Body.Bytes(), &response); err != nil {
			t.Error("시험 교환 결과 오류")
			return
		}
		raw, ok := response["access_token"].(string)
		if !ok {
			t.Error("시험 교환 토큰 누락")
			return
		}
		parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Error("시험 토큰 형식 오류")
			return
		}
		key, err := database.ActiveSigningKey(r.Context())
		if err != nil {
			t.Error("시험 서명 키 조회 실패")
			return
		}
		var private jose.JSONWebKey
		if err := json.Unmarshal([]byte(key.PrivateKey), &private); err != nil {
			t.Error("시험 서명 키 형식 오류")
			return
		}
		var claims map[string]any
		if err := parsed.Claims(private.Public().Key, &claims); err != nil {
			t.Error("시험 토큰 서명 오류")
			return
		}
		now := time.Now().UTC().Unix()
		claims["iat"], claims["auth_time"], claims["exp"] = now-3591, now-3591, now+9
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: private.Key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", key.ID))
		if err != nil {
			t.Error("시험 토큰 서명기 오류")
			return
		}
		response["access_token"], err = jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			t.Error("시험 토큰 서명 실패")
			return
		}
		response["expires_in"] = 9
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, response); err != nil {
			t.Error(err)
		}
	})
	mux.HandleFunc("GET /jwks.json", authHandler.JWKS)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), clientLiveHeaderKey{}, w.Header()))
		// 정상 처리 완료 뒤 연결만 끊어 멱등성 저장을 실제로 재조회하게 한다.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "시험 본문 읽기 실패", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var message struct {
			ID     any `json:"id"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &message); err != nil {
			http.Error(w, "시험 JSON 해석 실패", http.StatusBadRequest)
			return
		}
		name := message.Params.Name
		mu.Lock()
		if message.ID != "negative-proof" && r.Header.Get("Authorization") != "" && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			proof := r.Header.Get("DPoP")
			if proofs[proof] || ids[message.ID.(string)] {
				violations = append(violations, "HTTP 시도 식별자·proof 재사용")
			}
			proofs[proof], ids[message.ID.(string)] = true, true
		}
		if key := r.Header.Get("Idempotency-Key"); key != "" {
			parsed, err := uuid.Parse(strings.Trim(key, `"`))
			if err != nil || parsed[6]>>4 != 7 {
				violations = append(violations, "UUIDv7 멱등성 키 형식 위반")
			}
			if old := keys[name]; old != "" && old != key {
				violations = append(violations, "동일 논리 쓰기의 키 변경")
			}
			keys[name] = key
		}
		drop := name == "graph_create" && !dropped
		if drop {
			dropped = true
		}
		mu.Unlock()
		if !drop {
			server.ServeHTTP(w, r)
			return
		}
		recorded := httptest.NewRecorder()
		server.ServeHTTP(recorded, r)
		if recorded.Code != http.StatusOK {
			mu.Lock()
			violations = append(violations, "유실 전 쓰기 실패")
			mu.Unlock()
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		connection.Close()
	})
	fixtures, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	goBin := os.Getenv("GO_BIN")
	if goBin == "" {
		goBin = "go"
	}
	command := exec.CommandContext(t.Context(), goBin, "test", "-json", "-race", "-tags=client_live", "-count=1", "-run=^TestClientLive", "./internal/client")
	command.Dir = filepath.Join("..", "..", "client")
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "TEST_DATABASE_URL=") || strings.HasPrefix(entry, "DATABASE_URL=") || strings.HasPrefix(entry, "CLIENT_LIVE_") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "CLIENT_LIVE_URL="+resource.String(),
		"CLIENT_LIVE_CERT="+base64.StdEncoding.EncodeToString(tlsServer.Certificate().Raw),
		"CLIENT_LIVE_CASES="+base64.StdEncoding.EncodeToString(fixtures), "CLIENT_LIVE_AGENT="+isolationAgent,
		"CLIENT_LIVE_RENEWAL="+strconv.FormatBool(renewal))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("클라이언트 실제 서비스 시험 실패: %v\n%s", err, output)
	}
	if bytes.Contains(output, []byte(`"Action":"skip"`)) || !bytes.Contains(output, []byte(`"Test":"TestClientLiveService"`)) {
		t.Fatal("클라이언트 시험이 없거나 건너뛰었다")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(violations) != 0 {
		t.Fatal(violations)
	}
	for _, definition := range toolDefinitions() {
		want := 1
		if definition.Name == "graph_create" {
			want = 2
		}
		// 멱등성 재생은 호출 처리기에서 이루어져 처리기 진입은 두 번이다.
		if seen[definition.Name] != want {
			t.Errorf("%s 처리기 호출 수=%d, 기대=%d", definition.Name, seen[definition.Name], want)
		}
	}
	if len(keys) != 8 || len(injected) != 6 || !dropped || !replayChecked {
		t.Fatal("쓰기 키 8종·응답 유실·교차 인스턴스 재생 시험 누락")
	}
	if renewal && len(tokens) < 2 {
		t.Fatal("실제 갱신 헤더 뒤 새 토큰이 보호 호출에 사용되지 않았다")
	}
	t.Log("실제 서비스의 도구 13종·PKCE·DPoP·쓰기 응답 유실과 키 유지·교차 인스턴스 proof 재생 거부 확인")
}
