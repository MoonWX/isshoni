// layers.spec (05 §19.3; 05 §12.4–§12.6): what a friend's page asks the server for, by what it shows (the layer
// policy of 05 §12.4), through the real server:
// - The stage gets the high layer and a thumbnail the low one: a frame height of 360 or less, while the share's
//   high layer is there to be had.
// - A fullscreen stage turns the thumbnails off: their video bytes stop within 3 s, and come back after it.
// - A tile scrolled out of view stops receiving video bytes within 3 s, and one scrolled into view receives them.
// - A tile whose video stops arriving says "Waiting for video…" (05 §12.6), and plays again when it comes back.
//
// What is shared: the fake display of 05 §19.3, a 1280 × 720 canvas, as a window without sound, in a headful run
// too. Its low layer is 640 × 360 (02 §8.6: 230 400 pixels), its high one 1280 × 720.
//
// Every test has a server of its own: the tests say what a whole room shows.
import type { Locator, Page } from '@playwright/test';

import { expect, fakeDisplay, test, type Account, type E2EServer, type TestFixtures } from './fixtures.ts';
import {
  readState,
  readStats,
  videoOf,
  waitForRoom,
  waitForState,
  waitForStats,
  type DebugShare,
  type DebugState,
  type StatsSample,
} from './stats.ts';

/** A share that started may take this long to reach the server and a viewer: two PeerConnections. */
const MEDIA_TIMEOUT_MS = 20_000;
/**
 * The high layer may come this much later than the low one. A sharer whose machine is busy (a CI runner encodes
 * every layer of every sharer in software) starts with the low layer alone and adds the high one when its bandwidth
 * estimate allows it: within a second on an idle machine, 10 to 17 s after the start in a container held to two
 * CPUs (watch.spec).
 */
const HIGH_LAYER_TIMEOUT_MS = 40_000;
/** What leaves the view stops receiving video within this (05 §19.3): 1 s of SUBSCRIBE_OFF_DELAY_MS, and the trip. */
const STOP_MS = 3_000;
/** Video counts as not forwarded when its bytes stand still for this long: a 15 fps layer sends every 67 ms. */
const QUIET_MS = 1_000;
/** The thumbnails' layer: 640 × 360 from the fake display's 1280 × 720. */
const LOW_HEIGHT = 360;
/** The stage's layer: the fake display's own size, well above what any lower layer has. */
const HIGH_HEIGHT = 540;
const STATS_INTERVAL_MS = 250;

// ---- People and shares ----

/** Someone who shares: their account, their page, and the share it publishes. */
interface Sharer {
  readonly account: Account;
  readonly page: Page;
  readonly shareId: string;
}

/** Signs the default context in as the admin and opens the lounge: the friend who watches. */
async function openViewer(server: E2EServer, page: Page): Promise<void> {
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(page.context(), admin);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');
}

interface SharingOptions {
  /** Runs before the sharer's page loads the app. */
  prepare?: (page: Page) => Promise<void>;
  /**
   * The sharer puts their own preview on their stage (a pick, 05 §12.2), so that their page decodes the others'
   * shares as thumbnails only: less work for a machine that runs many pages.
   */
  ownStage?: boolean;
}

/** One more person opens the lounge and shares a window, without sound; returns once the share is live. */
async function startSharing(
  server: E2EServer,
  asUser: TestFixtures['asUser'],
  options: SharingOptions = {},
): Promise<Sharer> {
  const user = await asUser(server);
  await fakeDisplay(user.page, 'window');
  await options.prepare?.(user.page);
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
  if (options.ownStage === true) {
    await user.page.getByRole('button', { name: /^Show your preview on the stage/ }).click();
    await waitForState(user.page, (s) => s.viewer.focusedShareId === own.id, {
      message: `${user.account.username}'s own preview is on their stage`,
    });
  }
  return { account: user.account, page: user.page, shareId: own.id };
}

// ---- The friend's page ----

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** The stage while it shows the sharer's share. */
function stage(page: Page, sharer: Sharer): Locator {
  return page.getByRole('region', { name: `Now watching: ${sharer.account.username}'s window` });
}

