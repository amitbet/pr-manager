# Hunk grouping pilot

Selected public PR: https://github.com/prometheus/prometheus/pull/18905

The merged PR has 1,820 additions and 348 deletions across 11 files. The experiment uses the earlier reviewed head `48256385413290df5a2d566d0090a430a457820d` and its merge base `a9564ef219fef314447fc6026443c4da66fdab72`, before four confirmed review fixes. The historical diff is smaller than the merged diff; report its own counts.

## Fixed conditions

- Reuse current `triage.BuildUnits`, default policy, and production review prompt assembly.
- Preserve all reviewable units; only the existing presorter can skip units. Force the remaining units to human review to keep classification/selection from confounding grouping.
- Reviewer and grouper: `claude-haiku-4-5` through the project's existing Claude CLI adapter, with tools disabled. Same review system prompt and output schema in all arms.
- No PR title/body, review comments, later fixes, or quality labels in the grouping or review prompts. Input is pinned code diff, unit IDs, and production context notes.
- Review once per focus group. No critic pass in this pilot; save raw issues and manually assess evidence. The normal application critic and code-map ranking are not evaluated here.
- One run per arm, 4 concurrent requests, interleaved by arm. Record usage and service seconds. Service-time sums are work proxies, not wall time for independent runs.

## Arms

1. Current declaration units: exact captured production review requests, default 24,000-byte unit limit and 32,000-byte related-diff context budget.
2. Deterministic grouping: greedily merge symbol-reference/test-name links, plus pairs of small changes in the same file. Maximum 12,000 diff bytes per group.
3. Model grouping: one model call partitions the same unit IDs into coherent review groups, maximum 12,000 diff bytes per group. Validate exact partition and size before reviewing.

Both grouped arms construct context by taking the best position in each member's production context ordering, then filling the same 32,000-byte related-diff budget. Remaining units are listed as omitted. Thus this tests grouping plus the minimal context-union policy needed to review groups, not a pure parser change. Group titles/reasons are withheld from review prompts to avoid giving only the model arm extra semantic hints.

## Predeclared checks

Validate every non-skipped unit occurs exactly once, no invented IDs, no oversized groups, no truncated focus diffs, and changed-line preservation. Record grouping call cost separately and include it in model-arm totals.

Compare prompt characters, reported input/output tokens, service seconds, review-task count, summary/focus word counts, and duplicate findings. Human review time is not measured; task and reading counts are only proxies.

Four reference defects, from public review comments at this historical revision and author-confirmed fixes:

- In-flight appender: `lastAppendID()` includes already-open appenders; the strict watermark check can evict a concurrently committed sample excluded from the written block.
- Single-timestamp head: inclusive write loop combined with `mint >= maxt` eviction guard fails to evict a single-timestamp head.
- Unsorted refs: the public method passes unsorted caller refs into sorted-postings machinery.
- Duplicate refs: duplicated refs reach block writing and can cause an out-of-order-series error.

Count a recovery only if an issue identifies the mechanism and a compatible concrete consequence. Generic concurrency/input-validation warnings do not count. Report raw issue recovery separately from whether the model marks the issue introduced and supported by visible code. Grouping adds no source access, so unchanged dependencies absent from the diffs can limit recovery in every arm. Nonmatching findings are not automatically false positives; audit individually or label unverified.

One PR, one model, and one stochastic sample cannot establish general recall, latency, or human productivity improvements. Reviewer comments are an incomplete reference set. Public historical code may have appeared in model training.
