// a11y.spec (05 §19.3, §16.6): axe (WCAG 2.0 A and AA, 2.1 AA) on the pages a friend meets, against a real server,
// finds no serious or critical violation, in the light and the dark colour scheme (05 §16.6: AA in both): the login,
// invite, reset and setup pages, the room with a share (and its debug overlay, "Stats for nerds"), the admin's user
// list, and every state of the device-link page (M2 02 §10.1: enter, invalid, confirm on the same, a different and an
// unknown network, linked, denied, rate_limited, error). In the room the tile list is reachable with the keyboard and
// F switches the stage's fullscreen, and the overlay shows the friend's tile as the page receives it and as the
// server forwards it, with no IP address.
//
// What axe can't judge (a screen reader's reading order, the live regions' timing) is the manual matrix's and the
// component tests' (05 §19.2, §19.4).
import AxeBuilder from '@axe-core/playwright';
import type { BrowserContext, Page } from '@playwright/test';

import { expect, fakeDisplay, test, type E2EServer } from './fixtures.ts';
import { readState, waitForRoom, waitForState } from './stats.ts';

/** The WCAG levels of 05 §19.3. */
const TAGS = ['wcag2a', 'wcag2aa', 'wcag21aa'];
/** A share that started may take this long to reach the server and a viewer: two PeerConnections. */
const MEDIA_TIMEOUT_MS = 20_000;
/** IPv4 and IPv6 addresses, which the debug overlay never shows (05 §10.7). */
const IP = /\b\d{1,3}(?:\.\d{1,3}){3}\b|\b[0-9a-f]{1,4}(?::[0-9a-f]{0,4}){2,7}\b/i;

// Two pages that encode and decode in the room test: room for a CI machine that runs them in software.
test.describe.configure({ timeout: 120_000 });

/** How often settled() reads the page's colours, and for how long at most. */
const SETTLE_POLL_MS = 100;
const SETTLE_TIMEOUT_MS = 5_000;

/**
 * Waits until the page's colours stand still: the text and background colour of every element, read twice in a row
 * the same. A switch of the colour scheme reaches the elements over a few frames (some later than others, even
 * under reduced motion), and axe must read the scheme's own colours, not a mix of the two.
 */
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

/**
 * Runs axe on what the page shows now (or inside `include`), in the light and then the dark colour scheme, and
 * fails with the serious and critical violations, each with its rule, its help and the elements it found. Reduced
 * motion makes the switch between the schemes quick (05 §16.6: no transitions under prefers-reduced-motion).
 */
async function expectAccessible(page: Page, what: string, include?: string): Promise<void> {
  for (const colorScheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme, reducedMotion: 'reduce' });
    await settled(page);
    let builder = new AxeBuilder({ page }).withTags(TAGS);
    if (include !== undefined) builder = builder.include(include);
    const { violations, passes } = await builder.analyze();
    // Something was checked: a selector that matched nothing would pass any page.
    expect(passes.length, `axe checked ${what} (${colorScheme})`).toBeGreaterThan(0);
    const serious = violations
      .filter((v) => v.impact === 'serious' || v.impact === 'critical')
      .map((v) => {
        const nodes = v.nodes.map(
          (n) => `${n.target.join(' ')}: ${n.html.slice(0, 120)}\n      ${n.failureSummary ?? ''}`,
        );
        return `${v.id} (${v.impact ?? ''}): ${v.help}\n    ${nodes.join('\n    ')}`;
      });
    expect(serious, `axe on ${what} (${colorScheme})`).toEqual([]);
  }
  await page.emulateMedia({ colorScheme: null, reducedMotion: null });
}

/** The page's own <h1> is there: the page has rendered (05 §16.6: focus moves to it after a route change). */
async function pageReady(page: Page, name: string | RegExp): Promise<void> {
  await expect(page.getByRole('heading', { level: 1, name })).toBeVisible();
}

/** A browser context that is nobody yet: the pages a friend opens before an account exists. */
async function anonymous(browser: { newContext(): Promise<BrowserContext> }): Promise<Page> {
  const context = await browser.newContext();
  return context.newPage();
}

test('the login, invite and reset pages', async ({ server, browser }) => {
  const page = await anonymous(browser);
  try {
    await page.goto(`${server.url}/login`);
    await pageReady(page, /log in/i);
    await expectAccessible(page, 'the login page');

    const invite = await server.createInvite({ note: 'a11y', maxUses: 1 });
    await page.goto(invite.url);
    await expect(page.getByLabel('Username')).toBeVisible();
    await expectAccessible(page, 'the invite page');

    await page.goto(await resetLink(server, browser));
    await expect(page.getByLabel(/new password/i).first()).toBeVisible();
    await expectAccessible(page, 'the reset page');
  } finally {
    await page.context().close();
  }
});

