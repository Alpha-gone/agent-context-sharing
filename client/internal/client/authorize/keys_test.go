package authorize

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

func rotationFixture(t *testing.T) (*fixture, *Manager, Credential, *ecdsa.PrivateKey, []jwk) {
	t.Helper()
	f := newFixture(t)
	m := f.manager("5s")
	old, err := m.Credentials(t.Context(), Challenge{})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a, b := publicJWK(f.signer), publicJWK(key)
	a.Kid, b.Kid = "key", "rotated"
	return f, m, old, key, []jwk{a, b}
}

func rotatedHeader(t *testing.T, f *fixture, m *Manager, key *ecdsa.PrivateKey, kid string, mutate func(map[string]any)) http.Header {
	t.Helper()
	exp := f.now.Unix() + 7200
	claims := map[string]any{"iss": issuerURL, "sub": f.subject, "aud": resourceURL, "exp": exp, "cnf": map[string]string{"jkt": m.jkt}}
	if mutate != nil {
		mutate(claims)
	}
	raw, err := signJWT(key, map[string]any{"alg": "ES256", "kid": kid, "jku": "https://untrusted.test/keys"}, claims)
	if err != nil {
		t.Fatal(err)
	}
	return refreshHeader(raw, exp)
}

func serveRotation(t *testing.T, m *Manager, keys []jwk, calls *atomic.Int32, response func(*http.Request) error) {
	t.Helper()
	m.transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != issuerURL+"/jwks.json" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("DPoP") != "" || r.Header.Get("Cookie") != "" {
			t.Error("신뢰 주소 또는 비밀 없는 JWKS 조회 계약이 다릅니다")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > keyRefreshTimeout || time.Until(deadline) <= 0 {
			t.Error("JWKS 공유 조회에 기한 상한이 없습니다")
		}
		if response != nil {
			if err := response(r); err != nil {
				return nil, err
			}
		}
		return jsonResponse(r, map[string]any{"keys": keys})
	})
}

func TestRefreshRotatedKeyAndValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		kid     string
		mutate  func(map[string]any)
		want    error
		oldKey  bool
		missing bool
	}{
		{name: "신구 키 공존"},
		{name: "issuer", mutate: func(c map[string]any) { c["iss"] = "https://other.test" }, want: contract.ErrProtocol},
		{name: "resource", mutate: func(c map[string]any) { c["aud"] = "https://other.test/mcp" }, want: contract.ErrProtocol},
		{name: "계정 변경", mutate: func(c map[string]any) { c["sub"] = "other" }, want: contract.ErrIdentityChanged},
		{name: "DPoP 결합", mutate: func(c map[string]any) { c["cnf"] = map[string]string{"jkt": "other"} }, want: contract.ErrProtocol},
		{name: "만료", mutate: func(c map[string]any) { c["exp"] = 1 }, want: contract.ErrProtocol},
		{name: "만료 헤더", mutate: func(c map[string]any) { c["exp"] = c["exp"].(int64) + 1 }, want: contract.ErrProtocol},
		{name: "nbf", mutate: func(c map[string]any) { c["nbf"] = c["exp"] }, want: contract.ErrProtocol},
		{name: "알 수 없는 키", missing: true, want: contract.ErrProtocol},
		{name: "새 키의 잘못된 서명", oldKey: true, want: contract.ErrProtocol},
		{name: "알려진 키의 잘못된 서명", kid: "key", want: contract.ErrProtocol},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, m, old, key, keys := rotationFixture(t)
			var calls atomic.Int32
			served := keys
			if test.missing {
				served = keys[:1]
			}
			serveRotation(t, m, served, &calls, nil)
			if test.oldKey {
				key = f.signer
			}
			kid := test.kid
			if kid == "" {
				kid = "rotated"
			}
			header := rotatedHeader(t, f, m, key, kid, test.mutate)
			err := m.Refresh(t.Context(), header)
			if !errors.Is(err, test.want) {
				t.Fatalf("갱신 결과 = %v", err)
			}
			wantCalls := int32(1)
			if kid == "key" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls || (test.want != nil && m.current != old) {
				t.Fatal("조회 횟수 또는 실패 뒤 기존 자격 증명 보존이 다릅니다")
			}
			if test.want == nil {
				if m.current.raw != header.Get("Mcp-Access-Token") || m.current.identity != old.identity {
					t.Fatal("정상 키 회전 토큰이 반영되지 않았습니다")
				}
				if err := m.Refresh(t.Context(), refreshHeader(f.token(m.jkt, f.subject, f.now.Unix()+7200), f.now.Unix()+7200)); err != nil || calls.Load() != 1 {
					t.Fatalf("공존하는 구 키를 추가 조회 없이 검증하지 못했습니다: %v", err)
				}
			}
		})
	}
}