/** The main button of the sharer's tile: "Watch bo's window, 1 watching". */
function tile(page: Page, sharer: Sharer): Locator {
  return page.getByRole('button', { name: new RegExp(`^Watch ${escapeRegExp(sharer.account.username)}'s window,`) });
}

/** The list of tiles: a column from 1024 px, else a strip under the stage (05 §12.1). */
function tileList(page: Page): Locator {
  return page.getByRole('list', { name: 'Shares' });
}

/** The share as the page's state has it. */
function shareIn(state: DebugState, sharer: Sharer): DebugShare | undefined {
  return state.viewer.shares.find((s) => s.id === sharer.shareId);
}

/** Notes a measured time in the test's report, next to the limit it was held against. */
function measured(what: string, ms: number): void {
  test.info().annotations.push({ type: 'measured', description: `${what}: ${String(Math.round(ms))} ms` });
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** The exceptions nobody caught on a page, from now on: a test expects none. */
function uncaughtErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(String(error.stack ?? error)));
  return errors;
}

/**
 * Waits until the server's subscribe.status for the page says that it asked for this video layer of the share, and
 * that the server forwards it (01 §8.9). With `forwarded` false, only what the page asked for: the server may hold
 * the high layer back for the viewer's bandwidth (02 §10.2), and a busy CI machine may look like a slow link.
 */
function waitForLayer(page: Page, sharer: Sharer, layer: 'high' | 'low' | 'off', forwarded = true) {
  const what = forwarded ? 'forwards' : 'was asked for';
  return waitForState(
    page,
    (s) => {
      const sub = shareIn(s, sharer)?.subscription;
      return sub?.requestedVideo === layer && (!forwarded || sub.video === layer) && sub;
    },
    { message: `the server ${what} the ${layer} layer of ${sharer.account.username}'s share` },
  );
}

/** Waits until the page's state says which of the shares' tiles are in view (05 §12.4: at least 10 %). */
function waitForVisible(page: Page, shown: readonly Sharer[], hidden: readonly Sharer[]) {
  const names = (list: readonly Sharer[]) => list.map((s) => s.account.username).join(', ') || 'none';
  return waitForState(
    page,
    (s) => shown.every((x) => shareIn(s, x)?.visible === true) && hidden.every((x) => shareIn(s, x)?.visible === false),
    { message: `in view: ${names(shown)}; out of view: ${names(hidden)}` },
  );
}

// ---- What arrives ----

/** The video bytes the page has received for a share: cumulative, 0 while it has no video stream. */
function videoBytes(sample: StatsSample, shareId: string): number {
  return videoOf(sample, shareId)?.bytesReceived ?? 0;
}

/**
 * Waits until the page receives video of every share in `flowing` and none of `stopped`: between two samples at
 * least QUIET_MS apart, the video bytes of the first grew and those of the others did not. Returns the time of the
 * first sample from which each share of `stopped` received nothing more (an upper bound of when it stopped). The
 * error of a wait that times out has the last differences.
 */
async function waitForVideo(
  page: Page,
  flowing: readonly Sharer[],
  stopped: readonly Sharer[],
  timeout = MEDIA_TIMEOUT_MS,
): Promise<number> {
  const deadline = Date.now() + timeout;
  const samples: StatsSample[] = [];
  let last = 'no two samples';
  for (;;) {
    const sample = await readStats(page, Math.max(deadline - Date.now(), 1_000));
    const before = samples.findLast((earlier) => sample.at - earlier.at >= QUIET_MS);
    samples.push(sample);
    if (before) {
      const grown = (x: Sharer): number => videoBytes(sample, x.shareId) - videoBytes(before, x.shareId);
      if (flowing.every((x) => grown(x) > 0) && stopped.every((x) => grown(x) === 0)) {
        // The first sample that has every final count of `stopped`.
        const final = (s: StatsSample): boolean =>
          stopped.every((x) => videoBytes(s, x.shareId) === videoBytes(sample, x.shareId));
        return (samples.find(final) ?? sample).at;
      }
      const all = [...flowing, ...stopped];
      last = `${all.map((x) => `${x.account.username} +${String(grown(x))} B`).join(', ')} in ${String(sample.at - before.at)} ms`;
    }
    if (Date.now() >= deadline) {
      const names = (list: readonly Sharer[]) => list.map((x) => x.account.username).join(', ') || 'none';
      throw new Error(
        `video of ${names(flowing)}, and none of ${names(stopped)}: not within ${String(timeout)} ms. ` +
          `Last video received: ${last}`,
      );
    }
    await sleep(STATS_INTERVAL_MS);
  }
}

