package web

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
)

type filterGraphStore struct {
	*fakeGraphStore
	filter model.GraphListFilter
	after  string
	calls  int
}

func (s *filterGraphStore) ListGraphs(_ context.Context, _ model.ID, filter model.GraphListFilter, cursor string, _ int) ([]model.GraphListItem, string, error) {
	s.filter, s.after = filter, cursor
	s.calls++
	return nil, s.cursor, nil
}

// graphFilterForm는 실제 렌더링된 GET 양식에서 성공적으로 제출되는 입력만 모은다.
func graphFilterForm(t *testing.T, body string) url.Values {
	t.Helper()
	_, form, found := strings.Cut(body, `<form class="panel actions" method="get" action="/graphs">`)
	if !found {
		t.Fatal("그래프 필터 GET 양식이 없습니다")
	}
	form, _, found = strings.Cut(form, "</form>")
	if !found {
		t.Fatal("필터 양식이 닫히지 않았습니다")
	}
	if strings.Contains(form, "<select") {
		t.Fatal("복수 등급을 단일 선택으로 표시했습니다")
	}
	values := url.Values{}
	inputs := regexp.MustCompile(`<input\b[^>]*>`).FindAllString(form, -1)
	attributes := regexp.MustCompile(`([a-z][a-z0-9-]*)="([^"]*)"`)
	var grades []string
	for _, input := range inputs {
		attrs := map[string]string{}
		for _, attribute := range attributes.FindAllStringSubmatch(input, -1) {
			attrs[attribute[1]] = html.UnescapeString(attribute[2])
		}
		if attrs["name"] == "grade" {
			grades = append(grades, attrs["value"])
			if attrs["type"] != "checkbox" || attrs["id"] != "grade-"+attrs["value"] || !strings.Contains(form, `<label for="`+attrs["id"]+`"`) {
				t.Fatal("등급 입력이 레이블을 가진 체크박스가 아닙니다")
			}
			if !strings.Contains(input, " checked") {
				continue
			}
		}
		values.Add(attrs["name"], attrs["value"])
	}
	if !slices.Equal(grades, []string{"owner", "editor", "viewer"}) || !strings.Contains(form, "선택하지 않으면 모든 등급을 조회합니다.") || !strings.Contains(form, "<legend>등급</legend>") {
		t.Fatal("등급 선택 집합·안내·그룹 의미가 다릅니다")
	}
	if values.Has("cursor") {
		t.Fatal("새 필터 제출이 이전 페이지 커서를 유지합니다")
	}
	return values
}

