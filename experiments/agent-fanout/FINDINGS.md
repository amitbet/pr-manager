# Agent fan-out POC: results

Question: should the analyze step run one Claude Code agent that hands the
review tasks to subagents, instead of one `claude -p` per review group?

Harness: `triage/fanout_poc_test.go` (`PR_FANOUT_POC=1`). Four PRs from
`../grouping-tuning`, production grouping and prompts, every unit forced
to review. Reviewer is `claude-sonnet-5-5` in every arm; the subagents
use the `sonnet` alias, which resolved to the same model. In the agent
arms Go writes each group's prompt and schema to `tasks/T<n>.md`, one
agent run launches the subagents, each writes `out/T<n>.json`, and Go
decodes the files exactly as it decodes a normal reply. A missing file
falls back to a normal call. One sample per arm.

## Arms

| arm | how |
| --- | --- |
| baseline, baseline-b | today: one `claude -p` per group, `-j 8` |
| agent-1 | Haiku 4.5 orchestrator, one task per subagent |
| agent-4 | Haiku 4.5 orchestrator, four tasks per subagent |
| agent-1-sonnet | Sonnet 5.5 orchestrator, one task per subagent |
| agent-1-opus | Opus 5.5 orchestrator, one task per subagent |

## Wall time and cost

| arm | next.js (9 units) | kubernetes (25) | grafana (36) | prometheus (71) |
| --- | --- | --- | --- | --- |
| baseline | 13s $0.55 | 16s $0.74 | 24s $0.88 | 33s $2.48 |
| baseline-b | 12s $0.55 | 22s $0.76 | 22s $0.88 | |
| agent-1-sonnet | 27s $0.63 | 32s $0.77 | 37s $0.95 | 42s $2.29 |
| agent-1-opus | 34s $0.68 | 44s $0.84 | 85s $1.28 | 56s $2.49 |
| agent-1 | 25s $0.59 | 63s $0.78 | 70s $1.02 | |
| agent-4 | 30s $0.57 | 48s $0.74 | 81s $1.06 | |

Cost is `total_cost_usd` from the CLI's last result event per run (with
subagents the CLI prints one cumulative result event each time the main
loop resumes).

## What it shows

1. Reliability is not the problem. Every arm reviewed every unit with no
   fallbacks; file handoff between subagents and Go works.
2. The agent is slower in every case: 1.3-2.5x with the best orchestrator
   (Sonnet). The orchestrator's turns sit before and after the parallel
   reviews, and Go already runs the reviews 8 at a time.
3. It does not save tokens. Each subagent carries Claude Code's
   general-purpose agent prompt and tool definitions (cache reads go from
   ~25k to 330k-1.4M per PR) and spends output tokens on Read/Write calls.
   The baseline replaces the system prompt with its own, so each call is
   lean. Cost is equal to 15% higher, except prometheus at -8%.
4. Orchestrator model: Sonnet 5.5 is the fastest. Haiku is slower (more
   turns), Opus adds $0.14-0.40 per PR and is slower again. Packing four
   tasks per subagent runs them one after another and only adds time.
5. Quality could not be compared. These PRs drew 0-1 issues per arm. On
   prometheus the baseline reviewer raised two low issues that the critic
   rejected; the agent arms raised none before the critic.

## Side finding

Every `claude -p` process also bills a small `claude-haiku-4-5-20251001`
call of Claude Code's own, about $0.015 each: $0.37 of the $2.48
baseline on prometheus (15%), $0.08-0.13 on the others. The agent arms
pay it once. Turning that background call off for the review processes
would cut more than the agent design saves.

## Verdict

Keep the Go fan-out. If the agent design is revisited, the configuration
to start from is a Sonnet 5.5 orchestrator with one task per subagent,
and its case would have to be cross-unit reasoning, not speed or cost.