// ---- The tests ----

test('the stage gets the high layer and a thumbnail the low one; a fullscreen stage turns the thumbnails off', async ({
  startServer,
  page,
  asUser,
}) => {
  // Long enough for a high layer that takes its time, twice.
  test.setTimeout(150_000);
  const server = await startServer();
  await openViewer(server, page);
  const viewerErrors = uncaughtErrors(page);
  const alex = await startSharing(server, asUser);
  const bo = await startSharing(server, asUser);

  // The newest share is on the stage, the other one in the column of tiles (05 §12.1: a window 1024 px wide or
  // more).
  await expect(stage(page, bo)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await expect(tile(page, alex)).toBeVisible();

  // The thumbnail: the low layer, while the share has the high one too.
  await waitForState(page, (s) => shareIn(s, alex)?.layers.includes('high'), {
    timeout: HIGH_LAYER_TIMEOUT_MS,
    message: `room.state lists the high layer of ${alex.account.username}'s share`,
  });
  await waitForLayer(page, alex, 'low');
  const thumbnail = await waitForStats(page, (s) => (videoOf(s, alex.shareId)?.framesDecoded ?? 0) > 0 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the thumbnail decodes',
  });
  const later = await waitForStats(
    page,
    (s) =>
      (videoOf(s, alex.shareId)?.framesDecoded ?? 0) > (videoOf(thumbnail, alex.shareId)?.framesDecoded ?? 0) + 15 && s,
    { timeout: MEDIA_TIMEOUT_MS, message: 'the thumbnail keeps decoding' },
  );
  for (const sample of [thumbnail, later]) {
    const height = videoOf(sample, alex.shareId)?.frameHeight ?? 0;
    expect(height, "the thumbnail's frame height").toBeGreaterThan(0);
    expect(height, "the thumbnail's frame height").toBeLessThanOrEqual(LOW_HEIGHT);
  }

  // The stage: the high layer.
  await waitForStats(page, (s) => (videoOf(s, bo.shareId)?.frameHeight ?? 0) >= HIGH_HEIGHT, {
    timeout: HIGH_LAYER_TIMEOUT_MS,
    message: `the stage gets the high layer: a frame height of ${String(HIGH_HEIGHT)} or more`,
  });
  await waitForLayer(page, bo, 'high', false);

  // Fullscreen: the stage alone fills the screen, and the thumbnail that nobody can see gets no video. The stage
  // keeps its own.
  await stage(page, bo).getByRole('button', { name: 'Fullscreen', exact: true }).click();
  const fullscreenAt = Date.now();
  await waitForState(page, (s) => s.viewer.fullscreen, { message: 'the stage is fullscreen' });
  await expect(page.locator(':fullscreen')).toHaveCount(1);
  await expect(tile(page, alex)).toHaveCount(0);
  const stoppedAt = await waitForVideo(page, [bo], [alex]);
  measured('the thumbnail receives no more video after the click on Fullscreen', stoppedAt - fullscreenAt);
  expect(stoppedAt - fullscreenAt, 'from the click on Fullscreen to the last video of the thumbnail (ms)') //
    .toBeLessThanOrEqual(STOP_MS);
  await waitForLayer(page, alex, 'off');
  await waitForLayer(page, bo, 'high', false);

  // Out of fullscreen the thumbnail is back, and so is its video, at the low layer.
  await stage(page, bo).getByRole('button', { name: 'Exit fullscreen', exact: true }).click();
  await waitForState(page, (s) => !s.viewer.fullscreen, { message: 'the stage is not fullscreen' });
  await expect(tile(page, alex)).toBeVisible();
  await waitForLayer(page, alex, 'low');
  const back = await readStatsAfter(page, alex);
  expect(videoOf(back, alex.shareId)?.frameHeight ?? 0).toBeLessThanOrEqual(LOW_HEIGHT);
  expect(viewerErrors).toEqual([]);
});

