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

test('Triage displays a failed job instead of silently doing nothing', async ({ page }) => {
  const result = await (await page.request.get('/api/results/ui-smoke-cached')).json();
  await page.goto('/');
  await expect(page.locator('#list .pr-item').first()).toBeVisible();
  await page.locator('#url').fill(`${result.pr.local_path}/missing-checkout`);
  await page.getByRole('button', { name: 'Triage', exact: true }).click();
  await expect(page.locator('#main .error')).toContainText('missing-checkout');
  await expect(page.locator('#go')).toBeEnabled();
});
