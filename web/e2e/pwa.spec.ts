// pwa.spec (05 §19.3; README S77): the manifest parses, the service worker activates, and an offline navigation
// shows the Offline screen from the cached shell (05 §16.1–16.2).
//
// Service workers are blocked in every other spec (playwright.config.ts): a production build registers one, and it
// would answer their navigations from its cache. This file turns them on for itself. Each test that registers a
// worker does it on a server of its own: a worker belongs to an origin, and the port makes the origin.
import type { Page } from '@playwright/test';

import { expect, test } from './fixtures.ts';

test.use({ serviceWorkers: 'allow' });

/** The fields of the manifest (05 §16.1) that an install reads. */
interface Manifest {
  id: string;
  name: string;
  short_name: string;
  start_url: string;
  scope: string;
  display: string;
  background_color: string;
  theme_color: string;
  icons: { src: string; sizes: string; type: string; purpose?: string }[];
}

/** What boot step 6 registered (05 §4): the active worker of the page's registration, once there is one. */
async function activeWorker(page: Page): Promise<{ scope: string; script: string; state: string }> {
  return page.evaluate(async () => {
    const registration = await navigator.serviceWorker.ready;
    const worker = registration.active;
    if (!worker) throw new Error('no active service worker');
    if (worker.state !== 'activated') {
      await new Promise<void>((resolve) => {
        worker.addEventListener('statechange', () => {
          if (worker.state === 'activated') resolve();
        });
      });
    }
    return { scope: registration.scope, script: worker.scriptURL, state: worker.state };
  });
}

/** The Offline screen (05 §4): what the cached shell shows when /api/v1/info can't be reached. */
const OFFLINE_HEADING = "Can't reach the server";
/** /info is tried four times, 1, 2 and 4 s apart, before the Offline screen shows (05 §4 step 4). */
const OFFLINE_SCREEN_MS = 20_000;

test('the manifest parses and has what an install needs', async ({ server, request, page }) => {
  const res = await request.get(`${server.url}/manifest.webmanifest`);
  expect(res.status()).toBe(200);
  const manifest = JSON.parse(await res.text()) as Manifest;
  expect(manifest).toMatchObject({
    id: '/',
    name: 'isshoni',
    short_name: 'isshoni',
    start_url: '/',
    scope: '/',
    display: 'standalone',
  });
  expect(manifest.icons.map((i) => i.sizes)).toEqual(expect.arrayContaining(['192x192', '512x512']));
  expect(manifest.icons.some((i) => i.purpose === 'maskable')).toBe(true);
  // Every icon is there, in the type the manifest says.
  for (const icon of manifest.icons) {
    const file = await request.get(new URL(icon.src, server.url).href);
    expect(file.status(), icon.src).toBe(200);
    expect(file.headers()['content-type'], icon.src).toContain(icon.type);
  }

  // The page links it, and Chrome's own parser reads it without an error.
  await page.goto(`${server.url}/login`);
  await expect(page.locator('link[rel="manifest"]')).toHaveAttribute('href', '/manifest.webmanifest');
  const cdp = await page.context().newCDPSession(page);
  const parsed = (await cdp.send('Page.getAppManifest')) as {
    url: string;
    errors: { message: string; critical: number }[];
    data?: string;
  };
  expect(parsed.url).toBe(`${server.url}/manifest.webmanifest`);
  expect(parsed.errors).toEqual([]);
  expect(JSON.parse(parsed.data ?? '{}')).toEqual(manifest);
});

test('the service worker activates, controls the page and precaches the shell', async ({ startServer, page }) => {
  const own = await startServer();
  await page.goto(`${own.url}/login`);
  expect(await activeWorker(page)).toEqual({
    scope: `${own.url}/`,
    script: `${own.url}/sw.js`,
    state: 'activated',
  });
  // clients.claim(): the page that registered it is controlled without a reload.
  await page.waitForFunction(() => navigator.serviceWorker.controller !== null);

  // One shell cache, named after the shell version that /version.json has (05 §16.2, §17.1), with `/` in it.
  const version = (await (await page.request.get(`${own.url}/version.json`)).json()) as { shell: string };
  const cached = await page.evaluate(async () => {
    const names = await caches.keys();
    const shell = names.length === 1 && names[0] !== undefined ? await caches.open(names[0]) : null;
    return { names, root: shell !== null && (await shell.match('/')) !== undefined };
  });
  expect(cached).toEqual({ names: [`isshoni-shell-${version.shell}`], root: true });

  // The worker's own file isn't cached by the HTTP cache either: updateViaCache 'none', and the server says no-cache.
  const sw = await page.request.get(`${own.url}/sw.js`);
  expect(sw.status()).toBe(200);
  expect(sw.headers()['cache-control']).toContain('no-cache');
});

