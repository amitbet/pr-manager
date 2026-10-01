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
// without a special case. Path separators in a part (a GitLab subgroup,
// an Azure DevOps org/project) are escaped, so each repo keeps its own
// file; parts without them are as they always were.
func repoKey(ref triage.PRRef) string {
	parts := []string{}
	for _, p := range []string{ref.Host, ref.Owner, ref.Repo} {
		if p = strings.TrimSpace(p); p != "" && p != "." && p != ".." {
			parts = append(parts, pathPart(p))
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
	// Written to a temporary file and renamed over the old one, so a
	// crash mid-write never leaves a truncated file behind that blocks
	// every later dismissal.
	tmp, err := os.CreateTemp(filepath.Dir(f), filepath.Base(f)+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), f)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
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
	// Recounted every time, dismissals or not: ApplyDismissed re-places
	// every unit, and a result saved after an earlier load (the overview,
	// the sequence, a thread refresh) can carry counts from dismissals
	// that have since been restored.
	triage.ApplyDismissed(units, r.tierPolicy(), d.lookup(r.PR.PRRef))
	rep := &triage.Report{Units: units}
	r.Counts = rep.Counts()
	r.Impact, r.Likelihood, r.Attention = rep.Scores()
}

// add records dismissals, replacing earlier ones with the same key so a
// changed reason wins.
func (d *dismissals) add(ref triage.PRRef, xs ...Dismissal) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ds, err := d.load(ref)
	if err != nil {
		return err
	}
	replace := map[string]bool{}
	for _, x := range xs {
		replace[x.Key] = true
	}
	out := ds[:0]
	for _, old := range ds {
		if !replace[old.Key] {
			out = append(out, old)
		}
	}
	now := time.Now()
	for _, x := range xs {
		x.Created = now
		out = append(out, x)
	}
	return d.save(ref, out)
}

// remove restores a dismissed item, dropping every record under keys.
func (d *dismissals) remove(ref triage.PRRef, keys ...string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ds, err := d.load(ref)
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	out := ds[:0]
	for _, old := range ds {
		if !drop[old.Key] {
			out = append(out, old)
		}
	}
	return d.save(ref, out)
}

// restoreKeys are the keys to drop to restore the item dismissed under
// key: an issue can be matched by several records (its own severity, a
// worse one, the legacy key), and dropping only the one that matched
// first would leave it dismissed by the next. Its repeats were dismissed
// with it, so they come back with it.
func restoreKeys(r *PRResult, key string) []string {
	keys := []string{key}
	units := resultUnits(r)
	for _, u := range units {
		for _, is := range u.Issues {
			if is.DismissKey != key {
				continue
			}
			keys = append(keys, triage.IssueKeys(u.ID, is)...)
			for _, x := range triage.Repeats(units, u.ID, is.Title) {
				keys = append(keys, triage.IssueKeys(x.Unit.ID, x.Unit.Issues[x.Index])...)
			}
		}
	}
	return keys
}

// dismissRequest picks the issue or lint finding to dismiss out of a unit.
type dismissRequest struct {
	Unit   string `json:"unit"`
	Kind   string `json:"kind"` // issue (default) | lint
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// records are the stored records for the request: the issue's, and one
// for each issue that repeats it (see triage.Dedupe), so a repeat stays
// dismissed if its link to the issue is lost on a later push.
func (req dismissRequest) records(r *PRResult) ([]Dismissal, error) {
	x, err := req.record(r)
	if err != nil {
		return nil, err
	}
	out := []Dismissal{x}
	if x.Kind != "issue" {
		return out, nil
	}
	units := resultUnits(r)
	for _, rep := range triage.Repeats(units, x.Unit, x.Title) {
		y, err := dismissRequest{Unit: rep.Unit.ID, Kind: "issue", Index: rep.Index, Reason: req.Reason}.record(r)
		if err == nil {
			out = append(out, y)
		}
	}
	return out, nil
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
		xs, err := req.records(res)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := d.add(res.PR.PRRef, xs...); err != nil {
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
		if err := d.remove(res.PR.PRRef, restoreKeys(res, r.PathValue("dkey"))...); err != nil {
			writeErr(w, 500, err)
			return
		}
		reply(w, r.PathValue("key"))
	})
}
