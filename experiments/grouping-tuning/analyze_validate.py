"""Compare the finalist configurations against ungrouped review.

Cost is review calls and input tokens. The risks grouping could introduce
are measured directly: units left unreviewed, fallback calls caused by a
reply that skipped a member, failed calls, and findings the ungrouped run
made that the grouped run did not.
"""
import collections, difflib, json, pathlib, sys

ROOT = pathlib.Path(__file__).resolve().parent
rows = json.loads((ROOT / "results/validate.json").read_text())
arms = list(dict.fromkeys(r["arm"] for r in rows))
by_arm = collections.defaultdict(list)
for r in rows:
    by_arm[r["arm"]].append(r)

BASE = "off"


import re

STOP = set("the a an of to in for and or is are be with when not no non after before "
           "will may can could should would that this it its on at by from as into".split())


def words(t):
    return {w for w in re.findall(r"[A-Za-z][A-Za-z0-9]+", t.lower()) if w not in STOP and len(w) > 2}


def same(a, b):
    """Two titles describe one finding.

    Wording varies a lot between runs, so this is deliberately loose: a
    strong fuzzy match, or a shared distinctive identifier. It is a
    prescreen; the counts in FINDINGS are adjudicated by hand.
    """
    if difflib.SequenceMatcher(None, a.lower(), b.lower()).ratio() > 0.5:
        return True
    wa, wb = words(a), words(b)
    if not wa or not wb:
        return False
    # A shared long token (an identifier) is strong evidence on its own.
    for w in wa & wb:
        if len(w) >= 8:
            return True
    # Near-identical identifiers, e.g. bitSetGrowBytes / bitSetGrowthBytes.
    for x in wa:
        for y in wb:
            if len(x) >= 8 and len(y) >= 8 and difflib.SequenceMatcher(None, x, y).ratio() > 0.85:
                return True
    return len(wa & wb) / min(len(wa), len(wb)) > 0.55


def findings(rs):
    """Findings as (unit id, title), the anchor plus what was claimed."""
    out = []
    for r in rs:
        for anchor, title in zip(r.get("issue_anchors") or [], r.get("issue_titles") or []):
            out.append((anchor, title))
    return out


base_rows = by_arm.get(BASE, [])
base_find = findings(base_rows)
base_calls = sum(r["review_calls"] for r in base_rows)
base_in = sum(r["input_tokens"] for r in base_rows)

print(f"{len(base_rows)} PRs, ungrouped: {base_calls} review calls, {base_in:,} input tokens, "
      f"{len(base_find)} issues\n")
hdr = (f"{'arm':<22}{'groups':>7}{'calls':>7}{'call-':>7}{'in-tok':>10}{'tok-':>7}"
       f"{'unrev':>7}{'fallbk':>7}{'err':>5}{'iss':>5}{'match':>7}")
print(hdr)
print("-" * len(hdr))
for arm in arms:
    rs = by_arm[arm]
    calls = sum(r["review_calls"] for r in rs)
    intok = sum(r["input_tokens"] for r in rs)
    unrev = sum(len(r.get("units_unreviewed") or []) for r in rs)
    fallback = calls - sum(r["groups"] for r in rs)
    errs = sum(r["errors"] for r in rs)
    find = findings(rs)
    matched = 0
    pool = list(find)
    for anchor, title in base_find:
        for i, (a2, t2) in enumerate(pool):
            if a2.split(":")[0] == anchor.split(":")[0] and same(title, t2):
                matched += 1
                pool.pop(i)
                break
    print(f"{arm:<22}{sum(r['groups'] for r in rs):>7}{calls:>7}"
          f"{1-calls/base_calls:>6.0%} {intok:>10,}{1-intok/base_in:>6.0%}"
          f"{unrev:>7}{fallback:>7}{errs:>5}{len(find):>5}"
          f"{(matched/len(base_find) if base_find else 0):>6.0%}")

print("\nSeverity mix:")
sev = ["critical", "high", "medium", "low"]
print(f"{'arm':<22}" + "".join(f"{s:>10}" for s in sev))
for arm in arms:
    c = collections.Counter()
    for r in by_arm[arm]:
        c.update(r["issues_by_severity"])
    print(f"{arm:<22}" + "".join(f"{c.get(s,0):>10}" for s in sev))

print("\nPer-PR review calls:")
prs = list(dict.fromkeys(r["pr"] for r in rows))
print(f"{'pr':<34}" + "".join(f"{a.replace('chars=','').replace(' members=','/'):>14}" for a in arms))
for pr in prs:
    line = f"{pr:<34}"
    for arm in arms:
        r = next((x for x in by_arm[arm] if x["pr"] == pr), None)
        line += f"{r['review_calls'] if r else 0:>14}"
    print(line)
