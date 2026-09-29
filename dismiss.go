package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// Dismissal is a review issue, static-analysis finding or the like that a
// person rejected. They are kept per repository rather than per PR, so a
// claim rejected on one PR is already rejected when the next push brings
// it back, and so the file is a record of what this reviewer gets wrong
// about this codebase.
type Dismissal struct {
	Key  string `json:"key"`
	Kind string `json:"kind"` // issue | lint
	// Pattern is the shape of the claim, for matching issues that are not
	// literally the same one. Nothing matches on it yet; it is written now
	// so the record is already there when something does.
	Pattern  string    `json:"pattern,omitempty"`
	Unit     string    `json:"unit"`
	File     string    `json:"file,omitempty"`
	Line     int       `json:"line,omitempty"`
	Title    string    `json:"title"`
	Severity string    `json:"severity,omitempty"`
	Evidence string    `json:"evidence,omitempty"`
	Reason   string    `json:"reason,omitempty"` // why the person rejected it
	PR       int       `json:"pr,omitempty"`
	PRURL    string    `json:"pr_url,omitempty"`
	Created  time.Time `json:"created"`
}

// dismissals stores one file per repository under the app cache.
type dismissals struct {
	dir string
	mu  sync.Mutex
}

func newDismissals(o options) (*dismissals, error) {
	d := &dismissals{dir: filepath.Join(o.cache, "dismissed")}
	return d, os.MkdirAll(d.dir, 0o755)
}

// repoKey names the file a repository's dismissals live in. Local
// checkouts land under owner "local", so they get a file of their own
// without a special case.
func repoKey(ref triage.PRRef) string {
	parts := []string{}
	for _, p := range []string{ref.Host, ref.Owner, ref.Repo} {
		if p = strings.TrimSpace(p); p != "" && !strings.ContainsAny(p, `/\`) && !strings.Contains(p, "..") {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "__")
}

func (d *dismissals) file(ref triage.PRRef) (string, error) {
	k := repoKey(ref)
	if k == "" {
		return "", errors.New("no repository for these dismissals")
	}
	return filepath.Join(d.dir, k+".json"), nil
}

func (d *dismissals) load(ref triage.PRRef) ([]Dismissal, error) {
	f, err := d.file(ref)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(f)
	if os.IsNotExist(err) {
		return []Dismissal{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ds []Dismissal
	if err := json.Unmarshal(b, &ds); err != nil {
		return nil, err
	}
	return ds, nil
}

func (d *dismissals) save(ref triage.PRRef, ds []Dismissal) error {
	f, err := d.file(ref)
	if err != nil {
		return err
	}
	if len(ds) == 0 {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Created.After(ds[j].Created) })
	b, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f, b, 0o644)
}

// lookup is the set of dismissed keys for a repository, in the form
// triage.ApplyDismissed wants. A missing or broken file dismisses
// nothing, so a bad record can never hide an issue.
func (d *dismissals) lookup(ref triage.PRRef) triage.Dismissed {
	d.mu.Lock()
	defer d.mu.Unlock()
	ds, err := d.load(ref)
	if err != nil {
		return func(string) (string, bool) { return "", false }
	}
	by := make(map[string]string, len(ds))
	for _, x := range ds {
		by[x.Key] = x.Reason
	}
	return func(k string) (string, bool) {
		why, ok := by[k]
		return why, ok
	}
}

// apply marks a result's dismissed issues and re-places its units. It runs
// every time a result is loaded, so dismissing something takes effect on
// the cached triage without re-running the PR.
func (d *dismissals) apply(r *PRResult) {
	if d == nil || r.PR == nil {
		return
	}
	units := resultUnits(r)
	if triage.ApplyDismissed(units, r.tierPolicy(), d.lookup(r.PR.PRRef)) == 0 {
		return
	}
	rep := &triage.Report{Units: units}
	r.Counts = rep.Counts()
	r.Impact, r.Likelihood, r.Attention = rep.Scores()
}

// add records one dismissal, replacing an earlier one with the same key so
// a changed reason wins.
func (d *dismissals) add(ref triage.PRRef, x Dismissal) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ds, err := d.load(ref)
	if err != nil {
		return err
	}
	out := ds[:0]
	for _, old := range ds {
		if old.Key != x.Key {
			out = append(out, old)
		}
	}
	x.Created = time.Now()
	return d.save(ref, append(out, x))
}

// remove restores a dismissed item.
func (d *dismissals) remove(ref triage.PRRef, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ds, err := d.load(ref)
	if err != nil {
		return err
	}
	out := ds[:0]
	for _, old := range ds {
		if old.Key != key {
			out = append(out, old)
		}
	}
	return d.save(ref, out)
}

// dismissRequest picks the issue or lint finding to dismiss out of a unit.
type dismissRequest struct {
	Unit   string `json:"unit"`
	Kind   string `json:"kind"` // issue (default) | lint
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// record builds the stored record from the live issue, so the wording, the
// evidence and the severity are kept exactly as the reviewer claimed them.
func (req dismissRequest) record(r *PRResult) (Dismissal, error) {
	var unit *triage.Unit
	for _, u := range resultUnits(r) {
		if u.ID == req.Unit {
			unit = u
			break
		}
	}
	if unit == nil {
		return Dismissal{}, fmt.Errorf("no unit %q in this result", req.Unit)
	}
	x := Dismissal{Kind: req.Kind, Unit: unit.ID, File: unit.File, Reason: strings.TrimSpace(req.Reason), PR: r.PR.Number, PRURL: r.PR.URL}
	if x.Kind == "" {
		x.Kind = "issue"
	}
	switch x.Kind {
	case "issue":
		if req.Index < 0 || req.Index >= len(unit.Issues) {
			return Dismissal{}, fmt.Errorf("no issue %d on %s", req.Index, unit.ID)
		}
		is := unit.Issues[req.Index]
		x.Key, x.Pattern = triage.IssueKey(unit.ID, is), triage.IssuePattern(is)
		x.Title, x.Severity, x.Evidence, x.Line = is.Title, is.Severity, is.Evidence, is.Line
	case "lint":
		if req.Index < 0 || req.Index >= len(unit.Lint) {
			return Dismissal{}, fmt.Errorf("no lint finding %d on %s", req.Index, unit.ID)
		}
		f := unit.Lint[req.Index]
		x.Key = triage.LintKey(unit.ID, f)
		x.Title, x.Severity, x.Line = f.Label()+": "+f.Message, f.Severity, f.Line
	default:
		return Dismissal{}, fmt.Errorf("unknown kind %q (want issue or lint)", x.Kind)
	}
	return x, nil
}

// routes serve the Issues tab. They work off the result key, which the UI
// always has, and answer with the whole re-placed result: dismissing an
// issue can move its unit out of human review, and the page has to show
// that straight away.
func (d *dismissals) routes(mux *http.ServeMux, t *triager) {
	const base = "/api/results/{key}/dismissals"
	reply := func(w http.ResponseWriter, key string) {
		res, err := t.Load(key)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, res)
	}
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		ds, err := d.load(res.PR.PRRef)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, ds)
	})
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		var req dismissRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		x, err := req.record(res)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := d.add(res.PR.PRRef, x); err != nil {
			writeErr(w, 500, err)
			return
		}
		reply(w, r.PathValue("key"))
	})
	mux.HandleFunc("DELETE "+base+"/{dkey}", func(w http.ResponseWriter, r *http.Request) {
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if err := d.remove(res.PR.PRRef, r.PathValue("dkey")); err != nil {
			writeErr(w, 500, err)
			return
		}
		reply(w, r.PathValue("key"))
	})
}
