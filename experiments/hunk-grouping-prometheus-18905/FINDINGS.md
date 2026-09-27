# Hunk grouping pilot: results

One PR (prometheus/prometheus#18905 at the reviewed head `4825638`), one model
(`claude-haiku-4-5`), one run per arm. Everything below is a measurement on that
single sample. See PROTOCOL.md for the predeclared conditions and checks.

## What was run

59 reviewable declaration units, 72,940 diff bytes. Five arms, same reviewer,
same review prompt and schema, same 32,000-byte related-diff context budget.

| arm | grouping method | tasks |
| --- | --- | --- |
| baseline | current declaration units, captured production requests | 59 |
| deterministic | greedy symbol-reference and test-name links, 12 KB cap | 12 |
| model | one model call, "group into coherent review tasks" | 21 |
| linked | one model call, instruction rewritten around defect surfaces | 20 |
| oracle | reference-defect partners force-merged using the answer key | 20 |

The `linked` and `oracle` arms were added after the first three returned the
result in the next section. `oracle` is a ceiling measurement, not a deployable
strategy: it is built from the known answers.

## Result 1: grouping is a large, real cost win

| arm | tasks | input tokens | output tokens | service min | reviewer words |
| --- | ---: | ---: | ---: | ---: | ---: |
| baseline | 59 | 932,129 | 392,432 | 61.3 | 5,837 |
| deterministic | 12 | 234,373 | 142,105 | 22.5 | 1,453 |
| model | 21 | 494,120 | 217,271 | 31.9 | 2,283 |
| linked | 20 | 388,936 | 229,828 | 33.3 | 2,281 |
| oracle | 20 | 355,633 | 200,689 | 29.8 | 2,384 |

Model-arm totals include the grouping call. Service seconds are a work proxy
summed across concurrent requests, not wall time.

Against baseline: deterministic cuts tasks 80%, input tokens 75%, reviewer
reading 76%. The model grouper costs more and produces a worse reduction than
the deterministic rule, and it adds a 73-second serial call on the critical path.

Human review time was not measured. Task and word counts are proxies for it.

## Result 2: grouping did not change defect recovery at all

Four reference defects, from public review comments at this revision and
author-confirmed fixes (`data/reference-defects.json`). Scored by hand under the
PROTOCOL rule: mechanism plus a compatible concrete consequence.

| arm | defect pairs co-located | defects recovered |
| --- | ---: | ---: |
| baseline | 0 / 4 | 0 / 4 |
| deterministic | 0 / 4 | 0 / 4 |
| model | 0 / 4 | 0 / 4 |
| linked | 0 / 4 | 0 / 4 |
| oracle | 4 / 4 | 0 / 4 |

Nothing regressed either. Total raw issues stayed in the 2-9 band across arms,
and the distinct-concern counts track it: 7 for baseline, 2 deterministic,
7 model, 6 linked, 8 oracle.

Near misses, all scored as non-recoveries:

- `baseline-007-0`, `model-004-0`, `oracle-003-0` reach the right consequence for
  the in-flight appender defect, a series evicted while holding samples above the
  watermark, through the wrong mechanism: a missing series lock rather than
  `lastAppendID()` counting already-open appenders plus the strict `>`.
- `oracle-007-1` reasons about the correct write-to-evict window but inverts the
  outcome, describing a sample that is in the block rather than in neither the
  block nor the head.

## Result 3: no grouping strategy co-located the defect pairs, and the cap is not why

Every pair fits inside the 12 KB cap with room to spare:

| pair | bytes | cap |
| --- | ---: | ---: |
| `compactHeadViewLocked` + `hasAppendIDAbove` | 3,693 | 12,000 |
| `compactHeadViewLocked` + `truncateSeries` | 4,512 | 12,000 |
| `compactHeadViewLocked` + late-append test | 7,871 | 12,000 |
| `CompactSelectedSeries` + `filterSeriesAndSortPostings` | 7,708 | 12,000 |

The first three share a member, and all four of those units together are 10,048
bytes, so a single valid partition scores 4/4. The oracle arm is that partition.
Nothing forced the pairs apart; every proposable strategy chose to split them.

The model grouper split them because it built a taxonomy: "Type Definitions and
Basic Helper Functions", "Block Metadata Tests", "Compaction Benchmarks". Tidy by
layer and kind, which is the arrangement that hides a defect spanning a producer
and its consumer.

## Result 4: instructing the model to co-locate changed its reasoning, not its output

The `linked` arm's grouping prompt names the objective directly: keep producer
with consumer, write with evict, invariant with guard, function with its new
test, and do not group by file, layer or kind.

It worked on reasoning and failed on partitioning. Group reasons mention another
unit's symbol 24 times, against 2 for the original prompt. But 8 of those 24 name
a symbol that is not in the group being described. The clearest case is its
group 15: the late-append test alone, with the reason "verifies that samples
appended between block write and eviction survive via the watermark check in
`hasAppendIDAbove`" - while `hasAppendIDAbove` sits in group 0.

Two causes, and the second is structural:

1. The links that carry these defects are indirect. `compactHeadViewLocked` never
   names `truncateSeries` or `hasAppendIDAbove`; it calls `evict(...)`, a
   function-typed parameter. The implementation is one hop away through a
   function value, the guard two. No textual signal connects them, and all three
   grouping strategies build their links from textual signals.
2. A partition cannot express the dependency graph. `compactHeadViewLocked`
   belongs with three different partners; other units have the same problem.
   Any partition must cut most edges. The right output shape is overlapping
   groups, not a partition.

## Result 5: the binding constraint is reviewer depth, not packaging

This is what the oracle arm settles. Its forced group 18 held all four units for
the in-flight appender defect, and both halves of the single-timestamp defect, in
focus together. It reported zero issues. Its focus list includes:

> Loop bound inclusivity: verify `mint <= maxt` correctly processes the final
> chunk range when MaxTime aligns with chunk boundary

The contradicting guard, `if h.MinTime() >= maxt`, was in the same prompt, in
`truncateSeries`. The reviewer named the exact line to check and did not follow
through. Forced group 19 did the same thing: "Verify `compactHeadViewLocked` loop
bound `mint <= maxt` correctly captures samples at chunk-range boundaries", zero
issues.

Perfect co-location produced zero additional recoveries. Co-location is not the
lever.

Separately, three of the four defects are not recoverable from the diffs at all.
They need unchanged source: `lastAppendID()` and `incompleteAppends` semantics in
`isolation.go`, and the sorted-input contract of `index.NewListPostings`. Only
`single_timestamp` is fully visible in the diff, and it is the one the oracle had
perfectly framed and still missed. The realistic ceiling for grouping alone on
this PR was 1 of 4; the achieved result was 0.

10 of the 26 issues across the first four arms set `depends_on_unseen_code`,
including every near miss. The reviewer reports being blocked by missing source.

## Result 6: grouping does not reduce duplicate findings, and adds a new kind

| arm | issues | distinct concerns | duplicates |
| --- | ---: | ---: | ---: |
| baseline | 8 | 7 | 1 |
| deterministic | 2 | 2 | 0 |
| model | 9 | 7 | 2 |
| linked | 7 | 6 | 1 |
| oracle | 8 | 7 | 1 |

Baseline's duplicate is the expected kind: the same global-callback race reported
from two units. `model-018-0` and `model-018-1` are a new kind, the same finding
emitted twice inside one group, once per test member.

## What this does and does not support

Supported on this sample: grouping is a large cost and reading-volume win;
deterministic grouping beats model grouping on both cost and simplicity; neither
helps nor hurts defect recovery; co-location of defect surfaces does not improve
recovery even when perfect.

Not supported: any claim about general recall, latency, or human productivity.
One PR, one model, one stochastic sample, an incomplete reference set of four
reviewer comments, and public historical code that may be in training data. The
recovery comparison is 0 against 0, which cannot distinguish "grouping does not
help recall" from "this reviewer configuration finds none of these defects".
