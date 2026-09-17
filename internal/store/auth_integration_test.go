package store

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

// TestUpdatePasswordHashIfMatchesIntegration은 재해시가 읽은 기존 값과 같을 때만
// 반영돼 동시에 성공한 로그인 요청이 이미 바뀐 해시를 덮어쓰지 않음을 확인한다.
func TestUpdatePasswordHashIfMatchesIntegration(t *testing.T) {
	store := newIntegrationStore(t)
	accountID := newTestID(t)
	// 앞 8자리는 UUIDv7 시간 상위 비트뿐이라 65초 안의 재실행이 충돌한다. 임의 비트가 남는
	// 뒷부분을 써서 login_id 32자 상한 안에서 실행마다 다른 값을 만든다.
	loginID := "rehash_" + strings.ReplaceAll(accountID.String(), "-", "")[7:]
	oldHash := "legacy-" + accountID.String()
	newHash := "bcrypt-sha256-v1-" + accountID.String()
	if err := store.CreateAccount(t.Context(), Account{ID: accountID, LoginID: loginID, PasswordHash: oldHash, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("재해시 대상 계정 생성: %v", err)
	}

	type updateResult struct {
		updated bool
		err     error
	}
	results := make(chan updateResult, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Go(func() {
			updated, err := store.UpdatePasswordHashIfMatches(t.Context(), accountID, oldHash, newHash)
			results <- updateResult{updated: updated, err: err}
		})
	}
	waitGroup.Wait()
	close(results)

	updatedCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("기존 해시 조건부 갱신: %v", result.err)
		}
		if result.updated {
			updatedCount++
		}
	}
	if updatedCount != 1 {
		t.Fatalf("동시 조건부 갱신 성공 수 = %d, want 1", updatedCount)
	}
	account, err := store.AccountByLoginID(t.Context(), loginID)
	if err != nil {
		t.Fatalf("재해시 대상 계정 조회: %v", err)
	}
	if account.PasswordHash != newHash {
		t.Fatalf("갱신한 비밀번호 해시 = %q, want %q", account.PasswordHash, newHash)
	}
}

// TestSigningKeyIntegration은 JWT 헤더의 kid로 검증 키 하나만 읽는 경로를 확인한다.
func TestSigningKeyIntegration(t *testing.T) {
	store := newIntegrationStore(t)
	key := SigningKey{
		ID:         "key-" + newTestID(t).String(),
		Algorithm:  "ES256",
		PublicKey:  "test-public-key",
		PrivateKey: "test-private-key",
		State:      "retired",
		CreatedAt:  time.Now().UTC(),
	}
	if err := store.CreateSigningKey(t.Context(), key); err != nil {
		t.Fatalf("서명 키 생성: %v", err)
	}
	stored, err := store.SigningKey(t.Context(), key.ID)
	if err != nil {
		t.Fatalf("서명 키 조회: %v", err)
	}
	if stored.ID != key.ID || stored.PublicKey != key.PublicKey {
		t.Fatalf("조회한 서명 키 = %+v, want ID %q와 공개 키 %q", stored, key.ID, key.PublicKey)
	}
	if _, err := store.SigningKey(t.Context(), "missing-"+key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("없는 서명 키 조회 = %v, want ErrNotFound", err)
	}
	if _, err := store.ActiveSigningKey(t.Context()); errors.Is(err, ErrNotFound) {
		active := key
		active.ID = "active-" + newTestID(t).String()
		active.State = "active"
		if err := store.CreateSigningKey(t.Context(), active); err != nil {
			t.Fatalf("초기 활성 서명 키 생성: %v", err)
		}
	}
	duplicate := key
	duplicate.ID = "duplicate-active-" + newTestID(t).String()
	duplicate.State = "active"
	if err := store.CreateSigningKey(t.Context(), duplicate); !errors.Is(err, ErrActiveSigningKeyExists) {
		t.Fatalf("두 번째 활성 서명 키 생성 = %v, want ErrActiveSigningKeyExists", err)
	}
}

