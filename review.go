package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// Draft is a pending review comment. It lives in .cache/drafts until the
// review is submitted, so a reload or a server restart doesn't lose it.
type Draft struct {
	ID      string    `json:"id"`
	Path    string    `json:"path"`
	Line    int       `json:"line"`
	Side    string    `json:"side"` // LEFT (old file) | RIGHT (new file)
	Body    string    `json:"body"`
	Created time.Time `json:"created"`
}

type reviews struct {
	dir     string
	fetcher *triage.PRFetcher
	dryRun  bool

	dmu sync.Mutex // guards draft files

	mu    sync.Mutex
	files map[string][]string // "sha:path" -> lines
}

func newReviews(o options, fetcher *triage.PRFetcher) (*reviews, error) {
	r := &reviews{dir: filepath.Join(o.cache, "drafts"), fetcher: fetcher, dryRun: o.reviewDryRun, files: map[string][]string{}}
	return r, os.MkdirAll(r.dir, 0o755)
}

func prRef(req *http.Request) (triage.PRRef, error) {
	n, err := strconv.Atoi(req.PathValue("number"))
	host, owner, repo := triage.NormalizeHost(req.PathValue("host")), req.PathValue("owner"), req.PathValue("repo")
	if err != nil || owner == "" || repo == "" || strings.ContainsAny(host+owner+repo, `/\`) || strings.Contains(host+owner+repo, "..") {
		return triage.PRRef{}, errors.New("bad PR ref")
	}
	return triage.PRRef{Host: host, Owner: owner, Repo: repo, Number: n}, nil
}

func (r *reviews) draftFile(ref triage.PRRef) string {
	return filepath.Join(r.dir, ref.FileKey()+".json")
}

func (r *reviews) load(ref triage.PRRef) ([]Draft, error) {
	b, err := os.ReadFile(r.draftFile(ref))
	if os.IsNotExist(err) {
		return []Draft{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ds []Draft
	return ds, json.Unmarshal(b, &ds)
}

// forResult is the pending comments on res's PR or local checkout.
func (r *reviews) forResult(res *PRResult) []Draft {
	ref := res.PR.PRRef
	if res.PR.LocalPath != "" {
		ref = triage.PRRef{Owner: "local", Repo: localPathID(localSource(res.PR))}
	}
	ds, _ := r.load(ref)
	return ds
}

func (r *reviews) save(ref triage.PRRef, ds []Draft) error {
	if len(ds) == 0 {
		err := os.Remove(r.draftFile(ref))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		return err
	}
	// Written to a temporary file and renamed over the old one, so a crash
	// mid-write never leaves a truncated drafts file behind.
	f := r.draftFile(ref)
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

// fileLines returns a file at a commit from the PR's cached clone.
func (r *reviews) fileLines(ref triage.PRRef, sha, path string) ([]string, error) {
	key := sha + ":" + path
	r.mu.Lock()
	lines, ok := r.files[key]
	r.mu.Unlock()
	if ok {
		return lines, nil
	}
	s, err := triage.Git(r.fetcher.RepoDir(ref), "show", key)
	if err != nil {
		return nil, err
	}
	lines = strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	r.mu.Lock()
	if len(r.files) > 500 {
		r.files = map[string][]string{}
	}
	r.files[key] = lines
	r.mu.Unlock()
	return lines, nil
}

// readCheckoutFile is readLocalFile with the checkout's root resolved
// first: readLocalFile compares the resolved file against the root, so an
// unresolved root under a symlink (/var on macOS) rejects every file.
func readCheckoutFile(dir, path string) ([]byte, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	return readLocalFile(root, path)
}

// readFixFile reads path from a fix checkout. ok is false, with no error,
// when the checkout no longer exists, so the caller can fall back to the
// PR head.
func readFixFile(dir, path string) (b []byte, ok bool, err error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, false, nil
	}
	b, err = readCheckoutFile(dir, path)
	return b, err == nil, err
}

func (r *reviews) routes(mux *http.ServeMux, t *triager) {
	const pr = "/api/prs/{host}/{owner}/{repo}/{number}"
	const local = "/api/local/{key}"
	localResult := func(req *http.Request) (*PRResult, triage.PRRef, error) {
		res, err := t.Load(req.PathValue("key"))
		if err != nil {
			return nil, triage.PRRef{}, err
		}
		if res.PR.LocalPath == "" {
			return nil, triage.PRRef{}, errors.New("not a local result")
		}
		return res, triage.PRRef{Owner: "local", Repo: localPathID(localSource(res.PR))}, nil // as forResult
	}
	mux.HandleFunc("GET "+local+"/file", func(w http.ResponseWriter, req *http.Request) {
		res, _, err := localResult(req)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		path := req.URL.Query().Get("path")
		if !safeRepoPath(path) {
			writeErr(w, 400, errors.New("bad file path"))
			return
		}
		found := false
		for _, f := range res.Files {
			if f.Path == path || f.OldPath == path {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, 404, errors.New("file is not in the reviewed diff"))
			return
		}
		var b []byte
		fixed := false
		if res.LocalFixDir != "" && req.URL.Query().Get("side") != "base" {
			// A fix checkout that was cleaned up falls back to the head.
			b, fixed, err = readFixFile(res.LocalFixDir, path)
		}
		switch {
		case fixed || err != nil:
		case req.URL.Query().Get("side") == "base":
			var s string
			s, err = triage.Git(res.PR.LocalPath, "show", res.PR.BaseOid+":"+path)
			b = []byte(s)
		case res.PR.Rev != "":
			var s string
			s, err = triage.Git(res.PR.LocalPath, "show", res.PR.HeadOid+":"+path)
			b = []byte(s)
		default:
			b, err = readCheckoutFile(res.PR.LocalPath, path)
		}
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]any{"lines": strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")})
	})
	mux.HandleFunc("GET "+local+"/drafts", func(w http.ResponseWriter, req *http.Request) {
		_, ref, err := localResult(req)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		ds, err := r.load(ref)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, ds)
	})
	mux.HandleFunc("POST "+local+"/drafts", func(w http.ResponseWriter, req *http.Request) {
		_, ref, err := localResult(req)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		var d Draft
		if err = json.NewDecoder(req.Body).Decode(&d); err != nil {
			writeErr(w, 400, err)
			return
		}
		d.Body = strings.TrimSpace(d.Body)
		if !safeRepoPath(d.Path) || d.Line <= 0 || (d.Side != "LEFT" && d.Side != "RIGHT") || d.Body == "" {
			writeErr(w, 400, errors.New("draft needs path, line, side and body"))
			return
		}
		r.dmu.Lock()
		defer r.dmu.Unlock()
		ds, err := r.load(ref)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if d.ID == "" {
			var b [6]byte
			_, _ = rand.Read(b[:])
			d.ID, d.Created = hex.EncodeToString(b[:]), time.Now()
			ds = append(ds, d)
		} else {
			found := false
			for i := range ds {
				if ds[i].ID == d.ID {
					ds[i].Body = d.Body
					found = true
				}
			}
			if !found {
				writeErr(w, 404, errors.New("no such draft"))
				return
			}
		}
		if err = r.save(ref, ds); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, ds)
	})
	mux.HandleFunc("DELETE "+local+"/drafts/{id}", func(w http.ResponseWriter, req *http.Request) {
		_, ref, err := localResult(req)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		r.dmu.Lock()
		defer r.dmu.Unlock()
		ds, err := r.load(ref)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out := ds[:0]
		for _, d := range ds {
			if d.ID != req.PathValue("id") {
				out = append(out, d)
			}
		}
		if err = r.save(ref, out); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, out)
	})

	// GET ?key=<result key>&path=...&side=head|base -> {lines: [...]}
	mux.HandleFunc("GET "+pr+"/file", func(w http.ResponseWriter, req *http.Request) {
		ref, err := prRef(req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		res, err := t.Load(req.URL.Query().Get("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if res.PR == nil {
			writeErr(w, 404, errors.New("result has no PR"))
			return
		}
		path := req.URL.Query().Get("path")
		sha := res.PR.HeadOid
		if req.URL.Query().Get("side") == "base" {
			sha = res.PR.BaseOid
		} else if res.LocalFixDir != "" {
			// A fixed result's head is the fix checkout, not the PR head
			// blob: it has the fix's edits and the files it added. It is not
			// cached, as a follow-up fix rewrites it in place.
			if !safeRepoPath(path) {
				writeErr(w, 400, errors.New("bad file path"))
				return
			}
			b, ok, err := readFixFile(res.LocalFixDir, path)
			if err != nil {
				writeErr(w, 404, err)
				return
			}
			if ok {
				writeJSON(w, 200, map[string]any{"lines": strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")})
				return
			}
			// The fix checkout was cleaned up: the PR head is the next best.
		}
		lines, err := r.fileLines(ref, sha, path)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]any{"lines": lines})
	})

	mux.HandleFunc("GET "+pr+"/drafts", func(w http.ResponseWriter, req *http.Request) {
		ref, err := prRef(req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		ds, err := r.load(ref)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, ds)
	})

	mux.HandleFunc("POST "+pr+"/drafts", func(w http.ResponseWriter, req *http.Request) {
		ref, err := prRef(req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		var d Draft
		if err := json.NewDecoder(req.Body).Decode(&d); err != nil {
			writeErr(w, 400, err)
			return
		}
		d.Body = strings.TrimSpace(d.Body)
		if d.Path == "" || d.Line <= 0 || (d.Side != "LEFT" && d.Side != "RIGHT") || d.Body == "" {
			writeErr(w, 400, errors.New("draft needs path, line > 0, side LEFT|RIGHT and a body"))
			return
		}
		r.dmu.Lock()
		defer r.dmu.Unlock()
		ds, err := r.load(ref)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if d.ID == "" {
			var b [6]byte
			_, _ = rand.Read(b[:])
			d.ID, d.Created = hex.EncodeToString(b[:]), time.Now()
			ds = append(ds, d)
		} else {
			found := false
			for i := range ds {
				if ds[i].ID == d.ID {
					ds[i].Body, found = d.Body, true
				}
			}
			if !found {
				writeErr(w, 404, errors.New("no such draft"))
				return
			}
		}
		if err := r.save(ref, ds); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, ds)
	})

	mux.HandleFunc("DELETE "+pr+"/drafts/{id}", func(w http.ResponseWriter, req *http.Request) {
		ref, err := prRef(req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		r.dmu.Lock()
		defer r.dmu.Unlock()
		ds, err := r.load(ref)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out := ds[:0]
		for _, d := range ds {
			if d.ID != req.PathValue("id") {
				out = append(out, d)
			}
		}
		if err := r.save(ref, out); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, out)
	})

	// POST {event: COMMENT|APPROVE|REQUEST_CHANGES, body, commit_id}
	// submits every draft as one GitHub review and clears them.
	mux.HandleFunc("POST "+pr+"/review", func(w http.ResponseWriter, req *http.Request) {
		ref, err := prRef(req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		var in struct {
			Event    string `json:"event"`
			Body     string `json:"body"`
			CommitID string `json:"commit_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeErr(w, 400, err)
			return
		}
		r.dmu.Lock()
		defer r.dmu.Unlock()
		ds, err := r.load(ref)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		review := triage.Review{CommitID: in.CommitID, Event: in.Event, Body: strings.TrimSpace(in.Body)}
		for _, d := range ds {
			review.Comments = append(review.Comments, triage.ReviewComment{Path: d.Path, Line: d.Line, Side: d.Side, Body: d.Body})
		}
		if err := review.Validate(); err != nil {
			writeErr(w, 400, err)
			return
		}
		if r.dryRun {
			writeJSON(w, 200, map[string]any{"dry_run": true, "payload": review})
			return
		}
		url, err := triage.SubmitReview(ref, review)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		if err := r.save(ref, nil); err != nil {
			writeErr(w, 500, fmt.Errorf("review posted (%s) but clearing drafts failed: %w", url, err))
			return
		}
		writeJSON(w, 200, map[string]any{"html_url": url})
	})
}
