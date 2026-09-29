"""Score groupings by how readable each step is when shown as one unit.

Structural metrics, then a reading-effort model in reviewer-minutes, then
a check that the ranking survives changing the model's assumptions.
"""
import collections, itertools, json, math, pathlib, statistics

ROOT = pathlib.Path(__file__).resolve().parent
rows = json.loads((ROOT / "results/steps.json").read_text())
strategies = list(dict.fromkeys(r["strategy"] for r in rows))
prs = list(dict.fromkeys(r["pr"] for r in rows))
BIG, TINY = 400, 10  # SmartBear/Cisco: review effectiveness falls past ~400 LOC


def effort(steps, rate=6, jump=1.0, step=0.25, switch=0.1, fatigue=1.5):
    """Reviewer-minutes to read a PR as these steps.

    rate: changed lines per minute (SmartBear: 300-500 LOC/hour is 5-8).
    jump: minutes to find, read and come back from a definition in
          another step. step: orienting on a new step. switch: moving to
          another file inside a step. fatigue: lines past BIG in one step
          read that much slower.
    """
    t = 0.0
    for s in steps:
        c = s["changed"]
        t += step + (min(c, BIG) + fatigue * max(0, c - BIG)) / rate
        t += switch * max(0, s["member_files"] - 1) + jump * s["dangling"]
    return t


by = collections.defaultdict(dict)
for r in rows:
    by[r["strategy"]][r["pr"]] = r

print(f"{len(prs)} PRs, {sum(by[strategies[0]][p]['total_changed'] for p in prs)} changed lines, "
      f"{sum(len(by[strategies[0]][p]['steps']) for p in prs)} declaration units\n")
hdr = (f"{'strategy':<21}{'steps':>6}{'med':>6}{'p90':>6}{'max':>6}{'>400':>6}{'<10':>6}"
       f"{'files':>7}{'closure':>9}{'dangl':>7}{'dup':>6}{'self-cont':>10}{'effort':>9}")
print(hdr)
print("-" * len(hdr))
summary = {}
for s in strategies:
    steps = [st for p in prs for st in by[s][p]["steps"]]
    ch = sorted(st["changed"] for st in steps)
    tgt = sum(st["targets"] for st in steps)
    dang = sum(st["dangling"] for st in steps)
    dup = sum(st["changed"] for st in steps) / sum(by[s][p]["total_changed"] for p in prs)
    selfc = sum(1 for st in steps if st["dangling"] == 0 and st["changed"] <= BIG) / len(steps)
    # effort relative to today's walkthrough, per PR, geometric mean
    rel = [effort(by[s][p]["steps"]) / effort(by[strategies[0]][p]["steps"]) for p in prs]
    gm = math.exp(sum(math.log(x) for x in rel) / len(rel))
    summary[s] = gm
    print(f"{s:<21}{len(steps):>6}{statistics.median(ch):>6.0f}{ch[int(.9*(len(ch)-1))]:>6}{ch[-1]:>6}"
          f"{sum(1 for c in ch if c > BIG):>6}{sum(1 for c in ch if c < TINY):>6}"
          f"{statistics.mean(st['member_files'] for st in steps):>7.2f}"
          f"{(1 - dang / tgt if tgt else 1):>8.0%}{dang:>7}{dup:>6.2f}{selfc:>9.0%}{gm:>8.0%}")

print("\nsteps: review steps | med/p90/max: changed lines per step | >400 / <10: oversized / trivial steps")
print("files: files a step's own changes span | closure: changed declarations a step uses that it also shows")
print("dangl: references a reader must leave the step for | dup: lines read / lines changed")
print("self-cont: steps with nothing dangling and <= 400 lines | effort: reviewer-minutes vs today (geo mean)")

# Does the ranking survive different assumptions?
grid = list(itertools.product([4, 6, 10], [0.5, 1.0, 2.0], [0.1, 0.25, 0.5], [1.0, 1.5, 2.0]))
wins = collections.Counter()
ranks = collections.defaultdict(list)
for rate, jump, step, fat in grid:
    for p in prs:
        e = {s: effort(by[s][p]["steps"], rate, jump, step, 0.1, fat) for s in strategies}
        order = sorted(strategies, key=e.get)
        wins[order[0]] += 1
        for i, s in enumerate(order):
            ranks[s].append(i + 1)
n = len(grid) * len(prs)
print(f"\nSensitivity: {len(grid)} assumption sets x {len(prs)} PRs = {n} rankings "
      f"(rate 4-10 lines/min, jump 0.5-2 min, step 0.1-0.5 min, fatigue 1-2x)")
print(f"{'strategy':<21}{'wins':>7}{'mean rank':>11}{'worst':>7}")
for s in sorted(strategies, key=lambda s: statistics.mean(ranks[s])):
    print(f"{s:<21}{wins[s]:>6.0%}{statistics.mean(ranks[s]):>11.2f}{max(ranks[s]):>7}".replace(
        f"{wins[s]:>6.0%}", f"{wins[s]/n:>6.0%}"))
