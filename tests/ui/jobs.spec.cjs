const { test, expect } = require('@playwright/test');

for (const surface of ['sidebar', 'triage log', 'fix log']) {
  test(`Stop from ${surface} requires confirmation and updates the log`, async ({ page }) => {
    const job = { id: 'stop-test', kind: surface === 'fix log' ? 'fix' : 'triage', url: 'https://github.com/example/repo/pull/1', status: 'running', cancelable: true, stage: 'fetch', started: new Date().toISOString() };
    let cancels = 0;
    await page.route('**/api/jobs', route => route.fulfill({ json: [job] }));
    await page.route('**/api/jobs/stop-test', route => route.fulfill({ json: job }));
    await page.route('**/api/jobs/stop-test/log**', route => route.fulfill({ json: [] }));
    await page.route('**/api/jobs/stop-test/cancel', async route => {
      cancels++;
      job.status = 'cancelled';
      job.cancelable = false;
      await route.fulfill({ json: {} });
    });
    await page.goto('/');
    const row = page.locator('#jobs .job-item');
    await expect(row).toBeVisible();
    if (surface !== 'sidebar') await row.locator('.u').click();
    const container = page.locator(surface === 'sidebar' ? '#jobs' : surface === 'fix log' ? '#job-log' : '#main');
    await container.getByRole('button', { name: 'Stop', exact: true }).click();
    const warning = page.locator('.ask-dlg');
    await expect(warning).toContainText('Unfinished work may be lost');
    expect(cancels).toBe(0);
    await warning.getByRole('button', { name: 'Cancel', exact: true }).click();
    expect(cancels).toBe(0);
    await container.getByRole('button', { name: 'Stop', exact: true }).click();
    await warning.getByRole('button', { name: 'Stop action', exact: true }).click();
    await expect.poll(() => cancels).toBe(1);
    await expect(container.getByRole('button', { name: 'Stop', exact: true })).toHaveCount(0);
    if (surface !== 'sidebar') await expect(container).toContainText('stopped');
  });
}

test('a running job past its cancellation boundary has no Stop button', async ({ page }) => {
  const job = { id: 'committing', kind: 'fix', url: 'example', status: 'running', cancelable: false, stage: 'commit' };
  await page.route('**/api/jobs', route => route.fulfill({ json: [job] }));
  await page.route('**/api/jobs/committing', route => route.fulfill({ json: job }));
  await page.route('**/api/jobs/committing/log**', route => route.fulfill({ json: [] }));
  await page.goto('/');
  await page.locator('#jobs .job-item').click();
  await expect(page.locator('#job-log')).toContainText('committing');
  await expect(page.locator('.job-stop')).toHaveCount(0);
});
