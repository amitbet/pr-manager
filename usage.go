package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// Every model call's tokens are kept under <cache>/usage, one JSON line
// per call (llm.UsageRecord), one file per month. Settings → Statistics
// shows them (GET /api/usage): totals, by day, by model, and by repository
// and change (a PR, or a local checkout's branch), each split by activity:
// analysis (triage, overview, sequence), chat, fixing, review comments,
// translation.

var usageCategories = []struct{ id, label string }{
	{"analysis", "Analysis"}, {"chat", "Chat"}, {"fixing", "Fixing"}, {"comments", "Review comments"}, {"translation", "Translation"}, {"other", "Other"},
}

// usageCategory is what a call was for.
func usageCategory(r llm.UsageRecord) string {
	switch {
	case strings.HasPrefix(r.Tool, "translate"):
		return "translation"
	case r.Activity == "chat":
		return "chat"
	case r.Activity == "fix":
		return "fixing"
	case r.Activity == "comments":
		return "comments"
	case r.Activity == "triage" || r.Activity == "overview" || r.Activity == "sequence":
		return "analysis"
	}
	return "other"
}

type usageStore struct {
	dir string
	mu  sync.Mutex
}

func (u *usageStore) add(r llm.UsageRecord) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if os.MkdirAll(u.dir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(u.dir, r.T.Format("2006-01")+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// read returns the records since since (zero: all), oldest first.
func (u *usageStore) read(since time.Time) []llm.UsageRecord {
	u.mu.Lock()
	defer u.mu.Unlock()
	paths, _ := filepath.Glob(filepath.Join(u.dir, "*.jsonl"))
	sort.Strings(paths)
	var out []llm.UsageRecord
	for _, p := range paths {
		month, err := time.ParseInLocation("2006-01", strings.TrimSuffix(filepath.Base(p), ".jsonl"), time.Local)
		if err == nil && !since.IsZero() && month.AddDate(0, 1, 0).Before(since) {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var r llm.UsageRecord
			if json.Unmarshal(sc.Bytes(), &r) == nil && !r.T.Before(since) {
				out = append(out, r)
			}
		}
		f.Close()
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].T.Before(out[b].T) })
	return out
}

// usageCtx tags the calls made in ctx for result r: with activity unless
// a job tagged them already, and with r's change and key.
func usageCtx(ctx context.Context, activity string, r *PRResult) context.Context {
	ctx = llm.WithUsageDefault(ctx, llm.UsageTag{Activity: activity})
	if r == nil || r.PR == nil {
		return ctx
	}
	return llm.WithUsage(ctx, llm.UsageTag{Source: orDefault(r.PR.URL, localSrcOf(r.PR)), Key: r.Key})
}

type usageSum struct {
	In    int            `json:"in"`
	Out   int            `json:"out"`
	Calls int            `json:"calls"`
	By    map[string]int `json:"by"` // tokens by category
}

func (s *usageSum) add(r llm.UsageRecord) {
	if s.By == nil {
		s.By = map[string]int{}
	}
	s.In += r.In
	s.Out += r.Out
	s.Calls++
	s.By[usageCategory(r)] += r.In + r.Out
}

type usageItem struct {
	Source string    `json:"source"`
	Label  string    `json:"label"`
	Sub    string    `json:"sub,omitempty"` // the branch
	Last   time.Time `json:"last"`
	usageSum
}

type usageRepo struct {
	Repo  string       `json:"repo"`
	Items []*usageItem `json:"items"`
	usageSum
}

type usageDay struct {
	Day string `json:"day"`
	usageSum
}

type usageModel struct {
	Model string `json:"model"`
	usageSum
}

type usageReport struct {
	From       time.Time           `json:"from"`
	To         time.Time           `json:"to"`
	Categories []map[string]string `json:"categories"`
	Totals     usageSum            `json:"totals"`
	Days       []*usageDay         `json:"days"`
	Models     []*usageModel       `json:"models"`
	Repos      []*usageRepo        `json:"repos"`
	Tools      []*usageModel       `json:"tools"` // by tool: classify, review, reply...
}

