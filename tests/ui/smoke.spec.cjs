const { test: base, expect } = require('@playwright/test');

// Catch startup failures even when the static HTML still looks healthy.
const test = base.extend({
  page: async ({ page }, use) => {
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('response', response => {
      if (response.status() >= 400 && /\.(js|css)(?:\?|$)/.test(response.url())) {
        errors.push(`${response.status()} ${response.url()}`);
      }
    });
    await use(page);
    expect(errors, 'UI must load its assets and run without uncaught JavaScript errors').toEqual([]);
  },
});

test.beforeEach(async ({ request }) => {
  // Each browser starts from the same seeded cache, including after triage.
  for (const result of await (await request.get('/api/results')).json()) {
    if (result.key !== 'ui-smoke-cached') await request.delete(`/api/results/${result.key}`);
  }
});

test('startup renders an empty sidebar and wires header controls', async ({ page }) => {
  await page.route('**/api/results', route => route.fulfill({ json: [] }));
  await page.goto('/');
  await expect(page.locator('#app-version')).not.toBeEmpty();
  await expect(page.locator('#list')).toHaveText('none yet');
  await expect(page.locator('#main')).toContainText('Enter a PR link or repository path');
  await page.getByRole('button', { name: 'Hide the sidebar', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Show the sidebar', exact: true })).toHaveAttribute('aria-expanded', 'false');
  await page.getByRole('button', { name: 'Show the sidebar', exact: true }).click();
  await expect(page.locator('#side')).toBeVisible();
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('dialog', { name: 'Settings', exact: true })).toBeVisible();
  await page.getByRole('tab', { name: 'Chat agent', exact: true }).click();
  await expect(page.locator('#chat_agent')).toBeVisible();
  await page.getByRole('tab', { name: 'Fixes', exact: true }).click();
  await expect(page.locator('#fix_agent')).toBeVisible();
});

test('cached result renders its overview, code diff and Issues tab', async ({ page }) => {
  await page.goto('/');
  const row = page.locator('#list .pr-item').filter({ hasText: 'Cached greeting review' });
  await expect(row).toBeVisible();
  await row.click();
  await expect(page.locator('#main h2')).toContainText('Cached greeting review');
  await expect(page.locator('#main')).toContainText('Show a cached review in the UI.');
  await page.getByRole('button', { name: 'Classic', exact: true }).click();
  await expect(page.locator('#main .unit')).toBeVisible();
  await expect(page.locator('#main .diff')).toBeVisible();
  await expect(page.locator('#main .diff')).toContainText('Hello from the smoke test');
  await page.locator('[data-act="tab"][data-tab="issues"]').click();
  await expect(page.locator('#main')).toContainText('nothing to report');
});

test('Triage runs a local job and displays the finished review', async ({ page }) => {
  const result = await page.request.get('/api/results/ui-smoke-cached');
  expect(result.ok()).toBeTruthy();
  const { pr } = await result.json();
  await page.goto('/');
  await expect(page.locator('#list .pr-item').first()).toBeVisible();
  await page.locator('#url').fill(pr.local_path);
  const started = page.waitForResponse(response => response.url().endsWith('/api/triage') && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Triage', exact: true }).click();
  const response = await started;
  expect(response.status()).toBe(202);
  const job = await response.json();
  expect(job.id).toBeTruthy();
  await expect(page.locator('#main .pr-head')).toBeVisible();
  await expect(page).toHaveURL(/key=local__/);
  await page.getByRole('button', { name: 'Classic', exact: true }).click();
  await expect(page.locator('#main .diff')).toBeVisible();
  await expect(page.locator('#main .diff')).toContainText('Hello from the smoke test');
  await expect(page.locator('#go')).toBeEnabled();
});

test('local branch offers to stack on the PR it builds on, and to unstack', async ({ page }) => {
  const cached = await (await page.request.get('/api/results/ui-smoke-cached')).json();
  const serve = (pr) => page.route('**/api/results/ui-smoke-cached', route => route.fulfill({ json: { ...cached, pr: { ...cached.pr, owner: 'o', repo: 'r', head_ref: 'second', uncommitted: false, ahead: 1, ...pr } } }));
  await serve({ stack_candidates: [{ branch: 'first', url: 'https://github.com/o/r/pull/7', number: 7, title: 'First' }] });
  await page.goto('/?key=ui-smoke-cached');
  await expect(page.locator('#main')).toContainText("Builds on #7 (first), which isn't merged yet.");
  await expect(page.getByRole('button', { name: 'Stack on #7', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Create PR into main', exact: true })).toBeVisible();
  await page.unroute('**/api/results/ui-smoke-cached');
  await serve({ base_ref: 'first', stacked_on: 'first', default_base: 'main', stack_pr: 'https://github.com/o/r/pull/7' });
  await page.reload();
  await expect(page.locator('#main')).toContainText('Stacked on #7 (first)');
  await expect(page.getByRole('button', { name: 'Use main instead', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Create PR into first', exact: true })).toBeVisible();
});

test('Triage displays a failed job instead of silently doing nothing', async ({ page }) => {
  const result = await (await page.request.get('/api/results/ui-smoke-cached')).json();
  await page.goto('/');
  await expect(page.locator('#list .pr-item').first()).toBeVisible();
  await page.locator('#url').fill(`${result.pr.local_path}/missing-checkout`);
  await page.getByRole('button', { name: 'Triage', exact: true }).click();
  await expect(page.locator('#main .error')).toContainText('missing-checkout');
  await expect(page.locator('#go')).toBeEnabled();
});

test('extra reviewers are set up in Settings and their findings are labelled on the Issues tab', async ({ page }) => {
  const result = await (await page.request.get('/api/results/ui-smoke-cached')).json();
  const unit = result.files[0].units[0];
  unit.issues = [
    { severity: 'high', line: 3, title: 'Greeting is never localized', failure_scenario: 'a French user sees English', source: 'cc-review' },
    { severity: 'high', line: 3, title: 'Greeting not localized', failure_scenario: 'x', source: 'codex-sec', same_as: { unit: unit.id, title: 'Greeting is never localized' } },
    { severity: 'low', line: 1, title: 'Package comment missing', failure_scenario: 'lint warns' },
  ];
  result.reviewers = [
    { name: 'cc-review', model: 'claude-code/opus', at: new Date().toISOString(), found: 1, kept: 1 },
    { name: 'codex-sec', model: 'codex/gpt', at: new Date().toISOString(), found: 1, kept: 1 },
  ];
  await page.route('**/api/results/ui-smoke-cached', route => route.fulfill({ json: result }));
  await page.goto('/');

  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('tab', { name: 'Reviewers', exact: true }).click();
  await expect(page.locator('#reviewers-list')).toContainText('None set up');
  await page.getByRole('button', { name: 'Add reviewer', exact: true }).click();
  await page.getByLabel('reviewer name').fill('cc-review');
  await page.getByLabel('reviewer name').press('Tab');
  await page.getByLabel('reviewer skill').fill('/code-review high');
  await page.getByLabel('reviewer skill').press('Tab');
  await page.keyboard.press('Escape');

  await page.locator('#list .pr-item').filter({ hasText: 'Cached greeting review' }).click();
  await page.locator('[data-act="tab"][data-tab="issues"]').click();
  const main = page.locator('#main');
  await expect(main.getByRole('button', { name: 'Run reviewers (1)' })).toBeVisible();
  await expect(main.locator('.rev-runs')).toContainText('cc-review: found 1, 1 kept');
  // The repeat is folded into its issue's row, which names both reviewers.
  const row = main.locator('.claim').filter({ hasText: 'Greeting is never localized' });
  await expect(row.locator('.src-chip')).toHaveText(['cc-review', 'codex-sec']);
  await expect(main.locator('.claim').filter({ hasText: 'Package comment missing' }).locator('.src-chip')).toHaveText('built-in');
  // Hiding the built-in review leaves the reviewers' issue.
  await main.locator('[data-act="issues-source"][data-src=""]').click();
  await expect(main.locator('.claim').filter({ hasText: 'Package comment missing' })).toHaveCount(0);
  await expect(row).toBeVisible();

  // Leave the settings as the other tests expect them.
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('button', { name: 'remove reviewer' }).click();
  await expect(page.locator('#reviewers-list')).toContainText('None set up');
});
