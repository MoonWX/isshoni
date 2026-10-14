// version.spec (05 §19.3, §16.4; 01 §6.1): what the page does when it and the server are of different versions. The
// page's signaling socket goes through page.routeWebSocket, which rewrites what the server says:
// - `welcome.serverVersion` of another (non-dev) version: the shell is stale. The page reloads once, before it joins
//   the room, guarded by sessionStorage['isshoni.reloadedFor']; when the reload didn't help (the welcome after it is
//   still of another version) it keeps running, joins, and logs a warning: the handshake worked (05 §16.4).
// - The same while the page shares: no reload, which would end the share; the update pill says to reload after
//   sharing, and offers its Reload once the share is over.
// - `error {protocol_unsupported}` answering hello: the VersionMismatch screen at once. Its Reload reloads; when the
//   reload didn't help the screen stays, says so, and shows both versions. A server older than the page asks for
//   the admin, without a Reload.
//
// The stale-build path exists only between non-dev builds (01 §6.1): `task e2e` builds 0.0.0-e2e.local and CI its
// 0.0.0-ci.<run> (06 §7.2, §8.2). Against a dev build the tests of it are skipped, with a note.
import type { Page, WebSocketRoute } from '@playwright/test';

import { expect, fakeDisplay, test, type E2EServer } from './fixtures.ts';
import { readState, waitForRoom, waitForState } from './stats.ts';

/** The version the routed server claims: not a dev build, and not this build's. */
const OTHER_VERSION = '9.9.9';
const RELOADED_FOR_KEY = 'isshoni.reloadedFor';
/** A share that started may take this long to be live: its PeerConnection to the server. */
const MEDIA_TIMEOUT_MS = 20_000;

/** A message on the socket, as 01 §5 frames it. */
interface Envelope {
  type: string;
  id?: string;
  re?: string;
  data?: Record<string, unknown>;
}

/** The SPA's own version, from the build's /version.json (05 §17.1). */
async function spaVersion(server: Pick<E2EServer, 'url'>, page: Page): Promise<string> {
  const res = await page.request.get(`${server.url}/version.json`);
  expect(res.ok(), 'GET /version.json').toBe(true);
  return ((await res.json()) as { version: string }).version;
}

/** 04 §15: a SemVer prerelease that starts with `dev`. */
function isDevVersion(v: string): boolean {
  const core = v.split('+', 1)[0] ?? '';
  const dash = core.indexOf('-');
  return dash >= 0 && core.slice(dash + 1).startsWith('dev');
}

/** Skips the test against a dev build, where the stale-build path can't be taken. */
async function needsNonDevBuild(server: Pick<E2EServer, 'url'>, page: Page): Promise<string> {
  const version = await spaVersion(server, page);
  test.skip(
    isDevVersion(version),
    `the SPA is a dev build (${version}): run \`task e2e\`, which builds 0.0.0-e2e.local`,
  );
  expect(version).not.toBe(OTHER_VERSION);
  return version;
}

/** What the route saw on each of the page's sockets, oldest first. */
interface Sockets {
  /** The types the page sent on each socket. */
  readonly sent: string[][];
  /** The welcomes the page got. */
  welcomes: number;
}

/**
 * Routes the page's signaling sockets to the server, passing everything through, except that a welcome for which
 * `stale(n)` is true (n counts from 1) says OTHER_VERSION. Closes go through with their code, so a drop keeps the
 * server's grace (01 §4.2).
 */
async function routeSignaling(page: Page, stale: (n: number) => boolean): Promise<Sockets> {
  const seen: Sockets = { sent: [], welcomes: 0 };
  await page.routeWebSocket(/\/ws$/, (ws: WebSocketRoute) => {
    const sent: string[] = [];
    seen.sent.push(sent);
    const server = ws.connectToServer();
    ws.onMessage((message) => {
      sent.push((JSON.parse(String(message)) as Envelope).type);
      server.send(message);
    });
    server.onMessage((message) => {
      const env = JSON.parse(String(message)) as Envelope;
      if (env.type === 'welcome' && env.data && stale(++seen.welcomes)) {
        env.data['serverVersion'] = OTHER_VERSION;
        ws.send(JSON.stringify(env));
        return;
      }
      ws.send(message);
    });
    ws.onClose((code, reason) => {
      void server.close({ code, reason });
    });
    server.onClose((code, reason) => {
      void ws.close({ code, reason });
    });
  });
  return seen;
}

/**
 * Answers the page's hello itself, with error {protocol_unsupported} and close 4426 (01 §6.1), as a server that
 * speaks protocols serverMin to serverMax does. No socket reaches the server.
 */
async function routeUnsupported(page: Page, params: { serverMin: number; serverMax: number }): Promise<void> {
  await page.routeWebSocket(/\/ws$/, (ws: WebSocketRoute) => {
    ws.onMessage((message) => {
      const env = JSON.parse(String(message)) as Envelope;
      if (env.type !== 'hello') return;
      ws.send(
        JSON.stringify({
          type: 'error',
          ...(env.id !== undefined ? { re: env.id } : {}),
          data: {
            code: 'protocol_unsupported',
            scope: 'connection',
            retryable: false,
            params: { ...params, serverVersion: OTHER_VERSION },
          },
        }),
      );
      void ws.close({ code: 4426, reason: 'protocol_unsupported' });
    });
  });
}

