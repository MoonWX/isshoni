// desktop-link.spec (M2 03 §8.1, §8.2, §22.4 A; README WA71): the first-run wizard of the desktop app, with its real
// Go parts (internal/desktop/e2ehost: 02's servers and managers, the app's AddServer and its reload) and the desktop
// SPA in Chrome at http://wails.localhost, against real e2e servers:
// - an address → the confirm step → Link: AddServer, the window reloads (the harness counts Shell.Reload, and the
//   spec reloads Chrome's page, as the shell would), the link step shows the code, its QR code and the short URL, and
//   the app opened the browser at /link?code=…; a second browser context, signed in, approves there → linked, and
//   the wizard goes to the room;
// - a code declined in the browser: "Linking was declined in the browser", and Try again gets a new code;
// - an unknown server from the command line (--server, as a link or PendingServer would bring it): "Add server
//   {host}?" with no connection to that server before Add (a counting TCP proxy stands in front of it); its /info
//   names another host, so the confirm step says "This server calls itself …";
// - "Sign in with username and password instead": a wrong password, then the right one;
// - the second server: with A active and B added and linked, no request and no hello of the page to A ever carries
//   a token that the app handed out for B (the harness records every token's hash with the active server, 03 §5.8).
//
// Every test starts servers of its own: the app's device-flow and password requests come from 127.0.0.1, whose
// device-code-ip and auth-ip budgets the worker's server shares with other specs.
import { createHash } from 'node:crypto';
import net from 'node:net';
import type { Page } from '@playwright/test';

import { expect, test, type DesktopApp } from './fixtures.ts';

test.describe.configure({ timeout: 120_000 });

/** The device flow's poll interval is 5 s; an approval reaches the app within two of them. */
const LINKED_TIMEOUT_MS = 30_000;

/** The host[:port] of a URL. */
function hostOf(url: string): string {
  return new URL(url).host;
}

/** Waits until the app reloaded the window (Shell.Reload) more than `before` times, then reloads Chrome's page. */
async function followReload(desktop: DesktopApp, before: number): Promise<void> {
  await expect
    .poll(async () => (await desktop.state()).reloads, { message: 'the app reloads the window' })
    .toBeGreaterThan(before);
  await desktop.page.reload();
}

/** The wizard's server step, typed address, the confirm step's Link and the reload into the link step. */
async function addByAddress(desktop: DesktopApp, address: string): Promise<void> {
  const { page } = desktop;
  const field = page.getByLabel('Your isshoni server');
  await expect(field).toBeFocused();
  await field.fill(address);
  await field.press('Enter');
  await expect(page.getByRole('heading', { name: `Link isshoni to` })).toContainText(`(${hostOf(address)})?`);
  const before = (await desktop.state()).reloads;
  await page.getByRole('button', { name: 'Link', exact: true }).click();
  await followReload(desktop, before);
}

/** The code the link step shows, once it does. */
async function shownCode(page: Page): Promise<string> {
  const code = page.getByTestId('link-code');
  await expect(code).toHaveText(/^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$/, { timeout: 15_000 });
  return (await code.textContent()) ?? '';
}