test('a tile scrolled out of view stops receiving video within 3 s, and one scrolled into view receives it', async ({
  startServer,
  page,
  asUser,
}) => {
  // Four people share; each of their pages encodes its own share and decodes the others' as thumbnails.
  test.setTimeout(150_000);
  const server = await startServer();
  // A phone held upright, 320 px wide: the tiles are a strip under the stage, 44 % of the width each (05 §12.1),
  // so the strip shows two of the three and a sliver of the third, too little to count as shown (under 10 %).
  await page.setViewportSize({ width: 320, height: 640 });
  await openViewer(server, page);
  const viewerErrors = uncaughtErrors(page);
  const alex = await startSharing(server, asUser, { ownStage: true });
  const bo = await startSharing(server, asUser, { ownStage: true });
  const cy = await startSharing(server, asUser, { ownStage: true });
  const dee = await startSharing(server, asUser, { ownStage: true });

  // Dee's share, the newest, is on the stage; the strip has Cy's, Bo's and Alex's, newest first.
  await expect(stage(page, dee)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  for (const sharer of [cy, bo, alex]) await expect(tile(page, sharer)).toHaveCount(1);
  const list = tileList(page);
  await expect(list.getByRole('listitem')).toHaveCount(3);

  // The strip is still at its start: Cy's tile, which came in at the front when Dee's share took the stage, is in
  // view, and Alex's, the last one, is not. (Chrome re-snaps a strip to the tile it was snapped to, Bo's, when a
  // tile comes in before it; the layout keeps a strip that was at its start there, README S72.) The server sends no
  // video for the tile out of view, and the two in view get theirs.
  expect(await list.evaluate((el) => el.scrollLeft)).toBe(0);
  await waitForVisible(page, [cy, bo], [alex]);
  await waitForLayer(page, alex, 'off');
  await waitForLayer(page, cy, 'low');
  await waitForLayer(page, bo, 'low');
  await waitForVideo(page, [cy, bo, dee], [alex]);

  // The strip is scrolled to its end: Cy's tile leaves the view, and Alex's comes in.
  await list.evaluate((el) => {
    el.scrollTo({ left: el.scrollWidth, behavior: 'instant' });
  });
  const scrolledAt = Date.now();
  await waitForVisible(page, [bo, alex], [cy]);
  const stoppedAt = await waitForVideo(page, [bo, alex, dee], [cy]);
  measured("Cy's tile receives no more video after it left the view", stoppedAt - scrolledAt);
  expect(stoppedAt - scrolledAt, "from the scroll to the last video of Cy's tile (ms)").toBeLessThanOrEqual(STOP_MS);
  await waitForLayer(page, cy, 'off');
  await waitForLayer(page, alex, 'low');
  const alexBack = await readStatsAfter(page, alex);
  expect(videoOf(alexBack, alex.shareId)?.frameHeight ?? 0).toBeLessThanOrEqual(LOW_HEIGHT);

  // And back: Cy's tile is in view again and gets its video; Alex's goes.
  await list.evaluate((el) => {
    el.scrollTo({ left: 0, behavior: 'instant' });
  });
  await waitForVisible(page, [cy, bo], [alex]);
  await waitForVideo(page, [cy, bo, dee], [alex]);
  await waitForLayer(page, cy, 'low');
  expect(viewerErrors).toEqual([]);
});

test('a tile whose video stops arriving says "Waiting for video…", and plays again when it comes back', async ({
  startServer,
  page,
  asUser,
}) => {
  test.setTimeout(120_000);
  const server = await startServer();
  await openViewer(server, page);
  const viewerErrors = uncaughtErrors(page);
  // Alex's page notes its PeerConnections, so the test can take the picture off its sender.
  const alex = await startSharing(server, asUser, { prepare: notePeerConnections });
  const bo = await startSharing(server, asUser);
  await expect(stage(page, bo)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await waitForLayer(page, alex, 'low');
  await waitForVideo(page, [alex, bo], []);
  const thumbnail = tile(page, alex);
  await expect(thumbnail).not.toContainText('Waiting for video…');

  // No more frames from Alex's page: the sender sends nothing (replaceTrack(null)), while the share stays live and
  // its tile asks for video. After 3 s without a new frame, measured at the stats' pace of 2 s, the tile says so.
  await alex.page.evaluate(async () => {
    const pcs = (window as unknown as { __e2ePCs: Set<RTCPeerConnection> }).__e2ePCs;
    const held: [RTCRtpSender, MediaStreamTrack][] = [];
    for (const pc of pcs) {
      for (const sender of pc.getSenders()) {
        if (sender.track?.kind !== 'video') continue;
        held.push([sender, sender.track]);
        await sender.replaceTrack(null);
      }
    }
    if (held.length === 0) throw new Error('no video sender on the page');
    (window as unknown as { __e2eHeld: typeof held }).__e2eHeld = held;
  });
  const frozenAt = Date.now();
  await waitForState(page, (s) => shareIn(s, alex)?.frozen, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${alex.account.username}'s tile is frozen`,
  });
  measured('the tile says "Waiting for video…" after its last frame was sent', Date.now() - frozenAt);
  await expect(thumbnail).toHaveAccessibleDescription('Waiting for video…');
  await expect(thumbnail).toContainText('Waiting for video…');
  // Only that tile: the stage plays on.
  const state = await readState(page);
  expect(shareIn(state, alex)).toMatchObject({ status: 'live', frozen: true });
  expect(shareIn(state, bo)?.frozen).toBe(false);
  await expect(stage(page, bo)).not.toContainText('Waiting for video…');

  // The picture comes back: the tile plays again, and says nothing anymore.
  await alex.page.evaluate(async () => {
    const held = (window as unknown as { __e2eHeld: [RTCRtpSender, MediaStreamTrack][] }).__e2eHeld;
    for (const [sender, track] of held) await sender.replaceTrack(track);
  });
  await waitForState(page, (s) => shareIn(s, alex)?.frozen === false, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${alex.account.username}'s tile is no longer frozen`,
  });
  await expect(thumbnail).not.toContainText('Waiting for video…');
  await readStatsAfter(page, alex);
  expect(viewerErrors).toEqual([]);
});

