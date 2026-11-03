// download.spec (docs/m2/06-windows-release.md §7.5, §7.6, §16 "E2E"): /download against a real server, signed out
// (the page is public).
//
// - The e2e server is a build without a release of its own (0.0.0-e2e.local, CI's 0.0.0-ci.<run>, or a dev build):
//   the page links to the latest release, says why, and shows the PowerShell command with the server's origin. The
//   origin is http://127.0.0.1:<port>, so there is no isshoni:// link (https only, 02 §12.1) and no "Open isshoni".
// - With /api/v1/info routed to server.version 0.2.0: the exact installer of that release.
// - GET /install-desktop.ps1, the command's script, is pre-filled with the server's origin (06 §7.4).
// - axe (WCAG 2.0 A and AA, 2.1 AA, as a11y.spec) finds no serious or critical violation, in the light and the dark
//   colour scheme.
import AxeBuilder from '@axe-core/playwright';
import type { Browser, Page } from '@playwright/test';

import { expect, test } from './fixtures.ts';

/** The WCAG levels of 05 §19.3, as a11y.spec runs them. */
const TAGS = ['wcag2a', 'wcag2aa', 'wcag21aa'];
const LATEST = 'https://github.com/MoonWX/isshoni/releases/latest';
const INSTALLER_0_2_0 =
  'https://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe';
/** 06 §7.4's release-version regex. */
const RELEASE_VERSION = /^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$/;

/** A browser context that is nobody: the page is public. */
async function anonymous(browser: Browser): Promise<Page> {
  const context = await browser.newContext();
  return context.newPage();
}

/** The page has rendered: its <h1>. */
async function pageReady(page: Page): Promise<void> {
  await expect(page.getByRole('heading', { level: 1, name: 'Desktop app' })).toBeVisible();
}

/** The Windows section, by its heading. */
function windows(page: Page) {
  return page.getByRole('region', { name: 'Windows' });
}

/** How often settled() reads the page's colours, and for how long at most (as a11y.spec). */
const SETTLE_POLL_MS = 100;
const SETTLE_TIMEOUT_MS = 5_000;

/** Waits until every element's colours read the same twice in a row: the colour scheme's switch has arrived. */
async function settled(page: Page): Promise<void> {
  const colours = (): Promise<string> =>
    page.evaluate(() =>
      [...document.querySelectorAll('body *')]
        .map((el) => {
          const style = getComputedStyle(el);
          return `${style.color}/${style.backgroundColor}`;
        })
        .join(' '),
    );
  const deadline = Date.now() + SETTLE_TIMEOUT_MS;
  let before = await colours();
  for (;;) {
    await page.waitForTimeout(SETTLE_POLL_MS);
    const now = await colours();
    if (now === before || Date.now() > deadline) return;
    before = now;
  }
}

/** axe on the page in the light and then the dark colour scheme: no serious or critical violation. */
async function expectAccessible(page: Page, what: string): Promise<void> {
  for (const colorScheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme, reducedMotion: 'reduce' });
    await settled(page);
    const { violations, passes } = await new AxeBuilder({ page }).withTags(TAGS).analyze();
    expect(passes.length, `axe checked ${what} (${colorScheme})`).toBeGreaterThan(0);
    const serious = violations
      .filter((v) => v.impact === 'serious' || v.impact === 'critical')
      .map((v) => `${v.id} (${v.impact ?? ''}): ${v.help}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`);
    expect(serious, `axe on ${what} (${colorScheme})`).toEqual([]);
  }
  await page.emulateMedia({ colorScheme: null, reducedMotion: null });
}

test('a build without a release links the latest release and the command for this server', async ({
  server,
  browser,
}) => {
  const page = await anonymous(browser);
  try {
    const info = (await (await page.request.get(`${server.url}/api/v1/info`)).json()) as {
      server: { version: string; publicUrl: string };
    };
    expect(info.server.version, 'the e2e server is not a release').not.toMatch(RELEASE_VERSION);
    expect(info.server.publicUrl).toBe(server.url);

    await page.goto(`${server.url}/download`);
    await pageReady(page);
    const section = windows(page);
    await expect(
      section.getByText(`This server is a development build (${info.server.version})`, { exact: false }),
    ).toBeVisible();
    await expect(section.getByRole('link', { name: 'Latest release' })).toHaveAttribute('href', LATEST);
    await expect(section.getByRole('link', { name: 'Download for Windows' })).toHaveAttribute('href', LATEST);
    // WA49 recorded the bare form (no TLS prefix).
    await expect(section.locator('code')).toHaveText(`irm ${server.url}/install-desktop.ps1 | iex`);

    // The three numbered steps; an http:// server has no app link, so step 3 points to the Start menu.
    const steps = section.locator('ol > li');
    await expect(steps).toHaveCount(3);
    await expect(steps.nth(2)).toContainText('Start isshoni from the Start menu');
    await expect(page.getByRole('button', { name: 'Open isshoni' })).toHaveCount(0);

    await expectAccessible(page, 'the download page');
  } finally {
    await page.context().close();
  }
});

test('a server of release 0.2.0 links the exact installer', async ({ server, browser }) => {
  const page = await anonymous(browser);
  try {
    await page.route('**/api/v1/info', async (route) => {
      const response = await route.fetch();
      const body = (await response.json()) as { server: Record<string, unknown> };
      await route.fulfill({ response, json: { ...body, server: { ...body.server, version: '0.2.0' } } });
    });
    await page.goto(`${server.url}/download`);
    await pageReady(page);
    const section = windows(page);
    await expect(section.getByRole('link', { name: 'Download for Windows' })).toHaveAttribute('href', INSTALLER_0_2_0);
    await expect(section.getByRole('link', { name: 'Release notes' })).toHaveAttribute(
      'href',
      'https://github.com/MoonWX/isshoni/releases/tag/v0.2.0',
    );
    await expect(section.getByText(/development build/)).toHaveCount(0);
    await expect(section.locator('code')).toHaveText(`irm ${server.url}/install-desktop.ps1 | iex`);

    await expectAccessible(page, 'the download page of a release');
  } finally {
    await page.context().close();
  }
});

test("the command's script is pre-filled for this server", async ({ server, request }) => {
  const res = await request.get(`${server.url}/install-desktop.ps1`);
  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toBe('text/plain; charset=utf-8');
  const body = await res.text();
  expect(body).toContain(`$IsshoniServer  = '${server.url}'`);
  // Not a release: the script installs the latest one.
  expect(body).toContain("$IsshoniVersion = ''");
});
