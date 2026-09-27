import json, pathlib, re

ROOT = pathlib.Path(__file__).resolve().parent
R = ROOT / 'results'
ARMS = ['baseline', 'deterministic', 'model', 'linked', 'oracle']
jobs = json.loads((R / 'jobs.json').read_text())
u = {x['id']: x for x in json.loads((ROOT / 'data/units.json').read_text())}

# Each pair is two units a reviewer must read together to see one reference
# defect. Co-location is a property of the grouping alone, independent of
# whether the reviewer then finds the bug.
LINKS = [
    ('inflight_appender', 'watermark and guard',
     'tsdb/db.go:(*DB).compactHeadViewLocked', 'tsdb/head.go:hasAppendIDAbove'),
    ('inflight_appender', 'implementation and late-append test',
     'tsdb/db.go:(*DB).compactHeadViewLocked',
     'tsdb/db_test.go:TestCompactSelectedSeries_LateAppendDuringCompactionSurvivesRestart'),
    ('single_timestamp', 'write loop and eviction guard',
     'tsdb/db.go:(*DB).compactHeadViewLocked', 'tsdb/head.go:(*Head).truncateSeries'),
    ('unsorted_refs', 'input and ordering',
     'tsdb/db.go:(*DB).CompactSelectedSeries', 'tsdb/head_read.go:(*Head).filterSeriesAndSortPostings'),
]

summary, issues = {}, []
for arm in ARMS:
    selected = [x for x in jobs if x['arm'] == arm]
    if not selected:
        continue
    results = []
    for j in selected:
        path = R / f'{arm}-{j["index"]:03d}.json'
        if not path.exists():
            continue
        x = json.loads(path.read_text())
        if x.get('error'):
            continue
        results.append(x)
        args = x['response']['tool_calls'][0]['arguments']
        for k, issue in enumerate(args.get('issues', [])):
            issues.append({'id': f'{arm}-{j["index"]:03d}-{k}', 'members': j['members'], **issue})
    words = lambda a: len(re.findall(r'\S+', str(a.get('headline', '')) + ' ' + str(a.get('summary', '')) +
                                     ' ' + ' '.join(a.get('focus', []))))
    args = [x['response']['tool_calls'][0]['arguments'] for x in results]
    item = {
        'tasks': len(selected), 'completed': len(results),
        'prompt_characters': sum(sum(len(m['content']) for m in j['request']['messages']) for j in selected),
        'input_tokens': sum(x['response']['usage']['input_tokens'] for x in results),
        'output_tokens': sum(x['response']['usage']['output_tokens'] for x in results),
        'service_seconds': round(sum(x['seconds'] for x in results), 1),
        'summary_focus_words': sum(words(a) for a in args),
        'raw_issues': sum(len(a.get('issues', [])) for a in args),
        'issues_marked_introduced': sum(1 for a in args for i in a.get('issues', []) if i.get('introduced_by_pr')),
        'issues_needing_unseen_code': sum(1 for a in args for i in a.get('issues', []) if i.get('depends_on_unseen_code')),
        'largest_focus_bytes': max(sum(len(u[m]['diff'].encode()) for m in j['members']) for j in selected),
        'median_group_size': sorted(len(j['members']) for j in selected)[len(selected) // 2],
    }
    gfile = {'model': 'grouping.json', 'linked': 'grouping-linked.json'}.get(arm)
    if gfile and (R / gfile).exists():
        g = json.loads((R / gfile).read_text())
        item['grouping_input_tokens'] = g['response']['usage']['input_tokens']
        item['grouping_output_tokens'] = g['response']['usage']['output_tokens']
        item['grouping_seconds'] = round(g['seconds'], 1)
    item['total_input_tokens'] = item['input_tokens'] + item.get('grouping_input_tokens', 0)
    item['total_output_tokens'] = item['output_tokens'] + item.get('grouping_output_tokens', 0)
    item['total_service_seconds'] = round(item['service_seconds'] + item.get('grouping_seconds', 0), 1)
    summary[arm] = item

colo = []
for defect, name, a, b in LINKS:
    row = {'defect': defect, 'relationship': name, 'a': a, 'b': b,
           'pair_bytes': len(u[a]['diff'].encode()) + len(u[b]['diff'].encode())}
    for arm in summary:
        gs = json.loads((R / f'{arm}-groups.json').read_text())
        owner = {m: i for i, g in enumerate(gs) for m in g['members']}
        row[arm] = owner[a] == owner[b]
    colo.append(row)
for arm in summary:
    summary[arm]['defect_pairs_colocated'] = sum(1 for r in colo if r[arm])

(R / 'metrics.json').write_text(json.dumps(summary, indent=2) + '\n')
(R / 'co-location.json').write_text(json.dumps(colo, indent=2) + '\n')
(R / 'issues.json').write_text(json.dumps(issues, indent=2) + '\n')
print(json.dumps(summary, indent=2))
print(json.dumps(colo, indent=2))