test('offline, a navigation shows the Offline screen from the cached shell', async ({ startServer, context, page }) => {
  const own = await startServer();
  await page.goto(`${own.url}/login`);
  await activeWorker(page);
  await page.waitForFunction(() => navigator.serviceWorker.controller !== null);

  await context.setOffline(true);
  const response = await page.goto(`${own.url}/r/lounge`);
  // The worker answered: navigations go to the network first, then the cached `/` (05 §16.2).
  expect(response?.fromServiceWorker()).toBe(true);
  await expect(page.getByRole('heading', { name: OFFLINE_HEADING })).toBeVisible({ timeout: OFFLINE_SCREEN_MS });
  await expect(page).toHaveURL(`${own.url}/r/lounge`);

  await expect(page.getByRole('button', { name: 'Try now' })).toBeVisible();

  // Back online, the Offline screen tries again by itself (the `online` event) and the app goes on to the page:
  // the login form, since nobody is signed in.
  await context.setOffline(false);
  await expect(page.getByRole('heading', { name: OFFLINE_HEADING })).toBeHidden({ timeout: OFFLINE_SCREEN_MS });
  await expect(page).toHaveURL(/\/login\?next=%2Fr%2Flounge$/);
});

// Web Push itself needs the browser's push service (Google's, for Chrome) and a phone to show it on: that is the
// manual M-IOS-2 and M-AND-1 (05 §19.4). Here the notifications page runs on the real server and the real browser
// provider, up to the permission prompt, which a test never answers (a headful Chrome would wait for a person).
test('the notifications page: the switch where Chrome can push, the choices on the server, a block', async ({
  startServer,
  context,
  page,
}) => {
  const own = await startServer();
  const admin = own.admin;
  if (!admin) throw new Error('the server has no admin');
  await own.signIn(context, admin);
  const info = (await (await page.request.get(`${own.url}/api/v1/info`)).json()) as { push?: object };
  expect(info.push).toBeDefined(); // push.enabled is on by default (04 §14)

  await page.goto(`${own.url}/account/notifications`);
  await expect(page.getByRole('heading', { level: 1, name: 'Notifications' })).toBeVisible();
  const toggle = page.getByRole('switch', { name: 'Notifications on this device' });
  await expect(toggle).toBeVisible();
  await expect(toggle).not.toBeChecked();

  // The choices are the server's: a change is there after a reload.
  const share = page.getByRole('checkbox', { name: 'When someone starts sharing' });
  await expect(share).toBeChecked();
  await expect(page.getByRole('checkbox', { name: 'Admin alerts' })).toBeChecked();
  const saved = page.waitForResponse(
    (r) => r.url().endsWith('/api/v1/push/preferences') && r.request().method() === 'PUT' && r.ok(),
  );
  await share.uncheck();
  await saved;
  await page.reload();
  await expect(page.getByRole('checkbox', { name: 'When someone starts sharing' })).not.toBeChecked();

  // Notifications blocked in the browser's settings: the page says how to allow them, and offers no switch.
  // Playwright has no "deny"; Chrome's DevTools protocol does, for this test's own browser context.
  const cdp = await context.newCDPSession(page);
  const { targetInfo } = await cdp.send('Target.getTargetInfo');
  await cdp.send('Browser.setPermission', {
    permission: { name: 'notifications' },
    setting: 'denied',
    origin: own.url,
    ...(targetInfo.browserContextId !== undefined ? { browserContextId: targetInfo.browserContextId } : {}),
  });
  await page.reload();
  await expect(page.getByText(/Notifications are blocked for isshoni in this browser/)).toBeVisible();
  await expect(toggle).toBeHidden();
});

test('with the server gone, the cached shell still starts and shows the Offline screen', async ({
  startServer,
  page,
}) => {
  const own = await startServer();
  await page.goto(`${own.url}/login`);
  await activeWorker(page);
  await page.waitForFunction(() => navigator.serviceWorker.controller !== null);

  // Nothing answers on the server's port any more: every byte of the next page comes from the worker's cache.
  await own.stop();
  const response = await page.goto(`${own.url}/r/lounge`);
  expect(response?.fromServiceWorker()).toBe(true);
  expect(response?.status()).toBe(200);
  await expect(page.getByRole('heading', { name: OFFLINE_HEADING })).toBeVisible({ timeout: OFFLINE_SCREEN_MS });
  await expect(page.getByRole('button', { name: 'Try now' })).toBeVisible();
});
