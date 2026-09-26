# Install and release

Each release contains one executable with the web UI, code-map indexer and
standard scoring rules embedded. No source checkout, Node.js, shell scripts or
adjacent UI files are needed. Git and GitHub CLI (`gh`) remain runtime dependencies
for GitHub PRs. Log in with `gh auth login`. Install the CLI or configure credentials
for whichever model provider you choose.

## Homebrew on macOS

The tap lives in this repository, so tap it by URL once:

```sh
brew tap amitbet/pr-manager https://github.com/amitbet/pr-manager
brew install --cask amitbet/pr-manager/pr-manager
gh auth login
pr-manager serve
```

Homebrew installs `git` and `gh` as dependencies. The server listens on
`127.0.0.1:8765`, prints its URL and opens your browser. Stop it with Ctrl+C.
If the port is taken it picks another one. UI settings are saved to
`ui-settings.json` in the cache directory, so they survive restarts and port
changes. `-addr` accepts another address.

The initial cask removes quarantine from this unsigned executable using a
post-install hook. Apple signing and notarization are not configured yet.

The macOS arm64 desktop app has its own cask:

```sh
brew install --cask amitbet/pr-manager/pr-manager-desktop
```

It installs `PR Manager.app` into `/Applications` and removes quarantine the same
way. If you download `PR-Manager-macos-arm64.zip` from GitHub Releases instead,
macOS blocks the first launch because the app is ad-hoc signed and not notarized.
Allow it under System Settings → Privacy & Security → Open Anyway, or run
`xattr -dr com.apple.quarantine "/Applications/PR Manager.app"`.

## Direct downloads, including Windows and Linux

Download the archive for your OS and architecture from GitHub Releases and verify
it against `checksums.txt`. Extract `pr-manager` or `pr-manager.exe` into a directory
on your PATH. macOS and Linux use `.tar.gz`; Windows uses `.zip`. Both amd64 and
arm64 are built. On Windows, install Git and GitHub CLI separately, then run:

```powershell
gh auth login
pr-manager.exe serve
```

## Scoop on Windows

This repository is also a Scoop bucket:

```powershell
scoop bucket add pr-manager https://github.com/amitbet/pr-manager
scoop install pr-manager            # CLI, amd64 and arm64
scoop install pr-manager-desktop    # desktop app, amd64
```

Both install `git` and `gh` as dependencies. The desktop app gets a Start
menu shortcut named PR Manager. The binaries are not code-signed, but Scoop
downloads them itself, so Windows SmartScreen does not block them the way it
blocks an exe downloaded in a browser.

## Data and code maps

Installed commands use `os.UserCacheDir()/pr-manager`:

- macOS: `~/Library/Caches/pr-manager`
- Linux: `$XDG_CACHE_HOME/pr-manager`, or `~/.cache/pr-manager`
- Windows: `%LocalAppData%\pr-manager`

Results, drafts and clones live under this directory. `-cache DIR` overrides their
root; `-codemap DIR` independently selects the map output. Existing checkout-local
`.cache` data is not migrated automatically. `make ui` uses `.cache` and
`.cache/map` for development.

PR triage builds missing code maps with the bundled indexer. It links the repo from
the local code directory (`-code-root`, `PR_MANAGER_CODE_ROOT`) if it is there, else
clones it under `CACHE/workspace/code`, or uses the workspace selected by
`PR_MANAGER_WORKSPACE`. `pr-manager index -org ORG` indexes a whole GitHub or GitHub
Enterprise org at once (see the README's "Indexing your own repos").
A workspace can also contain `repos.yaml` metadata alongside `code/` checkouts.

You can also index one local checkout:

```sh
pr-manager codemap build -C /path/to/repo
pr-manager codemap top
pr-manager codemap lookup 'repo/path/to/file.go'
pr-manager -C /path/to/repo -classifier off -summarizer off
```

`codemap build -C` writes a map for that checkout, replacing the map at its output
location. Use `-output DIR` and the triage command's `-codemap DIR` for separate maps.
Use `codemap build -workspace DIR` to rank several repositories together.

Go type-aware indexing needs a Go toolchain on PATH. If it is absent, or the source
requires a newer Go type checker than the executable contains, the indexer warns
and retains declarations and complexity without typed call edges. Install Go and
use `codemap build -force` after upgrading to rebuild degraded graphs. Other
language parsers are compiled into the binary.

`codemap build -config FILE` overrides the embedded scoring rules. Output and graph
cache paths in that file are relative to its directory; `-output` and `-cache`
override them. `pr-manager serve -codemap-config FILE` uses the same override for
automatic builds. Start from `codemap/indexer/default.yaml`.

## Maintainer setup

The Homebrew tap is this repository. CI commits `Casks/pr-manager.rb` to the
default branch when it publishes a release, so `brew tap amitbet/pr-manager
https://github.com/amitbet/pr-manager` picks it up. No separate tap repository or
extra token is needed. The workflow `GITHUB_TOKEN` uploads the release assets
and pushes the cask. If the default branch is protected, allow GitHub Actions to
push to it. The Scoop bucket works the same way. CI regenerates
`bucket/pr-manager.json` and `bucket/pr-manager-desktop.json` with
`scripts/scoop-manifest.sh` and commits them with the casks.

A push to `main` publishes a release. CI resolves the next patch version, tags
that commit, and uploads the binaries. The first release is `v0.1.0`. The next
pushes become `v0.1.1`, `v0.1.2`, and so on. If the commit already has a stable
version tag, CI publishes that tag instead of incrementing the patch number.

`.github/workflows/ci.yml` runs vet, tests, and the smoke check, then creates
the release with the CLI archives and checksums and updates the CLI cask. It
does not wait for the desktop apps. Each desktop build (macOS arm64, Linux
amd64, Windows amd64) adds its package to the release when it finishes. After
all three are in, CI uploads `desktop-checksums.txt` and updates the desktop
cask. `checksums.txt` covers the GoReleaser CLI archives. If a desktop build
fails, the release stays published without that package. Pull requests run the
same tests and desktop builds without publishing.

Pushing a version tag yourself, such as `v1.0.0`, still runs
`.github/workflows/release.yml`. Use that for a minor or major bump. Tags
created by CI do not start that workflow again. A prerelease tag such as
`v0.2.0-rc.1` publishes release assets and leaves the casks and Scoop
manifests unchanged.

To validate locally:

```sh
goreleaser check
goreleaser release --snapshot --clean
python3 scripts/smoke-release.py dist/pr-manager_darwin_arm64_v8.0/pr-manager
```

Use the actual native executable path produced under `dist/` for the smoke check.
Snapshot mode builds archives and the cask locally without publishing. Releases
include version, commit and build date, available with `pr-manager version`.
GoReleaser is pinned to v2.18.2 in CI and in the tag workflow.
