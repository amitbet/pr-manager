package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/amitbet/pr-manager/internal/activity"
)

// A job's activity log lives in memory while the server runs. When a
// triage or fix job finishes, the log is also saved under
// results/logs/<key>/<job id>.json for each result it belongs to: the
// result the job made, and for a fix the result it fixed. The chat agent
// reads them from there, so a restart doesn't lose how a result was made.
// git and gh threads are left out: they are commands, not reasoning.

// keptLogs is how many job logs a result keeps, newest first.
const keptLogs = 8

type savedLog struct {
	Job     job               `json:"job"`
	Threads []activity.Thread `json:"threads"`
}

func (t *triager) logDir(key string) string { return filepath.Join(t.results, "logs", key) }

// saveJobLog writes j's log for its result and for the other keys given.
// A job that only reused a cached result has nothing worth keeping.
func (t *triager) saveJobLog(j *job, keys ...string) {
	snap := t.snapshot(j)
	if snap.Cached != nil {
		return
	}
	keys = append([]string{snap.Key}, keys...)
	var threads []activity.Thread
	for _, th := range j.log.Snapshot() {
		if th.Kind != "git" && th.Kind != "gh" {
			threads = append(threads, th)
		}
	}
	b, err := json.Marshal(savedLog{Job: snap, Threads: threads})
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" || seen[k] || strings.ContainsAny(k, `/\`) {
			continue
		}
		seen[k] = true
		dir := t.logDir(k)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("job log %s: %v", j.ID, err)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, snap.ID+".json"), b, 0o644); err != nil {
			log.Printf("job log %s: %v", j.ID, err)
			continue
		}
		pruneLogs(dir)
	}
}

// pruneLogs keeps the newest keptLogs logs in dir.
func pruneLogs(dir string) {
	es, err := os.ReadDir(dir)
	if err != nil || len(es) <= keptLogs {
		return
	}
	type f struct {
		name string
		mod  int64
	}
	var fs []f
	for _, e := range es {
		if info, err := e.Info(); err == nil {
			fs = append(fs, f{e.Name(), info.ModTime().UnixNano()})
		}
	}
	sort.Slice(fs, func(a, b int) bool { return fs[a].mod > fs[b].mod })
	for _, x := range fs[min(keptLogs, len(fs)):] {
		os.Remove(filepath.Join(dir, x.name))
	}
}

// savedLogs are the logs saved for key, newest first.
func (t *triager) savedLogs(key string) []savedLog {
	es, err := os.ReadDir(t.logDir(key))
	if err != nil {
		return nil
	}
	var out []savedLog
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(t.logDir(key), e.Name()))
		if err != nil {
			continue
		}
		var s savedLog
		if json.Unmarshal(b, &s) == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Job.Started.After(out[b].Job.Started) })
	return out
}
