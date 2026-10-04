# pr-manager

PR Manager helps you review and fix code changes with a guided walkthrough and a code map. In **Pre-PR** mode, open your repository directory to review changes since your branch diverged from `origin/main` or the remote's default branch, including unpushed commits and uncommitted edits. Add `#` and a commit or branch to the path to review that instead, as committed: `~/src/app#a82c38` is the one commit against its parent, and `~/src/app#feature` is the branch from where it left the default branch. The branch button next to the input lists the checkout's branches and recent commits to pick from. **Create PR** works only on the checked-out branch. Fixing such a review asks first: **Check out and fix** switches the checkout to the branch (a commit gets a new `pr-manager/<sha>` branch at it; the working tree has to be clean) and fixes there, and **Fix current code** keeps the checkout's branch and has the fixer find the reviewed code in it as it is now, skipping issues that no longer apply. In **On-PR** mode, paste a PR link to review the changes, fix issues, and approve the PR.

Reviewing [prometheus/prometheus#19805](https://github.com/prometheus/prometheus/pull/19805),
a TSDB fix that preserves mapped chunks during WAL replay.

Review walkthrough with the review notes and code diff side by side:

![Review walkthrough for the Prometheus TSDB fix, showing review notes beside the diff](docs/images/prometheus-review-walkthrough.png)

Code map with the PR's changes marked on the files they touch:

![Prometheus code map colored by impact and likelihood, with PR changes overlaid](docs/images/prometheus-codemap.png)

## Install the desktop app

macOS Apple silicon, with [Homebrew](https://brew.sh):

```sh
brew tap amitbet/pr-manager https://github.com/amitbet/pr-manager
brew install --cask amitbet/pr-manager/pr-manager-desktop
```

Open **PR Manager** from Applications. The app includes its own icon.

Windows amd64, with [Scoop](https://scoop.sh):

```powershell
scoop bucket add pr-manager https://github.com/amitbet/pr-manager
scoop install pr-manager-desktop
```

Open **PR Manager** from the Start menu. Scoop creates the shortcut with the app icon.

Linux amd64, download and extract the desktop app:

```sh
curl -fLO https://github.com/amitbet/pr-manager/releases/latest/download/pr-manager-linux-amd64.tar.gz
tar -xzf pr-manager-linux-amd64.tar.gz
```

Run `./pr-manager-linux-amd64`. The app has a window icon; the archive does not
install an application-menu shortcut. It requires GTK 3 and WebKit2GTK 4.1.
The archive also holds the `pr-manager` command, which runs without them.

## Update the desktop app

macOS:

```sh
brew update
brew upgrade --cask amitbet/pr-manager/pr-manager-desktop
```

Windows:

```powershell
scoop update
scoop update pr-manager-desktop
```

Linux: close the app and repeat the two download and extract commands above
in the same directory to replace the executable.

Before opening the app for the first time, run `gh auth login`. On Linux,
install `git` and `gh` first. See [installation details](docs/install.md) for
the CLI and other builds.

## What it does

PR Manager sorts every change in a PR into one of four buckets:

- **human**: someone has to read it
- **skim**: the generated summary is enough
- **auxiliary**: tests, docs and fixtures, not the code the PR ships; reviewed, and raised to skim or human when the review finds something or a test is weakened
- **none**: can't change behavior (generated code, formatting, a pure rename, comments only), or code whose measured impact and likelihood are too low to need a look

When it isn't sure, a change goes up a bucket, never down.

Beyond sorting, it can:

- walk you through the changes one step at a time, most important first
- show the PR's changes on a map of the code, colored by impact and by how likely each part is to break
- collect the review's issues, lint findings and open GitHub comments in one list
- fix issues on a local branch, check the fix, and try again
- draw the call flow the PR changes as a sequence diagram
- re-review only what changed after a push

## Command line

```sh
pr-manager serve                                  # the UI in your browser
pr-manager -pr https://github.com/acme/api/pull/566
pr-manager -C ../some-repo -base origin/main      # a local branch
pr-manager fix                                    # review and fix in place
```

By default it uses a Claude Code or Codex subscription logged in on this machine.
API keys, Bedrock, Vertex AI, Foundry, Azure OpenAI and Ollama work too.

## Documentation

- [Installation and release](docs/install.md)
- [How triage works](docs/how-it-works.md): the pipeline, scoring and review budgets
- [Using the UI](docs/ui.md)
- [Command line](docs/cli.md)
- [Review and fix from the command line](docs/fix.md), including CI
- [Models and providers](docs/providers.md)
- [Code map](docs/codemap.md)
- [Eval and OpenJev](docs/eval.md)
- [Development](docs/development.md) and [desktop build](docs/desktop.md)

## License

Apache License 2.0. See [LICENSE](LICENSE).
