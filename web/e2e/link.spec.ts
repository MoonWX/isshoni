// link.spec (docs/m2/02-device-auth.md §16.3; README WA63): the device flow end to end against a real server, with
// Node standing in for the desktop app's Go code and Chrome for the friend's browser.
//
// - The app (Node's fetch) starts a flow (POST /api/v1/device/code); the friend, signed in, opens
//   verificationUriComplete, sees the code, renames the app and links it; Node's poll gets 200 with an isa_ token;
//   a Node WebSocket whose hello carries the token gets welcome; the Devices page lists the app, and Remove closes
//   Node's socket with 4401.
// - Signed out: /link?code=… goes to /login?next=…, and logging in comes back to the confirm card.
// - The network notes: `different` (both IPv4) and `unknown` (the app over IPv6, the browser over IPv4) each need
//   the "I started this myself" tick before Link; `same` has none (the first case).
//
// Addresses. The e2e server trusts X-Forwarded-For from a loopback peer (fixtures.ts, fromNewAddress), so the app's
// requests and the browser's each say where they come from: the lookup's `network` compares the two (02 §5.3), and
// each flow's /device/code spends the device-code-ip budget (10, then 1 per 6 min) of an address of its own. The
// addresses here are from 198.18.0.0/15 and 2001:db8:18::/48, which fixtures.ts and the other specs don't use.
import type { BrowserContext, Page } from '@playwright/test';

import { expect, test, type E2EServer } from './fixtures.ts';

/** What the test app reports: its kind, OS and version (02 §5.2), and its proposed name. */
const APP = { clientKind: 'desktop', os: 'windows', appVersion: '0.2.0', deviceName: 'DESKTOP-E2E' } as const;

/** How long a poll loop waits for the token at most; a flow lives 600 s, the steps here take seconds. */
const POLL_TIMEOUT_MS = 30_000;
const SOCKET_TIMEOUT_MS = 10_000;

let addresses = 0;

/** An IPv4 address no other call of this worker returned (198.18.0.0/15, RFC 2544). */
function newIPv4(): string {
  const n = ++addresses;
  return `198.18.${String(Math.floor(n / 250))}.${String((n % 250) + 1)}`;
}

/** An IPv6 address no other call of this worker returned (2001:db8::/32, RFC 3849). */
function newIPv6(): string {
  return `2001:db8:18::${(++addresses).toString(16)}`;
}

/** POST /api/v1/device/code's answer (02 §5.2). */
interface CodeAnswer {
  deviceCode: string;
  userCode: string;
  verificationUri: string;
  verificationUriComplete: string;
  expiresIn: number;
  interval: number;
}

/** POST /api/v1/device/token's 200 (02 §5.4). */
interface TokenAnswer {
  accessToken: string;
  tokenType: string;
  refreshToken: string;
  device: { id: string; name: string };
  user: { id: string; username: string };
}

