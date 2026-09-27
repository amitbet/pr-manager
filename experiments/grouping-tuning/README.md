# Grouping parameter sweep

Tunes the review-grouping rule shipped in `triage/group.go`. The pilot in
`../hunk-grouping-prometheus-18905` established that grouping is a cost
measure and not a recall measure; this asks what the caps should be.

## PRs

Five merged public PRs, fetched at their merge base so the diff is what the
PR actually changed (`fetch.py`, cached under `data/`).

| PR | language | units | strong links |
| --- | --- | ---: | ---: |
| prometheus/prometheus#18905 | Go | 71 | 183 |
| grafana/grafana#133630 | Go | 36 | 56 |
| kubernetes/kubernetes#142277 | Go | 25 | 47 |
| elastic/elasticsearch#160261 | Java | 76 | 148 |
| vercel/next.js#99189 | TS | 9 | 2 |

217 review units. A *strong link* is a pair where one unit names the other's
symbol, or one is a test named after the other: the pairs the rule is trying
to keep together. It is a yardstick for what the rule does, not a quality
score — the pilot showed co-location does not improve defect recovery.

## Fixed conditions

Rule skips (generated files, formatting) are dropped and every remaining
unit is forced to human review, so classification and the review budget
cannot confound the grouping measurement. Prompt assembly, context ranking
and the 32,000-character related-diff budget are the production ones.

## Phase 1: structural sweep (`TestGroupingSweep`, no model calls)

Prompt assembly is deterministic, so cost can be measured exactly without
paying for a review. Sweeps `max_chars` × `max_members` plus link-weight
variants, and records review calls, total prompt characters, group size
distribution and link preservation. `PR_TUNING=1 go test ./triage/ -run
TestGroupingSweep`. Output: `results/sweep.json`, read by `analyze.py`.

## Phase 2: finalist validation (`TestGroupingValidate`, model calls)

Runs five configurations through the real reviewer (`claude-haiku-4-5` via
the project's Claude CLI adapter), including ungrouped review as the
reference. Every call is cached by request hash so the run can be resumed
and re-analysed without paying again.

What phase 1 cannot see is whether large groups degrade the review. Phase 2
measures the failure modes directly:

- units left unreviewed,
- fallback calls, which happen when a group reply skips a member,
- failed or truncated calls,
- findings the ungrouped run made that a grouped run did not, matched on the
  anchor unit plus a fuzzy title match.

`PR_TUNING_LLM=1 go test ./triage/ -run TestGroupingValidate -timeout 4h`.
Output: `results/validate.json`, read by `analyze_validate.py`.

## Limits

Five PRs, one model, one sample per configuration. Findings agreement is a
noisy metric: the reviewer is stochastic, so a finding missing from one arm
is not proof that the configuration caused it. Treat the cost numbers as
solid (they are deterministic) and the quality numbers as directional.
