// unmute.spec (05 §19.3; 05 §10.3, §12.7): a browser that wants a gesture before a page plays sound. This file's
// Chrome runs without the autoplay flag of the other specs, so a page that nobody clicked yet may play muted video
// but no sound, as a friend's browser does when a link opened the room.
// - The friend's stage plays the picture at once, and "Tap to unmute" covers it, with a pill in the header: the
//   sound is received, and waits for the tap.
// - The tap starts it: the <audio> element plays, with a sound level above 0.05, and the buttons go. A later change
//   of what is heard needs no tap: a newer share's sound plays by itself.
// - The M key is that tap too (05 §12.7), and so is the header's pill.
//
// Nothing may touch the friend's page before the share's sound has been refused. Every script that Playwright runs
// in a page counts there as a user gesture: page.evaluate, the queries behind a locator (isVisible included), and
// the DOM snapshots of a trace, which Playwright takes around every action. A page with that (sticky) activation
// may play sound, so stats.ts's readers, any expect on a locator, and the config's trace would unlock the very page
// this spec needs locked. So this file's trace has screenshots but no DOM snapshots, and until the sound is refused
// the spec reads the page through a CDP session of its own, whose Runtime.evaluate is no gesture (QuietReader).
// Signing in over REST, page.goto, bringToFront and setViewportSize give no activation either.
//
// What is shared: the fake display of 05 §19.3 with its 1 kHz tone, started by the sharer's click on Share (the
// gesture its AudioContext needs). Every test has a server of its own: the tests say what a whole room shows.
import type { Locator, Page } from '@playwright/test';

import {
  chromeArgs,
  enableDebug,
  expect,
  fakeDisplay,
  test,
  type Account,
  type E2EServer,
  type TestFixtures,
} from './fixtures.ts';
import {
  audioOf,
  readState,
  videoOf,
  waitForRoom,
  waitForState,
  waitForStats,
  type DebugState,
  type StatsSample,
} from './stats.ts';

/**
 * Chrome's own default policy, said out loud: a headful Chrome (CI) has it anyway, and a headless one (a local run)
 * would otherwise play sound without a gesture.
 */
const AUTOPLAY_DEFAULT = '--autoplay-policy=document-user-activation-required';

test.use({
  launchOptions: { args: [...chromeArgs({ autoplay: false }), AUTOPLAY_DEFAULT] },
  // The config's trace (retain-on-failure) without its DOM snapshots: see above.
  trace: { mode: 'retain-on-failure', snapshots: false, screenshots: true, sources: true },
});

/** A share that started may take this long to reach the server and a viewer: two PeerConnections. */
const MEDIA_TIMEOUT_MS = 20_000;
const POLL_INTERVAL_MS = 100;

// ---- Reading a page without unlocking it ----

/** Reads a page without giving it a user activation: a CDP session's Runtime.evaluate is no gesture. */
interface QuietReader {
  /** The value of a JavaScript expression in the page (a promise is awaited); it must be JSON. */
  value(expression: string): Promise<unknown>;
  /** window.__isshoni.state(), or null while the page has no handle. */
  state(): Promise<DebugState | null>;
  /** window.__isshoni.stats(), or null while the page has no handle. */
  stats(): Promise<StatsSample | null>;
}