/**
 * Keeps the page's PeerConnections in window.__e2ePCs as the app adds its transceivers (the publisher adds each
 * track with addTransceiver, 05 §13.4). Call it before the page loads.
 */
async function notePeerConnections(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const pcs = new Set<RTCPeerConnection>();
    (window as unknown as { __e2ePCs: Set<RTCPeerConnection> }).__e2ePCs = pcs;
    // eslint-disable-next-line @typescript-eslint/unbound-method -- called below with the PeerConnection as `this`
    const addTransceiver = RTCPeerConnection.prototype.addTransceiver;
    RTCPeerConnection.prototype.addTransceiver = function (
      this: RTCPeerConnection,
      ...args: Parameters<RTCPeerConnection['addTransceiver']>
    ) {
      pcs.add(this);
      return addTransceiver.apply(this, args);
    };
  });
}

/** Waits until the page decodes more frames of the share than it has now, and returns that sample. */
async function readStatsAfter(page: Page, sharer: Sharer): Promise<StatsSample> {
  const now = await waitForStats(page, (s) => s, { message: 'a sample' });
  const before = videoOf(now, sharer.shareId)?.framesDecoded ?? 0;
  return waitForStats(page, (s) => (videoOf(s, sharer.shareId)?.framesDecoded ?? 0) > before + 5 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `more decoded frames of ${sharer.account.username}'s share`,
  });
}