func TestRefreshKeysFailureCooldownAndRecovery(t *testing.T) {
	for _, failure := range []string{"전송", "빈 키", "중복 키", "개인 키"} {
		t.Run(failure, func(t *testing.T) {
			f, m, old, key, keys := rotationFixture(t)
			var calls atomic.Int32
			served := keys
			var response func(*http.Request) error
			switch failure {
			case "전송":
				response = func(*http.Request) error { return io.ErrUnexpectedEOF }
			case "빈 키":
				served = nil
			case "중복 키":
				served = []jwk{keys[1], keys[1]}
			case "개인 키":
				served = append([]jwk(nil), keys...)
				served[1].D = []byte(`"secret"`)
			}
			serveRotation(t, m, served, &calls, response)
			for range 20 {
				if err := m.Refresh(t.Context(), rotatedHeader(t, f, m, key, "rotated", nil)); !errors.Is(err, contract.ErrProtocol) {
					t.Fatal(err)
				}
			}
			if calls.Load() != 1 || m.current != old || len(m.metadata.keys) != 1 {
				t.Fatal("실패 재조회가 제한되지 않았거나 기존 상태를 바꿨습니다")
			}
			f.now = f.now.Add(keyRefreshInterval - time.Nanosecond)
			if err := m.Refresh(t.Context(), rotatedHeader(t, f, m, key, "rotated", nil)); !errors.Is(err, contract.ErrProtocol) || calls.Load() != 1 {
				t.Fatal("조회 간격이 끝나기 전에 다시 조회했습니다")
			}
			f.now = f.now.Add(time.Nanosecond)
			serveRotation(t, m, keys, &calls, nil)
			if err := m.Refresh(t.Context(), rotatedHeader(t, f, m, key, "rotated", nil)); err != nil || calls.Load() != 2 {
				t.Fatalf("간격 이후 키 조회가 복구되지 않았습니다: %v", err)
			}
		})
	}
}

