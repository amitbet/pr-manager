package ghread

import "testing"

// The agent's gh only reads: the read subcommands, and gh api GETs and
// graphql queries.
func TestGHReadOnly(t *testing.T) {
	ok := [][]string{
		{"pr", "view", "12", "--comments"},
		{"pr", "checks", "12"},
		{"run", "view", "99", "--log-failed"},
		{"search", "prs", "--repo", "o/r", "flaky"},
		{"api", "repos/o/r/pulls/12/comments", "--paginate", "--jq", ".[].body"},
		{"api", "-X", "GET", "search/issues", "-f", "q=repo:o/r is:open"},
		{"api", "graphql", "-f", "query={ viewer { login } }"},
	}
	for _, a := range ok {
		if err := Check(a); err != nil {
			t.Errorf("%v: %v", a, err)
		}
	}
	bad := [][]string{
		{},
		{"pr", "merge", "12"},
		{"pr", "comment", "12", "-b", "hi"},
		{"pr", "view", "12", "--web"},
		{"pr", "checks", "12", "--watch"},
		{"auth", "token"},
		{"co", "12"}, // an alias
		{"repo", "delete", "o/r"},
		{"api", "-X", "DELETE", "repos/o/r"},
		{"api", "--method=PATCH", "repos/o/r"},
		{"api", "-XPOST", "repos/o/r/issues"},
		{"api", "repos/o/r/issues", "-f", "title=x"}, // fields POST
		{"api", "repos/o/r/issues", "--input", "body.json"},
		{"api", "graphql", "-f", "query=mutation { addStar(input: {}) { clientMutationId } }"},
		{"api", "graphql", "-F", "query=@q.graphql"},
		{"api", "repos/o/r", "-H", "X-HTTP-Method-Override: DELETE"},
	}
	for _, a := range bad {
		if err := Check(a); err == nil {
			t.Errorf("%v was allowed", a)
		}
	}
}
