# Review and fix from the command line

`pr-manager fix` runs the UI's **Fix all issues** without the UI: it reviews
the change, fixes every issue the review found, checks the fixed code, and
fixes what the check still finds, for up to `-rounds` rounds (3 by default).
The rounds follow the same rules as the UI: an issue a round worked on that
the check still finds gets another try if the patch changed its code (not if
the fixer left it alone); a new issue only at medium or worse;
an issue a check had found fixed that comes back is left for you. Then the
fixed code is triaged once and the report printed.

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
  job does not review its own fixes again.
- `-codemap off` skips building a code map on the runner. Impact scores then
  come from the review alone; point `-codemap` at a map you cache or build to
  keep them.
- Any provider works: `OPENAI_API_KEY` with `-summarizer openai-api`, or the
  cloud providers (Bedrock, Vertex, Foundry, Azure OpenAI) configured through
  their usual environment.
- Pin a version with `gh release download v0.1.29 ...` for repeatable runs.

To gate a PR without changing it, run the plain review instead:
`pr-manager -base "origin/${{ github.base_ref }}" -fail-on-human`.