func TestGraphListFilterCanChangeAndClearGrades(t *testing.T) {
	accountID := testID(t)
	graphs := &filterGraphStore{fakeGraphStore: &fakeGraphStore{accountID: accountID}}
	server, err := New(fakeAuthentication{accountID: accountID}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs?name=검색&grade=owner&grade=editor", nil))
	form := graphFilterForm(t, response.Body.String())
	form["grade"] = []string{"editor"}
	changed := httptest.NewRecorder()
	server.ServeHTTP(changed, sessionRequest(http.MethodGet, "/graphs?"+form.Encode(), nil))
	if changed.Code != http.StatusOK || graphs.filter.Name != "검색" || !slices.Equal(graphs.filter.Grades, []model.GraphGrade{model.GraphGradeEditor}) {
		t.Fatal("일부 등급 해제가 남은 선택을 바꿨습니다")
	}
	form = graphFilterForm(t, changed.Body.String())
	form.Del("grade")
	cleared := httptest.NewRecorder()
	server.ServeHTTP(cleared, sessionRequest(http.MethodGet, "/graphs?"+form.Encode(), nil))
	if cleared.Code != http.StatusOK || graphs.filter.Name != "검색" || len(graphs.filter.Grades) != 0 || len(graphFilterForm(t, cleared.Body.String())["grade"]) != 0 {
		t.Fatal("전체 선택 해제가 모든 등급 조회로 돌아가지 않았습니다")
	}
}

func TestGraphListFilterFormPreservesGrades(t *testing.T) {
	for _, test := range []struct {
		name   string
		query  []string
		grades []string
	}{
		{name: "모든 등급"},
		{name: "기존 빈 값", query: []string{""}},
		{name: "소유자", query: []string{"owner"}, grades: []string{"owner"}},
		{name: "편집자", query: []string{"editor"}, grades: []string{"editor"}},
		{name: "열람자", query: []string{"viewer"}, grades: []string{"viewer"}},
		{name: "복수 등급", query: []string{"viewer", "owner"}, grades: []string{"owner", "viewer"}},
		{name: "세 등급", query: []string{"viewer", "owner", "editor"}, grades: []string{"owner", "editor", "viewer"}},
		{name: "중복과 빈 값", query: []string{"owner", "", "owner", "editor"}, grades: []string{"owner", "editor"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			accountID := testID(t)
			graphs := &filterGraphStore{fakeGraphStore: &fakeGraphStore{accountID: accountID, cursor: "next+/=&"}}
			server, err := New(fakeAuthentication{accountID: accountID}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			name := `검색 "<&+ 이름`
			query := url.Values{"name": {name}, "cursor": {"previous"}}
			if test.query != nil {
				query["grade"] = test.query
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs?"+query.Encode(), nil))
			if response.Code != http.StatusOK || graphs.calls != 1 || graphs.after != "previous" || graphs.filter.Name != name {
				t.Fatal("현재 필터가 목록 조회로 전달되지 않았습니다")
			}
			form := graphFilterForm(t, response.Body.String())
			if form.Get("name") != name || !slices.Equal(form["grade"], test.grades) {
				t.Fatalf("현재 필터 양식 = %v", form)
			}
			// 더 보기 주소를 실제로 따라가도 필터와 선택 표시가 유지돼야 한다.
			link := regexp.MustCompile(`<a href="([^"]+)">더 보기</a>`).FindStringSubmatch(response.Body.String())
			if len(link) != 2 {
				t.Fatal("더 보기 링크가 없습니다")
			}
			next := httptest.NewRecorder()
			server.ServeHTTP(next, sessionRequest(http.MethodGet, html.UnescapeString(link[1]), nil))
			if next.Code != http.StatusOK || graphs.after != graphs.cursor || graphs.filter.Name != name {
				t.Fatal("페이지 이동이 현재 조건과 다음 커서를 유지하지 않았습니다")
			}
			if !slices.Equal(graphFilterForm(t, next.Body.String())["grade"], test.grades) {
				t.Fatal("페이지 이동 뒤 선택한 등급이 바뀌었습니다")
			}
			form.Set("name", "바꾼 이름")
			resubmitted := httptest.NewRecorder()
			server.ServeHTTP(resubmitted, sessionRequest(http.MethodGet, "/graphs?"+form.Encode(), nil))
			var want []model.GraphGrade
			for _, grade := range test.grades {
				want = append(want, model.GraphGrade(grade))
			}
			if resubmitted.Code != http.StatusOK || graphs.calls != 3 || graphs.after != "" || graphs.filter.Name != "바꾼 이름" || !slices.Equal(graphs.filter.Grades, want) {
				t.Fatal("이름 재제출이 선택한 등급을 유지하지 않았습니다")
			}
		})
	}
}

func TestGraphListFilterRejectsInvalidGrade(t *testing.T) {
	accountID := testID(t)
	graphs := &filterGraphStore{fakeGraphStore: &fakeGraphStore{accountID: accountID}}
	server, err := New(fakeAuthentication{accountID: accountID}, graphs, Config{SecureCookie: func(*http.Request) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, sessionRequest(http.MethodGet, "/graphs?grade=owner&grade=unknown", nil))
	if response.Code != http.StatusBadRequest || graphs.calls != 0 || !strings.Contains(response.Body.String(), "등급 필터 값이 올바르지 않습니다.") {
		t.Fatal("잘못된 등급을 조회 전에 거부하지 않았습니다")
	}
}