func TestRefreshKeysCancellationIsolated(t *testing.T) {
	f, m, old, key, keys := rotationFixture(t)
	var calls atomic.Int32
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	serveRotation(t, m, keys, &calls, func(r *http.Request) error {
		select {
		case <-release:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	})
	header := rotatedHeader(t, f, m, key, "rotated", nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled, active := make(chan error, 1), make(chan error, 1)
	go func() { canceled <- m.Refresh(ctx, header) }()
	go func() { active <- m.Refresh(t.Context(), header) }()
	waitKeyWaiters(t, m, 2)
	cancel()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("개별 취소가 공유 조회 종료를 기다립니다")
	}
	if m.current != old {
		t.Fatal("취소한 호출이 기존 토큰을 변경했습니다")
	}
	waitKeyWaiters(t, m, 1)
	unblock()
	select {
	case err := <-active:
		if err != nil || calls.Load() != 1 || m.current.raw != header.Get("Mcp-Access-Token") {
			t.Fatalf("남은 대기자의 갱신이 취소되었습니다: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("남은 대기자의 갱신이 끝나지 않았습니다")
	}
}

func TestRefreshKeysMetadataGeneration(t *testing.T) {
	_, m, _, _, keys := rotationFixture(t)
	var calls atomic.Int32
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	serveRotation(t, m, keys, &calls, func(r *http.Request) error {
		select {
		case <-release:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	})
	md, version := m.metadata, m.metadataVersion
	result := make(chan error, 1)
	go func() {
		_, err := m.refreshKeys(t.Context(), md, version)
		result <- err
	}()
	waitKeyWaiters(t, m, 1)
	// 재인가의 새 발견 정보 게시와 동일한 세대 경계를 재현한다.
	m.mu.Lock()
	m.metadata.keys = keys[1:]
	m.metadataVersion++
	m.mu.Unlock()
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("오래된 조회가 끝나지 않았습니다")
	}
	if len(m.metadata.keys) != 1 || m.metadata.keys[0].Kid != "rotated" || m.metadataVersion != version+1 {
		t.Fatal("오래된 JWKS 조회가 새 발견 정보를 덮어썼습니다")
	}
	cached, err := m.refreshKeys(t.Context(), md, version)
	if err != nil || len(cached) != 1 || cached[0].Kid != "rotated" || calls.Load() != 1 {
		t.Fatalf("동일한 발견 경계의 최신 캐시를 재사용하지 못했습니다: %v", err)
	}
	m.mu.Lock()
	m.metadata.JWKS = "https://other.test/keys"
	m.metadataVersion++
	m.mu.Unlock()
	if _, err := m.refreshKeys(t.Context(), md, version); !errors.Is(err, contract.ErrProtocol) || calls.Load() != 1 {
		t.Fatal("다른 발견 경계의 키를 재사용했습니다")
	}
}

func TestRefreshMalformedTokenDoesNotFetchKeys(t *testing.T) {
	f, m, old, key, keys := rotationFixture(t)
	var calls atomic.Int32
	serveRotation(t, m, keys, &calls, nil)
	invalidClaims, err := signJWT(key, map[string]string{"alg": "ES256", "kid": "unknown"}, []string{})
	if err != nil {
		t.Fatal(err)
	}
	nullClaims, err := signJWT(key, map[string]string{"alg": "ES256", "kid": "unknown"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"malformed", ".e30.", encoding.EncodeToString([]byte(`{"alg":"HS256","kid":"unknown"}`)) + ".e30.AA", invalidClaims, nullClaims} {
		if err := m.Refresh(t.Context(), refreshHeader(raw, f.now.Unix()+7200)); !errors.Is(err, contract.ErrProtocol) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 || m.current != old {
		t.Fatal("잘못된 JWT 외피가 조회를 시작하거나 토큰을 바꿨습니다")
	}
	if err := m.Refresh(t.Context(), rotatedHeader(t, f, m, key, "rotated", nil)); err != nil || calls.Load() != 1 {
		t.Fatalf("외피 거부가 정상 조회 간격을 소비했습니다: %v", err)
	}
}

func waitKeyWaiters(t *testing.T, m *Manager, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		ready := m.keyFlight != nil && m.keyFlight.waiters == count
		m.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("JWKS 대기자 %d명이 준비되지 않았습니다", count)
}

func TestRefreshKeysConcurrentAndWaiterLimit(t *testing.T) {
	f, m, _, key, keys := rotationFixture(t)
	var calls atomic.Int32
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	serveRotation(t, m, keys, &calls, func(r *http.Request) error {
		select {
		case <-release:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	})
	header := rotatedHeader(t, f, m, key, "rotated", nil)
	var group sync.WaitGroup
	defer group.Wait()
	defer unblock()
	for range keyRefreshWaiters {
		group.Go(func() {
			if err := m.Refresh(t.Context(), header); err != nil {
				t.Error(err)
			}
		})
	}
	waitKeyWaiters(t, m, keyRefreshWaiters)
	if err := m.Refresh(t.Context(), header); !errors.Is(err, contract.ErrProtocol) {
		t.Fatalf("대기 상한 초과를 허용했습니다: %v", err)
	}
	unblock()
	group.Wait()
	if calls.Load() != 1 || m.current.raw != header.Get("Mcp-Access-Token") {
		t.Fatal("동시 갱신의 조회 병합·토큰 반영이 다릅니다")
	}
	for range 20 {
		if err := m.Refresh(t.Context(), rotatedHeader(t, f, m, key, "unknown", nil)); !errors.Is(err, contract.ErrProtocol) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("성공 직후 임의 kid가 무제한 조회를 만들었습니다")
	}
}

func TestRefreshKeysCancellationAndClose(t *testing.T) {
	for _, mode := range []string{"마지막 취소", "기한", "종료"} {
		t.Run(mode, func(t *testing.T) {
			f, m, old, key, keys := rotationFixture(t)
			var calls atomic.Int32
			cleaned := make(chan struct{})
			serveRotation(t, m, keys, &calls, func(r *http.Request) error {
				<-r.Context().Done()
				close(cleaned)
				return r.Context().Err()
			})
			header := rotatedHeader(t, f, m, key, "rotated", nil)
			ctx, cancel := context.WithCancel(t.Context())
			if mode == "기한" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
			}
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- m.Refresh(ctx, header) }()
			waitKeyWaiters(t, m, 1)
			if mode == "종료" {
				m.Close()
			} else if mode == "마지막 취소" {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil || (mode == "마지막 취소" && !errors.Is(err, context.Canceled)) || (mode == "기한" && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("취소·기한·종료 결과 = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("취소·기한·종료가 공유 조회를 끝내지 못했습니다")
			}
			select {
			case <-cleaned:
			default:
				t.Fatal("공유 조회 HTTP 정리 전에 반환했습니다")
			}
			if mode != "종료" && m.current != old {
				t.Fatal("실패한 갱신이 기존 자격 증명을 바꿨습니다")
			}
		})
	}
}
