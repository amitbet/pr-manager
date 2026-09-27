"""Prescreen review issues against the four reference defects.

Keyword prescreen only. It narrows what a human has to read; the recovery
verdicts in FINDINGS.md are assigned by hand under the PROTOCOL rule (mechanism
plus a compatible concrete consequence).
"""
import json, pathlib, re, sys

ROOT = pathlib.Path(__file__).resolve().parent
R = ROOT / 'results'
ARMS = ['baseline', 'deterministic', 'model', 'linked', 'oracle']

PROBES = {
    'inflight_appender': r'lastAppendID|in-?flight|incompleteAppends|hasAppendIDAbove|watermark|already[- ]open appender',
    'single_timestamp': r'MinTime\s*==\s*MaxTime|single[- ](timestamp|sample)|mint\s*>=\s*maxt|>=\s*maxt|overlap',
    'unsorted_refs': r'NewListPostings|sorted|sort\b|unsorted|order of (the )?refs',
    'duplicate_refs': r'duplicat|dedup|out-of-order series',
}


def issues(arm):
    jobs = [j for j in json.loads((R / 'jobs.json').read_text()) if j['arm'] == arm]
    out = []
    for j in jobs:
        p = R / f"{arm}-{j['index']:03d}.json"
        if not p.exists():
            continue
        x = json.loads(p.read_text())
        if x.get('error'):
            continue
        for k, i in enumerate(x['response']['tool_calls'][0]['arguments'].get('issues', [])):
            out.append({'id': f"{arm}-{j['index']:03d}-{k}", 'members': j['members'], **i})
    return out


def main():
    report = {}
    for arm in ARMS:
        if not (R / f'{arm}-groups.json').exists() and arm != 'baseline':
            continue
        xs = issues(arm)
        hits = {}
        for name, probe in PROBES.items():
            rx = re.compile(probe, re.I)
            hits[name] = [x['id'] for x in xs
                          if rx.search(' '.join(str(x.get(f, '')) for f in ('title', 'evidence', 'failure_scenario')))]
        report[arm] = {'issues': len(xs), 'candidates': hits}
        (R / f'issues-{arm}.md').write_text('\n'.join(
            f"## {x['id']}  [{x.get('severity')}] introduced={x.get('introduced_by_pr')} unseen={x.get('depends_on_unseen_code')}\n"
            f"**{x.get('title')}**\n\n- members: {', '.join(x['members'])}\n"
            f"- evidence: {x.get('evidence')}\n- scenario: {x.get('failure_scenario')}\n"
            for x in xs))
    (R / 'defect-candidates.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