/** The friend's browser opens the approval page and answers it. */
async function answer(page: Page, url: string, approve: boolean): Promise<void> {
  await page.goto(url);
  await expect(page.getByRole('heading', { level: 1, name: 'Link the isshoni app?' })).toBeVisible();
  if (!approve) {
    await page.getByRole('button', { name: "Don't link" }).click();
    await expect(page.getByRole('heading', { level: 1, name: 'Not linked' })).toBeVisible();
    return;
  }
  const tick = page.getByRole('checkbox', { name: 'I started this myself' });
  if ((await tick.count()) > 0) await tick.check();
  await page.getByRole('button', { name: /^Link( anyway)?$/ }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Linked' })).toBeVisible();
}

/** The app is linked to origin, and the wizard went on to a room. */
async function expectLinked(desktop: DesktopApp, origin: string): Promise<void> {
  await expect(desktop.page).toHaveURL(/\/r\/[^/]+$/, { timeout: LINKED_TIMEOUT_MS });
  const st = await desktop.call<{ origin: string; status: string }>('Auth', 'State');
  expect(st).toMatchObject({ origin, status: 'linked' });
}

test('address → confirm → code, QR code and short URL → approved in a second browser context → linked', async ({
  startServer,
  startDesktop,
  asUser,
}) => {
  const server = await startServer();
  const friend = await asUser(server);
  const desktop = await startDesktop();
  const { page } = desktop;
  await page.goto(`${desktop.origin}/`);
  await expect(page).toHaveURL(`${desktop.origin}/desktop/setup`);
  await expect(page.getByText(/Installed from your isshoni page\? Go back to it and click/)).toBeVisible();

  // A room URL of the server works as its address.
  await addByAddress(desktop, `${server.url}/r/lounge`);
  // The reloaded page dropped ?link=1 and asked for a code once.
  const code = await shownCode(page);
  await expect(page).toHaveURL(`${desktop.origin}/desktop/setup`);
  const host = hostOf(server.url);
  await expect(page.getByRole('heading', { level: 1 })).toContainText(`(${host})`);
  await expect(page.getByText(`or open ${host}/link and enter ${code}`)).toBeVisible();
  await expect(page.getByText(/^Expires in \d+:\d\d$/)).toBeVisible();
  const qr = page.getByRole('img', { name: 'QR code of the link page' });
  await expect(qr).toBeVisible();
  expect(await qr.locator('path').count()).toBeGreaterThan(0);
  await expect(page.getByText(/there's no recording and no telemetry/)).toBeVisible();

  // Go opened the browser once, at the canonical URI; "Open browser again" repeats it.
  const complete = `${server.url}/link?code=${code}`;
  await expect.poll(async () => (await desktop.state()).openedUrls).toEqual([complete]);
  await page.getByRole('button', { name: 'Open browser again' }).click();
  await expect.poll(async () => (await desktop.state()).openedUrls).toEqual([complete, complete]);

  await answer(friend.page, complete, true);
  await expectLinked(desktop, server.url);
  const st = await desktop.call<{ user: { username: string } | null }>('Auth', 'State');
  expect(st.user?.username).toBe(friend.account.username);
});

test('a code declined in the browser: the notice, and Try again gets a new code', async ({
  startServer,
  startDesktop,
  asUser,
}) => {
  const server = await startServer();
  const friend = await asUser(server);
  const desktop = await startDesktop();
  const { page } = desktop;
  await page.goto(`${desktop.origin}/`);
  await addByAddress(desktop, server.url);
  const code = await shownCode(page);

  await answer(friend.page, `${server.url}/link?code=${code}`, false);
  await expect(page.getByRole('alert')).toHaveText('Linking was declined in the browser.', {
    timeout: LINKED_TIMEOUT_MS,
  });
  const st = await desktop.call<{ status: string; notice: string }>('Auth', 'State');
  expect(st).toMatchObject({ status: 'unlinked', notice: 'denied' });

  await page.getByRole('button', { name: 'Try again' }).click();
  const next = await shownCode(page);
  expect(next).not.toBe(code);
});

/** A TCP proxy in front of a server that counts the connections made through it. */
async function countingProxy(target: string): Promise<{ url: string; connections: () => number; close(): void }> {
  const to = new URL(target);
  let count = 0;
  const sockets = new Set<net.Socket>();
  const proxy = net.createServer((client) => {
    count++;
    const upstream = net.connect(Number(to.port), to.hostname);
    for (const s of [client, upstream]) {
      sockets.add(s);
      s.on('close', () => sockets.delete(s));
      s.on('error', () => {
        client.destroy();
        upstream.destroy();
      });
    }
    client.pipe(upstream).pipe(client);
  });
  await new Promise<void>((resolve) => proxy.listen(0, '127.0.0.1', resolve));
  const addr = proxy.address() as net.AddressInfo;
  return {
    url: `http://127.0.0.1:${String(addr.port)}`,
    connections: () => count,
    close: () => {
      for (const s of sockets) s.destroy();
      proxy.close();
    },
  };
}

test('an unknown server shows "Add server {host}?" with no request before Add; then the password path, wrong and right', async ({
  startServer,
  startDesktop,
}) => {
  const server = await startServer();
  const account = await server.createUser();
  const proxy = await countingProxy(server.url);
  try {
    const desktop = await startDesktop({ args: ['--server', proxy.url] });
    const { page } = desktop;
    await page.goto(`${desktop.origin}/`);
    const shown = hostOf(proxy.url);
    await expect(page.getByRole('heading', { level: 1, name: `Add server ${shown}?` })).toBeVisible();
    await expect(
      page.getByText("Only add servers you trust. The server's admin can see what you share."),
    ).toBeVisible();
    // Nothing went to the server yet, not even after a while, and the app lists no server.
    await page.waitForTimeout(1000);
    expect(proxy.connections(), 'connections to the server before Add').toBe(0);
    expect(await desktop.call<unknown[]>('Auth', 'Servers')).toEqual([]);

    // Add probes; the server's /info names its own address, another host: the confirm step asks first.
    await page.getByRole('button', { name: 'Add', exact: true }).click();
    await expect(page.getByText(`This server calls itself ${hostOf(server.url)}.`)).toBeVisible();
    expect(proxy.connections()).toBeGreaterThan(0);
    const before = (await desktop.state()).reloads;
    await page.getByRole('button', { name: 'Link', exact: true }).click();
    await followReload(desktop, before);
    await shownCode(page);
    // The server stored is the one /info named.
    const listed = await desktop.call<{ origin: string; active: boolean }[]>('Auth', 'Servers');
    expect(listed).toEqual([expect.objectContaining({ origin: server.url, active: true })]);

    // "Sign in with username and password instead": a wrong password first.
    await page.getByRole('button', { name: 'Sign in with username and password instead' }).click();
    const username = page.getByLabel('Username');
    const password = page.getByLabel('Password', { exact: true });
    await expect(username).toHaveAttribute('autocomplete', 'off');
    await expect(password).toHaveAttribute('autocomplete', 'off');
    await username.fill(account.username);
    await password.fill(`${account.password}-wrong`);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('alert')).toHaveText('Wrong username or password.');
    await expect(password).toHaveValue('');
    await expect(username).toHaveValue(account.username);

    await password.fill(account.password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expectLinked(desktop, server.url);
    // The add question was answered although the server stored is another host: the app takes the next link.
    expect(await desktop.call('App', 'PendingLink')).toBeNull();
  } finally {
    proxy.close();
  }
});

/** The first 8 bytes of SHA-256 in hex: how the harness records a token (e2ehost's tokenRecord). */
function tokenHash(token: string): string {
  return createHash('sha256').update(token).digest('hex').slice(0, 16);
}

/** Records every bearer token the page sends to origin: REST Authorization headers and the hello of /ws. */
function recordTokensTo(page: Page, origin: string): string[] {
  const seen: string[] = [];
  page.on('request', (req) => {
    if (!req.url().startsWith(`${origin}/`)) return;
    const auth = req.headers()['authorization'];
    if (auth?.startsWith('Bearer ')) seen.push(auth.slice('Bearer '.length));
  });
  const ws = origin.replace(/^http/, 'ws');
  page.on('websocket', (socket) => {
    if (!socket.url().startsWith(`${ws}/`)) return;
    socket.on('framesent', (f) => {
      try {
        const msg = JSON.parse(String(f.payload)) as { type?: string; data?: { auth?: { token?: string } } };
        const token = msg.data?.auth?.token;
        if (msg.type === 'hello' && typeof token === 'string') seen.push(token);
      } catch {
        // not JSON
      }
    });
  });
  return seen;
}

test('the second server: with A active and B added and linked, nothing to A carries a token B issued', async ({
  startServer,
  startDesktop,
}) => {
  const [a, b] = await Promise.all([startServer(), startServer()]);
  const [accountA, accountB] = await Promise.all([a.createUser(), b.createUser()]);
  const desktop = await startDesktop({ server: a.url });
  const { page } = desktop;
  const toA = recordTokensTo(page, a.url);
  const toB = recordTokensTo(page, b.url);

  const linked = await desktop.call<{ status: string }>(
    'Auth',
    'LinkWithPassword',
    accountA.username,
    accountA.password,
  );
  expect(linked.status).toBe('linked');
  await page.goto(`${desktop.origin}/r/lounge`);
  await expect(page.getByRole('banner')).toBeVisible();
  await expect.poll(() => toA.length, { message: 'the page talks to A with a token' }).toBeGreaterThan(0);

  // Settings → Servers' "Add a server": the wizard's server step, for B.
  await page.goto(`${desktop.origin}/desktop/setup?new=1`);
  await addByAddress(desktop, b.url);
  await shownCode(page);
  await page.getByRole('button', { name: 'Sign in with username and password instead' }).click();
  await page.getByLabel('Username').fill(accountB.username);
  await page.getByLabel('Password', { exact: true }).fill(accountB.password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expectLinked(desktop, b.url);
  await expect.poll(() => toB.length, { message: 'the page talks to B with a token' }).toBeGreaterThan(0);

  // Back to A: SwitchServer reloads the window.
  const before = (await desktop.state()).reloads;
  const seenOnA = toA.length;
  await desktop.call('Auth', 'SwitchServer', a.url);
  await page.evaluate(() => {
    history.replaceState(history.state, '', '/r/lounge');
  });
  await followReload(desktop, before);
  await expect(page.getByRole('banner')).toBeVisible();
  await expect.poll(() => toA.length, { message: 'the page talks to A again' }).toBeGreaterThan(seenOnA);
  // Give A's page time for its socket and the token refreshes it may do.
  await page.waitForTimeout(2000);

  const { tokens } = await desktop.state();
  // Every token went to the page of the server it was issued for, while that server was active.
  for (const t of tokens)
    expect(t.origin, `a token handed out while ${t.activeOrigin} was active`).toBe(t.activeOrigin);
  const issuedForB = new Set(tokens.filter((t) => t.activeOrigin === b.url).map((t) => t.sha256));
  const issuedForA = new Set(tokens.filter((t) => t.activeOrigin === a.url).map((t) => t.sha256));
  expect(issuedForB.size, 'tokens handed out for B').toBeGreaterThan(0);
  const toAHashes = toA.map(tokenHash);
  expect(
    toAHashes.filter((h) => issuedForB.has(h)),
    'requests and hellos to A that carry a token of B',
  ).toEqual([]);
  expect(
    toAHashes.every((h) => issuedForA.has(h)),
    'everything to A carries a token handed out for A',
  ).toBe(true);
  expect(
    toB.map(tokenHash).filter((h) => issuedForA.has(h)),
    'requests to B that carry a token of A',
  ).toEqual([]);
});
