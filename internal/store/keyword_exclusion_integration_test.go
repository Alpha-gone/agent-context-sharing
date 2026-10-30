package store

import (
	"slices"
	"testing"
	"time"

	"agent_context_sharing/internal/model"
)

func TestKeywordExclusionIntegration(t *testing.T) {
	database := newEmbeddingContractStore(t, "vector", 8)
	actor := newTestID(t)
	createTestAccount(t, database, actor)
	graph := createTestGraph(t, database, actor)
	nodes := map[string]model.ID{}
	for name, body := range map[string]string{
		"foo": "foo", "foo_bar": "foo bar", "baz": "baz", "baz_bar": "baz bar",
		"unrelated": "unrelated content", "phrase": "alpha beta", "gapped": "alpha gap beta",
		"cli": "git push", "cli_flag": "git push f", "other_cli": "git status",
	} {
		node := testSourceContext(t, graph, actor, "api://keyword-exclusion/"+name)
		node.Body = body
		stored, err := database.CreateContext(t.Context(), graph, node, nil)
		if err != nil {
			t.Fatal(err)
		}
		nodes[name] = stored.ID
	}
	for _, sample := range []struct {
		name, query string
		want        []string
	}{
		{"single_exclusion", "foo -bar", []string{"foo"}},
		{"exclusion_first", "-bar foo", []string{"foo"}},
		{"all_alternatives", "foo baz -bar", []string{"foo", "baz"}},
		{"multiple_exclusions", "foo baz -bar -baz", []string{"foo"}},
		{"cli_flag", "git push -f", []string{"cli", "other_cli"}},
		{"cli_no_overlap", "gitx pushx -f", nil},
		{"ordinary_or", "foo baz", []string{"foo", "foo_bar", "baz", "baz_bar"}},
		{"explicit_or", "foo OR baz -bar", []string{"foo", "baz"}},
		{"phrase_or", `"alpha beta" foo`, []string{"phrase", "foo", "foo_bar"}},
		{"phrase_or_exclusion", `"alpha beta" foo -bar`, []string{"phrase", "foo"}},
		{"excluded_phrase", `alpha -"alpha beta"`, []string{"gapped"}},
		{"negative_only", `-"alpha beta" -foo -baz -git -unrelated`, []string{"gapped"}},
		{"unicode_space", "foo\u3000baz\t-bar", []string{"foo", "baz"}},
		{"empty", " ", nil},
	} {
		t.Run(sample.name, func(t *testing.T) {
			values, err := database.KeywordCandidates(t.Context(), graph, sample.query, time.Now().UTC(), 20)
			if err != nil {
				t.Fatal(err)
			}
			var got, want []string
			for _, value := range values {
				if value.Context.GraphID != graph {
					t.Fatal("다른 그래프의 후보가 반환됐다")
				}
				got = append(got, value.Context.ID.String())
			}
			for _, name := range sample.want {
				want = append(want, nodes[name].String())
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("%q 후보: got=%v want=%v", sample.query, got, want)
			}
		})
	}
}
