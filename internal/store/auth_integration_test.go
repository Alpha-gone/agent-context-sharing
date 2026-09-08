package store

import (
	"errors"
	"os"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestPermissionIntegration은 유효 등급과 소유 그래프 수 질의를 실제 데이터베이스에서
// 확인한다. 두 질의는 메모리 대역으로 대체할 수 없어 여기에서만 검증된다.
func TestPermissionIntegration(t *testing.T) {
	store := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, store, accountID)

	// 부여가 하나도 없는 조합은 오류가 아니라 부재여야 한다. 「연산별 권한 검사」가
	// 유효 등급이 없을 때를 not_found로 확정했으므로 오류와 구분돼야 한다.
	grade, found, err := store.EffectiveGrade(t.Context(), newTestID(t), accountID)
	if err != nil {
		t.Fatalf("부여 없는 조합의 유효 등급 조회: %v", err)
	}
	if found || grade != "" {
		t.Fatalf("부여 없는 조합이 등급을 반환했다: %q, found=%v", grade, found)
	}

	directGraphID := createTestGraph(t, store, accountID)
	grantAccount(t, store, directGraphID, accountID, model.GraphGradeViewer)
	assertGrade(t, store, directGraphID, accountID, model.GraphGradeViewer)

	// 직접 부여와 팀 상속이 겹치면 「상속 규칙」대로 더 높은 등급을 쓴다.
	teamID := newTestID(t)
	createTestTeam(t, store, teamID, accountID, false)
	addTestTeamMember(t, store, teamID, accountID)
	grantTeam(t, store, directGraphID, teamID, model.GraphGradeOwner)
	assertGrade(t, store, directGraphID, accountID, model.GraphGradeOwner)

	// 삭제된 팀의 부여는 행이 남아도 적용되지 않는다.
	deletedTeamID := newTestID(t)
	createTestTeam(t, store, deletedTeamID, accountID, true)
	addTestTeamMember(t, store, deletedTeamID, accountID)
	deletedTeamGraphID := createTestGraph(t, store, accountID)
	grantTeam(t, store, deletedTeamGraphID, deletedTeamID, model.GraphGradeOwner)
	if _, found, err := store.EffectiveGrade(t.Context(), deletedTeamGraphID, accountID); err != nil || found {
		t.Fatalf("삭제된 팀의 등급이 적용됐다: found=%v, err=%v", found, err)
	}

	// 소유 그래프 수는 직접 부여와 활성 팀 상속을 합쳐 센다. 위에서 만든 그래프는
	// 팀 상속으로 소유자 등급을 갖는 directGraphID 하나뿐이다.
	count, err := store.OwnedGraphCount(t.Context(), accountID)
	if err != nil {
		t.Fatalf("소유 그래프 수 조회: %v", err)
	}
	if count != 1 {
		t.Fatalf("소유 그래프 수 = %d, want 1", count)
	}
}

// TestAuthorizationCodeIntegration은 인가 코드의 첫 소비와 재사용 판정을 실제
// 데이터베이스에서 확인한다. 인가 서버 단위 테스트는 메모리 대역을 쓰므로 이 경로의
// nullable 열 처리가 여기에서만 드러난다.
func TestAuthorizationCodeIntegration(t *testing.T) {
	store := newIntegrationStore(t)
	accountID := newTestID(t)
	createTestAccount(t, store, accountID)

	now := time.Now().UTC()
	code := AuthorizationCode{
		Hash:          "hash-" + newTestID(t).String(),
		ClientID:      "integration-client",
		AccountID:     accountID,
		RedirectURI:   "http://127.0.0.1:1234/callback",
		CodeChallenge: "challenge",
		Resource:      "https://example.test/mcp",
		IssuedAt:      now,
		ExpiresAt:     now.Add(time.Minute),
	}
	if err := store.CreateAuthorizationCode(t.Context(), code); err != nil {
		t.Fatalf("인가 코드 저장: %v", err)
	}

	// 첫 소비는 issued_token_id가 비어 있는 상태에서 일어난다.
	consumed, err := store.ConsumeAuthorizationCode(t.Context(), code.Hash, time.Now().UTC())
	if err != nil {
		t.Fatalf("첫 인가 코드 소비: %v", err)
	}
	if consumed.ClientID != code.ClientID || consumed.AccountID != accountID || consumed.CodeChallenge != code.CodeChallenge {
		t.Fatalf("소비한 인가 코드의 교환 정보가 다르다: %+v", consumed)
	}
	if consumed.IssuedTokenID != "" {
		t.Fatalf("첫 소비에서 발급 토큰 식별자가 비어 있지 않다: %q", consumed.IssuedTokenID)
	}

	tokenID := "token-" + newTestID(t).String()
	if err := store.SetAuthorizationCodeTokenID(t.Context(), code.Hash, tokenID); err != nil {
		t.Fatalf("발급 토큰 식별자 기록: %v", err)
	}

	// 재사용은 폐기할 토큰 식별자를 담아 거절한다.
	_, err = store.ConsumeAuthorizationCode(t.Context(), code.Hash, time.Now().UTC())
	used, ok := errors.AsType[CodeUsedError](err)
	if !ok {
		t.Fatalf("인가 코드 재사용 오류 = %v", err)
	}
	if used.TokenID != tokenID {
		t.Fatalf("재사용 오류의 토큰 식별자 = %q, want %q", used.TokenID, tokenID)
	}

	// 없는 코드는 재사용과 구분한다.
	if _, err := store.ConsumeAuthorizationCode(t.Context(), "hash-"+newTestID(t).String(), time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("없는 인가 코드 소비 = %v, want ErrNotFound", err)
	}
}

// newIntegrationStore는 실제 데이터베이스가 있을 때만 저장소를 연다. 건너뛰기 조건은
// TestStoreIntegration과 같다.
func newIntegrationStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("TEST_DATABASE_REQUIRED가 설정됐지만 TEST_DATABASE_URL이 비어 있다")
		}
		t.Skip("TEST_DATABASE_URL이 없어 통합 테스트를 건너뛴다")
	}
	graphName := os.Getenv("AGE_GRAPH_NAME")
	if graphName == "" {
		graphName = "agent_context"
	}
	store, err := New(t.Context(), databaseURL, graphName)
	if err != nil {
		t.Fatalf("저장소 준비: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// createTestGraph는 등급 판정 대상이 될 활성 그래프를 만든다.
func createTestGraph(t *testing.T, store *Store, createdBy model.ID) model.ID {
	t.Helper()
	now := time.Now().UTC()
	graphID := newTestID(t)
	graph := model.Graph{
		ID:             graphID,
		Name:           "permission integration",
		CreatedBy:      createdBy,
		CreatedAt:      now,
		LastActivityAt: now,
		Version:        1,
	}
	if _, err := store.CreateGraph(t.Context(), graph); err != nil {
		t.Fatalf("테스트 그래프 생성: %v", err)
	}
	return graphID
}

// assertGrade는 유효 등급이 기대한 값인지 확인한다.
func assertGrade(t *testing.T, store *Store, graphID, accountID model.ID, want model.GraphGrade) {
	t.Helper()
	grade, found, err := store.EffectiveGrade(t.Context(), graphID, accountID)
	if err != nil {
		t.Fatalf("유효 등급 조회: %v", err)
	}
	if !found || grade != want {
		t.Fatalf("유효 등급 = %q(found=%v), want %q", grade, found, want)
	}
}
