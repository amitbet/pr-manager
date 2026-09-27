"""Arm 5: oracle grouping. Ceiling measurement, not a deployable strategy.

Arms 2-4 all co-located 0 of the 4 reference defect pairs, though every pair
fits the cap. This arm forces the pairs together using the answer key, so the
reviewer gets the best case a perfect grouper could hand it. If recovery does
not improve here, co-location is not the lever and no grouping prompt will help.
"""
import json, pathlib, sys
import run
from analyze import LINKS

OUT, U, CAP = run.OUT, run.U, run.CAP
size = lambda ms: sum(len(U[m]['diff'].encode()) for m in ms)


def groups():
    """Extract the connected components of the required pairs first.

    compactHeadViewLocked appears in three pairs, so merging pair by pair
    undoes earlier merges. Components keep it with all three partners.
    """
    comp = {}
    for _, _, a, b in LINKS:
        comp.setdefault(a, {a}).add(b)
        if b in comp and comp[b] is not comp[a]:
            comp[a] |= comp[b]
        comp[b] = comp[a]
    seen, forced = [], []
    for c in comp.values():
        if c in seen:
            continue
        seen.append(c)
        assert size(c) <= CAP, (sorted(c), size(c))
        forced.append(sorted(c, key=run.ORDER.index))
    taken = {m for g in forced for m in g}
    gs = []
    for g in json.loads((OUT / 'linked-groups.json').read_text()):
        rest = [m for m in g['members'] if m not in taken]
        if rest:
            gs.append({'title': g['title'], 'members': rest, 'reason': g['reason']})
    for g in forced:
        gs.append({'title': 'forced: ' + ' + '.join(m.split(':')[-1] for m in g),
                   'members': g, 'reason': 'Oracle merge of reference-defect partners.'})
    return gs


def prepare():
    gs = groups()
    run.validate(gs)
    run.dump(OUT / 'oracle-groups.json', gs)
    jobs = [j for j in json.loads((OUT / 'jobs.json').read_text()) if j['arm'] != 'oracle']
    jobs += [{'arm': 'oracle', 'index': i, 'members': g['members'], 'request': run.group_request(g)}
             for i, g in enumerate(gs)]
    run.dump(OUT / 'jobs.json', jobs)
    owner = {m: i for i, g in enumerate(gs) for m in g['members']}
    print('oracle groups:', len(gs))
    for _, name, a, b in LINKS:
        print(f'  {owner[a] == owner[b]}  {name}')


if __name__ == '__main__':
    {'prepare': prepare}[sys.argv[1]]()
