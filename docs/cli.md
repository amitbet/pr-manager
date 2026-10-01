# Command line

```sh
go build -o pr-manager .

# default: a logged-in Claude Code or Codex (ChatGPT) subscription on this machine
./pr-manager -C ../some-repo -base origin/main

# Claude Code subscription: Opus 5.5 triages and reviews (Haiku 4.5 translates)
./pr-manager -classifier claude-code -summarizer claude-code

# Claude API key: same models
ANTHROPIC_API_KEY=... ./pr-manager -classifier anthropic -summarizer anthropic

# OpenAI
./pr-manager -classifier openai-api -summarizer openai-api

# fully local
./pr-manager -classifier ollama -classify-model qwen3.5:9b -summarizer ollama -summary-model qwen3.5:9b

# classify only, no review: OpenJev first pass, units it isn't sure about go to Claude
./pr-manager -summarizer off -classifier openjev -fallback claude-api

# presort only, no LLM
./pr-manager -classifier off -summarizer off

# review everything again instead of keeping what did not change since the last run
./pr-manager -incremental=false

# review, fix and re-review until clean, in place (before a PR, or in CI)
./pr-manager fix
./pr-manager fix -commit -fail-on high

# write the PR description / gate a script
./pr-manager -o triage.md && gh pr create --body-file triage.md
./pr-manager -fail-on-human      # exit 2 if anything needs a human
./pr-manager -out json -o triage.json
```

Policy: copy `triage.example.yaml` to `<repo>/.triage.yaml`.

`pr-manager fix` is the UI's **Fix all issues** from the command line: review, fix, check and fix again for up to `-rounds` rounds, then report, with exit 3 when issues at `-fail-on` or worse are left. See [review and fix from the command line](fix.md) for pre-PR use and a GitHub Actions job.

`-codemap DIR` (default the user cache directory plus `pr-manager/codemap`, `off` to disable) picks the code map. PR runs use the GitHub repo name as the map repo. Local `-C` runs use the checkout's directory name, or `-map-repo`.

## GitHub PRs

```sh
make ui                     # chooses a free port on 127.0.0.1 and opens the browser
make triage-prs REPO=acme/api AUTHOR=someone LIMIT=10   # triage an author's PRs into the UI cache
./pr-manager -pr https://github.com/acme/api/pull/566   # one PR, in the terminal
```

See [using the UI](ui.md) for what the UI does with them, and
[models and providers](providers.md) for picking a model.
