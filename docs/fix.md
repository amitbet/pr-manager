# Review and fix from the command line

`pr-manager fix` runs the UI's **Fix all issues** without the UI: it reviews
the change, fixes the issues the review found at medium or worse (the low
ones too with `-low`, or when nothing worse is left), checks the fixed code,
and fixes what the check still finds, for up to `-rounds` rounds (3 by
default). The rounds follow the same rules as the UI: an issue at medium or
worse a round worked on that the check still finds gets another try if the
patch changed its code (not if the fixer left it alone; a low one gets one
round); a new issue only at medium or worse, and only in code a round
wrote; an issue a check had found fixed that comes back is left for you. Each round's patch is also kept to the issues, tested, and audited
by a second model (below). Then the fixed code is triaged once and the
report printed.

It is the `pr-manager` command, so it comes with either package: the CLI
(`brew install --cask amitbet/pr-manager/pr-manager`, `scoop install
pr-manager`, or a release archive) or the desktop app, which puts
`pr-manager` on your PATH too. The Linux desktop archive holds both the app
and the `pr-manager` command.

## Before a PR

In a checkout, on your branch:

```sh
pr-manager fix                  # review the branch since origin's default branch, fix in place
pr-manager fix -commit          # commit your changes first, then the fixes, with written messages
pr-manager fix -in worktree     # fix on a new branch in a worktree under the cache instead
pr-manager fix -C ~/src/app -base origin/release-2
pr-manager fix -pr https://github.com/acme/web/pull/42   # a PR, fixed in a worktree under the cache
```

What is reviewed is what the UI's Pre-PR mode reviews: the branch since it
left the base, unpushed commits and uncommitted changes included. Fixes in
place go into the checked-out branch next to your own changes and are left
uncommitted unless you pass `-commit`. A worktree has none of your
uncommitted changes, so `-in worktree` needs them committed (or `-commit`).

Models come from the usual flags and defaults: a logged-in Codex or Claude
Code subscription, else a cloud or API key (see [models and providers](providers.md)).

## What each round does

A round asks the fixer for a patch, applies it, and then:

1. **Scope.** Before the patch counts, Go checks that it stays near the
   issues: every changed hunk inside a targeted function or within 20 lines
   of a targeted line, at most about twice the lines the issues should take
   (15 per issue plus 30% of the code they are in, 30 to 400), and no new
   exported name or command-line flag the fixer didn't name, with why, in
   its answer. A patch that fails goes back to the fixer once with what was
   out of scope. An agent's patch doesn't: its retry would start over and
   read everything again, so the patch stands and the audit and the trim
   judge the lines out of scope. The fixer's prompt lists what a fix doesn't add (a helper,
   an option, a check for a case that can't happen, a tidy-up, tests beyond
   the issue). A change you asked for from the chat is new code by request,
   so only the file limit applies to it.
2. **Tests.** With tests on (below), the packages the round changed are
   tested. A test that passed before the round and fails after it undoes
   the round, and the next round is told which test broke and how. This
   retry doesn't use up one of `-rounds`; a second break stops the fix.
3. **Check.** The fixer's model reviews the units the patch touched, and
   those of targeted review comments; a unit the patch left alone keeps
   its review. A new issue only starts another round if it is medium or
   worse and on a line a round wrote, or quotes one: a check that reads a
   whole function again finds more of what was there before, and chasing
   that is how rounds stop converging. Those issues stay on the result,
   except new low ones, which the check drops.
4. **Audit.** A second model checks the round (see below).

Once the rounds end, a **trim** round asks the fixer to take out what the
audits flagged, keeping the fixes. The trim is undone if it breaks a test,
its check fails, or the check finds again an issue the rounds fixed.

## The audit

The fixer and the check are the same model, so the check shares the
fixer's blind spots. After each round a second model, in a fresh context
and with none of the fixer's reasoning, reads the round's patch, the code
around it now, the fixer's own account of what it added, the round's test
result, and the repository's `REVIEW.md`, `CLAUDE.md` and `AGENTS.md` as
the merge base has them (a PR's own copies don't count). It answers:

- **Was each issue fixed?** yes, partial (part of it, or not every place
  the same bug is), symptom (the failure is guarded against but its cause
  is still there), or no. When the check found an issue fixed and the audit
  says no or symptom, the issue goes back into the rounds. Partial doesn't:
  what the audit says is left shows on the result, for you to judge.
- **What did the patch add that the issues didn't need?** A helper, an
  option, an abstraction, a check for a case that can't happen, an
  unrelated refactor, tests beyond the issue, or something that breaks the
  repository's conventions. Each finding has to quote a line the round
  added; Go drops the ones that don't, and keeps 10 a round at most.

What the trim keeps shows on the Issues tab as a low note from **fix
audit**. It doesn't count toward the unit's bucket or `-fail-on`: it says
what the fix did, not what is wrong with the code. Fixing it trims it.