async function quietReader(page: Page): Promise<QuietReader> {
  const cdp = await page.context().newCDPSession(page);
  const value = async (expression: string): Promise<unknown> => {
    const res = await cdp.send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    if (res.exceptionDetails) {
      const why = res.exceptionDetails.exception?.description ?? res.exceptionDetails.text;
      throw new Error(`in the page, ${expression}: ${why}`);
    }
    const result: unknown = res.result.value;
    return result;
  };
  return {
    value,
    state: async () => (await value('window.__isshoni ? window.__isshoni.state() : null')) as DebugState | null,
    stats: async () => (await value('window.__isshoni ? window.__isshoni.stats() : null')) as StatsSample | null,
  };
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Reads until `pick` returns something other than undefined, null or false, and returns that. */
async function waitQuietly<S, T>(
  read: () => Promise<S>,
  pick: (value: S) => T | undefined | null | false,
  message: string,
  timeout = 10_000,
): Promise<T> {
  const deadline = Date.now() + timeout;
  let last = 'nothing';
  for (;;) {
    try {
      const value = await read();
      const got = pick(value);
      if (got !== undefined && got !== null && got !== false) return got;
      last = JSON.stringify(value);
    } catch (err) {
      // A read that met a navigation: the next one reads the new document.
      const navigated = /context was destroyed|Cannot find (default )?(execution )?context|target navigated/i;
      if (!(err instanceof Error && navigated.test(err.message))) throw err;
    }
    if (Date.now() >= deadline) throw new Error(`${message}: not within ${String(timeout)} ms. Last read: ${last}`);
    await sleep(POLL_INTERVAL_MS);
  }
}

/** Where the page shows "Tap to unmute": over the stage, in the header (05 §10.3). */
const TAP_BUTTONS = `[...document.querySelectorAll('button')]
  .filter((b) => b.textContent.trim() === 'Tap to unmute')
  .map((b) => (b.closest('[data-viewer-stage]') ? 'stage' : b.closest('header') ? 'header' : 'elsewhere'))
  .sort()`;

// ---- People and shares ----

/** Someone who shares: their account, their page, and the share it publishes. */
interface Sharer {
  readonly account: Account;
  readonly page: Page;
  readonly shareId: string;
}

/**
 * Opens the lounge in `page`, signed in as `account`, and waits there without touching the page: the friend who
 * watches. Returns the page's quiet reader.
 */
async function openViewer(server: E2EServer, page: Page, account: Account): Promise<QuietReader> {
  await server.signIn(page.context(), account);
  const quiet = await quietReader(page);
  await page.goto(`${server.url}/`);
  await waitQuietly(
    () => quiet.state(),
    (s) => s?.connection.state === 'ready' && s.room.joinState === 'joined' && s.room.roomId === 'lounge',
    "the friend's page joins the lounge",
  );
  return quiet;
}

/** One more person opens the lounge and shares a window with its sound; returns once the share is live. */
async function startSharing(server: E2EServer, asUser: TestFixtures['asUser']): Promise<Sharer> {
  const user = await asUser(server);
  await fakeDisplay(user.page, 'window+audio');
  await user.page.goto(`${server.url}/`);
  await waitForRoom(user.page, 'lounge');
  await user.page.bringToFront();
  await user.page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await user.page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
  const own = await waitForState(user.page, (s) => s.viewer.shares.find((x) => x.local && x.status === 'live'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${user.account.username}'s share is live`,
  });
  return { account: user.account, page: user.page, shareId: own.id };
}

// ---- The friend's page ----

/** The stage while it shows the sharer's share. */
function stage(page: Page, sharer: Sharer): Locator {
  return page.getByRole('region', { name: `Now watching: ${sharer.account.username}'s window` });
}

/** The large "Tap to unmute" button over the stage. */
function tapToUnmute(page: Page): Locator {
  return page.locator('[data-viewer-stage]').getByRole('button', { name: 'Tap to unmute', exact: true });
}

/** The same tap as a pill in the room's header. */
function headerPill(page: Page): Locator {
  return page.getByRole('banner').getByRole('button', { name: 'Tap to unmute', exact: true });
}

/** The exceptions nobody caught on a page, from now on: a test expects none. */
function uncaughtErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(String(error.stack ?? error)));
  return errors;
}

/**
 * The friend's page before the tap, read quietly: the share is on the stage, its picture plays, its sound arrives
 * but the browser refused to play it, and both taps are offered.
 */
async function expectBlocked(quiet: QuietReader, sharer: Sharer): Promise<void> {
  const state = await waitQuietly(
    () => quiet.state(),
    (s) =>
      s?.viewer.focusedShareId === sharer.shareId &&
      s.viewer.audibleShareId === sharer.shareId &&
      s.viewer.audio === 'blocked' &&
      s,
    "the share is on the friend's stage, and its sound is refused",
    MEDIA_TIMEOUT_MS,
  );
  expect(state.viewer).toMatchObject({ audioPlaying: false, videoBlocked: false, focusMode: 'auto' });
  // The picture plays: tile videos are muted, and a muted video needs no gesture. The sound arrives.
  await waitQuietly(
    () => quiet.stats(),
    (s) =>
      s !== null &&
      (videoOf(s, sharer.shareId)?.framesDecoded ?? 0) > 10 &&
      (audioOf(s, sharer.shareId)?.bytesReceived ?? 0) > 0,
    'decoded video, and audio bytes received, before the tap',
    MEDIA_TIMEOUT_MS,
  );
  expect(await quiet.value("document.querySelector('[data-viewer-stage] video')?.paused")).toBe(false);
  expect(await quiet.value("document.querySelector('audio[data-isshoni-audio]')?.paused")).toBe(true);
  expect(await quiet.value(TAP_BUTTONS)).toEqual(['header', 'stage']);
  // Still refused: nothing in the page has played it meanwhile.
  expect((await quiet.state())?.viewer.audio).toBe('blocked');
}

