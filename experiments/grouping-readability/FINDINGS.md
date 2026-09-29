# Readability of grouped review steps

Question: if a group of changes is shown to a person as one review step,
how readable is it? Hypothesis under test: one file per step reads best,
especially when the changed definitions it uses from other files are
included.

No model calls and no people. Six PRs (the five from `../grouping-tuning`
plus perfectscale/psc-coroot-node-agent#43), 233 declaration units, 4,900
changed lines. `PR_READABILITY=1 go test ./triage/ -run
TestGroupingReadability` writes `results/steps.json` in under a second;
`analyze.py` scores it.

## Strategies

| strategy | a step is |
| --- | --- |
| decl (today) | one declaration unit, as the walkthrough does now |
| file | every changed unit in one file |
| file-300 | a file, split into runs of adjacent units of at most 300 changed lines |
| file+types | a file, plus the changed type/var/const/class definitions it uses from elsewhere |
| file+defs | a file, plus every changed definition it uses from elsewhere |
| link (shipped) | the review grouping in `triage/group.go` |
| link-seams-300 | the same links, clustered with no cap, then split at the weakest link until parts are at most 300 lines |
| +defs | any of the above, plus the definitions its members use |

Included definitions are shown for reference; they remain reviewed in their
own step, so the `+defs` strategies overlap.

## Structure

| strategy | steps | median lines | steps < 10 lines | steps > 400 | files/step | closure | self-contained |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| decl (today) | 233 | 8 | 134 | 0 | 1.00 | 0% | 30% |
| file | 45 | 57 | 8 | 2 | 1.00 | 36% | 42% |
| file+types | 45 | 57 | 8 | 2 | 1.00 | 49% | 53% |
| file+defs | 45 | 62 | 7 | 5 | 1.00 | 100% | 89% |
| link (shipped) | 62 | 51 | 10 | 0 | 1.47 | 34% | 37% |
| link-seams-300 | 53 | 52 | 12 | 0 | 1.62 | 42% | 49% |
| link-seams-300+defs | 53 | 137 | 12 | 4 | 1.62 | 100% | 92% |

*Closure*: of the changed declarations a step's code names, the share the
step also shows. *Self-contained*: nothing to look up elsewhere and at most
400 changed lines, the size past which review effectiveness falls in the
SmartBear/Cisco study.

Closure is 100% for the `+defs` strategies by construction, so that column
is not evidence for them. What the exploration measures is what inclusion
costs: 38% more lines to read for `file+defs` (69% for
`link-seams-300+defs`), and more oversized steps.

## Reading effort

Reviewer-minutes per PR, relative to today's walkthrough: reading at 6
changed lines a minute, 0.25 min to orient on each step, 0.1 min per extra
file within a step, 1 min to look up a definition that is in another step,
and lines past 400 in one step read 1.5x slower. Across 486 rankings that
vary every one of those assumptions, today's walkthrough never wins and has
a mean rank of 7.5 of 9. Any grouping is a large improvement.

Whether including definitions pays off depends on two numbers only a
reviewer can calibrate: what a lookup costs, and how cheaply an included
definition is skimmed.

| lookup cost | included read at | file | file+defs | link-seams-300 | link-seams-300+defs |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 min | 100% | 80% | 94% | **76%** | 98% |
| 1 min | 25% | 80% | 77% | 76% | **76%** |
| 2 min | 50% | 71% | **65%** | 65% | 66% |
| 2 min | 25% | 71% | 61% | 65% | **60%** |
| 3 min | 25% | 65% | 51% | 59% | **50%** |

Cheap lookups and definitions read in full: don't include them. Costly
lookups and definitions skimmed: include them. The walkthrough has no jump
to definition, so a lookup means leaving the step, and an included
definition shown collapsed is mostly a signature. That puts it, on my
assumption, in the lower rows, where inclusion takes effort to 50-65% of
today.

`file+defs` and `link-seams-300+defs` are within a point or two of each
other there. The file-based one is simpler, keeps every step to one file,
and on #43 reads as caller, feature, tests:

1. `registry.go`: 12 lines, with the three systemd helpers it calls shown
   for reference.
2. `systemd.go`: 135 lines, the whole DbusClient error cache, nothing to
   look up.
3. `systemd_test.go`: 168 lines, with the definitions it exercises.

The link-based cut of the same PR puts `getOrCreateContainer` and the test
file in one step and `getContainerMetadata` with the DbusClient block in
the other.

## Where one file per step breaks

Large test files. Prometheus `db_test.go` is one 1,150-line step of 19
independent tests; Kubernetes `quantity_test.go` is 534 lines of 18. Tests
rarely depend on one another, so a test file should split per test, each
test carrying the definitions it exercises.

Included definitions can also outweigh the step's own change
(`compact_test.go`: 144 own lines, 270 included). Shown collapsed, that
costs little; shown in full, it is what makes inclusion lose in the upper
rows above.

## Limits

Structural proxies and a cost model, no reviewers. The lookup cost and skim
rate decide the inclusion question and are assumptions. References are
matched by name, so they miss indirect ones (a function passed as a value,
as in the Prometheus pilot) and can over-match common names. Step order,
within and between steps, is not evaluated. Six PRs.
