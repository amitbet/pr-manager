"""Aggregate the sweep. Cost is prompt characters and review calls relative
to grouping off; the structural risk is how large the biggest group gets."""
import collections, json, pathlib, statistics

ROOT = pathlib.Path(__file__).resolve().parent
rows = json.loads((ROOT / "results/sweep.json").read_text())
off = {r["pr"]: r for r in rows if r["config"] == "off"}

by = collections.defaultdict(list)
for r in rows:
    if r["config"] != "off":
        by[(r["max_chars"], r["max_members"], r["config"])].append(r)


def agg(rs):
    calls = sum(r["calls"] for r in rs)
    prompt = sum(r["prompt_chars"] for r in rs)
    base_calls = sum(off[r["pr"]]["calls"] for r in rs)
    base_prompt = sum(off[r["pr"]]["prompt_chars"] for r in rs)
    kept = sum(r["strong_links_kept"] for r in rs)
    total = sum(r["strong_links_total"] for r in rs)
    return {
        "calls": calls, "call_cut": 1 - calls / base_calls,
        "prompt_cut": 1 - prompt / base_prompt,
        "largest": max(r["largest_group"] for r in rs),
        "median_largest": statistics.median(r["largest_group"] for r in rs),
        "links": kept / total if total else 0,
        "singleton_frac": sum(r["singletons"] for r in rs) / calls,
    }


print(f"5 PRs, {sum(r['units'] for r in off.values())} review units, "
      f"{sum(r['calls'] for r in off.values())} calls with grouping off\n")

grid = {k: agg(v) for k, v in by.items() if k[2].startswith("chars=") and "members=" in k[2]}
print(f"{'chars':>6} {'memb':>5} {'calls':>6} {'call-':>6} {'prompt-':>8} {'links':>6} {'biggest':>8} {'med.big':>8}")
for (c, m, _), a in sorted(grid.items()):
    print(f"{c:>6} {m if m else '-':>5} {a['calls']:>6} {a['call_cut']:>5.0%} {a['prompt_cut']:>8.0%} "
          f"{a['links']:>6.0%} {a['largest']:>8} {a['median_largest']:>8.0f}")

print("\nWeight variants:")
print(f"{'config':>26} {'calls':>6} {'call-':>6} {'prompt-':>8} {'links':>6} {'biggest':>8}")
for k, v in sorted(by.items()):
    if "members=" in k[2]:
        continue
    a = agg(v)
    print(f"{k[2]:>26} {a['calls']:>6} {a['call_cut']:>5.0%} {a['prompt_cut']:>8.0%} {a['links']:>6.0%} {a['largest']:>8}")
