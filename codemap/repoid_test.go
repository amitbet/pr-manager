package codemap

import "testing"

func TestParseRemote(t *testing.T) {
	for _, tc := range []struct{ remote, host, owner, repo string }{
		{"https://github.com/acme/api.git", "github.com", "acme", "api"},
		{"git@github.com:acme/api.git", "github.com", "acme", "api"},
		{"ssh://git@ghe.example.com/acme/api.git", "ghe.example.com", "acme", "api"},
		{"https://ghe.example.com:8443/acme/api", "ghe.example.com:8443", "acme", "api"},
		{"https://gitlab.com/g/sub/r.git", "gitlab.com", "g/sub", "r"},
		{"git@gitlab.com:g/sub/r.git", "gitlab.com", "g/sub", "r"},
		{"ssh://git@gitlab.example.com:2222/g/a/b/r.git", "gitlab.example.com", "g/a/b", "r"},
		{"https://bitbucket.corp.com/scm/proj/repo.git", "bitbucket.corp.com", "proj", "repo"},
		{"https://bitbucket.corp.com/bitbucket/scm/proj/repo.git", "bitbucket.corp.com", "proj", "repo"},
		{"ssh://git@bitbucket.corp.com:7999/proj/repo.git", "bitbucket.corp.com", "proj", "repo"},
		{"https://dev.azure.com/org/proj/_git/repo", "dev.azure.com", "org/proj", "repo"},
		{"https://org@dev.azure.com/org/proj/_git/repo", "dev.azure.com", "org/proj", "repo"},
		{"git@ssh.dev.azure.com:v3/org/proj/repo", "dev.azure.com", "org/proj", "repo"},
		{"https://org.visualstudio.com/DefaultCollection/proj/_git/repo", "dev.azure.com", "org/proj", "repo"},
		{"org@vs-ssh.visualstudio.com:v3/org/proj/repo", "dev.azure.com", "org/proj", "repo"},
	} {
		h, o, r, ok := ParseRemote(tc.remote)
		if !ok || h != tc.host || o != tc.owner || r != tc.repo {
			t.Errorf("ParseRemote(%q) = %q %q %q %v, want %q %q %q", tc.remote, h, o, r, ok, tc.host, tc.owner, tc.repo)
		}
	}
	for _, bad := range []string{"", "/tmp/repo", "../other", "https://github.com/acme", "file:///srv/git/api.git"} {
		if h, o, r, ok := ParseRemote(bad); ok {
			t.Errorf("ParseRemote(%q) = %q %q %q, want no match", bad, h, o, r)
		}
	}
}

func TestRepoID(t *testing.T) {
	a := RemoteID("git@github.com:Acme/API.git")
	for _, id := range []string{
		RemoteID("https://github.com/acme/api"),
		RemoteID("ssh://git@github.com:22/acme/api.git"),
		RepoID("", "acme", "api"),
	} {
		if id != a || a != "github.com/acme/api" {
			t.Errorf("identity %q, want %q", id, a)
		}
	}
	if RemoteID("git@ssh.dev.azure.com:v3/org/proj/repo") != RemoteID("https://dev.azure.com/org/proj/_git/repo") {
		t.Error("Azure DevOps ssh and https remotes differ")
	}
	if RepoID("", "", "api") != "" {
		t.Error("an identity without an owner")
	}
}

func TestRepoNameSameNamedRepos(t *testing.T) {
	m := &Map{Meta: Meta{Repos: map[string]RepoMeta{
		"api":        {Remote: "github.com/other/api"},
		"acme__api":  {Remote: "github.com/acme/api"},
		"legacy":     {},
		"g%2Fsub__r": {Remote: "gitlab.com/g/sub/r"},
	}}}
	for _, tc := range []struct{ id, name, want string }{
		{"github.com/other/api", "api", "api"},
		{"github.com/acme/api", "api", "acme__api"},
		{"github.com/third/api", "api", ""}, // same name, another repo
		{"gitlab.com/g/sub/r", "r", "g%2Fsub__r"},
		{"github.com/acme/legacy", "legacy", "legacy"}, // no identity recorded: by name
		{"", "legacy", "legacy"},
		{"", "api", ""}, // no identity to match, and api belongs to other
		{"github.com/acme/missing", "missing", ""},
	} {
		if got := m.RepoName(tc.id, tc.name); got != tc.want {
			t.Errorf("RepoName(%q, %q) = %q, want %q", tc.id, tc.name, got, tc.want)
		}
	}
	if (*Map)(nil).RepoName("github.com/acme/api", "api") != "" {
		t.Error("a nil map has a repo")
	}
}