/** Waits until the page plays the share's sound: the element plays and the level is above 0.05. */
async function expectHeard(page: Page, sharer: Sharer): Promise<StatsSample> {
  await waitForState(page, (s) => s.viewer.audio === 'playing' && s.viewer.audioPlaying, {
    message: "the friend's <audio> element plays",
  });
  const sample = await waitForStats(page, (s) => (audioOf(s, sharer.shareId)?.audioLevel ?? 0) > 0.05 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `a sound level above 0.05 for ${sharer.account.username}'s share`,
  });
  await expect(page.locator('audio[data-isshoni-audio]')).toHaveJSProperty('paused', false);
  await expect(tapToUnmute(page)).toHaveCount(0);
  await expect(headerPill(page)).toHaveCount(0);
  return sample;
}

// ---- The tests ----

test('"Tap to unmute" covers the playing picture until the tap starts the sound', async ({
  startServer,
  page,
  asUser,
}) => {
  const server = await startServer();
  if (!server.admin) throw new Error('the server has no admin');
  const viewerErrors = uncaughtErrors(page);
  const quiet = await openViewer(server, page, server.admin);
  const alex = await startSharing(server, asUser);
  await page.bringToFront();
  await expectBlocked(quiet, alex);

  // The tap. It is no pick (05 §12.2): the focus stays automatic.
  await tapToUnmute(page).click();
  const first = await expectHeard(page, alex);
  expect((await readState(page)).viewer.focusMode).toBe('auto');
  await expect(stage(page, alex)).toBeVisible();
  await waitForStats(
    page,
    (s) => (audioOf(s, alex.shareId)?.totalAudioEnergy ?? 0) > (audioOf(first, alex.shareId)?.totalAudioEnergy ?? 0),
    { message: 'the audio energy rises after the tap' },
  );

  // A newer share takes the stage and the sound by itself (05 §12.2): the element that the tap started takes the
  // new source and keeps playing, with no second tap.
  const bo = await startSharing(server, asUser);
  await page.bringToFront();
  await expect(stage(page, bo)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await waitForState(page, (s) => s.viewer.audibleShareId === bo.shareId, {
    message: "the newer share is the one heard on the friend's page",
  });
  await expectHeard(page, bo);
  expect(viewerErrors).toEqual([]);
});

test('the M key and the header pill are the tap too', async ({ startServer, page, browser, asUser }) => {
  const server = await startServer();
  if (!server.admin) throw new Error('the server has no admin');
  const viewerErrors = uncaughtErrors(page);
  const quiet = await openViewer(server, page, server.admin);
  const alex = await startSharing(server, asUser);
  await page.bringToFront();
  await expectBlocked(quiet, alex);

  // M while the browser wants a gesture: the key press is that gesture (05 §12.7).
  await page.keyboard.press('m');
  await expectHeard(page, alex);
  expect(viewerErrors).toEqual([]);

  // Another friend, whose page nobody touched either, unmutes with the header's pill. A context of their own: a
  // browser that played sound in a tab may let the next tab of the site play it too.
  const account = await server.createUser();
  const context = await browser.newContext();
  try {
    await enableDebug(context);
    const other = await context.newPage();
    const otherErrors = uncaughtErrors(other);
    const otherQuiet = await openViewer(server, other, account);
    await expectBlocked(otherQuiet, alex);
    await headerPill(other).click();
    await expectHeard(other, alex);
    expect(otherErrors).toEqual([]);
  } finally {
    await context.close();
  }
});
