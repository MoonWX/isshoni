// desktop-relink.spec (M2 03 §8.3, §22.4 A; README WA71): the active server signs the desktop app out, and the app's
// pages follow, with its real Go parts (internal/desktop/e2ehost) and the desktop SPA in Chrome at
// http://wails.localhost, against a real e2e server:
// - the friend removes this PC on the Devices page in their browser: the server closes the page's socket, the SPA
//   tells Go (Auth.SessionEnded), 02's refresh gets invalid_grant, and the page goes to /desktop/relink with "This
//   PC was signed out of {server} …"; Link again shows a code, which the friend approves at /link → linked, and the
//   page goes back to a room;
// - Remove server on the relink page drops the server (Auth.RemoveServer), and the window reloads into the wizard's
//   server step with no server listed;
// - Switch server lists the servers; Sign out of the active one (Auth.Unlink) keeps it as Not linked.
//
// Each test starts a server of its own: the app's requests come from 127.0.0.1 and spend that address's budgets.
import type { Page } from '@playwright/test';

import { expect, test, type DesktopApp, type E2EServer, type UserSession } from './fixtures.ts';

test.describe.configure({ timeout: 120_000 });

/** C1's close, Go's refresh and the page's route change; and a device-flow approval within two 5 s polls. */
const FOLLOW_TIMEOUT_MS = 30_000;

/** The harness names the PC this way (e2ehost's AppInfo.DeviceName). */
const DEVICE_NAME = 'e2e PC';

/** A desktop app linked to server as the friend's account (02 §14), its page in a room. */
async function linkedDesktop(
  startDesktop: (o?: { server?: string }) => Promise<DesktopApp>,
  server: E2EServer,
  friend: UserSession,
): Promise<DesktopApp> {
  const desktop = await startDesktop({ server: server.url });
  const st = await desktop.call<{ status: string }>(
    'Auth',
    'LinkWithPassword',
    friend.account.username,
    friend.account.password,
  );
  expect(st.status).toBe('linked');
  await desktop.page.goto(`${desktop.origin}/r/lounge`);
  await expect(desktop.page.getByRole('banner')).toBeVisible();
  return desktop;
}

/** The friend removes this PC on the Devices page. */
async function removeDevice(page: Page, server: E2EServer): Promise<void> {
  await page.goto(`${server.url}/account/devices`);
  const row = page
    .getByRole('region', { name: 'Linked devices' })
    .getByRole('listitem')
    .filter({ hasText: DEVICE_NAME });
  await row.getByRole('button', { name: 'Remove' }).click();
  await page
    .getByRole('dialog', { name: `Remove ${DEVICE_NAME}?` })
    .getByRole('button', { name: 'Remove' })
    .click();
  await expect(row).toHaveCount(0);
}

/** The relink page, for the signed_out notice. */
async function expectSignedOut(desktop: DesktopApp): Promise<void> {
  const { page } = desktop;
  await expect(page).toHaveURL(`${desktop.origin}/desktop/relink`, { timeout: FOLLOW_TIMEOUT_MS });
  await expect(page.getByRole('status').filter({ hasText: /^This PC was signed out of / })).toContainText(
    '(removed on the Devices page, or not used for 90 days)',
  );
  const st = await desktop.call<{ status: string; notice: string }>('Auth', 'State');
  expect(st).toMatchObject({ status: 'unlinked', notice: 'signed_out' });
}

