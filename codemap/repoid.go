package codemap

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// A repo's identity is host/namespace/name, lowercased: github.com/acme/api,
// gitlab.com/group/sub/api, dev.azure.com/org/project/api. The map names
// repos by their workspace directory (a bare name like "api" unless two
// repos share it), and records each one's identity in meta.json so a PR's
// repo is found by who it is, not by what its directory is called.

var scpRemote = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// ParseRemote splits a git remote URL into host, owner and repo. Owner is
// the whole namespace path, so it may hold slashes (GitLab subgroups,
// Azure DevOps org/project); repo is the last segment. It understands
// https and ssh URLs, scp-style user@host:path, GitLab subgroups,
// Bitbucket Server's /scm/ prefix and Azure DevOps' _git and v3 paths.
// Host is lowercased but otherwise as written (github.com stays).
func ParseRemote(remote string) (host, owner, repo string, ok bool) {
	s := strings.TrimSpace(remote)
	var p string
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		host, p = u.Host, u.Path
		if u.Scheme != "http" && u.Scheme != "https" {
			host = u.Hostname() // an ssh port is not part of who the repo is
		}
	} else if m := scpRemote.FindStringSubmatch(s); m != nil && !strings.Contains(s, "://") {
		host, p = m[1], m[2]
	} else {
		return "", "", "", false
	}
	host = strings.ToLower(host)
	parts := strings.FieldsFunc(strings.TrimSuffix(strings.TrimRight(p, "/"), ".git"), func(r rune) bool { return r == '/' })
	switch {
	case host == "ssh.dev.azure.com" || host == "vs-ssh.visualstudio.com":
		// git@ssh.dev.azure.com:v3/org/project/repo
		if len(parts) > 0 && parts[0] == "v3" {
			parts = parts[1:]
		}
		host = "dev.azure.com"
	case strings.HasSuffix(host, ".visualstudio.com"):
		// https://org.visualstudio.com/[DefaultCollection/]project/_git/repo
		org := strings.TrimSuffix(host, ".visualstudio.com")
		if len(parts) > 0 && strings.EqualFold(parts[0], "DefaultCollection") {
			parts = parts[1:]
		}
		parts, host = append([]string{org}, parts...), "dev.azure.com"
	}
	for i, seg := range parts {
		if i > 0 && i < len(parts)-1 && seg == "_git" { // Azure DevOps: org/project/_git/repo
			parts = append(parts[:i:i], parts[i+1:]...)
			break
		}
	}
	for i, seg := range parts {
		if seg == "scm" && len(parts)-i >= 3 { // Bitbucket Server: [context/]scm/project/repo
			parts = parts[i+1:]
			break
		}
	}
	if len(parts) < 2 || host == "" {
		return "", "", "", false
	}
	return host, strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], true
}

// RepoID is the identity of host/owner/repo. An empty host is github.com,
// as in pr-manager's PR refs; ports are dropped.
func RepoID(host, owner, repo string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || host == "www.github.com" || host == "api.github.com" {
		host = "github.com"
	}
	owner, repo = strings.Trim(strings.TrimSpace(owner), "/"), strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return ""
	}
	return strings.ToLower(host + "/" + owner + "/" + repo)
}

// RemoteID is the identity of a git remote URL, "" if it can't be read.
func RemoteID(remote string) string {
	host, owner, repo, ok := ParseRemote(remote)
	if !ok {
		return ""
	}
	return RepoID(host, owner, repo)
}

// RepoName finds a repo in the map: the one whose identity is id, else
// the one called name when the map does not know its identity (a map
// built before identities were recorded, or a checkout with no remote).
// It returns "" when the map does not have the repo; in particular a
// repo called name with a different identity is not a match.
func (m *Map) RepoName(id, name string) string {
	if m == nil {
		return ""
	}
	if id != "" {
		var hits []string
		for k, rm := range m.Meta.Repos {
			if rm.Remote == id {
				if k == name {
					return k
				}
				hits = append(hits, k)
			}
		}
		if len(hits) > 0 {
			sort.Strings(hits)
			return hits[0]
		}
	}
	if rm, ok := m.Meta.Repos[name]; ok && rm.Remote == "" {
		return name
	}
	return ""
}
