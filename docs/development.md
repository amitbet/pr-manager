# Development

Build from source with Go (`go build -o pr-manager .`), git and an authenticated
[gh](https://cli.github.com). Keep gh current (`brew upgrade gh`): older releases
reject some `gh pr view --json` fields. For the desktop build see
[desktop build](desktop.md).

## Layout

```
main.go          flags; triage, eval, serve, prs commands
serve.go         UI server, job runner, result cache
chat.go          the UI's chat agent: the context it is given about a result, and its workspace
chatactions.go   the chat agent's actions: the catalog it is told about, re-reviewing some units, review thread replies
chatshell.go     the chat agent's read-only shell: where the app keeps things, and the history fetched for blame
chatbundle.go    the chat agent's material as files, job-log selection, code-map entries of the changed files
chatlog.go       job activity logs saved with the results they made (results/logs/<key>/)
review.go        draft comments, file content for context expansion, review submission
ui/              the UI, embedded into the binary: index.html, css/ per component, js/ ES modules (no build step;
                 main.js renders the page and routes data-act clicks to each component's `actions`)
llm/             provider adapters for Anthropic, OpenAI, Ollama, Codex and Claude Code, plus Bedrock, Vertex AI, Foundry and Azure OpenAI (cloud.go); the tool loop that lets the API providers read a workspace (agent.go, fstools.go)
                 (Call only, forced tool choice, per-response usage) + OpenJev client
triage/          diff, units, policy, presort, impact, likelihood, classify, prcontext (rest of the PR for review), summarize,
                 workspace (review tools), tiers, pipeline, render, eval, pr (GitHub fetch)
codemap/         code-map reader and lookup (stdlib only)
cmd/codemap/     standalone development entry point for the bundled indexer
codemap/indexer/ indexer, embedded default rules, language extractors (type-checked Go, tree-sitter Java/Python/C#/Rust and shell/PowerShell/C/C++/PHP/Scala/Kotlin/Ruby/Swift/Dart, lexed TS/JS), PageRank and rollback rules
scripts/         development indexer wrapper and standalone release smoke check
testdata/eval/   labeled fixtures
```
