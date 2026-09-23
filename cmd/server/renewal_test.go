package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
)

// fakeRenewer는 갱신 판정 결과를 고정해 HTTP 계층의 헤더 기록만 확인한다.
type fakeRenewer struct {
	accountID model.ID
	renewed   authz.Token
	err       error
}

func (fake fakeRenewer) VerifyAndRenew(context.Context, string, string) (model.ID, authz.Token, error) {
	return fake.accountID, fake.renewed, fake.err
}

// serveWithRenewal은 검증 함수를 부르는 처리기를 갱신 미들웨어로 감싸 응답을 만든다.
func serveWithRenewal(t *testing.T, service tokenRenewer) *httptest.ResponseRecorder {
	t.Helper()
	verify := verifyWithRenewal(service)
	inner := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, err := verify(request.Context(), "token", "https://service.test/mcp"); err != nil {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		// 본문을 쓰기 전에 검증을 부르는 mcp의 순서를 그대로 흉내 낸다.
		writer.Write([]byte(`{"result":{}}`))
	})
	recorder := httptest.NewRecorder()
	withRenewalHeader(inner).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	return recorder
}

// TestRenewedTokenTravelsInResponseHeader는 갱신된 토큰이 응답 본문이 아니라 헤더로
// 나가는지 확인한다. 「토큰 갱신」이 본문에 담지 않기로 확정했다.
func TestRenewedTokenTravelsInResponseHeader(t *testing.T) {
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	recorder := serveWithRenewal(t, fakeRenewer{accountID: model.ID{1}, renewed: authz.Token{Raw: "renewed.jwt.value", ID: "jti", ExpiresAt: expires}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("응답 상태 = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get(renewedTokenHeader); got != "renewed.jwt.value" {
		t.Fatalf("%s = %q, want renewed.jwt.value", renewedTokenHeader, got)
	}
	if got := recorder.Header().Get(renewedExpiresHeader); got != strconv.FormatInt(expires.Unix(), 10) {
		t.Fatalf("%s = %q, want %d", renewedExpiresHeader, got, expires.Unix())
	}
	if body := recorder.Body.String(); body != `{"result":{}}` {
		t.Fatalf("응답 본문 = %s, 갱신 토큰이 본문에 섞였다", body)
	}
}

// TestRequestSucceedsWithoutRenewal은 갱신 구간 밖의 요청이 헤더 없이 정상 처리되는지
// 확인한다. 「토큰 갱신」이 갱신 실패를 오류로 만들지 않기로 확정했다.
func TestRequestSucceedsWithoutRenewal(t *testing.T) {
	recorder := serveWithRenewal(t, fakeRenewer{accountID: model.ID{1}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("응답 상태 = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get(renewedTokenHeader); got != "" {
		t.Fatalf("갱신하지 않았는데 %s = %q", renewedTokenHeader, got)
	}
}

// TestVerificationFailureStopsRequest는 검증 실패가 갱신과 무관하게 요청을 막는지 확인한다.
func TestVerificationFailureStopsRequest(t *testing.T) {
	recorder := serveWithRenewal(t, fakeRenewer{err: errors.New("unauthenticated")})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("응답 상태 = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if got := recorder.Header().Get(renewedTokenHeader); got != "" {
		t.Fatalf("검증 실패인데 %s = %q", renewedTokenHeader, got)
	}
}

// TestVerificationErrorsAreClassifiedForTransport는 인가 서버의 검증 오류가 전송 계층이
// 가를 수 있는 형태로 옮겨지는지 확인한다.
//
// `mcp`는 `authz`를 의존하지 않으므로 이 경계가 자격 증명 실패만 `mcp.ErrUnauthenticated`로
// 감싼다. 내부 장애까지 감싸면 전송 계층이 그것을 401로 답해, 클라이언트가 재인증하고
// 돌아와도 같은 자리에서 다시 막힌다.
func TestVerificationErrorsAreClassifiedForTransport(t *testing.T) {
	for name, testCase := range map[string]struct {
		err             error
		unauthenticated bool
	}{
		"자격 증명 실패": {authz.ErrInvalidCredential, true},
		"내부 조회 장애": {fmt.Errorf("%w: 폐기 목록 조회", authz.ErrUnavailable), false},
	} {
		t.Run(name, func(t *testing.T) {
			verify := verifyWithRenewal(fakeRenewer{err: testCase.err})
			_, err := verify(t.Context(), "token", "https://service.test/mcp")
			if err == nil {
				t.Fatal("검증 실패가 오류로 나오지 않았다")
			}
			if got := errors.Is(err, mcp.ErrUnauthenticated); got != testCase.unauthenticated {
				t.Fatalf("ErrUnauthenticated 여부 = %t, want %t; 오류 = %v", got, testCase.unauthenticated, err)
			}
			// 어느 쪽이든 원인은 보존해 로그에서 추적할 수 있어야 한다.
			if !errors.Is(err, testCase.err) {
				t.Fatalf("원인이 보존되지 않았다: %v", err)
			}
		})
	}
}
