// Playwright config of the real-app CDP spec (M2 03 §22.4 B): the e2e build of isshoni.exe (`task desktop:build:e2e`,
// -tags e2e: the fake engine, DevTools on), driven over the Chrome DevTools Protocol of its own WebView2. Windows only;
// the main config (web/playwright.config.ts) never runs these files, whose names end in .cdp.ts.
//
//   ISSHONI_DESKTOP_EXE=<path to the e2e isshoni.exe> npx playwright test -c e2e/cdp/playwright.config.ts
//
// The app has to open its WebView2's remote debugging port for the spec. At beta.34 Wails overwrites
// WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS in every build (03 §5.4), so the e2e build takes the port from its own
// e2e-only variable ISSHONI_E2E_CDP_PORT and appends --remote-debugging-port to WindowsOptions.AdditionalBrowserArgs
// (README WA72, cmd/isshoni-desktop/flavor_e2e.go). The same file takes the data folder from ISSHONI_E2E_DATA_DIR in
// place of %LOCALAPPDATA% and opens Credential Manager under a test prefix (03 §22.4 B), so a run never touches the
// owner's servers, settings, WebView2 profile or isshoni:v1: entries; the spec refuses an app that ignored the folder.
// ISSHONI_CDP_ENDPOINT=http://127.0.0.1:<port> attaches to an app that is already running instead of starting one. CI
// runs it in windows-desktop from WA72 on.
//
// The spec's end-to-end case starts `isshoni serve` through ../fixtures.ts, whose binary is ISSHONI_BIN (a relative
// value counts from the repository root). Unset, it is bin/isshoni.exe, which windows-desktop builds next to the e2e
// isshoni.exe (go build ./cmd/isshoni, with web/dist embedded). The people's browsers are the installed Google Chrome
// (channel 'chrome', as in the main config), headless: they only approve a link and watch a share, which needs no
// picker. The W19 case also runs a production build: ISSHONI_DESKTOP_PROD_EXE, or in CI the stage's isshoni.exe.
import { defineConfig } from '@playwright/test';

import { chromeArgs } from '../fixtures.ts';

process.env['ISSHONI_BIN'] ??= process.platform === 'win32' ? 'bin/isshoni.exe' : 'bin/isshoni';

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
  use: {
    channel: 'chrome',
    headless: true,
    launchOptions: { args: chromeArgs() },
    serviceWorkers: 'block',
  },
});
