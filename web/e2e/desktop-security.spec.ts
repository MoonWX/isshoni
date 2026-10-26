// desktop-security.spec (M2 03 §22.4 A, README WA48): the desktop app's webview security, with the app's real Go
// parts (internal/desktop/e2ehost: the asset handler, the /wails guard, the bound services) serving the desktop SPA
// to Chrome at http://wails.localhost:
// - the HTML carries the CSP (header and <meta>), which refuses an injected inline script; no service worker or
//   manifest is served;
// - an external link and window.open reach the system browser through App.OpenExternal (the harness records the
//   Shell's OpenURL), and the page stays on the asset origin;
// - /wails/runtime refuses a foreign Origin, and from the page itself the runtime's own objects (W21:
//   Browser.OpenURL('ms-settings:…') and ('search-ms:…'), Clipboard.Text(), Window.Hide()) get 403 and reach no
//   handler, while a bound-method Call reaches the stub runtime behind the guard;
// - /link opens in the system browser (RequireBrowser), and no access token ever reaches web storage although the
//   page was handed tokens.
//
// The last case links the app to the worker's unmodified e2e server with a password (POST /api/v1/device/password,
// 02 §14), which answers the desktop origin with 02's CORS (protocol.DesktopOrigins, wired by README WA56).
import { DESKTOP_ORIGIN, expect, test, type DesktopApp } from './fixtures.ts';

/** The page is up: the router rendered a page (with no server, the wizard's stub). */
async function ready(desktop: DesktopApp): Promise<void> {
  await expect(desktop.page.locator('h1').first()).toBeVisible();
}

test('the page carries the CSP, refuses inline script and has no service worker', async ({ startDesktop }) => {
  const desktop = await startDesktop();
  const { page } = desktop;
  const res = await page.goto(`${desktop.origin}/`);
  expect(res?.status()).toBe(200);
  const csp = res?.headers()['content-security-policy'] ?? '';
  for (const directive of [
    "default-src 'self'",
    "script-src 'self'",
    "worker-src 'none'",
    "manifest-src 'none'",
    "object-src 'none'",
    "frame-ancestors 'none'",
  ]) {
    expect(csp).toContain(directive);
  }
  expect(csp).not.toContain('unsafe-inline');
  expect(csp).not.toContain('unsafe-eval');
  await ready(desktop);
  expect(await page.locator('meta[http-equiv="Content-Security-Policy"]').getAttribute('content')).toBe(csp);

  expect(await page.evaluate(() => [location.origin, isSecureContext] as const)).toEqual([DESKTOP_ORIGIN, true]);

  // An injected inline script doesn't run, and the browser reports the violation.
  const inline = await page.evaluate(
    () =>
      new Promise<{ ran: boolean; violation: string }>((resolve) => {
        let violation = '';
        document.addEventListener('securitypolicyviolation', (e) => {
          violation = e.violatedDirective;
        });
        const s = document.createElement('script');
        s.textContent = 'window.__e2eInline = true';
        document.head.append(s);
        setTimeout(() => {
          resolve({ ran: (window as { __e2eInline?: boolean }).__e2eInline === true, violation });
        }, 200);
      }),
  );
  expect(inline.ran).toBe(false);
  expect(inline.violation).toMatch(/^script-src/);

  const statuses = await page.evaluate(async () =>
    Promise.all(['/sw.js', '/manifest.webmanifest'].map((p) => fetch(p).then((r) => r.status))),
  );
  expect(statuses).toEqual([404, 404]);
  expect(await page.evaluate(async () => (await navigator.serviceWorker.getRegistrations()).length)).toBe(0);
});

test('external links and window.open open the system browser; the page stays', async ({ startDesktop }) => {
  const desktop = await startDesktop();
  const { page } = desktop;
  await page.goto(`${desktop.origin}/`);
  await ready(desktop);
  const here = page.url();

  await page.evaluate(() => {
    for (const [id, href] of [
      ['e2e-external', 'https://example.com/e2e-link'],
      ['e2e-file', 'file:///C:/Windows/win.ini'],
    ] as const) {
      const a = document.createElement('a');
      a.id = id;
      a.href = href;
      a.textContent = id;
      document.body.append(a);
    }
  });
  await page.click('#e2e-external');
  await expect.poll(async () => (await desktop.state()).openedUrls).toEqual(['https://example.com/e2e-link']);

  expect(await page.evaluate(() => window.open('https://example.com/e2e-window'))).toBeNull();
  await expect
    .poll(async () => (await desktop.state()).openedUrls)
    .toEqual(['https://example.com/e2e-link', 'https://example.com/e2e-window']);

  // A file: link is refused by App.OpenExternal's rule: nothing opens, and the page doesn't follow it.
  await page.click('#e2e-file');
  await page.waitForTimeout(300);
  expect((await desktop.state()).openedUrls).toHaveLength(2);
  expect(page.url()).toBe(here);
  expect(page.context().pages()).toHaveLength(1);
});

