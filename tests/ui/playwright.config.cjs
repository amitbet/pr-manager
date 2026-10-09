const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  testMatch: '*.spec.cjs',
  workers: 1,
  timeout: 30_000,
  forbidOnly: !!process.env.CI,
  use: {
    baseURL: 'http://127.0.0.1:18766',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'chromium', use: { browserName: 'chromium' } },
    // Wails on macOS uses WebKit; exercise that engine as well.
    { name: 'webkit', use: { browserName: 'webkit' } },
  ],
  webServer: {
    command: 'node server.cjs',
    url: 'http://127.0.0.1:18766/api/config',
    reuseExistingServer: false,
    timeout: 120_000,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 10_000 },
  },
});
