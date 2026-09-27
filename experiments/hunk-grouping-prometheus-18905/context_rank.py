"""How often does production context hide a unit the focus unit actually references?

Reference link: the focus diff mentions the other unit's declared symbol by
name. That is a lower bound on real dependency, and it needs no code map.
"""
import json, pathlib, re

ROOT = pathlib.Path(__file__).resolve().parent
jobs = [j for j in json.loads((ROOT / 'results/jobs.json').read_text()) if j['arm'] == 'baseline']
U = {u['id']: u for u in json.loads((ROOT / 'data/units.json').read_text())}
ids = [j['members'][0] for j in jobs]
short = lambda i: U[i]['symbol'].split(' ')[-1].rsplit('.', 1)[-1]

shown_hits = hidden_hits = 0
worst = []
for j in jobs:
    a = j['members'][0]
    p = j['request']['messages'][1]['content']
    body, _, tail = p.partition('Also changed, not shown:')
    shown = set(re.findall(r'^#### (.*?) \(', body, re.M))
    linked = [b for b in ids if b != a and len(short(b)) >= 6
              and re.search(r'\b' + re.escape(short(b)) + r'\b', U[a]['diff'])]
    hid = [b for b in linked if b not in shown]
    shown_hits += len(linked) - len(hid)
    hidden_hits += len(hid)
    if hid:
        worst.append((len(hid), a, [short(b) for b in hid]))

tot = shown_hits + hidden_hits
print(f'referenced units across all 59 baseline prompts: {tot}')
print(f'  diff shown in context : {shown_hits} ({100*shown_hits//max(tot,1)}%)')
print(f'  named but hidden      : {hidden_hits} ({100*hidden_hits//max(tot,1)}%)')
print('\nunits whose prompt hid the most of their own references:')
for n, a, names in sorted(worst, reverse=True)[:8]:
    print(f'  {n}  {a.split(":")[-1][:45]:<45} -> {", ".join(names[:5])}')