// TestRotateSigningKeyIntegration은 동시에 시작한 회전 중 하나만 활성 키를 만들고,
// 충돌한 요청은 저장 실패가 아닌 활성 키 경합으로 구분하는지 확인한다.
func TestRotateSigningKeyIntegration(t *testing.T) {
	store := newIntegrationStore(t)
	if _, err := store.ActiveSigningKey(t.Context()); errors.Is(err, ErrNotFound) {
		key := SigningKey{
			ID:         "active-" + newTestID(t).String(),
			Algorithm:  "ES256",
			PublicKey:  "test-public-key",
			PrivateKey: "test-private-key",
			State:      "active",
			CreatedAt:  time.Now().UTC(),
		}
		if err := store.CreateSigningKey(t.Context(), key); err != nil {
			t.Fatalf("초기 활성 서명 키 생성: %v", err)
		}
	} else if err != nil {
		t.Fatalf("활성 서명 키 조회: %v", err)
	}

	connection, err := store.pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("서명 키 잠금 연결 확보: %v", err)
	}
	defer connection.Release()
	transaction, err := connection.Begin(t.Context())
	if err != nil {
		t.Fatalf("서명 키 잠금 트랜잭션 시작: %v", err)
	}
	defer transaction.Rollback(t.Context())
	if _, err := transaction.Exec(t.Context(), `LOCK TABLE public.signing_key IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("서명 키 테이블 잠금: %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		key := SigningKey{
			ID:         "rotate-" + newTestID(t).String(),
			Algorithm:  "ES256",
			PublicKey:  "test-public-key",
			PrivateKey: "test-private-key",
			State:      "active",
			CreatedAt:  time.Now().UTC(),
		}
		waitGroup.Go(func() {
			<-start
			results <- store.RotateSigningKey(t.Context(), key)
		})
	}
	close(start)

	deadline := time.Now().Add(time.Second)
	for {
		var waiting int
		err := transaction.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE relation = 'public.signing_key'::regclass AND mode = 'RowExclusiveLock' AND NOT granted`).Scan(&waiting)
		if err != nil {
			t.Fatalf("대기 중인 서명 키 회전 조회: %v", err)
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("대기 중인 서명 키 회전 수 = %d, want 2", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := transaction.Commit(t.Context()); err != nil {
		t.Fatalf("서명 키 테이블 잠금 해제: %v", err)
	}

	waitGroup.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrActiveSigningKeyExists):
			conflicted++
		default:
			t.Fatalf("동시 서명 키 회전 = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("동시 서명 키 회전 성공=%d, 경합=%d, want 각각 1", succeeded, conflicted)
	}
}

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

	// 소유자의 웹 직접 삭제 그래프는 복구 화면에서 같은 소유자가 되살릴 수 있어야 하므로,
	// deleted_at이 있어도 유효 등급 계산에서 제외하지 않는다. 삭제 상태의 화면·연산
	// 가용성은 이 계산과 별도로 판정하며, 목록은 삭제 그래프를 보이지 않게 한다.
	deletedGraphID := createTestGraph(t, store, accountID)
	grantAccount(t, store, deletedGraphID, accountID, model.GraphGradeOwner)
	deletedAt := time.Now().UTC()
	if _, err := store.pool.Exec(t.Context(), `UPDATE public.context_graph SET deleted_at = $1 WHERE graph_id = $2`, deletedAt, deletedGraphID.String()); err != nil {
		t.Fatalf("소프트 삭제 그래프 표시: %v", err)
	}
	assertGrade(t, store, deletedGraphID, accountID, model.GraphGradeOwner)
	listed, _, err := store.ListGraphs(t.Context(), accountID, model.GraphListFilter{}, "", 10)
	if err != nil {
		t.Fatalf("그래프 목록 조회: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != directGraphID {
		t.Fatalf("소프트 삭제 그래프가 목록에 노출됐다: %#v", listed)
	}

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

	tokenID := "token-" + newTestID(t).String()
	// PostgreSQL timestamptz는 마이크로초까지 보존하므로, 왕복 뒤 비교하는 기대값도
	// 같은 정밀도로 맞춘다.
	tokenExpiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	// 첫 소비는 발급 토큰 정보와 한 문장에서 기록한다.
	consumed, err := store.ConsumeAuthorizationCode(t.Context(), code.Hash, tokenID, time.Now().UTC(), tokenExpiresAt)
	if err != nil {
		t.Fatalf("첫 인가 코드 소비: %v", err)
	}
	if consumed.ClientID != code.ClientID || consumed.AccountID != accountID || consumed.CodeChallenge != code.CodeChallenge {
		t.Fatalf("소비한 인가 코드의 교환 정보가 다르다: %+v", consumed)
	}
	if consumed.IssuedTokenID != tokenID || consumed.IssuedTokenExpiresAt == nil || !consumed.IssuedTokenExpiresAt.Equal(tokenExpiresAt) {
		t.Fatalf("첫 소비의 발급 토큰 기록 = %+v, want %q와 %s", consumed, tokenID, tokenExpiresAt)
	}

	// 재사용은 폐기할 토큰 식별자를 담아 거절한다.
	_, err = store.ConsumeAuthorizationCode(t.Context(), code.Hash, "other-"+newTestID(t).String(), time.Now().UTC(), tokenExpiresAt)
	used, ok := errors.AsType[CodeUsedError](err)
	if !ok {
		t.Fatalf("인가 코드 재사용 오류 = %v", err)
	}
	if used.TokenID != tokenID {
		t.Fatalf("재사용 오류의 토큰 식별자 = %q, want %q", used.TokenID, tokenID)
	}
	if !used.TokenExpiresAt.Equal(tokenExpiresAt) {
		t.Fatalf("재사용 오류의 토큰 만료 시각 = %s, want %s", used.TokenExpiresAt, tokenExpiresAt)
	}

	// 없는 코드는 재사용과 구분한다.
	if _, err := store.ConsumeAuthorizationCode(t.Context(), "hash-"+newTestID(t).String(), "token-"+newTestID(t).String(), time.Now().UTC(), tokenExpiresAt); !errors.Is(err, ErrNotFound) {
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
	store, err := New(t.Context(), databaseURL, graphName, nil, nil)
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