/** A REST call of the app, as its Go client sends it: JSON, no cookie, no Origin, from `from`. */
async function appPost(server: E2EServer, path: string, body: unknown, from: string): Promise<Response> {
  return fetch(`${server.url}${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Forwarded-For': from },
    body: JSON.stringify(body),
  });
}

/** The app starts a flow from `from`. */
async function startFlow(server: E2EServer, from: string): Promise<CodeAnswer> {
  const res = await appPost(server, '/api/v1/device/code', APP, from);
  expect(res.status, 'POST /api/v1/device/code').toBe(200);
  return (await res.json()) as CodeAnswer;
}

/** When each flow was last polled (Date.now()): the next poll waits its interval, as the app's does. */
const lastPoll = new WeakMap<CodeAnswer, number>();

/** One poll (02 §5.4): the status and the body, a token pair or an error with its code and params. */
async function poll(
  server: E2EServer,
  flow: CodeAnswer,
  from: string,
): Promise<{ status: number; body: TokenAnswer | { error: { code: string; params?: { interval?: number } } } }> {
  lastPoll.set(flow, Date.now());
  const body = { grantType: 'device_code', deviceCode: flow.deviceCode };
  const res = await appPost(server, '/api/v1/device/token', body, from);
  return { status: res.status, body: (await res.json()) as TokenAnswer };
}

function errorCode(answer: Awaited<ReturnType<typeof poll>>): string | undefined {
  return 'error' in answer.body ? answer.body.error.code : undefined;
}

/**
 * Polls as the app does until the answer is no longer authorization_pending: `interval` seconds after the previous
 * poll, longer after slow_down (RFC 8628 §3.5; the server's params.interval says the new value).
 */
async function pollUntilDecided(
  server: E2EServer,
  flow: CodeAnswer,
  from: string,
): Promise<Awaited<ReturnType<typeof poll>>> {
  let interval = flow.interval;
  const deadline = Date.now() + POLL_TIMEOUT_MS;
  for (;;) {
    const wait = (lastPoll.get(flow) ?? 0) + interval * 1000 - Date.now();
    if (Date.now() + Math.max(0, wait) > deadline) throw new Error(`no decision within ${String(POLL_TIMEOUT_MS)} ms`);
    if (wait > 0) await new Promise((resolve) => setTimeout(resolve, wait));
    const answer = await poll(server, flow, from);
    const code = errorCode(answer);
    if (code !== 'authorization_pending' && code !== 'slow_down') return answer;
    if (code === 'slow_down' && 'error' in answer.body) interval = answer.body.error.params?.interval ?? interval + 5;
  }
}

/** The app's signaling connection: open after welcome; `closed` settles with the close code. */
interface AppSocket {
  readonly welcome: { user: { id: string; name: string } };
  readonly closed: Promise<number>;
  close(): void;
}

/**
 * A WebSocket to /ws whose hello carries the access token (02 §6.3; 01's hello.auth), as the app's Go signal client
 * sends it: no cookie, no Origin. Resolves at welcome; it pings every 10 s so that the server's idle timeout never
 * ends it before the test does.
 */
async function openAppSocket(server: E2EServer, token: string): Promise<AppSocket> {
  const ws = new WebSocket(`${server.url.replace(/^http/, 'ws')}/ws`);
  const closed = new Promise<number>((resolve) => {
    ws.addEventListener('close', (e) => {
      resolve(e.code);
    });
  });
  const pings = setInterval(() => {
    if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'ping', data: { t: Date.now() } }));
  }, 10_000);
  void closed.then(() => {
    clearInterval(pings);
  });
  const hello = {
    protocol: 1,
    minProtocol: 1,
    features: [],
    client: { kind: APP.clientKind, version: APP.appVersion, os: APP.os },
    role: 'viewer',
    auth: { scheme: 'bearer', token },
    caps: { decode: [] },
  };
  const welcome = await new Promise<AppSocket['welcome']>((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`no welcome within ${String(SOCKET_TIMEOUT_MS)} ms`));
    }, SOCKET_TIMEOUT_MS);
    ws.addEventListener('open', () => {
      ws.send(JSON.stringify({ type: 'hello', id: 'hello1', data: hello }));
    });
    ws.addEventListener('message', (e) => {
      const msg = JSON.parse(String(e.data)) as { type: string; re?: string; data?: unknown };
      if (msg.type === 'welcome' && msg.re === 'hello1') {
        clearTimeout(timer);
        resolve(msg.data as AppSocket['welcome']);
      } else if (msg.type === 'error') {
        clearTimeout(timer);
        reject(new Error(`hello was answered with error ${JSON.stringify((msg.data as { code?: string }).code)}`));
      }
    });
    void closed.then((code) => {
      clearTimeout(timer);
      reject(new Error(`the socket closed with ${String(code)} before welcome`));
    });
  });
  return {
    welcome,
    closed,
    close: () => {
      ws.close();
    },
  };
}

/** The approval page's confirm card is showing. */
async function confirmCard(page: Page): Promise<void> {
  await expect(page.getByRole('heading', { level: 1, name: 'Link the isshoni app?' })).toBeVisible();
}

/** The code as the page reads it to a screen reader: "W D J B, M J H T" (02 §10.1). */
function spoken(userCode: string): string {
  const [a = '', b = ''] = userCode.split('-');
  return `${Array.from(a).join(' ')}, ${Array.from(b).join(' ')}`;
}

/** Makes every request of the context come from `from`, as a browser behind that address. */
async function browseFrom(context: BrowserContext, from: string): Promise<void> {
  await context.setExtraHTTPHeaders({ 'X-Forwarded-For': from });
}

test('start, approve with a new name: Node’s poll gets an isa_ token, its WebSocket gets welcome, Remove closes it with 4401', async ({
  server,
  page,
}) => {
  const account = await server.createUser();
  await server.signIn(page.context(), account);
  // One address for the app and the browser: `network: same`.
  const ip = newIPv4();
  await browseFrom(page.context(), ip);

  const flow = await startFlow(server, ip);
  expect(flow.userCode).toMatch(/^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$/);
  expect(flow.verificationUriComplete).toBe(`${server.url}/link?code=${flow.userCode}`);
  expect(errorCode(await poll(server, flow, ip)), 'the poll before the approval').toBe('authorization_pending');

  await page.goto(flow.verificationUriComplete);
  await confirmCard(page);
  // The code left the address bar; the card shows it, and reads it letter by letter.
  await expect(page).toHaveURL(`${server.url}/link`);
  await expect(page.getByText(flow.userCode, { exact: true })).toBeVisible();
  await expect(page.getByText(spoken(flow.userCode), { exact: true })).toBeAttached();
  await expect(page.getByText('isshoni for Windows 0.2.0', { exact: true })).toBeVisible();
  await expect(
    page.getByText(new RegExp(`from ${ip.replaceAll('.', '\\.')} \\(this browser's network\\)`)),
  ).toBeVisible();
  // `same` has no tick.
  await expect(page.getByRole('checkbox', { name: 'I started this myself' })).toHaveCount(0);
  const name = page.getByLabel('Name for this computer');
  await expect(name).toHaveValue(APP.deviceName);
  await name.fill('E2E PC');
  await page.getByRole('button', { name: 'Link', exact: true }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Linked' })).toBeVisible();

  const answer = await pollUntilDecided(server, flow, ip);
  expect(answer.status, `the poll after the approval (${errorCode(answer) ?? 'ok'})`).toBe(200);
  const pair = answer.body as TokenAnswer;
  expect(pair.accessToken).toMatch(/^isa_[A-Za-z0-9_-]{43}$/);
  expect(pair.refreshToken).toMatch(/^isr_[A-Za-z0-9_-]{43}$/);
  expect(pair.tokenType).toBe('Bearer');
  expect(pair.device.name).toBe('E2E PC');
  expect(pair.user.username).toBe(account.username);
  // The code is spent.
  expect(errorCode(await poll(server, flow, ip))).toBe('invalid_grant');

  const socket = await openAppSocket(server, pair.accessToken);
  try {
    expect(socket.welcome.user.id).toBe(account.id);

    await page.goto(`${server.url}/account/devices`);
    const row = page
      .getByRole('region', { name: 'Linked devices' })
      .getByRole('listitem')
      .filter({ hasText: 'E2E PC' });
    await expect(row).toContainText('isshoni for Windows 0.2.0');
    await expect(row).toContainText(new RegExp(`Last seen .* from ${ip.replaceAll('.', '\\.')}`));
    await row.getByRole('button', { name: 'Remove' }).click();
    const dialog = page.getByRole('dialog', { name: 'Remove E2E PC?' });
    await expect(dialog).toContainText('E2E PC will be signed out and has to be linked again.');
    await dialog.getByRole('button', { name: 'Remove' }).click();
    await expect(row).toHaveCount(0);

    const code = await Promise.race([
      socket.closed,
      new Promise<string>((resolve) =>
        setTimeout(() => {
          resolve('not closed');
        }, SOCKET_TIMEOUT_MS),
      ),
    ]);
    expect(code, 'the app’s socket after Remove').toBe(4401);
  } finally {
    socket.close();
  }
});

test('signed out: /link?code=… goes to /login?next=…, and logging in comes back to the confirm card', async ({
  server,
  browser,
}) => {
  const account = await server.createUser();
  const context = await browser.newContext();
  try {
    // The login form spends this address's auth-ip budget, not the loopback address that other specs share.
    const ip = newIPv4();
    await browseFrom(context, ip);
    const page = await context.newPage();
    const flow = await startFlow(server, ip);

    await page.goto(flow.verificationUriComplete);
    await expect(page.getByRole('heading', { level: 1, name: /^Log in/ })).toBeVisible();
    const login = new URL(page.url());
    expect(login.pathname).toBe('/login');
    expect(login.searchParams.get('next')).toBe(`/link?code=${flow.userCode}`);

    await page.getByLabel('Username').fill(account.username);
    await page.getByLabel('Password', { exact: true }).fill(account.password);
    await page.getByRole('button', { name: 'Log in' }).click();
    await confirmCard(page);
    await expect(page.getByText(flow.userCode, { exact: true })).toBeVisible();
    await expect(page).toHaveURL(`${server.url}/link`);

    // Saying no reaches the app: its poll gets access_denied.
    await page.getByRole('button', { name: "Don't link" }).click();
    await expect(page.getByRole('heading', { level: 1, name: 'Not linked' })).toBeVisible();
    expect(errorCode(await pollUntilDecided(server, flow, ip))).toBe('access_denied');
  } finally {
    await context.close();
  }
});

for (const variant of [
  {
    network: 'different',
    what: 'both IPv4, other addresses',
    appFrom: newIPv4,
    note: 'This request came from a different network than this browser.',
    link: 'Link anyway',
  },
  {
    network: 'unknown',
    what: 'the app over IPv6, the browser over IPv4',
    appFrom: newIPv6,
    note: "isshoni couldn't check whether this request came from your network.",
    link: 'Link',
  },
] as const) {
  test(`network ${variant.network} (${variant.what}): the note, and ${variant.link} only after the tick`, async ({
    server,
    page,
  }) => {
    const account = await server.createUser();
    await server.signIn(page.context(), account);
    await browseFrom(page.context(), newIPv4());
    const appIp = variant.appFrom();
    const flow = await startFlow(server, appIp);

    await page.goto(flow.verificationUriComplete);
    await confirmCard(page);
    await expect(page.getByText(`from ${appIp}.`, { exact: false })).toBeVisible();
    await expect(page.getByText(/this browser's network/)).toHaveCount(0);
    await expect(page.getByText(variant.note, { exact: false })).toBeVisible();
    const tick = page.getByRole('checkbox', { name: 'I started this myself' });
    await expect(tick).not.toBeChecked();
    const link = page.getByRole('button', { name: variant.link, exact: true });
    await expect(link).toBeDisabled();
    await tick.check();
    await expect(link).toBeEnabled();
    await link.click();
    await expect(page.getByRole('heading', { level: 1, name: 'Linked' })).toBeVisible();
    const answer = await pollUntilDecided(server, flow, appIp);
    expect(answer.status).toBe(200);
  });
}
