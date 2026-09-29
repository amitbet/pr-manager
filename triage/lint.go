package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// LintFinding is one static-analysis result on a line this PR adds.
// Findings elsewhere in the file belong to the repository, not to the
// change, so they are dropped: a reviewer should only answer for what the
// PR is responsible for.
type LintFinding struct {
	Tool     string `json:"tool"`
	Rule     string `json:"rule,omitempty"`
	Severity string `json:"severity"` // error | warning
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Col      int    `json:"col,omitempty"`
	Message  string `json:"message"`
	// Dismissed is set when a person rejected the finding. It stays on
	// the record and stops being shown as open (see ApplyDismissed). The
	// unit's likelihood keeps it: that was measured when the PR was
	// triaged, and the finding was real when it was measured.
	Dismissed    bool   `json:"dismissed,omitempty"`
	DismissedWhy string `json:"dismissed_why,omitempty"`
	// DismissKey is the stored record to delete to restore it; set only
	// while the finding is dismissed.
	DismissKey string `json:"dismiss_key,omitempty"`
}

// Label names the finding's source: "golangci-lint errcheck".
func (f LintFinding) Label() string {
	if f.Rule == "" {
		return f.Tool
	}
	return f.Tool + " " + f.Rule
}

const (
	lintError   = "error"
	lintWarning = "warning"
)

var lintWeight = map[string]int{lintWarning: 1, lintError: 2}

// Linter runs the static analysis a repository is already set up for over
// the lines a PR changes. The result is deterministic, so it is worth more
// than a model's opinion on the same question: findings go into the review
// prompt (so the reviewer stops hunting for what a linter already knows),
// into the unit's likelihood, and onto the Issues tab on their own.
type Linter struct {
	Policy LintPolicy
	// Warn gets the problems that only cost coverage: a tool that is
	// missing, times out, or writes output we cannot parse. A linter
	// never fails a triage.
	Warn func(string)
}

func (l *Linter) on() bool { return l != nil && l.Policy.Enabled }

func (l *Linter) warn(format string, args ...any) {
	if l.Warn != nil {
		l.Warn("lint: " + fmt.Sprintf(format, args...))
	}
}

// Run puts the findings for the lines src adds onto the units that own
// them. dir is the repository at the PR head; "" leaves only the built-in
// scans, which read the diff. It never fails the triage.
func (l *Linter) Run(ctx context.Context, dir string, src *Source, units []*Unit) {
	if !l.on() {
		return
	}
	added := addedLines(src.Files)
	if len(added) == 0 {
		return
	}
	var found []LintFinding
	if l.Policy.Secrets {
		found = append(found, scanSecrets(src.Files)...)
	}
	if dir != "" {
		found = append(found, l.external(ctx, dir, src, added)...)
	}
	assignLint(units, onAddedLines(found, added), l.Policy.MaxPerUnit)
}

// external runs the repository's own linters in dir. Each one gets the
// changed paths it cares about and its own timeout, and they run together
// because they do not touch each other's state.
func (l *Linter) external(ctx context.Context, dir string, src *Source, added map[string]map[int]bool) []LintFinding {
	var paths []string
	for _, f := range src.Files {
		if f.Status != StatusDeleted && len(added[f.Path]) > 0 {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	tools := l.pick(dir, paths)
	if len(tools) == 0 {
		return nil
	}
	out := make([][]LintFinding, len(tools))
	var wg sync.WaitGroup
	for i, t := range tools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = l.run(ctx, dir, t, matching(paths, t.exts))
		}()
	}
	wg.Wait()
	var all []LintFinding
	for _, fs := range out {
		all = append(all, fs...)
	}
	return all
}

