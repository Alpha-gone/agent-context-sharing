package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/plan"
	"agent_context_sharing/internal/store"
)

// web08AuditStore는 감사 조회 인자를 기록하고 고정한 페이지를 돌려준다.
type web08AuditStore struct {
	*fakeGraphStore
	entries []model.AuditEntry
	next    string
	err     error
	cancel  context.CancelFunc
	calls   int
	filter  model.AuditFilter
	cursor  string
	limit   int
}

func (s *web08AuditStore) ListAuditEntries(_ context.Context, _, _ model.ID, filter model.AuditFilter, cursor string, limit int) ([]model.AuditEntry, string, error) {
	s.calls++
	s.filter, s.cursor, s.limit = filter, cursor, limit
	if s.cancel != nil {
		s.cancel()
	}
	return s.entries, s.next, s.err
}

func web08Audit(t *testing.T, backend *web08AuditStore, query string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	plans, err := plan.ParseAccountPlans(fmt.Sprintf(`{"%s":{"graph_page":{"default":2,"maximum":3}}}`, backend.accountID))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(fakeAuthentication{accountID: backend.accountID}, backend, Config{Plans: plans, SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs/"+backend.graphID.String()+"/audit"+query, nil).WithContext(ctx))
	return response
}

// TestWEB08AuditFilterAndPage는 UTC 날짜·대상 필터를 저장소 범위로 바꾸고, 두 기록을
// 출처와 함께 표시하며 다음 페이지가 같은 필터와 새 커서를 유지하는지 확인한다.
func TestWEB08AuditFilterAndPage(t *testing.T) {
	account, graph, target, actor := testID(t), testID(t), testID(t), testID(t)
	at := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	backend := &web08AuditStore{
		fakeGraphStore: &fakeGraphStore{accountID: account, graphID: graph, grade: model.GraphGradeOwner},
		entries: []model.AuditEntry{
			{ID: testID(t), Source: "web", Kind: "grant", Action: "grant", ActorID: actor, OccurredAt: at, Target: target.String(), Detail: "<owner> → viewer"},
			{ID: testID(t), Source: "operation", Kind: "operation", Action: "add", ActorID: actor, OccurredAt: at.Add(-time.Minute), Target: target.String(), Detail: "applied"},
		},
		next: "next-cursor",
	}
	query := "?from=2026-10-01&until=2026-10-02&target=" + target.String() + "&cursor=old-cursor"
	response := web08Audit(t, backend, query, t.Context())
	body := response.Body.String()
	if response.Code != http.StatusOK || backend.limit != 2 || backend.cursor != "old-cursor" || backend.filter.Target != target {
		t.Fatalf("감사 조회 = %d, 한도 %d, 커서 %q", response.Code, backend.limit, backend.cursor)
	}
	if !backend.filter.From.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !backend.filter.Until.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) || backend.filter.From.Location() != time.UTC {
		t.Fatalf("기간 = %v ~ %v, 종료일을 포함하는 UTC 제외 경계여야 한다", backend.filter.From, backend.filter.Until)
	}
	for _, want := range []string{
		`value="2026-10-01"`, `value="2026-10-02"`, `value="` + target.String() + `"`, "<caption>웹 관리 기록과 관리 연산 기록 (최근 순)</caption>",
		`<th scope="col">행위 계정</th>`, "<td>웹 관리</td>", "<td>관리 연산</td>", actor.String(), "&lt;owner&gt; → viewer", "2026-10-02T03:04:05Z",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("감사 화면에 %q가 없다", want)
		}
	}
	link := regexp.MustCompile(`<a href="([^"]+)">더 보기</a>`).FindStringSubmatch(body)
	if link == nil {
		t.Fatal("더 보기 링크가 없다")
	}
	next, err := url.Parse(html.UnescapeString(link[1]))
	if err != nil {
		t.Fatal(err)
	}
	values := next.Query()
	if next.Path != "/graphs/"+graph.String()+"/audit" || values.Get("from") != "2026-10-01" || values.Get("until") != "2026-10-02" || values.Get("target") != target.String() || values.Get("cursor") != "next-cursor" || len(values) != 4 {
		t.Fatalf("다음 페이지 = %s", next)
	}
	if strings.Contains(body, `name="cursor"`) {
		t.Fatal("새 필터 제출에 이전 커서가 실린다")
	}

	backend.next, backend.entries = "", nil
	if body := web08Audit(t, backend, "", t.Context()).Body.String(); strings.Contains(body, "더 보기") || !strings.Contains(body, "조건에 맞는 감사 기록이 없습니다.") || backend.filter != (model.AuditFilter{}) {
		t.Fatal("필터 없는 빈 목록이 다르다")
	}
}

