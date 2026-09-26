package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/amitbet/pr-manager/triage"
)

var scpRemote = regexp.MustCompile(`^(?:[^@]+@)?([^:]+):([^/]+)/(.+?)(?:\.git)?$`)

func localRepoRef(dir string) triage.PRRef {
	remote, err := triage.Git(dir, "remote", "get-url", "origin")
	if err == nil {
		s := strings.TrimSpace(remote)
		if u, e := url.Parse(s); e == nil && u.Host != "" {
			parts := strings.SplitN(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/", 2)
			if len(parts) == 2 {
				return triage.PRRef{Host: triage.NormalizeHost(u.Host), Owner: parts[0], Repo: parts[1]}
			}
		}
		if m := scpRemote.FindStringSubmatch(s); m != nil {
			return triage.PRRef{Host: triage.NormalizeHost(m[1]), Owner: m[2], Repo: strings.TrimSuffix(m[3], ".git")}
		}
		if host, owner, repo, err := triage.ParseRepo(s); err == nil {
			return triage.PRRef{Host: host, Owner: owner, Repo: repo}
		}
	}
	return triage.PRRef{Owner: "local", Repo: filepath.Base(dir)}
}

func localBase(dir string) (string, string, error) {
	if s, err := triage.Git(dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimSpace(s), strings.TrimPrefix(strings.TrimSpace(s), "origin/"), nil
	}
	for _, name := range []string{"main", "master"} {
		if _, err := triage.Git(dir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+name); err == nil {
			return "origin/" + name, name, nil
		}
	}
	return "", "", errors.New("cannot find origin's default branch; run git remote set-head origin -a or fetch origin/main")
}

func gitWithIndex(ctx context.Context, dir, index string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(e.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

type localSnapshot struct {
	info *triage.PRInfo
	src  *triage.Source
	raw  string
}

func inspectLocal(ctx context.Context, path string) (*localSnapshot, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("enter a repository path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	root, err := triage.Git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a Git checkout: %w", path, err)
	}
	dir, err := filepath.EvalSymlinks(filepath.FromSlash(strings.TrimSpace(root)))
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(dir, path) {
		return nil, fmt.Errorf("choose the repository root: %s", dir)
	}
	head, err := triage.Git(dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	head = strings.TrimSpace(head)
	branch, err := triage.Git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return nil, errors.New("checkout is detached; switch to a branch before triaging")
	}
	branch = strings.TrimSpace(branch)
	baseRef, baseBranch, err := localBase(dir)
	if err != nil {
		return nil, err
	}
	base, err := triage.Git(dir, "merge-base", baseRef, "HEAD")
	if err != nil {
		return nil, err
	}
	base = strings.TrimSpace(base)
	counts, err := triage.Git(dir, "rev-list", "--left-right", "--count", baseRef+"...HEAD")
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(counts)
	behind, ahead := 0, 0
	if len(parts) == 2 {
		behind, _ = strconv.Atoi(parts[0])
		ahead, _ = strconv.Atoi(parts[1])
	}
	status, err := triage.Git(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "pr-manager-index-*")
	if err != nil {
		return nil, err
	}
	index := tmp.Name()
	tmp.Close()
	os.Remove(index)
	defer os.Remove(index)
	if _, err = gitWithIndex(ctx, dir, index, "read-tree", "HEAD"); err != nil {
		return nil, err
	}
	if _, err = gitWithIndex(ctx, dir, index, "add", "-A"); err != nil {
		return nil, err
	}
	diffArgs := []string{"diff", "--cached", "--no-color", "--no-ext-diff", "-M", "-U5", base}
	raw, err := gitWithIndex(ctx, dir, index, diffArgs...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("no changes from " + baseRef)
	}
	files, err := triage.ParseDiff(raw)
	if err != nil {
		return nil, err
	}
	ws, err := gitWithIndex(ctx, dir, index, "diff", "--cached", "--no-color", "--no-ext-diff", "-M", "-U5", "-w", "--ignore-blank-lines", base)
	if err != nil {
		return nil, err
	}
	wsFiles, err := triage.ParseDiff(ws)
	if err != nil {
		return nil, err
	}
	real := map[string]bool{}
	for _, f := range wsFiles {
		if len(f.Hunks) > 0 || f.Binary {
			real[f.Path] = true
		}
	}
	src := &triage.Source{Files: files, RealChanges: real, Dir: dir, Base: base, Head: head, HeadDir: dir}
	src.Content = func(p string) ([]byte, error) { return readLocalFile(dir, p) }
	src.BaseContent = func(p string) ([]byte, error) { s, e := triage.Git(dir, "show", base+":"+p); return []byte(s), e }
	ref := localRepoRef(dir)
	title, _ := triage.Git(dir, "log", "-1", "--format=%s", "HEAD")
	if ahead == 0 {
		title = branch + " working changes"
	}
	author, _ := triage.Git(dir, "log", "-1", "--format=%an", "HEAD")
	info := &triage.PRInfo{PRRef: ref, LocalPath: dir, Title: strings.TrimSpace(title), Author: strings.TrimSpace(author), State: "LOCAL", BaseRef: baseBranch, HeadRef: branch, BaseOid: base, HeadOid: head, Ahead: ahead, Behind: behind, Uncommitted: strings.TrimSpace(status) != ""}
	digest := sha256.Sum256([]byte(raw))
	info.SnapshotHash = hex.EncodeToString(digest[:])
	for _, f := range files {
		for _, h := range f.Hunks {
			for _, line := range h.Lines {
				if strings.HasPrefix(line, "+") {
					info.Adds++
				}
				if strings.HasPrefix(line, "-") {
					info.Dels++
				}
			}
		}
	}
	src.Title = info.Title
	return &localSnapshot{info: info, src: src, raw: raw}, nil
}

func localCacheKey(s *localSnapshot, o options) string {
	pathHash := sha256.Sum256([]byte(s.info.LocalPath))
	diffHash := sha256.Sum256([]byte(s.info.BaseOid + "|" + s.info.HeadOid + "|" + s.raw))
	settings := sha256.Sum256([]byte(cacheKey(s.info.PRRef, s.info.HeadOid, o)))
	return fmt.Sprintf("local__%x__%x__%x", pathHash[:5], diffHash[:6], settings[:4])
}

func readLocalFile(dir, path string) ([]byte, error) {
	if !safeRepoPath(path) {
		return nil, errors.New("unsafe path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(dir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, errors.New("file is outside the repository")
	}
	return os.ReadFile(resolved)
}

func (t *triager) RunLocal(ctx context.Context, path string, jo jobOptions, progress func(string, int, int)) (*PRResult, error) {
	o := t.options(jo)
	progress("inspect", 0, 0)
	s, err := inspectLocal(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := ensureLocalCodeMap(ctx, o, s.info, progress); err != nil {
		return nil, err
	}
	key := localCacheKey(s, o)
	if !jo.Force {
		if r, err := t.Load(key); err == nil {
			return r, nil
		}
	}
	policy, attrs, err := sourceConfig(s.src)
	if err != nil {
		return nil, err
	}
	pipe, err := buildPipeline(o, policy, attrs)
	if err != nil {
		return nil, err
	}
	pipe.Progress = progress
	if m := loadCodeMap(o.codemapDir); m != nil {
		pipe.CodeMap = &triage.CodeMap{Map: m, Repo: s.info.Repo}
	}
	return t.runSource(ctx, key, s.info, s.src, pipe, o)
}

func ensureLocalCodeMap(ctx context.Context, o options, info *triage.PRInfo, progress func(string, int, int)) error {
	if o.codemapDir == "" || o.codemapDir == "off" || codeMapHas(loadCodeMap(o.codemapDir), info.Repo) {
		return nil
	}
	codeMapBuildMu.Lock()
	defer codeMapBuildMu.Unlock()
	if codeMapHas(loadCodeMap(o.codemapDir), info.Repo) {
		return nil
	}
	ws, err := workspaceDir(o)
	if err != nil {
		return err
	}
	progress("codemap", 0, 0)
	if err := addRepo(ctx, ws, info.Host, info.Owner, info.Repo, map[string]string{info.Repo: info.LocalPath}); err != nil {
		return err
	}
	if err := buildCodeMap(ctx, o, ws, info.Repo); err != nil {
		return err
	}
	if !codeMapHas(reloadCodeMap(o.codemapDir), info.Repo) {
		return fmt.Errorf("code map did not include %s", info.Repo)
	}
	return nil
}

func (t *triager) startLocal(path string, jo jobOptions) (*job, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("enter a repository path")
	}
	j, ctx, progress := t.newJob("triage", path)
	go func() {
		r, err := t.RunLocal(ctx, path, jo, progress)
		j.finish(err)
		t.mu.Lock()
		defer t.mu.Unlock()
		if err != nil {
			j.Status, j.Error = "error", err.Error()
		} else {
			j.Status, j.Key = "done", r.Key
			j.markCached(r)
		}
	}()
	return j, nil
}

func publishLocal(r *PRResult) (string, error) {
	if r.PR.LocalPath == "" {
		return "", errors.New("this is not a local result")
	}
	if r.PR.URL != "" {
		return r.PR.URL, nil
	}
	if _, err := triage.Git(r.PR.LocalPath, "fetch", "--quiet", "--no-tags", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", r.PR.BaseRef, r.PR.BaseRef)); err != nil {
		return "", err
	}
	s, err := inspectLocal(context.Background(), r.PR.LocalPath)
	if err != nil {
		return "", err
	}
	p := s.info
	if p.HeadOid != r.PR.HeadOid || p.BaseOid != r.PR.BaseOid || p.SnapshotHash != r.PR.SnapshotHash || p.HeadRef != r.PR.HeadRef {
		return "", errors.New("repository changed since triage; triage it again")
	}
	if p.Uncommitted {
		return "", errors.New("commit the working tree changes, then triage again before creating a PR")
	}
	if p.Ahead == 0 {
		return "", errors.New("branch has no commits ahead of origin/" + p.BaseRef)
	}
	if p.Owner == "local" {
		return "", errors.New("origin must be a GitHub repository to create a PR")
	}
	if p.HeadRef == p.BaseRef {
		name := "pr-manager/" + p.HeadOid[:10]
		if tip, err := triage.Git(p.LocalPath, "rev-parse", "--verify", "refs/heads/"+name); err == nil {
			if strings.TrimSpace(tip) != p.HeadOid {
				return "", errors.New("generated PR branch already exists at a different commit")
			}
			if _, err = triage.Git(p.LocalPath, "switch", name); err != nil {
				return "", err
			}
		} else if _, err = triage.Git(p.LocalPath, "switch", "-c", name); err != nil {
			return "", err
		}
		p.HeadRef = name
	}
	if _, err := triage.Git(p.LocalPath, "push", "-u", "origin", p.HeadRef); err != nil {
		return "", err
	}
	cmd := exec.Command("gh", "pr", "create", "--base", p.BaseRef, "--head", p.HeadRef, "--fill")
	cmd.Dir = p.LocalPath
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			if ghErr := triage.GHError(err, e.Stderr); ghErr != nil {
				return "", ghErr
			}
			return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(string(e.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func localPathID(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:10])
}
