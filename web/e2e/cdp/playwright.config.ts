// Playwright config of the real-app CDP spec (M2 03 §22.4 B): the e2e build of isshoni.exe (`task desktop:build:e2e`,
// -tags e2e: the fake engine, DevTools on), driven over the Chrome DevTools Protocol of its own WebView2. Windows only;
// the main config (web/playwright.config.ts) never runs these files, whose names end in .cdp.ts.
//
//   ISSHONI_DESKTOP_EXE=<path to the e2e isshoni.exe> npx playwright test -c e2e/cdp/playwright.config.ts
//
// The app has to open its WebView2's remote debugging port for the spec. At beta.34 Wails overwrites
// WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS in every build (03 §5.4), so the e2e build takes the port from its own
// e2e-only variable ISSHONI_E2E_CDP_PORT and appends --remote-debugging-port to WindowsOptions.AdditionalBrowserArgs
// (README WA72, cmd/isshoni-desktop/flavor_e2e.go). ISSHONI_CDP_ENDPOINT=http://127.0.0.1:<port> attaches to an app
// that is already running instead of starting one. CI runs it in windows-desktop from WA72 on.
import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: '*.cdp.ts',
  outputDir: '../../test-results/cdp',
  forbidOnly: Boolean(process.env['CI']),
  // One app at a time: it is a single-instance app with one window.
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
});
