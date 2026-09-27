import concurrent.futures, json, pathlib, subprocess, urllib.request, sys
ROOT=pathlib.Path(__file__).resolve().parent
p=json.loads((ROOT/'data/pr.json').read_text())
if len(sys.argv)>1:
    p['headRefOid']=sys.argv[1]
    (ROOT/'data/pr.diff').write_bytes(subprocess.check_output(['gh','api','-H','Accept: application/vnd.github.diff',f'repos/prometheus/prometheus/compare/{p["baseRefOid"]}...{p["headRefOid"]}']))
# PR diff is based on the merge base, not the base branch's current tip.
comparison=json.loads(subprocess.check_output(['gh','api',f'repos/prometheus/prometheus/compare/{p["baseRefOid"]}...{p["headRefOid"]}']))
base=comparison['merge_base_commit']['sha']
(ROOT/'data/revisions.json').write_text(json.dumps({'base':base,'head':p['headRefOid']},indent=2)+'\n')
def fetch(item):
    side,sha,path=item
    target=ROOT/'data'/side/path
    target.parent.mkdir(parents=True,exist_ok=True)
    url=f'https://raw.githubusercontent.com/prometheus/prometheus/{sha}/{path}'
    target.write_bytes(urllib.request.urlopen(url,timeout=60).read())
with concurrent.futures.ThreadPoolExecutor(max_workers=6) as ex:
    list(ex.map(fetch,[(side,sha,f['path']) for side,sha in [('base',base),('head',p['headRefOid'])] for f in p['files']]))
print('Fetched',len(p['files']),'files at both revisions',base,p['headRefOid'])


# Keep the fetched sources out of the pr-manager module: they are real Go
# packages whose imports this module does not have, so without their own
# go.mod `go build ./...` tries to compile them.
(pathlib.Path(__file__).resolve().parent / "data" / "go.mod").write_text(
    "module pr-triage-fixtures\n\ngo 1.26.0\n")
