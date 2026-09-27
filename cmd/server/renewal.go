package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"agent_context_sharing/internal/authz"
	"agent_context_sharing/internal/mcp"
	"agent_context_sharing/internal/model"
)

const (
	// renewedTokenHeader에는 「토큰 갱신」이 응답 헤더로 전달하기로 한 새 접근 토큰을 담는다.
	renewedTokenHeader = "Mcp-Access-Token"
	// renewedExpiresHeader에는 새 토큰의 만료 시각을 Unix 초로 담는다. 「토큰 갱신」의
	// 시계 기준이 서버이므로 클라이언트가 서버가 알린 이 값을 그대로 쓴다.
	renewedExpiresHeader = "Mcp-Access-Token-Expires-At"
)

// renewalHeaderKey는 응답 헤더를 토큰 검증 지점까지 나르는 비공개 컨텍스트 키다.
type renewalHeaderKey struct{}

// tokenRenewer는 갱신 헤더를 다는 데 필요한 인가 서버 계약만 노출한다.
type tokenRenewer interface {
	VerifyAndRenew(context.Context, authz.DPoPRequest) (model.ID, authz.Token, error)
}

// withRenewalHeader는 토큰 검증이 갱신 결과를 실을 응답 헤더를 요청 컨텍스트에 둔다.
//
// 「토큰 갱신」이 갱신을 `mcp`가 아니라 HTTP 계층에서 처리하라고 확정했으므로 판정과
// 헤더 기록을 모두 이 패키지에 둔다. `mcp`는 본문을 쓰기 전에 검증 함수를 부르므로
// 검증 지점에서 헤더를 달면 응답 본문보다 먼저 나간다.
func withRenewalHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := context.WithValue(request.Context(), renewalHeaderKey{}, writer.Header())
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

// verifyWithRenewal은 MCP 토큰을 검증하면서 갱신 구간에 든 토큰을 응답 헤더로 알린다.
//
// 검증 실패를 두 갈래로 옮기는 자리이기도 하다. `mcp`는 `authz`를 의존하지 않으므로
// 인가 서버의 오류를 전송 계층이 아는 형태로 바꾸는 일을 이 경계가 맡는다. 토큰 실패와
// proof 실패는 각각의 DPoP 도전으로, 내부 장애는 원인을 보존한 `500`으로 이어진다.
func verifyWithRenewal(service tokenRenewer) mcp.VerifyFunc {
	return func(ctx context.Context, request mcp.Authentication) (model.ID, error) {
		accountID, renewed, err := service.VerifyAndRenew(ctx, authz.DPoPRequest{AccessToken: request.AccessToken, Proof: request.Proof, Method: request.Method, Target: request.Target})
		if err != nil {
			if errors.Is(err, authz.ErrInvalidCredential) {
				return model.ID{}, fmt.Errorf("%w: %w", mcp.ErrInvalidToken, err)
			}
			if errors.Is(err, authz.ErrInvalidDPoPProof) {
				return model.ID{}, fmt.Errorf("%w: %w", mcp.ErrInvalidDPoPProof, err)
			}
			return model.ID{}, err
		}
		if renewed.Raw == "" {
			return accountID, nil
		}
		header, ok := ctx.Value(renewalHeaderKey{}).(http.Header)
		if !ok {
			return accountID, nil
		}
		header.Set(renewedTokenHeader, renewed.Raw)
		header.Set(renewedExpiresHeader, strconv.FormatInt(renewed.ExpiresAt.Unix(), 10))
		return accountID, nil
	}
}
