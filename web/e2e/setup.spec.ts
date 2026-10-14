// setup.spec (05 §19.3; 05 §14, 04 §7.7): the setup wizard on a fresh server, from the one-time setup link to the
// Lounge: the admin is created (step 1), the connection test finds UDP and TCP working with the TCP 443 row hidden,
// because an off-mode server has no 443 listener (step 2), an invite link comes with its QR code (step 3), and
// "Done" sets setupWizardDone. Then two servers of their own for the connection test's other faces: one with UDP
// turned off in the config, and one whose UDP the browser can't reach, where the admin gets fix text and a member
// gets "Send this to your admin" and no fix text.
//
// Every test starts its own server: the wizard needs one that isn't set up, and the others a config or a network
// of their own. TCP 443 itself ("TCP 443 ✓") is covered in Go, with TLS (04 §17).
import type { BrowserContext, Locator, Page } from '@playwright/test';

import { expect, test, uniqueName, type E2EServer } from './fixtures.ts';
import { waitForRoom } from './stats.ts';

/** A connection test runs its probes for up to 8 s each, in parallel, and the reply may take 10 s (05 §14.2). */
const TEST_TIMEOUT_MS = 20_000;

/** The connection test's panel on a page or in a dialog. */
function connTest(scope: Page | Locator): Locator {
  return scope.getByRole('region', { name: 'Connection test' });
}

/** One transport's row of the panel, by the start of its label ("Media over UDP", "Media over TCP (port 443)"). */
function row(panel: Locator, label: string): Locator {
  return panel.getByRole('listitem').filter({ hasText: label });
}

/** A row's label and state, without the note under it: ["Media over UDP (port 54321)", "Works"]. */
async function rowText(item: Locator): Promise<string[]> {
  return item.locator(':scope > span:not([aria-hidden])').allTextContents();
}

/**
 * The browser's UDP probes never reach the server, as behind a firewall that drops UDP: the probe's answer comes
 * without its candidates, so the browser has nowhere to send its checks and the probe times out after 8 s (✗). The
 * request still reaches the server, which makes the probe PC as before. TCP probes go through untouched.
 */
async function blockUdp(context: BrowserContext, server: E2EServer): Promise<void> {
  await context.route(`${server.url}/api/v1/conntest`, async (route) => {
    const body = route.request().postDataJSON() as { transport?: unknown } | null;
    if (body?.transport !== 'udp') {
      await route.continue();
      return;
    }
    const response = await route.fetch();
    if (response.status() !== 200) {
      await route.fulfill({ response });
      return;
    }
    const reply = (await response.json()) as { answer: string };
    const answer = reply.answer
      .split('\r\n')
      .filter((line) => !line.startsWith('a=candidate:'))
      .join('\r\n');
    await route.fulfill({ response, json: { ...reply, answer } });
  });
}

/** GET a JSON resource with the page's session. */
async function getJson<T>(page: Page, url: string): Promise<T> {
  const res = await page.request.get(url);
  expect(res.status(), `GET ${url}`).toBe(200);
  return (await res.json()) as T;
}

interface Me {
  user: { username: string; role: string };
}
interface Invites {
  invites: { state: string; maxUses: number; uses: number }[];
}
interface SettingsResponse {
  settings: { setupWizardDone: boolean };
}

test('the wizard: create the admin, test the connection, invite friends, done', async ({ startServer, page }) => {
  const own = await startServer({ setUp: false });
  expect(own.admin).toBeNull();

  // Step 1, /setup#<token>: create the admin.
  await page.goto(await own.setupUrl());
  await expect(page.getByRole('heading', { name: 'Create the admin account' })).toBeVisible();
  const username = uniqueName('admin');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(`${uniqueName('correct-horse')}-staple`);
  await page.getByRole('button', { name: 'Create account' }).click();

  // Step 2, the connection test, which starts by itself.
  await expect(page).toHaveURL(`${own.url}/admin/welcome?step=2`);
  await expect(page.getByRole('heading', { name: 'Test the connection', level: 1 })).toBeVisible();
  const me = await getJson<Me>(page, `${own.url}/api/v1/me`);
  expect(me.user).toMatchObject({ username, role: 'admin' });

  const panel = connTest(page);
  await expect(panel.getByText('Friends get the best quality.')).toBeVisible({ timeout: TEST_TIMEOUT_MS });
  const udp = row(panel, 'Media over UDP');
  const tcp = row(panel, 'Media over TCP');
  expect(await rowText(udp)).toEqual([expect.stringMatching(/^Media over UDP \(port \d+\)$/), 'Works']);
  // One TCP row: 7882's (the server's ICE-TCP port; the e2e server has a free one), and none for 443.
  await expect(tcp).toHaveCount(1);
  expect(await rowText(tcp)).toEqual([expect.stringMatching(/^Media over TCP \(port \d+\)$/), 'Works']);
  await expect(row(panel, 'port 443')).toHaveCount(0);
  await expect(row(panel, 'Round trip')).toContainText(/\d+ ms/);

  // Green: "Continue" is the main action.
  await page.getByRole('button', { name: 'Continue' }).click();

  // Step 3: an invite with the server's defaults, its link and its QR code.
  await expect(page).toHaveURL(`${own.url}/admin/welcome?step=3`);
  await expect(page.getByRole('heading', { name: 'Invite your friends', level: 1 })).toBeVisible();
  const link = page.getByRole('textbox', { name: 'Invite link' });
  await expect(link).toHaveValue(new RegExp(`^${own.url.replaceAll('.', '\\.')}/invite#[A-Za-z0-9_-]{16,}$`));
  await expect(page.getByRole('img', { name: 'QR code of the invite link' })).toBeVisible();
  await expect(page.getByText(/^Works for up to 10 people, until /)).toBeVisible();
  const invites = await getJson<Invites>(page, `${own.url}/api/v1/invites`);
  expect(invites.invites).toEqual([expect.objectContaining({ state: 'active', maxUses: 10, uses: 0 })]);

  // Done: setupWizardDone, and the Lounge.
  await page.getByRole('button', { name: 'Done, go to the Lounge' }).click();
  await expect(page).toHaveURL(`${own.url}/r/lounge`);
  await waitForRoom(page, 'lounge');
  const settings = await getJson<SettingsResponse>(page, `${own.url}/api/v1/admin/settings`);
  expect(settings.settings.setupWizardDone).toBe(true);
});