test('the runtime refuses foreign origins and its own objects; a bound call passes the guard', async ({
  startDesktop,
}) => {
  const desktop = await startDesktop();
  const { page } = desktop;
  await page.goto(`${desktop.origin}/`);
  await ready(desktop);

  // A page of another origin (here, the request as it would send it): 403 before anything reads the body.
  const foreign = await fetch(`${desktop.hostUrl}/wails/runtime`, {
    method: 'POST',
    headers: { Origin: 'https://evil.example', 'Content-Type': 'application/json', 'x-wails-client-id': 'e2e' },
    body: JSON.stringify({ object: 0, method: 0, args: { 'call-id': 'e2e-0', methodID: 1, args: [] } }),
  });
  expect(foreign.status).toBe(403);

  // From the page itself, built as the pinned runtime builds them (@wailsio/runtime's runtime.js: the
  // x-wails-client-id header and {object, method, args}; 03 §3.2 W21's ids).
  const statuses = await page.evaluate(async () => {
    const send = (object: number, method: number, args?: unknown): Promise<number> =>
      fetch('/wails/runtime', {
        method: 'POST',
        headers: { 'x-wails-client-id': 'e2e', 'Content-Type': 'application/json' },
        body: JSON.stringify(args === undefined ? { object, method } : { object, method, args }),
      }).then((r) => r.status);
    return {
      openSettings: await send(9, 0, { url: 'ms-settings:display' }),
      openSearch: await send(9, 0, { url: 'search-ms:query=x' }),
      clipboardText: await send(1, 1),
      windowHide: await send(6, 11),
      // The runtime's query form (an empty body) is refused the same way.
      openSettingsByQuery: await fetch(
        `/wails/runtime?object=9&method=0&args=${encodeURIComponent(JSON.stringify({ url: 'ms-settings:display' }))}`,
        { method: 'POST', headers: { 'x-wails-client-id': 'e2e' } },
      ).then((r) => r.status),
      call: await send(0, 0, { 'call-id': 'e2e-1', methodID: 1, args: [] }),
    };
  });
  expect(statuses).toEqual({
    openSettings: 403,
    openSearch: 403,
    clipboardText: 403,
    windowHide: 403,
    openSettingsByQuery: 403,
    call: 200,
  });
  const state = await desktop.state();
  expect(state.runtimeCalls).toEqual([{ object: 0, method: 0 }]);
  expect(state.openedUrls).toEqual([]);
  expect(state.windowVisible).toBe(true);
});

test('/link opens the system browser, and no access token reaches web storage', async ({ startDesktop, server }) => {
  // The e2e server answers the desktop origin (02 §6.4's CORS) and offers the device flow.
  const res = await fetch(`${server.url}/api/v1/info`, { headers: { Origin: DESKTOP_ORIGIN } });
  expect(res.headers.get('access-control-allow-origin')).toBe(DESKTOP_ORIGIN);
  expect(((await res.json()) as { features?: string[] }).features).toContain('deviceFlow');

  const account = await server.createUser();
  const desktop = await startDesktop({ server: server.url });
  // Linked with a password, as the wizard's form does (02 §14): Go holds the tokens.
  const st = await desktop.call<{ status: string; origin: string }>(
    'Auth',
    'LinkWithPassword',
    account.username,
    account.password,
  );
  expect(st).toMatchObject({ status: 'linked', origin: server.url });

  const { page } = desktop;
  await page.goto(`${desktop.origin}/link?code=WDJB-MJHT`);
  await expect(page.getByRole('heading', { name: 'Opened in your browser' })).toBeVisible();
  await expect.poll(async () => (await desktop.state()).openedUrls).toEqual([`${server.url}/link?code=WDJB-MJHT`]);
  expect(new URL(page.url()).origin).toBe(DESKTOP_ORIGIN);

  // The page was handed the active server's token for its own origin only (03 §5.8) …
  const { tokens } = await desktop.state();
  expect(tokens.length).toBeGreaterThan(0);
  for (const t of tokens) expect(t).toMatchObject({ origin: server.url, activeOrigin: server.url });
  // … and kept it in memory: nothing in web storage or cookies holds one.
  const stored = await page.evaluate(() => {
    const out: string[] = [document.cookie];
    for (const s of [localStorage, sessionStorage]) {
      for (let i = 0; i < s.length; i++) {
        const k = s.key(i) ?? '';
        out.push(k, s.getItem(k) ?? '');
      }
    }
    return out.join('\n');
  });
  expect(stored).not.toContain('isa_');
  expect(stored).not.toContain('isr_');
});
