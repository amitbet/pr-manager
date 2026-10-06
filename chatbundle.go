package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/internal/activity"
)

// The chat agent's material in full. The prompt holds an abridged copy:
// the diffs within a budget, and for the job logs an index (when the agent
// can read files) or the end of the most relevant threads (when it can't).
// For codex and claude-code the whole of it is also written to a temporary
// folder the agent may read:
//
//	context.md            the prompt's material with every diff in full
//	logs/INDEX.md         every job and thread, with its file
//	logs/<job>/<n>-*.txt  one thread: its lines and the model output on them

const (
	chatLogTail   = 3000 // chars of a thread's end in the prompt
	chatLogIndex  = 400  // threads listed in the prompt's index
	chatAreasMax  = 80   // code-map records of touched files and dirs
	chatPartners  = 5    // files that usually change with a touched one
	chatDataLimit = 1500 // chars of a line's attached output in the prompt
)

// writeChatBundle writes m to a new temporary folder and returns it.
func writeChatBundle(m *chatMaterial) (string, error) {
	dir, err := os.MkdirTemp("", "pr-manager-chat-material-")
	if err != nil {
		return "", err
	}
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d
	}
	write := func(rel, text string) error {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(text), 0o644)
	}
	full := *m
	full.bundle, full.codeNote = "", ""
	if err := write("context.md", chatContext(&full, bundleLimits)); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	var index strings.Builder
	index.WriteString("# Job logs, newest job first\n\nEach thread is one step of a job: a model call (classify, review, critic, a fix round), a subagent, or the job's own stage lines.\n")
	for i, s := range m.jobs {
		fmt.Fprintf(&index, "\n## %s\n", jobLine(s.Job))
		for k, th := range s.Threads {
			rel := logFile(i, s.Job, k, th)
			fmt.Fprintf(&index, "- %s: [%s] %s (%s, %d lines)\n", rel, th.Kind, th.Name, th.Status, len(th.Lines))
			if err := write(rel, threadText(th, -1)); err != nil {
				os.RemoveAll(dir)
				return "", err
			}
		}
	}
	if err := write("logs/INDEX.md", index.String()); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func slug(s string, n int) string {
	s = strings.Trim(unsafeName.ReplaceAllString(s, "_"), "_")
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// logFile is a thread's file in the bundle, relative to it.
func logFile(i int, j job, k int, th activity.Thread) string {
	return path.Join("logs", fmt.Sprintf("%02d-%s-%s", i+1, j.Kind, slug(j.ID, 16)), fmt.Sprintf("%03d-%s-%s.txt", k+1, slug(th.Kind, 10), slug(th.Name, 80)))
}

func jobLine(j job) string {
	s := fmt.Sprintf("%s job %s on %s, started %s: %s", j.Kind, j.ID, j.URL, j.Started.Format(time.RFC3339), j.Status)
	if j.Key != "" {
		s += ", made result " + j.Key
	}
	if j.Stage != "" {
		s += ", stage " + j.Stage
	}
	if j.Error != "" {
		s += ", error: " + j.Error
	}
	if j.Warning != "" {
		s += ", warning: " + j.Warning
	}
	return s
}

// threadText is a thread as text, each line with its time and any output
// attached to it, cut to dataMax (-1: whole).
func threadText(th activity.Thread, dataMax int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s · %s · started %s", th.Kind, th.Name, th.Status, th.Start.Format("15:04:05"))
	if !th.End.IsZero() {
		fmt.Fprintf(&b, ", took %s", th.End.Sub(th.Start).Round(100*time.Millisecond))
	}
	b.WriteString("\n")
	if th.Dropped > 0 {
		fmt.Fprintf(&b, "(%d earlier lines were dropped while the job ran)\n", th.Dropped)
	}
	for _, l := range th.Lines {
		fmt.Fprintf(&b, "%s %s\n", l.T.Format("15:04:05"), l.Text)
		if len(l.Data) > 0 {
			d := string(l.Data)
			if dataMax >= 0 {
				d = clipText(d, dataMax)
			}
			fmt.Fprintf(&b, "  output: %s\n", d)
		}
	}
	return b.String()
}

// writeLogIndex lists the logs in the bundle, for a prompt that can read
// them.
func writeLogIndex(b *strings.Builder, jobs []savedLog, bundle string) {
	if len(jobs) == 0 {
		b.WriteString("\n# Job logs\n\nNone saved for this result: it was triaged before logs were kept, or by another pr-manager.\n")
		return
	}
	fmt.Fprintf(b, "\n# Job logs, newest job first\n\nEach thread's full log is a file under %s (paths below are relative to it).\n", bundle)
	n := 0
	for i, s := range jobs {
		fmt.Fprintf(b, "\n## %s\n", jobLine(s.Job))
		for k, th := range s.Threads {
			if n++; n > chatLogIndex {
				fmt.Fprintf(b, "[more threads: see logs/INDEX.md]\n")
				return
			}
			fmt.Fprintf(b, "- %s: [%s] %s (%s)\n", logFile(i, s.Job, k, th), th.Kind, th.Name, th.Status)
		}
	}
}

// writeLogs puts the end of the most relevant threads in the prompt, for a
// provider that can't read files: threads about the units on screen first,
// then failed ones, then model calls, newest job first. A thread's end is
// where its answer and the reasoning that led to it are.
func writeLogs(b *strings.Builder, jobs []savedLog, view chatView, budget int) {
	if len(jobs) == 0 {
		return
	}
	b.WriteString("\n# Job logs, newest job first\n")
	for _, s := range jobs {
		fmt.Fprintf(b, "- %s\n", jobLine(s.Job))
	}
	type pick struct {
		job, k, rank int
		th           activity.Thread
	}
	var picks []pick
	for i, s := range jobs {
		for k, th := range s.Threads {
			rank := 3
			switch {
			case mentionsAny(th.Name, view.Units):
				rank = 0
			case th.Status == "error":
				rank = 1
			case th.Kind == "llm" || th.Kind == "agent":
				rank = 2
			}
			picks = append(picks, pick{i, k, rank, th})
		}
	}
	sort.SliceStable(picks, func(a, c int) bool { return picks[a].rank < picks[c].rank })
	left, shown := budget, 0
	for _, p := range picks {
		text := threadText(p.th, chatDataLimit)
		if len(text) > chatLogTail {
			text = "[the thread's start is left out]\n" + text[len(text)-chatLogTail:]
		}
		if left -= len(text); left < 0 {
			break
		}
		fmt.Fprintf(b, "\n### thread %d of job %s\n%s", p.k+1, jobs[p.job].Job.ID, text)
		shown++
	}
	if shown < len(picks) {
		fmt.Fprintf(b, "\n[%d more threads left out for length]\n", len(picks)-shown)
	}
}

func mentionsAny(s string, ids []string) bool {
	for _, id := range ids {
		if id != "" && strings.Contains(s, id) {
			return true
		}
	}
	return false
}

// areaCache keeps the file and directory records of the last repo read,
// so each question doesn't read the map's shard again.
var areaCache struct {
	sync.Mutex
	key  string
	recs map[string]*codemap.Record
}

// chatAreas are the code-map records of the files the result changes and
// of the directories above them: their history, complexity, rank and the
// files that usually change with them.
func chatAreas(m *codemap.Map, r *PRResult) []*codemap.Record {
	if m == nil || r.PR.Repo == "" {
		return nil
	}
	repo := codeMapRepo(m, r.PR.PRRef) // its name in the map, as triage looks it up
	areaCache.Lock()
	defer areaCache.Unlock()
	if key := codeMapVersion(m) + "|" + repo; areaCache.key != key {
		recs, err := codemap.ReadShard(m.Dir, repo)
		if err != nil {
			return nil
		}
		areaCache.key, areaCache.recs = key, map[string]*codemap.Record{}
		for _, rec := range recs {
			if rec.Level == "file" || rec.Level == "dir" {
				areaCache.recs[rec.Level+":"+strings.Trim(rec.Path, "/")] = rec
			}
		}
	}
	seen := map[string]bool{}
	var out []*codemap.Record
	add := func(k string) {
		if rec := areaCache.recs[k]; rec != nil && !seen[k] {
			seen[k] = true
			out = append(out, rec)
		}
	}
	for _, f := range r.Files {
		add("file:" + f.Path)
		for d := path.Dir(f.Path); d != "." && d != "/"; d = path.Dir(d) {
			add("dir:" + d)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	if len(out) > chatAreasMax {
		out = out[:chatAreasMax]
	}
	return out
}

func writeAreas(b *strings.Builder, recs []*codemap.Record) {
	if len(recs) == 0 {
		return
	}
	b.WriteString("\n# Code map of the changed files and their directories\n\nImpact is how much depends on the code (0-100), likelihood how likely a change there goes wrong, from its history and complexity; rank is the CodeRank percentile; rollback is how hard a bad change is to undo (0-100). History is the map's window.\n")
	for _, r := range recs {
		fmt.Fprintf(b, "- %s (%s): impact %d %s, likelihood %d %s, rank %.0f, rollback %d", r.Path, r.Level, r.Impact, r.ImpactLevel, r.Likelihood, r.LikelihoodLevel, r.Rank, r.Rollback)
		if len(r.RollbackTags) > 0 {
			var tags []string
			for _, t := range r.RollbackTags {
				tags = append(tags, t.ID)
			}
			fmt.Fprintf(b, " (%s)", strings.Join(tags, ", "))
		}
		if h := r.Hist; h != nil {
			fmt.Fprintf(b, "; %d commits, %d fixes, %d reverts, %d authors, last changed %d days before indexing", h.Commits, h.Fixes, h.Reverts, h.Authors, h.AgeDays)
		}
		if c := r.Cx; c != nil {
			fmt.Fprintf(b, "; worst function cyclomatic %d, nesting %d", c.Cyclo, c.Nest)
		}
		if r.DepFiles > 0 || len(r.DepRepos) > 0 {
			fmt.Fprintf(b, "; reached from %d files", r.DepFiles)
			if len(r.DepRepos) > 0 {
				fmt.Fprintf(b, " and repos %s", strings.Join(r.DepRepos, ", "))
			}
		}
		if len(r.CoChange) > 0 {
			var ps []string
			for _, p := range r.CoChange[:min(chatPartners, len(r.CoChange))] {
				ps = append(ps, fmt.Sprintf("%s (%d commits)", p.Path, p.N))
			}
			fmt.Fprintf(b, "; usually changes with %s", strings.Join(ps, ", "))
		}
		b.WriteString("\n")
	}
}
