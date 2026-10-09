// The smoke test of the e2e harness (05 §19.3, 06 S4): the binary under test starts from the fixtures, the setup
// is completed through `isshoni setup-url --json`, and a browser logs in and reaches the room. It proves the
// plumbing that every other spec stands on: the server fixtures, the accounts, the debug handle that stats.ts
// reads, and, in a headful run, Chrome's flag that picks the tone tab. Nobody shares through the app here.
import { expect, test, TONE_TAB_TITLE } from './fixtures.ts';
import { readState, readStats, waitForRoom, waitForState } from './stats.ts';

/** GET /api/v1/info, as far as these tests read it. */
interface Info {
  server: { version: string; publicUrl: string };
  registration: string;
  setupRequired: boolean;
}

test('the worker server is ready, set up, and serves the app of its own build', async ({ server, request }) => {
  const ready = await request.get(`${server.url}/readyz`);
  expect(ready.status()).toBe(200);

  const info = (await (await request.get(`${server.url}/api/v1/info`)).json()) as Info;
  expect(info.setupRequired).toBe(false);
  expect(info.server.publicUrl).toBe(server.url);
  expect(server.admin).not.toBeNull();

  // One version per build (06 §7.2): the embedded web app carries the binary's version.
  const spa = (await (await request.get(`${server.url}/version.json`)).json()) as { version: string };
  expect(spa.version).toBe(info.server.version);
});

test('the admin logs in and lands in the lounge', async ({ server, page }) => {
  const admin = server.admin;
  if (!admin) throw new Error('the worker server has no admin');

  // Not signed in: the app sends the page to the login form.
  await page.goto(`${server.url}/`);
  await expect(page).toHaveURL(/\/login(\?|$)/);
  await page.getByLabel('Username').fill(admin.username);
  await page.getByLabel('Password', { exact: true }).fill(admin.password);
  await page.getByRole('button', { name: 'Log in' }).click();

  await expect(page).toHaveURL(`${server.url}/r/lounge`);
  await expect(page.getByRole('heading', { name: 'Lounge' })).toBeVisible();
  const state = await waitForRoom(page, 'lounge');
  expect(state.connection.connectionId).not.toBeNull();
  expect(state.room.participants).toBeGreaterThanOrEqual(1);

  // The shape stats.ts declares for state(): a page that has just joined, with nothing shared.
  expect(state.viewer).toMatchObject({
    focusedShareId: null,
    focusMode: 'auto',
    audibleShareId: null,
    audioPlaying: false,
    videoBlocked: false,
    fullscreen: false,
    pipShareId: null,
    pageHidden: false,
    media: 'idle',
    subGen: 0,
    shares: [],
  });
  expect(['locked', 'playing', 'blocked', 'muted']).toContain(state.viewer.audio);
  expect(typeof state.viewer.volume).toBe('number');
  expect(typeof state.connection.resumed).toBe('boolean');

  // stats() answers with a sample; nobody shares, so no tile has numbers.
  const sample = await readStats(page);
  expect(sample.at).toBeGreaterThan(0);
  expect(Array.isArray(sample.pcs)).toBe(true);
  expect(sample.shares).toEqual({});
  expect(sample.outbound).toEqual([]);
});

test('a second person, in a browser context of their own, joins the same room', async ({
  server,
  context,
  page,
  asUser,
}) => {
  const admin = server.admin;
  if (!admin) throw new Error('the worker server has no admin');
  await server.signIn(context, admin);
  await page.goto(`${server.url}/`);
  const alone = await waitForRoom(page, 'lounge');

  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  const theirs = await waitForRoom(friend.page, 'lounge');
  expect(theirs.connection.connectionId).not.toBe(alone.connection.connectionId);

  // The admin's page hears of it: a newer room.state with one more person.
  await waitForState(
    page,
    (s) => s.room.participants > alone.room.participants && (s.room.rev ?? 0) > (alone.room.rev ?? 0),
    { message: 'the friend in the room of the admin' },
  );
  expect((await readState(friend.page)).room.participants).toBeGreaterThanOrEqual(2);

  // And the page shows it: the people panel lists both.
  await page.getByRole('button', { name: /here\. Show who/ }).click();
  const people = page.getByRole('dialog');
  await expect(people.getByText(`${admin.username} (you)`)).toBeVisible();
  await expect(people.getByText(friend.account.username, { exact: true })).toBeVisible();
});