// usageReport sums the records since from (zero: all) up to now.
func (t *triager) usageReport(from, now time.Time) *usageReport {
	recs := t.usage.read(from)
	rep := &usageReport{From: from, To: now}
	for _, c := range usageCategories {
		rep.Categories = append(rep.Categories, map[string]string{"id": c.id, "label": c.label})
	}
	if from.IsZero() && len(recs) > 0 {
		rep.From = recs[0].T
	}
	// The changes, as the saved results name them.
	type known struct{ title, branch string }
	names := map[string]known{}
	if list, err := t.List(); err == nil {
		for _, s := range list {
			src := s.PR.URL()
			if s.LocalPath != "" {
				src = localSrcOf(&triage.PRInfo{LocalPath: s.LocalPath, Rev: s.Rev})
			}
			if _, ok := names[src]; !ok {
				names[src] = known{s.Title, s.HeadRef}
			}
		}
	}
	days := map[string]*usageDay{}
	models := map[string]*usageModel{}
	tools := map[string]*usageModel{}
	repos := map[string]*usageRepo{}
	items := map[string]*usageItem{}
	for _, r := range recs {
		rep.Totals.add(r)
		day := r.T.Local().Format("2006-01-02")
		if days[day] == nil {
			days[day] = &usageDay{Day: day}
		}
		days[day].add(r)
		m := r.Provider + "/" + r.Model
		if models[m] == nil {
			models[m] = &usageModel{Model: m}
		}
		models[m].add(r)
		if tools[r.Tool] == nil {
			tools[r.Tool] = &usageModel{Model: r.Tool}
		}
		tools[r.Tool].add(r)
		repo, label, sub := usageNames(r.Source)
		if repos[repo] == nil {
			repos[repo] = &usageRepo{Repo: repo}
		}
		repos[repo].add(r)
		it := items[r.Source]
		if it == nil {
			n := names[r.Source]
			if n.title != "" {
				label = strings.TrimSpace(label + " " + n.title)
			}
			it = &usageItem{Source: r.Source, Label: label, Sub: orDefault(n.branch, sub)}
			items[r.Source] = it
			repos[repo].Items = append(repos[repo].Items, it)
		}
		it.add(r)
		it.Last = r.T
	}
	// Every day of the range, the empty ones too, up to 400 of them.
	start := rep.From.Local()
	if start.IsZero() {
		start = now
	}
	for d := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local); !d.After(now) && len(rep.Days) < 400; d = d.AddDate(0, 0, 1) {
		k := d.Format("2006-01-02")
		if days[k] == nil {
			days[k] = &usageDay{Day: k, usageSum: usageSum{By: map[string]int{}}}
		}
		rep.Days = append(rep.Days, days[k])
	}
	tokens := func(s usageSum) int { return s.In + s.Out }
	for _, m := range models {
		rep.Models = append(rep.Models, m)
	}
	sort.Slice(rep.Models, func(a, b int) bool { return tokens(rep.Models[a].usageSum) > tokens(rep.Models[b].usageSum) })
	for _, m := range tools {
		rep.Tools = append(rep.Tools, m)
	}
	sort.Slice(rep.Tools, func(a, b int) bool { return tokens(rep.Tools[a].usageSum) > tokens(rep.Tools[b].usageSum) })
	for _, r := range repos {
		sort.Slice(r.Items, func(a, b int) bool { return tokens(r.Items[a].usageSum) > tokens(r.Items[b].usageSum) })
		rep.Repos = append(rep.Repos, r)
	}
	sort.Slice(rep.Repos, func(a, b int) bool { return tokens(rep.Repos[a].usageSum) > tokens(rep.Repos[b].usageSum) })
	if rep.Totals.By == nil {
		rep.Totals.By = map[string]int{}
	}
	return rep
}

// usageNames names a call's change: its repository, the change (PR #12,
// or the checkout and its revision) and its branch, if the source says.
func usageNames(src string) (repo, label, sub string) {
	if src == "" {
		return "(none)", "not about a change", ""
	}
	if ref, err := triage.ParsePRRef(src); err == nil {
		repo = ref.Slug()
		if h := ref.HostName(); h != "github.com" {
			repo = h + "/" + repo
		}
		return repo, "#" + strconv.Itoa(ref.Number), ""
	}
	path, rev, _ := strings.Cut(src, "#")
	return filepath.Base(path) + " (local)", orDefault(rev, "working tree"), rev
}

func (t *triager) usageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		var from time.Time
		if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 {
			y, m, day := now.AddDate(0, 0, -(d - 1)).Date()
			from = time.Date(y, m, day, 0, 0, 0, 0, time.Local)
		}
		writeJSON(w, 200, t.usageReport(from, now))
	})
}