// pick chooses the linters to run. A named list is taken as given, minus
// the ones that are not installed. "auto" (the default) runs a tool only
// when the repository is configured for it and its files changed, so the
// findings come with the project's own rules rather than a stranger's
// defaults.
func (l *Linter) pick(dir string, paths []string) []linter {
	want := map[string]bool{}
	for _, n := range l.Policy.Tools {
		want[n] = true
	}
	skip := map[string]bool{}
	for _, n := range l.Policy.Skip {
		skip[n] = true
	}
	var out []linter
	for _, t := range knownLinters {
		switch {
		case skip[t.name]:
			continue
		case len(want) > 0 && !want[t.name]:
			continue
		case len(matching(paths, t.exts)) == 0:
			continue
		case len(want) == 0 && len(t.config) > 0 && !configured(dir, t.config):
			continue
		}
		if _, err := exec.LookPath(t.name); err != nil {
			if len(want) > 0 { // asked for by name: say it is missing
				l.warn("%s is not installed", t.name)
			}
			continue
		}
		out = append(out, t)
	}
	return out
}

// run executes one linter and parses what it wrote. Linters exit non-zero
// when they find something, so the exit code is ignored and only unusable
// output counts as a failure.
func (l *Linter) run(ctx context.Context, dir string, t linter, paths []string) []LintFinding {
	timeout := time.Duration(l.Policy.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(DefaultLintTimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	try := func(args []string) ([]LintFinding, error) {
		cmd := exec.CommandContext(ctx, t.name, args...)
		cmd.Dir = dir
		// A linter that needs the network or a writable home is not worth
		// waiting for; keep the environment as it is otherwise.
		out, err := cmd.Output()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("timed out after %s", timeout)
		}
		fs, perr := t.parse(out, dir)
		if perr != nil {
			if err != nil {
				return nil, fmt.Errorf("%w (%s)", perr, firstLine(err.Error()))
			}
			return nil, perr
		}
		return fs, nil
	}
	fs, err := try(t.args(paths))
	if err != nil && t.alt != nil {
		if alt, aerr := try(t.alt(paths)); aerr == nil {
			fs, err = alt, nil
		}
	}
	if err != nil {
		l.warn("%s: %v", t.name, err)
		return nil
	}
	for i := range fs {
		fs[i].Tool = t.name
	}
	return fs
}

// linter is one external tool.
type linter struct {
	name string
	// exts are the file extensions that make the tool worth running.
	exts []string
	// config are the files that show the repository uses the tool. A tool
	// with none (shellcheck) runs whenever its files changed.
	config []string
	// args builds the command line from the changed paths it matched.
	args func(paths []string) []string
	// alt is a second command line for tools whose output flag changed
	// between major versions, tried when the first writes nothing usable.
	alt   func(paths []string) []string
	parse func(out []byte, dir string) ([]LintFinding, error)
}

var knownLinters = []linter{
	{
		name:   "golangci-lint",
		exts:   []string{".go"},
		config: []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"},
		// v2 writes JSON to stdout with --output.json.path; v1 with
		// --out-format. Packages, not files: the tool type-checks them.
		args: func(p []string) []string {
			return append([]string{"run", "--issues-exit-code=0", "--output.json.path=stdout"}, goPackages(p)...)
		},
		alt: func(p []string) []string {
			return append([]string{"run", "--issues-exit-code=0", "--out-format=json"}, goPackages(p)...)
		},
		parse: parseGolangCI,
	},
	{
		name:   "eslint",
		exts:   []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts"},
		config: []string{"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts", ".eslintrc", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yml", ".eslintrc.yaml"},
		args: func(p []string) []string {
			return append([]string{"--format", "json", "--no-error-on-unmatched-pattern"}, p...)
		},
		parse: parseESLint,
	},
	{
		name:   "ruff",
		exts:   []string{".py", ".pyi"},
		config: []string{"ruff.toml", ".ruff.toml", "pyproject.toml"},
		args: func(p []string) []string {
			return append([]string{"check", "--output-format", "json", "--no-cache", "--force-exclude"}, p...)
		},
		parse: parseRuff,
	},
	{
		// No config to look for: shellcheck is fast, needs no project
		// setup, and its defaults are the ones everyone uses.
		name:  "shellcheck",
		exts:  []string{".sh", ".bash"},
		args:  func(p []string) []string { return append([]string{"--format=json", "--severity=warning"}, p...) },
		parse: parseShellcheck,
	},
}

// goPackages turns changed Go files into the package directories
// golangci-lint wants, each one listed once.
func goPackages(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		d := "./" + path.Dir(p)
		if path.Dir(p) == "." {
			d = "./"
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func parseGolangCI(out []byte, dir string) ([]LintFinding, error) {
	var res struct {
		Issues []struct {
			FromLinter string
			Text       string
			Severity   string
			Pos        struct {
				Filename string
				Line     int
				Column   int
			}
		}
	}
	if err := json.Unmarshal(jsonFrom(out, '{'), &res); err != nil {
		return nil, err
	}
	var fs []LintFinding
	for _, is := range res.Issues {
		fs = append(fs, LintFinding{
			Rule: is.FromLinter, Severity: lintSeverity(is.Severity, lintError),
			Path: relSlash(dir, is.Pos.Filename), Line: is.Pos.Line, Col: is.Pos.Column,
			Message: strings.TrimSpace(is.Text),
		})
	}
	return fs, nil
}

func parseESLint(out []byte, dir string) ([]LintFinding, error) {
	var res []struct {
		FilePath string `json:"filePath"`
		Messages []struct {
			RuleID   string `json:"ruleId"`
			Severity int    `json:"severity"` // 1 warning, 2 error
			Message  string `json:"message"`
			Line     int    `json:"line"`
			Column   int    `json:"column"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(jsonFrom(out, '['), &res); err != nil {
		return nil, err
	}
	var fs []LintFinding
	for _, f := range res {
		for _, m := range f.Messages {
			sev := lintWarning
			if m.Severity >= 2 {
				sev = lintError
			}
			fs = append(fs, LintFinding{
				Rule: m.RuleID, Severity: sev, Path: relSlash(dir, f.FilePath),
				Line: m.Line, Col: m.Column, Message: strings.TrimSpace(m.Message),
			})
		}
	}
	return fs, nil
}

func parseRuff(out []byte, dir string) ([]LintFinding, error) {
	var res []struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		Filename string `json:"filename"`
		Location struct {
			Row    int `json:"row"`
			Column int `json:"column"`
		} `json:"location"`
	}
	if err := json.Unmarshal(jsonFrom(out, '['), &res); err != nil {
		return nil, err
	}
	var fs []LintFinding
	for _, r := range res {
		fs = append(fs, LintFinding{
			Rule: r.Code, Severity: lintWarning, Path: relSlash(dir, r.Filename),
			Line: r.Location.Row, Col: r.Location.Column, Message: strings.TrimSpace(r.Message),
		})
	}
	return fs, nil
}

func parseShellcheck(out []byte, dir string) ([]LintFinding, error) {
	var res []struct {
		File    string `json:"file"`
		Line    int    `json:"line"`
		Column  int    `json:"column"`
		Level   string `json:"level"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(jsonFrom(out, '['), &res); err != nil {
		return nil, err
	}
	var fs []LintFinding
	for _, r := range res {
		fs = append(fs, LintFinding{
			Rule: fmt.Sprintf("SC%d", r.Code), Severity: lintSeverity(r.Level, lintWarning),
			Path: relSlash(dir, r.File), Line: r.Line, Col: r.Column,
			Message: strings.TrimSpace(r.Message),
		})
	}
	return fs, nil
}

// lintSeverity maps a tool's own level onto error/warning.
func lintSeverity(s, def string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error", "fatal", "critical":
		return lintError
	case "warning", "warn", "info", "style", "note", "low":
		return lintWarning
	}
	return def
}

// jsonFrom cuts anything a tool printed before its JSON (a version
// banner, a progress line) so one chatty linter is still readable.
func jsonFrom(b []byte, open byte) []byte {
	if i := indexByte(b, open); i > 0 {
		return b[i:]
	}
	return b
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// relSlash makes a tool's path repo-relative with forward slashes, since
// linters report absolute paths and we match against diff paths.
func relSlash(dir, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if dir != "" && filepath.IsAbs(p) {
		if rel, err := filepath.Rel(dir, p); err == nil {
			p = rel
		}
	}
	return filepath.ToSlash(strings.TrimPrefix(p, "./"))
}

func matching(paths, exts []string) []string {
	var out []string
	for _, p := range paths {
		for _, e := range exts {
			if strings.HasSuffix(p, e) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// configured reports whether dir holds one of the tool's config files. A
// pyproject.toml only counts with a [tool.ruff] section in it, since every
// Python project has one.
func configured(dir string, names []string) bool {
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if n == "pyproject.toml" && !strings.Contains(string(b), "[tool.ruff") {
			continue
		}
		return true
	}
	return false
}

// addedLines maps each changed file to the new-file line numbers the diff
// adds. Everything downstream is anchored to these.
func addedLines(files []FileDiff) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, f := range files {
		lines := map[int]bool{}
		for _, h := range f.Hunks {
			n := h.NewStart
			for _, l := range h.Lines {
				switch {
				case strings.HasPrefix(l, "+"):
					lines[n] = true
					n++
				case strings.HasPrefix(l, "-"), strings.HasPrefix(l, "\\"):
				default:
					n++
				}
			}
		}
		if len(lines) > 0 {
			out[f.Path] = lines
		}
	}
	return out
}

// onAddedLines keeps the findings that sit on a line the PR adds, and
// drops the duplicates two tools can report on the same line.
func onAddedLines(fs []LintFinding, added map[string]map[int]bool) []LintFinding {
	seen := map[string]bool{}
	var out []LintFinding
	for _, f := range fs {
		if !added[f.Path][f.Line] {
			continue
		}
		k := fmt.Sprintf("%s|%d|%s|%s", f.Path, f.Line, f.Label(), f.Message)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}

// assignLint hangs each finding on the unit whose hunks hold its line, so
// it travels with the code it is about: into that unit's review prompt,
// its likelihood and its row on the Issues tab. A finding on a line no
// unit claims goes to the file's first unit rather than being lost.
func assignLint(units []*Unit, fs []LintFinding, maxPerUnit int) {
	byFile := map[string][]*Unit{}
	for _, u := range units {
		u.Lint = nil
		byFile[u.File] = append(byFile[u.File], u)
	}
	for _, f := range fs {
		us := byFile[f.Path]
		if len(us) == 0 {
			continue
		}
		target := us[0]
		for _, u := range us {
			if unitHasLine(u, f.Line) {
				target = u
				break
			}
		}
		target.Lint = append(target.Lint, f)
	}
	for _, u := range units {
		sort.SliceStable(u.Lint, func(i, j int) bool {
			if a, b := lintWeight[u.Lint[i].Severity], lintWeight[u.Lint[j].Severity]; a != b {
				return a > b
			}
			return u.Lint[i].Line < u.Lint[j].Line
		})
		if maxPerUnit > 0 && len(u.Lint) > maxPerUnit {
			u.Lint = u.Lint[:maxPerUnit]
		}
	}
}

// unitHasLine reports whether the unit adds the given new-file line.
func unitHasLine(u *Unit, line int) bool {
	for _, h := range u.Hunks {
		n := h.NewStart
		for _, l := range h.Lines {
			switch {
			case strings.HasPrefix(l, "+"):
				if n == line {
					return true
				}
				n++
			case strings.HasPrefix(l, "-"), strings.HasPrefix(l, "\\"):
			default:
				n++
			}
		}
	}
	return false
}

// lintContext is what the review prompt says about a unit's findings.
// They are shown to the reviewer on their own, so the prompt's job is to
// stop the model repeating them and let it use one as evidence.
func lintContext(u *Unit) string {
	if len(u.Lint) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Static analysis already reported these on the lines this change adds:\n")
	n := 0
	for _, f := range u.Lint {
		if f.Dismissed {
			continue
		}
		fmt.Fprintf(&sb, "- [%s] line %d: %s\n", f.Label(), f.Line, f.Message)
		n++
	}
	if n == 0 {
		return ""
	}
	sb.WriteString("The reviewer sees them on their own, so do not repeat one as an issue. Use one as evidence when it causes a defect you can show going wrong.\n")
	return sb.String()
}

// secretRule is one credential pattern. Strong rules match a token format
// only a real credential has; the generic rule matches an assignment and
// is a warning, because test data looks the same.
type secretRule struct {
	id      string
	re      *regexp.Regexp
	what    string
	strong  bool
	capture int // submatch holding the value to check for a placeholder
}

var secretRules = []secretRule{
	{id: "private-key", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`), what: "a private key block", strong: true},
	{id: "aws-access-key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), what: "an AWS access key id", strong: true},
	{id: "github-token", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`), what: "a GitHub token", strong: true},
	{id: "slack-token", re: regexp.MustCompile(`\bxox[abprs]-[0-9A-Za-z-]{10,}\b`), what: "a Slack token", strong: true},
	{id: "slack-webhook", re: regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/+_-]{20,}`), what: "a Slack webhook URL", strong: true},
	{id: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), what: "a Google API key", strong: true},
	{id: "stripe-key", re: regexp.MustCompile(`\b[sr]k_(?:live|test)_[0-9A-Za-z]{16,}\b`), what: "a Stripe key", strong: true},
	{id: "npm-token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), what: "an npm token", strong: true},
	{id: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), what: "a signed JWT", strong: true},
	{id: "aws-secret-key", re: regexp.MustCompile(`(?i)aws_?secret_?access_?key\s*[:=]\s*["']?([A-Za-z0-9/+=]{40})\b`), what: "an AWS secret access key", strong: true, capture: 1},
	{id: "hardcoded-credential", re: regexp.MustCompile(`(?i)\b(?:password|passwd|secret|api_?key|access_?token|auth_?token|client_?secret)\b\s*[:=]\s*["']([^"'\s]{12,})["']`), what: "a credential written into the source", capture: 1},
}

// placeholder matches the values people write when they mean "fill this
// in", which is most of what the generic rule would otherwise report.
var placeholder = regexp.MustCompile(`(?i)^(?:x{3,}|\*{3,}|\.{3,}|\$|<|\{|%|changeme|change_me|your|my|some|example|placeholder|redacted|dummy|fake|sample|test|todo|none|null|nil|empty|password|secret|hunter2|s3cret|topsecret|abc|foo|bar)|(?:\$\{|\{\{|<%|%\()|(?i)(?:example|placeholder|redacted|changeme|xxxx|notreal|dummy)`)

// scanSecrets looks for credentials in the lines the PR adds. It needs no
// tool installed, which is why it is on by default: a leaked key is the
// one review finding that cannot wait for a person to notice it.
func scanSecrets(files []FileDiff) []LintFinding {
	var out []LintFinding
	for _, f := range files {
		if f.Binary || isLockPath(f.Path) {
			continue
		}
		for _, h := range f.Hunks {
			n := h.NewStart
			for _, l := range h.Lines {
				if !strings.HasPrefix(l, "+") {
					if !strings.HasPrefix(l, "-") && !strings.HasPrefix(l, `\`) {
						n++
					}
					continue
				}
				out = append(out, scanLine(f.Path, n, l[1:])...)
				n++
			}
		}
	}
	return out
}

func scanLine(path string, line int, text string) []LintFinding {
	if len(text) > 4000 {
		text = text[:4000]
	}
	var out []LintFinding
	for _, r := range secretRules {
		m := r.re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		if r.capture > 0 && r.capture < len(m) && placeholder.MatchString(m[r.capture]) {
			continue
		}
		sev := lintWarning
		if r.strong {
			sev = lintError
		}
		out = append(out, LintFinding{
			Tool: "secrets", Rule: r.id, Severity: sev, Path: path, Line: line,
			Message: "this line adds what looks like " + r.what + "; if it is real, rotate it and take it out of the history",
		})
	}
	return out
}

// isLockPath skips the generated files whose long random-looking strings
// (integrity hashes) are not credentials.
func isLockPath(p string) bool {
	base := path.Base(p)
	for _, n := range []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum", "Cargo.lock", "poetry.lock", "uv.lock", "composer.lock"} {
		if base == n {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.TrimSpace(s)
}
