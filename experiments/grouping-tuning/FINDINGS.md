# Grouping parameter sweep: results

Five merged public PRs (Go ×3, Java, TypeScript), 217 review units. Phase 1
is deterministic. Phase 2 is one run per configuration with
`claude-haiku-4-5`; 425 review calls, 6.9 hours of summed model time. See
README.md for the protocol.

## Outcome

Default changed from `max_chars: 12000` alone to `max_chars: 12000` plus
`max_members: 8`.

## Phase 1: cost is deterministic, and it saturates

| `max_chars` | `max_members` | calls | prompt cut | biggest group |
| ---: | ---: | ---: | ---: | ---: |
| 6,000 | 6 | 74 | 65% | 6 |
| 8,000 | 8 | 62 | 70% | 8 |
| 12,000 | 8 | 60 | 71% | 8 |
| 12,000 | – | 48 | 77% | 23 |
| 16,000 | – | 41 | 80% | 31 |
| 24,000 | – | 33 | 83% | 46 |
| 32,000 | – | 26 | 86% | 62 |

Past 12,000 characters the savings curve flattens while group size runs
away: 16,000 buys three more points and adds eight units to the worst group.

The win tracks how much internal structure a PR has, and is not uniform.
Prompt cut per PR at `12000/–`: Elasticsearch 88%, Kubernetes 79%, Grafana
73%, Prometheus 71%, next.js **11%**. next.js#99189 has 9 units and 2 strong
links, so there is nothing to group. Grouping pays off in proportion to a
PR's internal structure.

### Link weights

| variant at `chars=12000` | calls | prompt cut | strong links kept |
| --- | ---: | ---: | ---: |
| default | 48 | 77% | 50% |
| no test bonus | 47 | 77% | 50% |
| no same-file bonus | 70 | 66% | 48% |
| references only | 72 | 65% | 49% |
| wider same-file (6,000) | 43 | 79% | 53% |

The test-name bonus is redundant: removing it changes nothing, because a
changed test almost always names the symbol it exercises and the reference
link already fires. It is kept because it costs nothing and covers tests
that reach their subject indirectly.

The same-file bonus does most of the merging (77% → 66% without it) while
barely moving link preservation, so it is a cost lever rather than a
relatedness signal. Widening it to 6,000 characters looked strictly better
at `chars=6000` (73% vs 71% cut with a *smaller* worst group, 12 vs 17,
because filling groups early prevents one runaway chain). It was not carried
into phase 2 and has not been validated, so the shipped value stays 1,500.

## Phase 2: the member cap is what protects the review

| arm | calls | call cut | input tokens | token cut | unreviewed | fallbacks | errors | issues | high |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| ungrouped | 217 | – | 3,842,157 | – | 0 | 0 | 0 | 5 | 2 |
| `6000/6` | 74 | 66% | 1,537,969 | 60% | 0 | 0 | 0 | 6 | 3 |
| **`12000/8`** | **60** | **72%** | **1,195,479** | **69%** | **0** | **0** | **0** | **4** | **3** |
| `12000/–` | 48 | 78% | 856,726 | 78% | 0 | 0 | 0 | 1 | 0 |
| `32000/–` | 26 | 88% | 656,770 | 83% | 0 | 1 | 0 | 2 | 1 |

Coverage held everywhere: every one of the 217 units was reviewed in every
arm, and no call failed.

Two things separate the capped arms from the uncapped ones:

1. **High-severity findings.** Both capped arms reported 3, above ungrouped
   review's 2. The uncapped arms reported 0 and 1. The pilot saw the same
   thing independently: its deterministic arm, whose largest group was 16
   units, reported 2 issues against ungrouped review's 8.
2. **Dropped members.** The only fallback in the whole run was at
   `32000/–`, on the Prometheus PR, where 71 units became 8 groups and the
   reply came back missing a member. Coverage survived because the fallback
   re-reviews it alone, but it marks where the reply stops being reliable.

`12000/8` costs 9 points of token saving against uncapped and buys back the
findings. That is the shipped default.

## What these numbers cannot support

The findings counts are tiny: 1 to 6 per arm over 217 units. The reviewer is
also not reproducible at this rate — only 40% of ungrouped review's findings
reappear in the best grouped arm, and each arm surfaces defects the others
miss. Ungrouped review found an integer overflow in
`AllocationEstimators.bitSetWords` that no grouped arm found; `6000/6` found
a watch-cancellation bug in Grafana's `reconcile` that ungrouped review
missed.

So the high-severity split (3, 3 against 0, 1) is directional, not
significant. It is adopted because it agrees with the pilot's independent
observation and because the cap costs little: 9 points of a 78% saving, to
avoid a review whose group has 23 units. The cost numbers, by contrast, are
deterministic and hold.