test('the setup page', async ({ startServer, browser }) => {
  const own = await startServer({ setUp: false });
  const page = await anonymous(browser);
  try {
    await page.goto(await own.setupUrl());
    await expect(page.getByLabel('Username')).toBeVisible();
    await expectAccessible(page, 'the setup page');
  } finally {
    await page.context().close();
  }
});

/**
 * The app's start of a device flow from `from` (POST /api/v1/device/code, 02 §5.2), as its Go code sends it: no
 * cookie, no Origin. Returns the link the app shows, with the code.
 */
async function startLinkFlow(server: E2EServer, from: string): Promise<string> {
  const res = await fetch(`${server.url}/api/v1/device/code`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Forwarded-For': from },
    body: JSON.stringify({ clientKind: 'desktop', deviceName: 'DESKTOP-A11Y', os: 'windows', appVersion: '0.2.0' }),
  });
  expect(res.status, 'POST /api/v1/device/code').toBe(200);
  const flow = (await res.json()) as { verificationUriComplete: string };
  return flow.verificationUriComplete;
}

test('the device-link page (M2 02 §10.1): every state', async ({ server, page }) => {
  const account = await server.createUser();
  await server.signIn(page.context(), account);
  // The confirm card's network note compares the browser's address with the app's (02 §5.3). The e2e server reads
  // both from X-Forwarded-For (fixtures.ts); link.spec uses 198.18.0.0/16 and 2001:db8:18::/48, this test
  // 198.19.0.0/16 and 2001:db8:19::/48.
  const browserIp = '198.19.0.1';
  await page.context().setExtraHTTPHeaders({ 'X-Forwarded-For': browserIp });

  await page.goto(`${server.url}/link`);
  await pageReady(page, 'Enter the code shown in the isshoni app');
  await expectAccessible(page, 'the link page: enter');

  // A code nobody was given: invalid, with the code field.
  await page.getByLabel('Code').fill('bcdfbcdf');
  await page.getByRole('button', { name: 'Continue' }).click();
  await pageReady(page, "This code doesn't work");
  await expectAccessible(page, 'the link page: invalid');

  // `different`: the app on another IPv4 address. The warning and the tick; Link anyway, then linked.
  await page.goto(await startLinkFlow(server, '198.19.0.2'));
  await pageReady(page, 'Link the isshoni app?');
  await expect(page.getByRole('checkbox', { name: 'I started this myself' })).toBeVisible();
  await expectAccessible(page, 'the link page: confirm, from a different network');

  await page.getByRole('checkbox', { name: 'I started this myself' }).check();
  await page.getByRole('button', { name: 'Link anyway' }).click();
  await pageReady(page, 'Linked');
  await expectAccessible(page, 'the link page: linked');

  // `same`: the app on the browser's own address. No note and no tick; Don't link, then denied.
  await page.goto(await startLinkFlow(server, browserIp));
  await pageReady(page, 'Link the isshoni app?');
  await expect(page.getByRole('checkbox')).toHaveCount(0);
  await expectAccessible(page, 'the link page: confirm, from the same network');

  await page.getByRole('button', { name: "Don't link" }).click();
  await pageReady(page, 'Not linked');
  await expectAccessible(page, 'the link page: denied');

  // `unknown`: the app over IPv6, the browser over IPv4. The softer note, and the tick.
  await page.goto(await startLinkFlow(server, '2001:db8:19::1'));
  await pageReady(page, 'Link the isshoni app?');
  await expect(page.getByText("isshoni couldn't check whether this request came from your network.")).toBeVisible();
  await expect(page.getByRole('checkbox', { name: 'I started this myself' })).toBeVisible();
  await expectAccessible(page, 'the link page: confirm, from an unknown network');

  // Wrong codes until this user's device-lookup bucket is empty (10, then 1 per minute, 02 §5.3; the invalid code
  // above spent one, the found codes were refunded), then rate_limited. The bound leaves room for refills on a slow
  // run.
  await page.goto(`${server.url}/link`);
  await pageReady(page, 'Enter the code shown in the isshoni app');
  for (let tries = 1; ; tries++) {
    expect(tries, 'wrong codes until the lookup answers 429').toBeLessThanOrEqual(15);
    await page.getByLabel('Code').fill('bcdfbcdf');
    const answer = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/v1/device/lookup');
    await page.getByRole('button', { name: 'Continue' }).click();
    if ((await answer).status() === 429) break;
    await pageReady(page, "This code doesn't work");
  }
  await pageReady(page, 'Too many wrong codes');
  await expectAccessible(page, 'the link page: rate limited');

  // error: a lookup the server couldn't answer, with its request id and Try again.
  await page.route('**/api/v1/device/lookup', (route) =>
    route.fulfill({
      status: 500,
      contentType: 'application/json',
      body: JSON.stringify({ error: { code: 'internal', requestId: 'req-a11y' } }),
    }),
  );
  await page.goto(`${server.url}/link?code=BCDF-BCDF`);
  await expect(page.getByRole('button', { name: 'Try again' })).toBeVisible();
  await expectAccessible(page, 'the link page: error');
});

