"""Fetch PRs for the grouping sweep: merge-base diff plus head and base sources.

The diff is taken against the merge base, not the base branch tip, so it is
what the PR actually changed. Declaration parsing needs the real files, so
every path in the diff is fetched at both revisions.
"""
import base64, json, pathlib, re, subprocess, sys

ROOT = pathlib.Path(__file__).resolve().parent
PRS = [
    ("prometheus/prometheus", 18905),
    ("grafana/grafana", 133630),
    ("kubernetes/kubernetes", 142277),
    ("elastic/elasticsearch", 160261),
    ("vercel/next.js", 99189),
]


def gh(*args, binary=False):
    p = subprocess.run(["gh", *args], capture_output=True, check=True)
    return p.stdout if binary else p.stdout.decode()


def slug(repo, n):
    return repo.replace("/", "-") + "-" + str(n)


def paths(diff):
    return sorted({m.group(1) for m in re.finditer(r'^diff --git a/(.+?) b/', diff, re.M)})


def content(repo, path, sha, dest):
    try:
        raw = gh("api", f"repos/{repo}/contents/{path}?ref={sha}", "--jq", ".content")
    except subprocess.CalledProcessError:
        return  # deleted at that revision, or too large; unit builder copes
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_bytes(base64.b64decode(raw))


def fence(data):
    """Keep fetched sources out of the pr-manager module.

    They are real Go packages with imports this module does not have, so
    without their own go.mod `go build ./...` tries to compile them.
    """
    (data / "go.mod").write_text("module pr-triage-fixtures\n\ngo 1.26.0\n")


def main():
    (ROOT / "data").mkdir(parents=True, exist_ok=True)
    fence(ROOT / "data")
    index = []
    for repo, n in PRS:
        d = ROOT / "data" / slug(repo, n)
        meta_file = d / "pr.json"
        if meta_file.exists():
            index.append(json.loads(meta_file.read_text()))
            print("cached", slug(repo, n), flush=True)
            continue
        d.mkdir(parents=True, exist_ok=True)
        pr = json.loads(gh("api", f"repos/{repo}/pulls/{n}"))
        base_sha, head_sha = pr["base"]["sha"], pr["head"]["sha"]
        cmp = json.loads(gh("api", f"repos/{repo}/compare/{base_sha}...{head_sha}"))
        merge_base = cmp["merge_base_commit"]["sha"]
        diff = gh("api", f"repos/{repo}/compare/{merge_base}...{head_sha}",
                  "-H", "Accept: application/vnd.github.v3.diff")
        (d / "pr.diff").write_text(diff)
        fs = paths(diff)
        for p in fs:
            content(repo, p, head_sha, d / "head" / p)
            content(repo, p, merge_base, d / "base" / p)
        meta = {"repo": repo, "number": n, "title": pr["title"], "head": head_sha,
                "merge_base": merge_base, "files": len(fs),
                "additions": pr["additions"], "deletions": pr["deletions"], "dir": str(d)}
        meta_file.write_text(json.dumps(meta, indent=2) + "\n")
        index.append(meta)
        print("fetched", slug(repo, n), meta["files"], "files", flush=True)
    (ROOT / "data" / "index.json").write_text(json.dumps(index, indent=2) + "\n")


if __name__ == "__main__":
    main()