test('a server of its own waits for the setup and takes config overrides', async ({ startServer, page, request }) => {
  // No UDP media port: how the specs of a network that blocks UDP start their server.
  const own = await startServer({ setUp: false, overrides: { 'listen.ice_udp': '', 'registration.mode': 'approval' } });
  expect(own.admin).toBeNull();
  const info = (await (await request.get(`${own.url}/api/v1/info`)).json()) as Info;
  expect(info).toMatchObject({ setupRequired: true, registration: 'approval', server: { publicUrl: own.url } });

  await page.goto(`${own.url}/`);
  await expect(page.getByRole('heading', { name: "This server isn't set up yet" })).toBeVisible();

  // The link of the setup wizard, as `isshoni setup-url` mints it.
  const link = new URL(await own.setupUrl());
  expect(`${link.origin}${link.pathname}`).toBe(`${own.url}/setup`);
  expect(link.hash.length).toBeGreaterThan(1);
});

test('a server of its own comes back after a restart, with its data, at the same URL', async ({
  startServer,
  context,
  page,
  request,
}) => {
  const own = await startServer();
  const member = await own.createUser();
  const url = own.url;

  await own.restart();
  expect(own.url).toBe(url);
  // The same data directory: the member of before logs in, and the admin's session still creates accounts.
  await own.signIn(context, member);
  await page.goto(`${own.url}/`);
  await waitForRoom(page, 'lounge');
  await own.createUser();

  await own.stop();
  await expect(request.get(`${own.url}/readyz`, { timeout: 2_000 })).rejects.toThrow();
});

test('headful: the tab picker of getDisplayMedia picks the tone tab by itself', async ({
  server,
  context,
  page,
  headless,
}) => {
  test.skip(
    headless,
    'a headless Chrome cannot accept the picker (05 §19.3): CI runs headful, a local run with --headed',
  );

  // Stand-ins for the tone tab and for a page that shares, served under the server's origin (a secure context).
  // The picker goes by the tab's title; the request is the app's: a window, with sound (05 §13.2).
  const tone = `<!doctype html><title>${TONE_TAB_TITLE}</title><canvas width="320" height="180"></canvas>`;
  const sharer = `<!doctype html><title>smoke</title><button>Share</button><output></output><script>
    document.querySelector('button').addEventListener('click', () => {
      navigator.mediaDevices.getDisplayMedia({ video: { displaySurface: 'window' }, audio: true }).then(
        (stream) => {
          const surface = stream.getVideoTracks()[0].getSettings().displaySurface;
          document.querySelector('output').textContent = surface + ', audio tracks: ' + stream.getAudioTracks().length;
        },
        (err) => {
          document.querySelector('output').textContent = String(err);
        },
      );
    });
  </script>`;
  const serve = (url: string, body: string) =>
    context.route(url, (route) => route.fulfill({ contentType: 'text/html; charset=utf-8', body }));
  await serve(`${server.url}/e2e/smoke-tone.html`, tone);
  await serve(`${server.url}/e2e/smoke-sharer.html`, sharer);

  await page.goto(`${server.url}/e2e/smoke-sharer.html`);
  const toneTab = await context.newPage();
  await toneTab.goto(`${server.url}/e2e/smoke-tone.html`);
  await page.bringToFront();
  await page.getByRole('button', { name: 'Share' }).click();

  // Chrome reports a captured tab as "browser", whatever surface was asked for.
  await expect(page.getByRole('status')).toHaveText('browser, audio tracks: 1', { timeout: 20_000 });
});