/** Counts the page's loads (the first navigation and each reload). */
function countLoads(page: Page): () => number {
  let loads = 0;
  page.on('load', () => {
    loads++;
  });
  return () => loads;
}

async function signInAsNewUser(server: E2EServer, page: Page): Promise<void> {
  await server.signIn(page.context(), await server.createUser());
}

test('a welcome of another version: one reload before the room, then the page runs, with a warning', async ({
  server,
  page,
}) => {
  await needsNonDevBuild(server, page);
  await signInAsNewUser(server, page);
  const sockets = await routeSignaling(page, () => true);
  const loads = countLoads(page);
  const warnings: string[] = [];
  page.on('console', (msg) => {
    if (msg.type() === 'warning') warnings.push(msg.text());
  });

  await page.goto(`${server.url}/r/lounge`);
  // The reloaded page joins: the welcome after the reload is still of another version, and one reload is all.
  await waitForRoom(page, 'lounge');
  expect(loads()).toBe(2);
  expect(sockets.welcomes).toBe(2);
  // The first page reloaded before it joined: its socket never asked for the room.
  expect(sockets.sent.map((types) => types.filter((t) => t === 'room.join').length)).toEqual([0, 1]);
  expect(await page.evaluate((key) => sessionStorage.getItem(key), RELOADED_FOR_KEY)).toBe(OTHER_VERSION);
  expect(warnings.some((w) => w.includes('still a stale build after reloading'))).toBe(true);
  await expect(page.getByRole('heading', { name: 'This page is out of date' })).toHaveCount(0);

  // It stays: a later welcome (a reconnect) doesn't reload either.
  await page.evaluate(() => {
    window.__isshoni?.dropSocket();
  });
  await expect.poll(() => sockets.welcomes).toBe(3);
  await waitForState(page, (s) => s.connection.state === 'ready' && s.room.joinState === 'joined' && s);
  expect(loads()).toBe(2);
});

test('while the page shares, a welcome of another version brings the update pill instead of a reload', async ({
  startServer,
  page,
}) => {
  const own = await startServer();
  await needsNonDevBuild(own, page);
  const admin = own.admin;
  if (!admin) throw new Error('the e2e server has no admin');
  await own.signIn(page.context(), admin);
  await fakeDisplay(page, 'window');
  // The first welcome is of this build; the one after the drop below is not.
  const sockets = await routeSignaling(page, (n) => n > 1);
  const loads = countLoads(page);

  await page.goto(`${own.url}/r/lounge`);
  await waitForRoom(page, 'lounge');
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

  await page.evaluate(() => {
    window.__isshoni?.dropSocket();
  });
  await expect.poll(() => sockets.welcomes).toBe(2);
  const pill = page.getByRole('region', { name: 'App update' });
  await expect(pill).toContainText('Update ready. Reload after you stop sharing.');
  await expect(pill.getByRole('button', { name: 'Reload' })).toHaveCount(0);
  expect(loads()).toBe(1);
  const state = await readState(page);
  expect(state.viewer.shares.filter((s) => s.local).map((s) => s.status)).toEqual(['live']);

  // Once the share is over, the pill offers the reload.
  await page.getByRole('button', { name: 'Stop sharing', exact: true }).click();
  await expect(pill).toContainText('Update ready');
  await expect(pill.getByRole('button', { name: 'Reload' })).toBeVisible();
  expect(loads()).toBe(1);
});

test('protocol_unsupported shows VersionMismatch at once; a Reload that didn’t help keeps it, with both versions', async ({
  server,
  page,
}) => {
  const version = await spaVersion(server, page);
  await signInAsNewUser(server, page);
  await routeUnsupported(page, { serverMin: 2, serverMax: 3 });
  const loads = countLoads(page);

  await page.goto(`${server.url}/r/lounge`);
  const title = page.getByRole('heading', { name: 'This page is out of date' });
  await expect(title).toBeVisible();
  await expect(page.getByText('The server runs a newer isshoni. Reload to get the matching version.')).toBeVisible();
  await expect(page.getByText(`This page: ${version} · Server: ${OTHER_VERSION}`)).toBeVisible();
  expect(loads()).toBe(1);

  await page.getByRole('button', { name: 'Reload' }).click();
  await expect.poll(loads).toBe(2);
  await expect(title).toBeVisible();
  await expect(page.getByText("Reloading didn't help. Ask your admin to check the server.")).toBeVisible();
  await expect(page.getByText(`This page: ${version} · Server: ${OTHER_VERSION}`)).toBeVisible();
});

test('protocol_unsupported from a server older than the page asks for the admin, without a Reload', async ({
  server,
  page,
}) => {
  await signInAsNewUser(server, page);
  await routeUnsupported(page, { serverMin: 0, serverMax: 0 });

  await page.goto(`${server.url}/r/lounge`);
  await expect(page.getByRole('heading', { name: 'This page is out of date' })).toBeVisible();
  await expect(
    page.getByText('The server runs an older isshoni than this page. Ask your admin to update the server.'),
  ).toBeVisible();
  await expect(page.getByRole('button', { name: 'Reload' })).toHaveCount(0);
});
