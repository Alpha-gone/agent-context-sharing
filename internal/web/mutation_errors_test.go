package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

func TestManagementMutationErrorResponses(t *testing.T) {
	internal := errors.New("secret database connection detail")
	for _, test := range []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"부여/마지막 소유자", store.ErrLastOwner, http.StatusConflict, "마지막 소유자 등급은 낮출 수 없습니다."},
		{"부여/없는 대상", store.ErrNotFound, http.StatusNotFound, "등급을 바꿀 대상을 찾을 수 없습니다."},
		{"부여/장애", internal, http.StatusInternalServerError, "등급을 부여할 수 없습니다."},
		{"회수/마지막 소유자", store.ErrLastOwner, http.StatusConflict, "마지막 소유자 등급은 회수할 수 없습니다."},
		{"회수/없는 등급", store.ErrNotFound, http.StatusNotFound, "회수할 등급을 찾을 수 없습니다."},
		{"회수/장애", internal, http.StatusInternalServerError, "등급을 회수할 수 없습니다."},
		{"복구 요청/없는 대상", store.ErrNotFound, http.StatusNotFound, "복구 요청 대상을 찾을 수 없습니다."},
		{"복구 요청/상태", store.ErrInvalidState, http.StatusConflict, "현재 상태에서는 복구를 요청할 수 없습니다."},
		{"복구 요청/장애", internal, http.StatusInternalServerError, "복구 요청을 처리할 수 없습니다."},
		{"운영자 복구/없는 요청", store.ErrNotFound, http.StatusNotFound, "처리할 복구 요청을 찾을 수 없습니다."},
		{"운영자 복구/상태", store.ErrInvalidState, http.StatusConflict, "현재 상태에서는 그래프를 복구할 수 없습니다."},
		{"운영자 복구/장애", internal, http.StatusInternalServerError, "그래프를 복구할 수 없습니다."},
	} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/래핑=%t", test.name, wrapped), func(t *testing.T) {
				accountID, graphID := testID(t), testID(t)
				graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, grade: model.GraphGradeOwner}
				err := test.err
				if wrapped {
					err = fmt.Errorf("작업 문맥: %w", err)
				}
				target, form := mutationRequest(graphID, test.name)
				switch {
				case strings.HasPrefix(test.name, "부여/"):
					graphs.grantErr = err
				case strings.HasPrefix(test.name, "회수/"):
					graphs.revokeErr = err
				case strings.HasPrefix(test.name, "복구 요청/"):
					graphs.requestRestoreErr = err
				default:
					graphs.operatorRestoreErr = err
				}
				response := httptest.NewRecorder()
				newTestServerWith(t, accountID, graphs).ServeHTTP(response, sessionRequest(http.MethodPost, target, form))
				if response.Code != test.status || !strings.Contains(response.Body.String(), test.message) {
					t.Fatalf("응답 상태 = %d, 안내 일치 = %t, 기대 %d %q", response.Code, strings.Contains(response.Body.String(), test.message), test.status, test.message)
				}
				if strings.Contains(response.Body.String(), "secret database") || strings.Contains(response.Body.String(), "작업 문맥") {
					t.Fatal("내부 오류 원문이 응답에 노출됐다")
				}
			})
		}
	}
}

func TestManagementMutationsKeepSuccessRedirects(t *testing.T) {
	for _, action := range []string{"부여/성공", "회수/성공", "복구 요청/성공", "운영자 복구/성공"} {
		t.Run(action, func(t *testing.T) {
			accountID, graphID := testID(t), testID(t)
			graphs := &fakeGraphStore{accountID: accountID, graphID: graphID, grade: model.GraphGradeOwner}
			target, form := mutationRequest(graphID, action)
			response := httptest.NewRecorder()
			newTestServerWith(t, accountID, graphs).ServeHTTP(response, sessionRequest(http.MethodPost, target, form))
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != target {
				t.Fatalf("성공 이동 = %d %q, 기대 303 %q", response.Code, response.Header().Get("Location"), target)
			}
		})
	}
}

func mutationRequest(graphID model.ID, action string) (string, url.Values) {
	switch {
	case strings.HasPrefix(action, "부여/"):
		return "/graphs/" + graphID.String() + "/access", url.Values{"action": {"grant_account"}, "login_id": {"someone"}, "grade": {"owner"}}
	case strings.HasPrefix(action, "회수/"):
		return "/graphs/" + graphID.String() + "/access", url.Values{"action": {"revoke"}, "subject_id": {graphID.String()}, "subject_type": {"account"}}
	case strings.HasPrefix(action, "복구 요청/"):
		return "/graphs", url.Values{"restore_graph_id": {graphID.String()}}
	default:
		return "/operator/restores", url.Values{"graph_id": {graphID.String()}}
	}
}
