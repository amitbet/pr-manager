// Package ghread runs the gh CLI, logged in as the reader, for commands
// that only read: PRs, issues, checks, CI runs, releases, search, and gh
// api GET requests and graphql queries. The chat agent gets it as a tool:
// through the app's MCP server on codex and claude-code (mcpserver.go in
// package main), in the tool loop on the API providers (llm/webtool.go).
package ghread

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
)

// Max is the most chars of output returned.
const Max = 200 << 10

// Description is the tool's description, the same for every provider.
const Description = "Run the GitHub CLI, logged in as the reader, for commands that only read: pr view/list/diff/checks/status, issue view/list, run view/list (--log-failed for a failed job's log), workflow view/list, repo view, release view/list, label list, search, and gh api GET (or graphql queries, no mutations). Runs in the repository's clone, so `pr view 12` finds the repo; pass -R owner/repo for another. Examples: [\"pr\",\"checks\",\"12\"], [\"pr\",\"view\",\"12\",\"--comments\"], [\"api\",\"repos/o/r/pulls/12/comments\"]."

// Run runs gh with args in dir, if Check allows them.
func Run(ctx context.Context, dir string, args []string) (string, error) {
	if err := Check(args); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "PAGER=cat", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1")
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	text := b.String()
	if len(text) > Max {
		text = text[:Max] + "\n[cut]\n"
	}
	// pr checks exits 8 while some checks are still pending: the list is
	// the answer.
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 8 && len(args) > 1 && args[0] == "pr" && args[1] == "checks" {
		return text + "\n(some checks are still pending)\n", nil
	}
	if err != nil {
		return "", fmt.Errorf("gh %s: %v\n%s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

func commands() []string {
	var out []string
	for k := range ghRead {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ghRead are the gh commands that only read, by subcommand.
var ghRead = map[string][]string{
	"pr":       {"view", "list", "diff", "checks", "status"},
	"issue":    {"view", "list", "status"},
	"run":      {"view", "list"},
	"workflow": {"view", "list"},
	"repo":     {"view"},
	"release":  {"view", "list"},
	"label":    {"list"},
	"search":   {"repos", "issues", "prs", "commits", "code"},
}

// Check reports why args aren't a gh command that only reads, or nil.
func Check(args []string) error {
	if len(args) == 0 {
		return errors.New("no gh command")
	}
	for _, a := range args {
		if a == "-w" || a == "--web" || strings.HasPrefix(a, "--web=") || a == "--watch" {
			return fmt.Errorf("%s is not allowed", a)
		}
	}
	cmd := args[0]
	if cmd == "api" {
		return apiReadOnly(args[1:])
	}
	subs, ok := ghRead[cmd]
	if !ok {
		return fmt.Errorf("gh %s is not one of the read-only commands (%s, api)", cmd, strings.Join(commands(), ", "))
	}
	if len(args) < 2 {
		return fmt.Errorf("gh %s needs a subcommand: %s", cmd, strings.Join(subs, ", "))
	}
	for _, s := range subs {
		if args[1] == s {
			return nil
		}
	}
	return fmt.Errorf("gh %s %s is not read-only (allowed: %s)", cmd, args[1], strings.Join(subs, ", "))
}

var mutationWord = regexp.MustCompile(`(?i)\bmutation\b`)

// apiReadOnly allows gh api GET requests and graphql queries. Fields
// make gh POST unless the method is GET; for graphql that is a query,
// unless it is a mutation.
func apiReadOnly(args []string) error {
	method, endpoint := "", ""
	var fields []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			name, val, hasVal = strings.Cut(a, "=")
		} else if len(a) > 2 && a[0] == '-' && strings.ContainsRune("XfFHqtp", rune(a[1])) {
			name, val, hasVal = a[:2], a[2:], true
		}
		next := func() string {
			if hasVal {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch name {
		case "-X", "--method":
			method = strings.ToUpper(next())
		case "-f", "--raw-field", "-F", "--field":
			v := next()
			if (name == "-F" || name == "--field") && strings.Contains(v, "=@") {
				return errors.New("fields from files are not allowed")
			}
			fields = append(fields, v)
		case "-H", "--header":
			if v := next(); strings.Contains(strings.ToLower(v), "override") {
				return errors.New("method override headers are not allowed")
			}
		case "--input":
			return errors.New("--input is not allowed")
		case "-q", "--jq", "-t", "--template", "--hostname", "-p", "--preview", "--cache":
			next()
		default:
			if !strings.HasPrefix(a, "-") && endpoint == "" {
				endpoint = a
			}
		}
	}
	if endpoint == "" {
		return errors.New("gh api needs an endpoint")
	}
	if method != "" && method != "GET" {
		if !(method == "POST" && endpoint == "graphql") {
			return fmt.Errorf("gh api -X %s is not allowed: only GET", method)
		}
	}
	if len(fields) > 0 && method != "GET" {
		if endpoint != "graphql" {
			return errors.New("gh api with fields POSTs: add -X GET to send them as the query string")
		}
		for _, f := range fields {
			if mutationWord.MatchString(f) {
				return errors.New("graphql mutations are not allowed")
			}
		}
	}
	return nil
}