test('the admin’s user list', async ({ server, page }) => {
  const admin = server.admin;
  if (!admin) throw new Error('the e2e server has no admin');
  await server.signIn(page.context(), admin);
  await page.goto(`${server.url}/admin/users`);
  await pageReady(page, 'Users');
  await expect(page.getByRole('row', { name: new RegExp(`^${admin.username} You `) })).toBeVisible();
  await expectAccessible(page, 'the admin users page');
});

test('the room with a share: axe, the tile list by keyboard, F for fullscreen, the debug overlay', async ({
  startServer,
  asUser,
  page,
}) => {
  const own = await startServer();
  const admin = own.admin;
  if (!admin) throw new Error('the e2e server has no admin');

  // A friend shares a window; this page shares too, so its own preview is a tile next to the friend's stage.
  const friend = await asUser(own);
  await fakeDisplay(friend.page, 'window+audio');
  await friend.page.goto(`${own.url}/r/lounge`);
  await waitForRoom(friend.page, 'lounge');
  await share(friend.page);

  await own.signIn(page.context(), admin);
  await fakeDisplay(page, 'window');
  await page.goto(`${own.url}/r/lounge`);
  await waitForRoom(page, 'lounge');
  await share(page);
  const stage = page.getByRole('region', { name: `Now watching: ${friend.account.username}'s window` });
  await expect(stage).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await waitForState(page, (s) => s.viewer.media === 'connected' && s, { timeout: MEDIA_TIMEOUT_MS });
  await expectAccessible(page, 'the room with a share');

  // The tile list is one tab stop (05 §12.7): Tab reaches a tile of it from the top of the page.
  const tiles = page.getByRole('list', { name: 'Shares' });
  await expect(tiles.getByRole('listitem')).toHaveCount(1);
  await page.locator('body').focus();
  let reached = false;
  for (let i = 0; i < 40 && !reached; i++) {
    await page.keyboard.press('Tab');
    reached = await tiles.evaluate((list) => list.contains(document.activeElement));
  }
  expect(reached, 'Tab reaches the tile list').toBe(true);
  await expect(tiles.getByRole('button', { name: /^Show your preview/ })).toBeFocused();

  // F switches the stage's fullscreen, from anywhere but a text field (05 §12.7).
  await page.keyboard.press('f');
  await expect.poll(async () => (await readState(page)).viewer.fullscreen).toBe(true);
  await page.keyboard.press('f');
  await expect.poll(async () => (await readState(page)).viewer.fullscreen).toBe(false);

  // The debug overlay (Shift+D, 05 §10.7) over the room, with its own heading and close button. It shows both of the
  // page's PCs, the friend's tile as the page receives it and as the server forwards it (stats.watch), and what the
  // page sends; nowhere an IP address.
  await page.keyboard.press('Shift+D');
  const overlay = page.getByRole('complementary', { name: 'Stats for nerds' });
  await expect(overlay).toBeVisible();
  const friendShare = (await readState(page)).viewer.shares.find((s) => !s.local)?.id;
  if (friendShare === undefined) throw new Error('the friend’s share is not on the page');
  const tile = overlay.locator(`[data-debug-share="${friendShare}"]`);
  await expect(tile.locator('dt')).toContainText([/^Video$/, /^Server video$/], { timeout: MEDIA_TIMEOUT_MS });
  const pcs = overlay.getByRole('region', { name: 'Connections' }).locator('dt');
  await expect(pcs).toContainText([/^sub PC/, /^pub PC/]);
  await expect(overlay.getByRole('region', { name: 'Sending' })).toBeVisible();
  expect(await overlay.innerText(), 'the overlay shows no IP address').not.toMatch(IP);
  await expectAccessible(page, 'the debug overlay', '[data-debug-overlay]');
  await overlay.getByRole('button', { name: 'Close the stats' }).click();
  await expect(overlay).toHaveCount(0);
});

/** Clicks Share in the header and Share in the sheet, with the page in front (05 §19.3); returns once it is live. */
async function share(page: Page): Promise<void> {
  await page.bringToFront();
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
  await waitForState(page, (s) => s.viewer.shares.some((x) => x.local && x.status === 'live') && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the page’s share is live',
  });
}

/**
 * A one-time password reset link for a new member, made by the admin (POST …/password-reset, 03 §7.10), from a
 * browser context of its own.
 */
async function resetLink(server: E2EServer, browser: { newContext(): Promise<BrowserContext> }): Promise<string> {
  const admin = server.admin;
  if (!admin) throw new Error('the e2e server has no admin');
  const member = await server.createUser();
  const context = await browser.newContext();
  try {
    await server.signIn(context, admin);
    const res = await context.request.post(`${server.url}/api/v1/admin/users/${member.id}/password-reset`, {
      data: {},
    });
    expect(res.status(), 'POST …/password-reset').toBe(201);
    const body = (await res.json()) as { url: string };
    return body.url;
  } finally {
    await context.close();
  }
}
