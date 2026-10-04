# Default reviewer: Opus 5.5 medium vs Sonnet 5.5 medium vs Opus 5.5 low

`run.sh NAME MODEL EFFORT` runs `pr-manager eval` over `fixtures/` with
that reviewer on Claude Code: the full production pipeline (grouping,
critic, dedupe, review tools on the fixture's `head_dir`), code map off.
`shim/claude` keeps each CLI run's stream under `logs/` for cost, tokens
and the reviewer's raw answers. Two runs per config.

Fixtures:

- `prometheus-18905`: prometheus/prometheus#18905 at `4825638`, the
  revision reviewers commented on, with four reference defects from
  `../hunk-grouping-prometheus-18905/data/reference-defects.json`, scored
  by hand: in-flight appender (IA), single timestamp (ST), unsorted refs
  (UR), duplicate refs (DR).
- `inventory-error-cache`: the existing eval case, one `must_find` and two
  `must_not_find` rules.

## Results

Wall time is both fixtures in one eval, run 1 / run 2. Cost is run 1,
where every config started on a cold cache; run 2 reused cache from run 1
and cost $2.16 / $0.55 / $0.91.

| config | wall | cost (cold) | output tokens | tool calls | inventory | prometheus, raised by reviewer | prometheus, in final issues |
| --- | --- | ---: | ---: | ---: | --- | --- | --- |
| Opus 5.5 medium (today) | 114s / 109s | $4.54 | 35k | 53 / 52 | found, no FP | IA, DR, UR / IA, DR, UR | none / none |
| Sonnet 5.5 medium | 47s / 52s | $1.81 | 18k | 5 / 6 | found, no FP | none / none | none / none |
| Opus 5.5 low | 75s / 59s | $3.75 | 18k | 0 / 0 | found, no FP | DR, UR / DR+UR | DR / DR+UR |

ST was found by no one. UR is a partial match everywhere: the reviewers
see that unsorted refs break the postings' sort order but name `Intersect`
as the consequence, and the critic rejected it when it stood alone.

## What it shows

1. Opus medium is the only config that finds the in-flight appender race,
   the hardest of the four (the earlier Haiku pilot found 0/4), and it
   found it in both runs.
2. Its findings do not reach the Issues tab. Opus medium marked IA and DR
   `depends_on_unseen_code: true`, which turns them into "unverified:"
   items under *what to check*. The fixture's `head_dir` has only the
   changed files, so the index writer the claims depend on really is
   unseen here; on a full worktree it would be readable. Opus low marked
   DR as seen and kept it as an issue.
3. Opus low does not use the review tools at all (0 calls in both runs),
   so `-review-tools` does nothing at that effort. It is about 40% faster
   than medium and makes half the output tokens; cold cost is 17% lower.
4. Sonnet medium is the fastest and cheapest (60% less than Opus medium)
   but raised none of the four prometheus defects in either run.
5. All three find the inventory defect at the right severity and avoid
   both traps; that case does not separate them.

## Verdict

Keep Opus 5.5 medium as the default: it is the only config that finds the
hard defect, consistently. Sonnet medium is not a substitute for review
quality. Opus low is a reasonable "fast" choice, not a default.

Two runs, one PR with known defects: recall numbers are directional.

Follow-up: rerun Opus medium with a full prometheus checkout at
`4825638` as `head_dir`. If it then marks IA and DR as seen, they reach the
Issues tab; if not, the `depends_on_unseen_code` demotion is hiding real
defects and is worth a look on its own.
