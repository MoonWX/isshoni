// Playwright config (05 §19.3): the per-PR end-to-end specs of e2e/, in Google Chrome, against real servers.
//
// `task e2e` builds the binary and runs `npm run e2e` with ISSHONI_BIN; the CI e2e job does the same with the
// build job's binary (06 §8.2). e2e/global-setup.ts finds the binary; the servers come from e2e/fixtures.ts.
//
// - Chrome is the installed Google Chrome (channel 'chrome'; `task setup:e2e` installs it where it is missing).
//   CI runs it headful under xvfb with a window manager, because only a headful Chrome accepts the tab picker
//   of getDisplayMedia, and under X11 the picker needs a window manager. A local run is headless, and its specs
//   share through the fake-display seam. For windows and the real picker, run
//   `ISSHONI_BIN=bin/isshoni npm run e2e -- --headed` in web/ (`task e2e` leaves its binary in bin/).
// - A failed test keeps its trace in test-results/, next to the servers' logs (server-<worker>-<n>.log), and the
//   HTML report goes to playwright-report/. CI also keeps the video of a failed test and uploads all of it when
//   the job fails. Video is off outside CI: it needs Playwright's own ffmpeg (`npx playwright install ffmpeg`),
//   which a machine that already had Chrome doesn't have, and the trace shows every step anyway.
// - Service workers are blocked: a production build registers one, and it would answer navigations from its
//   cache. The PWA spec turns them on for itself (`test.use({ serviceWorkers: 'allow' })`).
import { defineConfig } from '@playwright/test';

import { chromeArgs } from './e2e/fixtures.ts';

const ci = Boolean(process.env['CI']);

export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',
  outputDir: './test-results',
  globalSetup: './e2e/global-setup.ts',

  // A stray test.only must not turn the CI job green.
  forbidOnly: ci,
  // One worker in CI. There all browser windows share the one X display of Xvfb, and Chrome's capture picker
  // lists the display's windows: when a window of another worker goes away at that moment, Chrome logs an X
  // BadWindow error and getDisplayMedia never answers (about one pick in 25 with two workers, none with one).
  ...(ci ? { workers: 1 } : {}),
  // One retry in CI: a test that passes the second time is reported as flaky, with the trace of its failure.
  retries: ci ? 1 : 0,
  // The CI job has 20 minutes: a run that hangs ends here, as a failure, in time for its upload.
  globalTimeout: ci ? 15 * 60_000 : 0,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: ci
    ? [['github'], ['list'], ['html', { outputFolder: './playwright-report', open: 'never' }]]
    : [['list'], ['html', { outputFolder: './playwright-report', open: 'never' }]],

  use: {
    channel: 'chrome',
    headless: !ci,
    launchOptions: { args: chromeArgs() },
    serviceWorkers: 'block',
    trace: 'retain-on-failure',
    video: ci ? 'retain-on-failure' : 'off',
    screenshot: 'only-on-failure',
  },
});