test('with listen.ice_udp = "", the UDP row says that UDP is turned off', async ({ startServer, context, page }) => {
  const own = await startServer({ overrides: { 'listen.ice_udp': '' } });
  const admin = own.admin;
  if (!admin) throw new Error('the server has no admin');
  await own.signIn(context, admin);
  await page.goto(`${own.url}/admin/welcome?step=2`);

  const panel = connTest(page);
  await expect(panel.getByText('Works, but video may stutter on weak networks.')).toBeVisible({
    timeout: TEST_TIMEOUT_MS,
  });
  // ✗, without a port (the server has none), and why.
  const udp = row(panel, 'Media over UDP');
  expect(await rowText(udp)).toEqual(['Media over UDP', 'Turned off']);
  await expect(udp).toContainText('UDP media is turned off in the server config (listen.ice_udp).');
  expect(await rowText(row(panel, 'Media over TCP'))).toEqual([
    expect.stringMatching(/^Media over TCP \(port \d+\)$/),
    'Works',
  ]);
  await expect(row(panel, 'port 443')).toHaveCount(0);
  // Not green: "Test again" is the main action, and "Continue" is still there.
  await expect(page.getByRole('button', { name: 'Continue' })).toBeEnabled();
});

test('UDP blocked on the way: the admin gets fix text, a member gets none', async ({
  startServer,
  context,
  page,
  asUser,
}) => {
  const own = await startServer();
  const admin = own.admin;
  if (!admin) throw new Error('the server has no admin');

  // The admin, in the wizard's step 2.
  await blockUdp(context, own);
  await own.signIn(context, admin);
  await page.goto(`${own.url}/admin/welcome?step=2`);
  const panel = connTest(page);
  await expect(panel.getByText('Works, but video may stutter on weak networks.')).toBeVisible({
    timeout: TEST_TIMEOUT_MS,
  });
  const udpPort = /\(port (\d+)\)/.exec((await rowText(row(panel, 'Media over UDP')))[0] ?? '')?.[1];
  expect(udpPort).toBeDefined();
  expect((await rowText(row(panel, 'Media over UDP')))[1]).toBe('Blocked');
  expect((await rowText(row(panel, 'Media over TCP')))[1]).toBe('Works');
  const fix = panel.getByRole('region', { name: 'How to fix it' });
  await expect(fix).toBeVisible();
  await expect(fix.getByText(`Open UDP port ${String(udpPort)}.`)).toBeVisible();
  // The provider's line (the generic one on a machine that is no known cloud's) with its guide.
  await expect(fix.getByRole('link', { name: 'Firewall guide for this provider' })).toBeVisible();
  await expect(fix.getByText(`sudo ufw allow ${String(udpPort)}/udp`)).toBeVisible();
  await expect(panel.getByText('Send this to your admin:')).toHaveCount(0);

  // A member, with the same network, runs the test from the room's account menu.
  const friend = await asUser(own);
  await blockUdp(friend.context, own);
  await friend.page.goto(`${own.url}/`);
  await expect(friend.page).toHaveURL(`${own.url}/r/lounge`);
  await waitForRoom(friend.page, 'lounge');
  await friend.page.getByRole('button', { name: `Account menu, ${friend.account.username}` }).click();
  await friend.page.getByRole('button', { name: 'Test my connection' }).click();
  const dialog = friend.page.getByRole('dialog', { name: 'Test my connection' });
  await dialog.getByRole('button', { name: 'Test my connection' }).click();

  const theirs = connTest(dialog);
  await expect(theirs.getByText('Works, but video may stutter on weak networks.')).toBeVisible({
    timeout: TEST_TIMEOUT_MS,
  });
  expect((await rowText(row(theirs, 'Media over UDP')))[1]).toBe('Blocked');
  await expect(theirs.getByText('Send this to your admin:')).toBeVisible();
  await expect(theirs.getByRole('button', { name: 'Copy result' })).toBeVisible();
  await expect(theirs.getByRole('region', { name: 'How to fix it' })).toHaveCount(0);
  await expect(theirs.getByText(/sudo ufw|firewall-cmd|Open UDP port/)).toHaveCount(0);
  await expect(theirs.getByRole('link', { name: 'Firewall guide for this provider' })).toHaveCount(0);

  // And the wizard is not theirs.
  await friend.page.goto(`${own.url}/admin/welcome?step=2`);
  await expect(friend.page.getByRole('heading', { name: 'Nothing here' })).toBeVisible();
});