`-fix-auditor auto` (the default) picks the other subscription CLI than the
fixer's (a claude-code fix is audited by codex and the other way round),
else the other family's API key, else the fixer's own provider, which the
summary says isn't independent. `-fix-audit-model` and `-fix-audit-effort`
(default medium) tune it; `-fix-auditor off` turns it off. A failed audit is a
warning, never a failed fix. Each round's audit is saved on the result
(`fix_audit`): what the round added and removed, for how many issues, the
verdicts, what was flagged and what the trim removed.

## Tests

`-tests on` runs the repository's tests: during the review, after every fix
round, and on the fixed code at the end. It is on by default in the desktop
app and off for the CLI and `serve`, since it runs the change's code on
this machine. `-tests on` tests local checkouts and PRs whose branch is in
the repository itself (pushing there needs write access) or whose author is
in `-trusted-authors`; `-tests all` tests forks too.

For Go, `go test` runs on the packages the change touches, with coverage,
and with modules from the module cache only (`go mod download` runs first,
outside the sandbox). Otherwise `npm test` (when `node_modules` is there),
`cargo test --offline` or `pytest`, or `-test-cmd`, which runs through the
shell in the repository root. A run has `-test-timeout` (600s), its whole
process group is killed when it ends, and on macOS it runs under
`sandbox-exec` with no network beyond localhost (`-test-network` to allow
it) and no access to `~/.ssh`, `~/.aws`, gh's config and the keychain.

A test that fails is run again at the merge base. One that fails there too
is a low issue; one that only fails with the change, or is new, is a high
issue on the unit it is about, which puts the unit in human review. Go
coverage of a unit's added lines counts as evidence for a clean review
(see [how triage works](how-it-works.md)).

## When the rounds stop

Besides `-rounds`, a fix stops early, keeps what the rounds before did, and
says why in the summary when:

- two rounds in a row fixed nothing and left as much to do;
- a round put the targeted files back the way an earlier round, or the
  start, had them;
- a round's check found more new medium-or-worse issues in what the round
  wrote than it fixed (that round is undone);
- the fix added more than three times what its first round did, and at
  least 200 lines (the trim still runs);
- a round broke tests twice;
- the fix ran 30 minutes (the round running finishes);
- the PR's fixes, one continuing another, have run 10 rounds. The UI asks
  before going on.

## Output and exit codes

The report goes to stdout (`-o FILE` to write it to a file, `-out json` for
JSON with the fix summary: rounds, where the changes are, issues before and
after). Progress and the summary go to stderr.

| exit | meaning |
| --- | --- |
| 0 | done, and nothing at `-fail-on` or worse is left |
| 1 | an error: no reviewer, the review or the first fix round failed |
| 3 | issues at `-fail-on` (default medium) or worse are left after fixing |

`-fail-on off` always exits 0 once the fix ran. In GitHub Actions the report
is also added to the job summary (`$GITHUB_STEP_SUMMARY`).

## In CI

A GitHub Actions job that fixes each PR and pushes the fixes to its branch:

```yaml
name: Review and fix
on:
  pull_request:
permissions:
  contents: write
jobs:
  fix:
    # A fork's branch can't be pushed to, and its runs get no secrets.
    if: github.event.pull_request.head.repo.full_name == github.repository
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.head_ref }}   # a branch, not the detached merge commit
          fetch-depth: 0                # the base, to diff from
      - name: Install pr-manager
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          gh release download --repo amitbet/pr-manager --pattern 'pr-manager_*_linux_amd64.tar.gz' --output - | tar -xz -C "$RUNNER_TEMP" pr-manager
          echo "$RUNNER_TEMP" >> "$GITHUB_PATH"
      - name: Review and fix
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          pr-manager fix -base "origin/${{ github.base_ref }}" -summarizer claude-api -codemap off -commit -fail-on high
      - name: Push the fixes
        if: ${{ !cancelled() }}
        run: git push origin "HEAD:${{ github.head_ref }}"
```

- The push runs even when issues are left (exit 3), so the fixes that did land
  are kept; the job still fails.
- A push made with `GITHUB_TOKEN` does not start another workflow run, so the
  job does not review its own fixes again. With another token it does, so
  fix commits carry a `Fixed-by: pr-manager` trailer, and a branch whose
  last two commits have it is only reviewed, not fixed again, until someone
  pushes a commit of their own.
- Add `-tests on` to run the repository's tests during the review and after
  every round (the runner has the toolchain your other jobs set up).
- `-codemap off` skips building a code map on the runner. Impact scores then
  come from the review alone; point `-codemap` at a map you cache or build to
  keep them.
- Any provider works: `OPENAI_API_KEY` with `-summarizer openai-api`, or the
  cloud providers (Bedrock, Vertex, Foundry, Azure OpenAI) configured through
  their usual environment.
- Pin a version with `gh release download v0.1.29 ...` for repeatable runs.

To gate a PR without changing it, run the plain review instead:
`pr-manager -base "origin/${{ github.base_ref }}" -fail-on-human`.
