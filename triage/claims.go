package triage

import (
	"path/filepath"
	"regexp"
	"strings"
)

// maxClaims caps the absolute claims listed for one unit, and
// claimChars the length of each.
const (
	maxClaims  = 6
	claimChars = 200
)

// absoluteWords are the universal quantifiers and closed guarantees a
// claim is made with. Reading such a claim confirms nothing; only a
// failed attempt to break it does, so the reviewer is asked to try.
var absoluteWords = regexp.MustCompile(`(?i)\b(never|always|every|everything|cannot|can't|can never|no (one|caller|input|path|way)|nothing (can|will)|impossible|guarantee[sd]?|in all cases|at most once|exactly once)\b`)

// proseExts are files whose every added line is prose.
var proseExts = map[string]bool{".md": true, ".markdown": true, ".rst": true, ".adoc": true, ".txt": true}

// lineCommentOpeners start a comment line; a line beginning with one of
// them after its indentation is prose, not code.
var lineCommentOpeners = []string{"//", "/*", "*", "#", "--", "<!--", `"""`, "'''"}

// absoluteClaims returns the added comment and doc lines in u that make
// an absolute claim ("never", "always", "every", "cannot"): what the
// review prompt asks the reviewer to try to break.
func absoluteClaims(u *Unit) []string {
	prose := proseExts[strings.ToLower(filepath.Ext(u.File))]
	seen := map[string]bool{}
	var out []string
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			if len(l) == 0 || l[0] != '+' {
				continue
			}
			text := commentText(strings.TrimSpace(l[1:]), prose)
			if text == "" || !absoluteWords.MatchString(text) || seen[text] {
				continue
			}
			seen[text] = true
			out = append(out, clipRunes(text, claimChars))
			if len(out) == maxClaims {
				return out
			}
		}
	}
	return out
}

// quoteClaims joins claims as inline code for a note.
func quoteClaims(claims []string) string {
	q := make([]string, len(claims))
	for i, c := range claims {
		q[i] = "`" + strings.ReplaceAll(c, "`", "'") + "`"
	}
	return strings.Join(q, "; ")
}

// commentText is the prose in one trimmed line: all of it in a doc file,
// the line itself when it opens with a comment marker, and the trailing
// "// ..." comment after code. Empty when the line carries no prose.
func commentText(t string, prose bool) string {
	if prose {
		return t
	}
	for _, o := range lineCommentOpeners {
		if strings.HasPrefix(t, o) {
			return t
		}
	}
	// A space before it keeps "http://" out.
	if i := strings.Index(t, " // "); i >= 0 {
		return strings.TrimSpace(t[i+1:])
	}
	return ""
}
