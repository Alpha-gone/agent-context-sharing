package plan

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestReadRequestLimits(t *testing.T) {
	limits := Default()
	for _, sample := range []struct {
		name        string
		hops, nodes int
		wantError   bool
	}{
		{"zero_hop", 0, 200, false}, {"boundary", 4, 200, false},
		{"above", 5, 0, true}, {"negative", -1, 0, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			nodes, err := limits.CheckHop(sample.hops)
			if nodes != sample.nodes || (err != nil) != sample.wantError {
				t.Fatalf("홉 한도=%d, %v", nodes, err)
			}
			if sample.hops == 5 {
				value, ok := errors.AsType[LimitError](err)
				if !ok || value.Name != "max_hops" || value.Current != 5 || value.Allowed != 4 {
					t.Fatalf("홉 오류=%v", err)
				}
			}
		})
	}
	if nodes, err := (Limits{}).CheckHop(math.MaxInt); err != nil || nodes != 0 {
		t.Fatalf("무제한 홉=%d, %v", nodes, err)
	}
	if _, err := (Limits{MaxHopNodes: -1}).CheckHop(0); err == nil {
		t.Fatal("음수 노드 상한 허용")
	}
	for _, pageName := range []string{"graph_page", "relation_page"} {
		for _, value := range []*int{nil, new(1), new(200), new(201), new(0), new(-1)} {
			t.Run(pageName+"/"+requestName(value), func(t *testing.T) {
				page, err := limits.GraphPage.Resolve(pageName, value)
				if value == nil {
					if err != nil || page != 50 {
						t.Fatalf("기본 페이지=%d, %v", page, err)
					}
				} else if *value > 200 {
					limit, ok := errors.AsType[LimitError](err)
					if !ok || limit.Name != pageName || limit.Current != 201 || limit.Allowed != 200 {
						t.Fatalf("페이지 초과=%v", err)
					}
				} else if *value <= 0 {
					if _, ok := errors.AsType[model.FieldError](err); !ok {
						t.Fatalf("잘못된 페이지=%v", err)
					}
				} else if err != nil || page != *value {
					t.Fatalf("요청 페이지=%d, %v", page, err)
				}
			})
		}
	}
	if value, err := (PageSize{Default: 50}).Resolve("graph_page", new(math.MaxInt)); err != nil || value != math.MaxInt {
		t.Fatalf("페이지 무제한=%d, %v", value, err)
	}
	for _, page := range []PageSize{{}, {Default: 51, Maximum: 50}, {Default: 1, Maximum: -1}} {
		if _, err := page.Resolve("graph_page", nil); err == nil {
			t.Fatal("잘못된 페이지 구성 허용")
		}
	}
	for _, request := range []*int{nil, new(1), new(20000), new(20001), new(0), new(-1)} {
		t.Run("budget/"+requestName(request), func(t *testing.T) {
			value, err := limits.ResolveBudget(request)
			switch {
			case request == nil:
				if err != nil || value != 20000 {
					t.Fatalf("기본 예산=%d, %v", value, err)
				}
			case *request > 20000:
				limit, ok := errors.AsType[LimitError](err)
				if !ok || limit.Name != "context_budget" || limit.Current != 20001 || limit.Allowed != 20000 {
					t.Fatalf("예산 초과=%v", err)
				}
			case *request <= 0:
				if _, ok := errors.AsType[model.FieldError](err); !ok {
					t.Fatalf("잘못된 예산=%v", err)
				}
			default:
				if err != nil || value != *request {
					t.Fatalf("요청 예산=%d, %v", value, err)
				}
			}
		})
	}
	if value, err := (Limits{}).ResolveBudget(nil); err != nil || value != 0 {
		t.Fatalf("무제한 기본 예산=%d, %v", value, err)
	}
	if _, err := (Limits{}).ResolveBudget(new(math.MaxInt)); err != nil {
		t.Fatal(err)
	}
	if _, err := (Limits{ContextBudget: -1}).ResolveBudget(nil); err == nil {
		t.Fatal("음수 예산 구성 허용")
	}
}

func requestName(value *int) string {
	if value == nil {
		return "omitted"
	}
	return fmt.Sprint(*value)
}