test('removed on the Devices page: the relink page says why, and Link again links the PC again', async ({
  startServer,
  startDesktop,
  asUser,
}) => {
  const server = await startServer();
  const friend = await asUser(server);
  const desktop = await linkedDesktop(startDesktop, server, friend);
  const { page } = desktop;

  await removeDevice(friend.page, server);
  await expectSignedOut(desktop);
  for (const name of ['Link again', 'Switch server', 'Remove server']) {
    await expect(page.getByRole('button', { name, exact: true })).toBeVisible();
  }

  await page.getByRole('button', { name: 'Link again', exact: true }).click();
  await expect(page).toHaveURL(`${desktop.origin}/desktop/setup`);
  const code = page.getByTestId('link-code');
  await expect(code).toHaveText(/^[A-Z]{4}-[A-Z]{4}$/);
  const userCode = (await code.textContent()) ?? '';
  const complete = `${server.url}/link?code=${userCode}`;
  await expect.poll(async () => (await desktop.state()).openedUrls).toContain(complete);

  await friend.page.goto(complete);
  await expect(friend.page.getByRole('heading', { level: 1, name: 'Link the isshoni app?' })).toBeVisible();
  const tick = friend.page.getByRole('checkbox', { name: 'I started this myself' });
  if ((await tick.count()) > 0) await tick.check();
  await friend.page.getByRole('button', { name: /^Link( anyway)?$/ }).click();
  await expect(friend.page.getByRole('heading', { level: 1, name: 'Linked' })).toBeVisible();

  await expect(page).toHaveURL(/\/r\/[^/]+$/, { timeout: FOLLOW_TIMEOUT_MS });
  const st = await desktop.call<{ status: string; notice: string }>('Auth', 'State');
  expect(st).toMatchObject({ status: 'linked', notice: '' });
});

test('Remove server on the relink page: the server goes, and the window reloads into the wizard', async ({
  startServer,
  startDesktop,
  asUser,
}) => {
  const server = await startServer();
  const friend = await asUser(server);
  const desktop = await linkedDesktop(startDesktop, server, friend);
  const { page } = desktop;
  await removeDevice(friend.page, server);
  await expectSignedOut(desktop);

  // Switch server shows the list: this server, Not linked, active.
  await page.getByRole('button', { name: 'Switch server', exact: true }).click();
  const list = page.getByRole('region', { name: 'Your servers' });
  await expect(list.getByRole('listitem')).toHaveCount(1);
  await expect(list.getByRole('listitem')).toContainText('Not linked');

  const before = (await desktop.state()).reloads;
  await page.getByRole('button', { name: 'Remove server', exact: true }).click();
  await expect.poll(async () => (await desktop.state()).reloads).toBeGreaterThan(before);
  expect(await desktop.call<unknown[]>('Auth', 'Servers')).toEqual([]);
  await page.reload();
  await expect(page).toHaveURL(`${desktop.origin}/desktop/setup`);
  await expect(page.getByLabel('Your isshoni server')).toBeVisible();
});

test('Sign out (Auth.Unlink) keeps the active server as Not linked, and the page follows to the link step', async ({
  startServer,
  startDesktop,
  asUser,
}) => {
  const server = await startServer();
  const friend = await asUser(server);
  const desktop = await linkedDesktop(startDesktop, server, friend);
  const { page } = desktop;
  await removeDevice(friend.page, server);
  await expectSignedOut(desktop);
  // Link again by password through the bridge, as the link step's form would, then sign out.
  const st = await desktop.call<{ status: string }>(
    'Auth',
    'LinkWithPassword',
    friend.account.username,
    friend.account.password,
  );
  expect(st.status).toBe('linked');
  await page.goto(`${desktop.origin}/desktop/relink`);
  // Linked: the relink page has nothing to say and hands over to the wizard, which goes to the room.
  await expect(page).toHaveURL(/\/r\/[^/]+$/, { timeout: FOLLOW_TIMEOUT_MS });

  const res = await desktop.call<{ confirmed: boolean }>('Auth', 'Unlink');
  expect(res.confirmed).toBe(true);
  const after = await desktop.call<{ status: string; notice: string }>('Auth', 'State');
  expect(after.status).toBe('unlinked');
  const listed = await desktop.call<{ origin: string; linked: boolean; active: boolean }[]>('Auth', 'Servers');
  expect(listed).toEqual([expect.objectContaining({ origin: server.url, linked: false, active: true })]);
  // The page follows auth.state to the link step.
  await expect(page).toHaveURL(`${desktop.origin}/desktop/setup`, { timeout: FOLLOW_TIMEOUT_MS });
});
