package authz

import (
	"agent_context_sharing/internal/diagnostic"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

type obs05Events struct{ events []diagnostic.Event }

func (s *obs05Events) Emit(_ context.Context, e diagnostic.Event) bool {
	s.events = append(s.events, e)
	return false
}

func TestAuthenticationObservationFailureReasons(t *testing.T) {
	for _, sample := range []struct {
		name, level, reason string
		cause               error
		requestError        error
	}{
		{"wrapped cancellation", "INFO", "canceled", context.Canceled, nil},
		{"wrapped deadline", "INFO", "deadline_exceeded", context.DeadlineExceeded, nil},
		{"storage failure", "ERROR", "authentication_unavailable", errors.New("private-storage-error"), nil},
		{"request cancellation", "INFO", "canceled", nil, context.Canceled},
		{"request deadline", "INFO", "deadline_exceeded", nil, context.DeadlineExceeded},
	} {
		t.Run(sample.name, func(t *testing.T) {
			ctx := t.Context()
			if sample.requestError == context.Canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if sample.requestError == context.DeadlineExceeded {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			sink := &obs05Events{}
			source, _ := diagnostic.NewSource("failure-reasons")
			reporter, _ := diagnostic.NewReporter(diagnostic.Auth, sink, source)
			service := &Service{diagnostics: reporter}
			var failure error
			if sample.cause != nil {
				failure = &credentialFailure{ErrUnavailable, fmt.Errorf("lookup: %w", sample.cause)}
			}
			ctx = diagnostic.WithTrace(ctx, diagnostic.NewRequestTrace())
			service.authObservation(ctx, "token_validation", model.ID{}, "", model.ID{}, failure)
			if len(sink.events) != 1 {
				t.Fatalf("events = %d", len(sink.events))
			}
			event := sink.events[0]
			if err := event.Validate(); err != nil || event.Level != sample.level || event.Result != "failed" || event.ReasonCode != sample.reason {
				t.Fatalf("event = %+v, validation = %v", event, err)
			}
			if failure != nil && (!errors.Is(failure, ErrUnavailable) || !errors.Is(failure, sample.cause) || failure.Error() != "unavailable") {
				t.Fatal("인증 오류의 외부 계약 또는 내부 원인 변경")
			}
		})
	}
}
func TestOBS05AuthenticationCausesAndSafeFields(t *testing.T) {
	service := testService(t, newMemoryStore())
	sink := &obs05Events{}
	source, _ := diagnostic.NewSource("obs05")
	reporter, _ := diagnostic.NewReporter(diagnostic.Auth, sink, source)
	service.SetDiagnostics(reporter)
	ctx := diagnostic.WithTrace(t.Context(), diagnostic.NewRequestTrace())
	if _, err := service.Authenticate(ctx, "missing", "private password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal(err)
	}
	account, err := service.Register(ctx, "obs05_user", "private password")
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.WebSession(ctx, account, "web")
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return token.ExpiresAt }
	if _, _, err := service.Verify(ctx, token.Raw, "web"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal("만료가 자격 오류를 보존하지 않음")
	}
	foundExpired := false
	for _, e := range sink.events {
		if e.Validate() != nil {
			t.Fatal("이벤트 거부")
		}
		if e.ReasonCode == "expired" {
			foundExpired = true
			if e.IdentityVerified || e.Fields.Identity != (diagnostic.IdentityFields{}) {
				t.Fatal("만료 자격의 ID 기록")
			}
		}
	}
	if !foundExpired {
		t.Fatal("만료 원인 누락")
	}
	raw, _ := json.Marshal(sink.events)
	for _, secret := range []string{"private password", token.Raw, "obs05_user"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("자격·로그인 원문 누출")
		}
	}
}
func TestOBS05RefreshFlowAndRejectionDatabase(t *testing.T) {
	backend, _, _ := newAuthenticationContractDatabase(t)
	cfg := oauthTestConfig()
	cfg.AuthorizationCodes, cfg.AccessTokens, cfg.RefreshTokens = backend, backend, backend
	service, err := New(backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &obs05Events{}
	source, _ := diagnostic.NewSource("obs05")
	reporter, _ := diagnostic.NewReporter(diagnostic.Auth, sink, source)
	service.SetDiagnostics(reporter)
	ctx := diagnostic.WithTrace(t.Context(), diagnostic.NewRequestTrace())
	account, err := service.Register(ctx, "obs05_refresh", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	code, err := service.IssueAuthorizationCode(ctx, account, validAuthorizationRequest())
	if err != nil {
		t.Fatal(err)
	}
	input := validAuthorizationRequest()
	tokens, err := service.ExchangeAuthorizationCode(ctx, code, input.ClientID, input.RedirectURI, input.Resource, oauthVerifier)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Refresh(ctx, tokens.RefreshToken, input.ClientID, input.Resource, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Refresh(ctx, tokens.RefreshToken, input.ClientID, input.Resource, "")
	if err == nil {
		t.Fatal("재사용 허용")
	}
	flow := ""
	reuse := false
	for _, e := range sink.events {
		if e.Validate() != nil {
			t.Fatal("잘못된 auth 이벤트")
		}
		if e.Stage == "code_exchange" && e.Result == "completed" {
			flow = e.FlowID
		}
		if e.ReasonCode == "reuse_detected" {
			reuse = true
			if e.FlowID == "" || e.FlowID != flow {
				t.Fatal("갱신 재사용 흐름 연결 누락")
			}
		}
	}
	if !reuse || flow == "" {
		t.Fatal("교환·재사용 이벤트 누락")
	}
	raw, _ := json.Marshal(sink.events)
	for _, secret := range []string{tokens.AccessToken, tokens.RefreshToken, code, input.RedirectURI} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("OAuth 자격·URI 누출")
		}
	}
}

func TestOBS05OAuthRejectionClassesDatabase(t *testing.T) {
	backend, _, pool := newAuthenticationContractDatabase(t)
	cfg := oauthTestConfig()
	cfg.AuthorizationCodes, cfg.AccessTokens, cfg.RefreshTokens = backend, backend, backend
	service, err := New(backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &obs05Events{}
	source, _ := diagnostic.NewSource("obs05")
	reporter, _ := diagnostic.NewReporter(diagnostic.Auth, sink, source)
	service.SetDiagnostics(reporter)
	ctx := diagnostic.WithTrace(t.Context(), diagnostic.NewRequestTrace())
	account, err := service.Register(ctx, "obs05_classes", "correct password")
	if err != nil {
		t.Fatal(err)
	}
	input := validAuthorizationRequest()
	base := time.Now().UTC().Truncate(time.Second)
	issue := func() OAuthTokens {
		t.Helper()
		service.now = func() time.Time { return base }
		code, err := service.IssueAuthorizationCode(ctx, account, input)
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := service.ExchangeAuthorizationCode(ctx, code, input.ClientID, input.RedirectURI, input.Resource, oauthVerifier)
		if err != nil {
			t.Fatal(err)
		}
		return tokens
	}
	for _, reason := range []string{"individually_revoked", "family_revoked", "inactive_expired", "expired", "binding_violation"} {
		t.Run(reason, func(t *testing.T) {
			tokens := issue()
			before := len(sink.events)
			switch reason {
			case "individually_revoked":
				claims, claimErr := service.signedClaims(ctx, tokens.AccessToken, input.Resource)
				if claimErr != nil {
					t.Fatal(claimErr)
				}
				if err := service.Revoke(ctx, Token{ID: claims.ID, ExpiresAt: claims.Expiry.Time()}); err != nil {
					t.Fatal(err)
				}
				_, err = service.VerifyBearer(ctx, tokens.AccessToken)
			case "family_revoked":
				if _, err := service.Refresh(ctx, tokens.RefreshToken, input.ClientID, input.Resource, ""); err != nil {
					t.Fatal(err)
				}
				service.Refresh(ctx, tokens.RefreshToken, input.ClientID, input.Resource, "")
				_, err = service.VerifyBearer(ctx, tokens.AccessToken)
			case "inactive_expired":
				service.now = func() time.Time { return base.Add(7 * 24 * time.Hour) }
				_, err = service.Refresh(ctx, tokens.RefreshToken, input.ClientID, input.Resource, "")
			case "expired":
				service.now = func() time.Time { return base.Add(30 * 24 * time.Hour) }
				_, err = service.Refresh(ctx, tokens.RefreshToken, input.ClientID, input.Resource, "")
			case "binding_violation":
				_, err = service.Refresh(ctx, tokens.RefreshToken, "native", input.Resource, "")
			}
			if err == nil {
				t.Fatal("자격 거부가 성공으로 바뀜")
			}
			events := sink.events[before:]
			if len(events) == 0 || events[len(events)-1].ReasonCode != reason {
				t.Fatalf("원인 분류: %+v", events)
			}
			for _, e := range events {
				if e.Validate() != nil {
					t.Fatal("거부 이벤트 허용 필드 위반")
				}
			}
		})
	}
	// 저장소 장애는 잘못된 자격과 구분하되 DB 오류 원문을 싣지 않는다.
	tokens := issue()
	if _, err := pool.Exec(ctx, "ALTER TABLE public.oauth_grant RENAME TO obs05_saved_grant"); err != nil {
		t.Fatal(err)
	}
	_, err = service.VerifyBearer(ctx, tokens.AccessToken)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("저장소 장애가 자격 거부로 바뀜")
	}
	if sink.events[len(sink.events)-1].ReasonCode != "authentication_unavailable" {
		t.Fatal("저장소 장애 분류 누락")
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE public.obs05_saved_grant RENAME TO oauth_grant"); err != nil {
		t.Fatal(err)
	}
}