// TestWEB08AuditInputErrors는 잘못된 필터를 저장소 호출 없이 400으로 거부하는지 확인한다.
func TestWEB08AuditInputErrors(t *testing.T) {
	v4 := "0b6a4f1c-2d3e-4f50-8a6b-7c8d9e0f1a2b"
	for _, query := range []string{
		"?from=2026-10-1", "?until=2026-13-01", "?from=2026-10-03&until=2026-10-02", "?target=not-an-id", "?target=" + v4,
		"?unknown=1", "?from=2026-10-01&from=2026-10-02", "?cursor=a&cursor=b",
	} {
		t.Run(query, func(t *testing.T) {
			backend := &web08AuditStore{fakeGraphStore: &fakeGraphStore{accountID: testID(t), graphID: testID(t), grade: model.GraphGradeOwner}}
			response := web08Audit(t, backend, query, t.Context())
			if response.Code != http.StatusBadRequest || backend.calls != 0 || !strings.Contains(response.Body.String(), `class="error"`) {
				t.Fatalf("입력 오류 = %d, 저장소 호출 %d", response.Code, backend.calls)
			}
		})
	}
}

// TestWEB08AuditStoreErrors는 커서·접근 상실·입력·내부 장애를 구분하고 취소 뒤 렌더링하지
// 않으며, 커서 오류에서도 유효한 필터를 양식에 유지하는지 확인한다.
func TestWEB08AuditStoreErrors(t *testing.T) {
	for _, sample := range []struct {
		name     string
		err      error
		status   int
		canceled bool
	}{
		{"cursor", fmt.Errorf("감사 문맥: %w", store.ErrInvalidCursor), http.StatusBadRequest, false},
		{"lost_owner", fmt.Errorf("감사 문맥: %w", store.ErrNotFound), http.StatusNotFound, false},
		{"field", fmt.Errorf("감사 문맥: %w", model.FieldError{Field: "period", Message: "private field detail"}), http.StatusBadRequest, false},
		{"internal", errors.New("private database detail"), http.StatusInternalServerError, false},
		{"canceled", errors.New("private database detail"), 0, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			target := testID(t)
			backend := &web08AuditStore{err: sample.err, fakeGraphStore: &fakeGraphStore{accountID: testID(t), graphID: testID(t), grade: model.GraphGradeOwner}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if sample.canceled {
				backend.cancel = cancel
			}
			response := web08Audit(t, backend, "?from=2026-10-01&target="+target.String()+"&cursor=broken", ctx)
			body := response.Body.String()
			if strings.Contains(body, "private") || strings.Contains(body, "감사 문맥") {
				t.Fatal("내부 오류 원문이 노출됐다")
			}
			if sample.canceled {
				if response.Body.Len() != 0 {
					t.Fatal("취소 뒤 렌더링했다")
				}
				return
			}
			if response.Code != sample.status || !strings.Contains(body, `value="2026-10-01"`) || !strings.Contains(body, `value="`+target.String()+`"`) {
				t.Fatalf("감사 오류 = %d", response.Code)
			}
		})
	}
}
