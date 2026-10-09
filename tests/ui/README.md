# UI sanity tests

Run from the repository root:

```sh
npm ci --prefix tests/ui
cd tests/ui && npx playwright install chromium webkit && cd ../..
make test-ui
```

On Linux, use `npx playwright install --with-deps chromium webkit` to install
the browsers' system dependencies too.

The suite builds and runs the real Go server with its embedded UI in Chromium
and WebKit. It uses a temporary Git checkout and cache, with model providers
and linting disabled. It does not use your results, settings, GitHub or LLMs.
Set `PR_MANAGER_TEST_BINARY` to an absolute executable path to test a prebuilt
binary instead; CI uses this to test the same binary as the release smoke check.
Port 18766 must be free.

Coverage: empty startup and header controls, a cached review and code diff,
the Issues tab, successful local triage, and a failed job's visible error.
Every test also checks for uncaught JavaScript errors and failed JS/CSS loads.
Failed tests save screenshots and Playwright traces in `test-results/`, uploaded
by CI. These are sanity checks for the shared UI, not native window integration
or model-review quality tests.
