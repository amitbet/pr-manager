// Run the real embedded UI/API with a disposable checkout and cache.
const { spawn, execFileSync } = require('node:child_process');
const { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { resolve, join } = require('node:path');

const workspace = resolve(__dirname, '../..');
const root = mkdtempSync(join(tmpdir(), 'pr-manager-ui-'));
const cache = join(root, 'cache');
const repo = join(root, 'sample');
const helpers = join(root, 'bin');
mkdirSync(helpers);
for (const name of ['open', 'xdg-open']) writeFileSync(join(helpers, name), '#!/bin/sh\nexit 0\n', { mode: 0o755 });
mkdirSync(join(cache, 'results'), { recursive: true });
mkdirSync(repo);
const git = (...args) => execFileSync('git', ['-C', repo, ...args], { encoding: 'utf8' }).trim();
git('init', '-q', '-b', 'main');
writeFileSync(join(repo, 'greet.go'), 'package sample\n\nfunc Greeting() string { return "Hello" }\n');
git('add', '.');
git('-c', 'user.name=UI Test', '-c', 'user.email=ui@example.invalid', 'commit', '-qm', 'Initial greeting');
const head = git('rev-parse', 'HEAD');
git('update-ref', 'refs/remotes/origin/main', head);
writeFileSync(join(repo, 'greet.go'), 'package sample\n\nfunc Greeting() string { return "Hello from the smoke test" }\n');
const fixture = JSON.parse(readFileSync(join(__dirname, 'result.json'), 'utf8'));
Object.assign(fixture.pr, { local_path: repo, base_oid: head, head_oid: head });
fixture.created_at = new Date().toISOString();
writeFileSync(join(cache, 'results', `${fixture.key}.json`), JSON.stringify(fixture));

const binary = process.env.PR_MANAGER_TEST_BINARY
  ? resolve(process.env.PR_MANAGER_TEST_BINARY) : join(root, 'pr-manager');
if (!process.env.PR_MANAGER_TEST_BINARY) {
  execFileSync('go', ['build', '-o', binary, '.'], { cwd: workspace, stdio: 'inherit' });
}
// No LLMs, GitHub or personal settings are needed for these tests.
const child = spawn(binary, ['serve', '-addr', '127.0.0.1:18766', '-cache', cache,
  '-codemap', 'off', '-classifier', 'off', '-summarizer', 'off', '-lint', 'off'], {
  cwd: root,
  env: { ...process.env, PATH: `${helpers}${require('node:path').delimiter}${process.env.PATH}`,
    PR_MANAGER_WORKSPACE: '', PR_MANAGER_CODE_ROOT: '', PR_MANAGER_ORG: '' },
  stdio: 'inherit',
});
child.on('error', (error) => { console.error(error); rmSync(root, { recursive: true, force: true }); process.exit(1); });
child.on('exit', (code) => {
  rmSync(root, { recursive: true, force: true });
  process.exit(code ?? 1);
});
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => child.kill('SIGINT'));
