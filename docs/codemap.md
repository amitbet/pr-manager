# Code map

The code map is a CodeRank + rollback-difficulty index of your repos, built by `pr-manager index` / `pr-manager codemap` or the development wrapper `cmd/codemap`. See [codemap/README.md](../codemap/README.md) for the metrics and file format.

For development, `make codemap` indexes a workspace (a directory containing `code/<repo>`) into `.cache/map`:

```sh
make codemap WORKSPACE=~/ws                  # refresh (re-extracts only repos that changed)
make codemap WORKSPACE=~/ws MAP_REPO=api     # force one repo
make codemap-rank CODEMAP_CONFIG=my.yaml     # re-score after editing a scoring config
make codemap-lookup TARGET='api/internal/user/server.go:(*Server).GetProfile'
```

`WORKSPACE` defaults to `PR_MANAGER_WORKSPACE`.

## Indexing your own repos

The map is built from two sources, set with flags, environment variables or the UI's Settings → *Code map*:

- **Local code directory** (`-code-root DIR`, `PR_MANAGER_CODE_ROOT`): the git checkouts in `DIR/<repo>` or `DIR/<org>/<repo>` are symlinked into the workspace and indexed as they are on disk, uncommitted changes included. A checkout is named by its `origin` remote, so a repo cloned into a different directory name still matches its PRs.
- **Org** (`-org`, `PR_MANAGER_ORG`): a GitHub or GitHub Enterprise org or user, as `acme`, `ghe.example.com/platform` or its URL. Every repo you can see, except archived repos and forks, is cloned (blobless) under the cache's `workspace/code`. A repo in the local directory is linked instead of cloned.

```sh
pr-manager index -org acme                       # clone and index every repo of acme
pr-manager index -code-root ~/code               # index the checkouts under ~/code
pr-manager index -org acme -code-root ~/code     # both: local checkouts, clones for the rest
```

`index` pulls the clones it already has, and only re-extracts repos that changed. In the UI, **Index now** does the same and shows how many repos were linked, cloned, updated or failed. A PR from a repo that isn't in the map is added when it is triaged: linked from the local directory if it's there, else cloned. Cross-repo impact (which repos depend on the changed code) only counts repos that are in the map, so index the whole org.

A rebuilt map changes the result cache key, but already-triaged PRs are not re-run. The UI and `prs` reuse the newest cached result for the same PR head until you re-run it (`-force`, or confirm the prompt when you press **Triage** in the UI).
