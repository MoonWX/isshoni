// watch.spec (05 §19.3, 01 §19): one person shares with the real Share button, and a friend watches through the
// real server.
// - The share goes live, and room.state lists both of its layers, `high` and `low`.
// - The friend's stage decodes it at the high layer (a frame height of 540 or more) and hears it: a sound level
//   above 0.05, with its energy rising. The stage fits the window, with its controls.
// - The sharer sees the friend's name among the people who watch.
// - A friend who reloads the page watches again, and is counted once.
// - Stop ends it for everyone; the same page shares again, and a friend who comes later watches that. The share
//   also ends, at once, when the sharer's tab closes, and when what is shared goes away.
//
// What is shared: in a headful Chrome (CI, under Xvfb) the tone tab, e2e/tone.html, which Chrome's own tab picker
// selects by its title. A headless Chrome can't accept the picker, so a local headless run shares the fake display
// of 05 §19.3 instead: a canvas and the same 1 kHz tone, reported as a tab with its sound. Everything after the
// pick is the same code.
//
// Every test has a server of its own: the tests say what a whole room shows (its only share, an empty stage).
import type { BrowserContext, Locator, Page } from '@playwright/test';

import { expect, fakeDisplay, openToneTab, test, type E2EServer } from './fixtures.ts';
import {
  audioOf,
  readState,
  videoOf,
  waitForRoom,
  waitForState,
  waitForStats,
  type DebugShare,
  type StatsSample,
} from './stats.ts';

/** A share that started may take this long to reach the server and a viewer: a picker, two PeerConnections. */
const MEDIA_TIMEOUT_MS = 20_000;
/**
 * The high layer may come this much later than the low one. A sharer whose machine is busy (a CI runner encodes
 * both layers in software, next to everything else) starts with the low layer alone and adds the high one when its
 * bandwidth estimate allows it: within a second on an idle machine, and 10 to 17 s after the start in a container
 * that was held to two CPUs.
 */
const HIGH_LAYER_TIMEOUT_MS = 40_000;
/** "At once": what ends without a grace period or a timeout is gone within this, on a busy CI machine too. */
const AT_ONCE_MS = 3_000;

/**
 * Gives `page` something to share; call it before the page loads the app. Headful: the tone tab, opened in the
 * page's context and sounding, which is returned. Headless: the fake display, and null.
 */
async function prepareSource(
  context: BrowserContext,
  page: Page,
  server: E2EServer,
  headless: boolean,
): Promise<Page | null> {
  if (headless) {
    await fakeDisplay(page, 'browser+audio');
    return null;
  }
  const tone = await openToneTab(context, server);
  await tone.waitForFunction(() => (window as unknown as { __tone?: { audio: string } }).__tone?.audio === 'running');
  return tone;
}

/**
 * Clicks Share in the header and Share in the sheet: the picker opens (and picks the tone tab), or the fake
 * display starts. The page comes to the front first: Chrome refuses a pick from a page that is not (05 §19.3).
 */
async function clickShare(page: Page): Promise<void> {
  await page.bringToFront();
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  const sheet = page.getByRole('dialog', { name: 'Share your screen' });
  await sheet.getByRole('button', { name: 'Share', exact: true }).click();
}

/** The sharer's bar at the bottom of the page (05 §13.7). */
function sharePanel(page: Page): Locator {
  return page.getByRole('region', { name: 'Your share' });
}

/** The toasts: what a room.event says (05 §11.1). */
function toasts(page: Page): Locator {
  return page.locator('section[aria-label="Notifications"]');
}

/** The exceptions nobody caught on a page, from now on: a test expects none. */
function uncaughtErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(String(error.stack ?? error)));
  return errors;
}

/** The share this page publishes, once room.state has it live. */
function waitForOwnShare(page: Page): Promise<DebugShare> {
  return waitForState(page, (s) => s.viewer.shares.find((share) => share.local && share.status === 'live'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the page's own share is live",
  });
}

/** Waits until the page's room.state lists both layers of the share, `high` and `low`. */
function waitForBothLayers(page: Page, shareId: string): Promise<DebugShare> {
  return waitForState(
    page,
    (s) =>
      s.viewer.shares.find(
        (share) => share.id === shareId && share.layers.includes('high') && share.layers.includes('low'),
      ),
    { timeout: HIGH_LAYER_TIMEOUT_MS, message: `room.state lists the layers high and low of ${shareId}` },
  );
}

/** Waits until the page decodes the share's video and hears its sound, and returns that sample. */
function waitForPlayback(page: Page, shareId: string): Promise<StatsSample> {
  return waitForStats(
    page,
    (s) => (videoOf(s, shareId)?.framesDecoded ?? 0) > 0 && (audioOf(s, shareId)?.audioLevel ?? 0) > 0.05 && s,
    { timeout: MEDIA_TIMEOUT_MS, message: `decoded video and audible sound of ${shareId}` },
  );
}

/** Waits until the page shows no share at all: no tile, nothing on the stage, the room's empty state. */
async function expectNoShare(page: Page, timeout: number): Promise<void> {
  await waitForState(page, (s) => s.viewer.shares.length === 0 && s.viewer.focusedShareId === null, {
    timeout,
    message: `no share on ${page.url()}`,
  });
  await expect(page.getByText('Nobody is sharing yet.')).toBeVisible();
}

test('a friend watches a share: both layers, the high one on the stage, with its sound', async ({
  startServer,
  context,
  page,
  asUser,
  headless,
}) => {
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  // Long enough for a high layer that takes its time (HIGH_LAYER_TIMEOUT_MS), with everything else.
  test.setTimeout(120_000);
  await server.signIn(context, admin);
  await prepareSource(context, page, server, headless);
  const sharerErrors = uncaughtErrors(page);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  const friend = await asUser(server);
  const friendErrors = uncaughtErrors(friend.page);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');
  await expect(friend.page.getByText('Nobody is sharing yet.')).toBeVisible();

  await clickShare(page);

  // The friend is told, and the newest share is on the stage by itself.
  const stage = friend.page.getByRole('region', { name: `Now watching: ${admin.username}'s tab` });
  await expect(stage).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await expect(toasts(friend.page)).toContainText(`${admin.username} started sharing`);

  // The sharer: live, as a tab that came with its sound (no "no sound is shared" note), and room.state lists both
  // of its layers.
  const own = await waitForOwnShare(page);
  expect(own).toMatchObject({ kind: 'tab', own: true, local: true });
  await expect(sharePanel(page)).toContainText("You're live · Tab · Auto");
  await expect(sharePanel(page)).not.toContainText('sound');
  await waitForBothLayers(page, own.id);

  // The friend's page has the same share, with both layers, on the stage, and it is the one that is heard.
  const seen = await waitForState(
    friend.page,
    (s) => s.viewer.focusedShareId === own.id && s.viewer.audibleShareId === own.id && s,
    { message: "the share is focused and audible on the friend's page" },
  );
  expect(seen.viewer.shares).toHaveLength(1);
  expect(seen.viewer.shares[0]).toMatchObject({ id: own.id, own: false, local: false, kind: 'tab', status: 'live' });
  await waitForBothLayers(friend.page, own.id);

  // It plays: frames are decoded, at the high layer's size, and the tone is heard.
  const first = await waitForPlayback(friend.page, own.id);
  await waitForStats(friend.page, (s) => (videoOf(s, own.id)?.frameHeight ?? 0) >= 540, {
    timeout: HIGH_LAYER_TIMEOUT_MS,
    message: 'the stage gets the high layer: a frame height of 540 or more',
  });
  // And it keeps playing: more frames, more sound.
  const later = await waitForStats(
    friend.page,
    (s) =>
      (videoOf(s, own.id)?.framesDecoded ?? 0) > (videoOf(first, own.id)?.framesDecoded ?? 0) + 10 &&
      (audioOf(s, own.id)?.totalAudioEnergy ?? 0) > (audioOf(first, own.id)?.totalAudioEnergy ?? 0) &&
      s,
    { timeout: MEDIA_TIMEOUT_MS, message: 'more decoded frames and more audio energy than in the first sample' },
  );
  expect(audioOf(later, own.id)?.audioLevel ?? 0).toBeGreaterThan(0.05);
  expect(videoOf(later, own.id)?.codec).toMatch(/^h264\//);
  // The server says so too: the stage's layer and the sound.
  await waitForState(
    friend.page,
    (s) => s.viewer.shares[0]?.subscription?.video === 'high' && s.viewer.shares[0].subscription.audio === 'on',
    { message: "the friend's subscription is the high layer with the sound" },
  );
  expect((await readState(friend.page)).viewer).toMatchObject({
    media: 'connected',
    audio: 'playing',
    audioPlaying: true,
  });
  await expect(stage.locator('video')).toHaveJSProperty('paused', false);

  // The stage fills what the header leaves of the window (05 §12.1): the whole picture is in it, with the stage's
  // controls, and the page itself has nothing to scroll.
  await expect(stage).toBeInViewport({ ratio: 1 });
  await expect(stage.getByRole('button', { name: 'Mute' })).toBeInViewport({ ratio: 1 });
  await expect(stage.getByRole('button', { name: 'Fullscreen' })).toBeInViewport({ ratio: 1 });
  const overflow = await friend.page.evaluate(() => {
    const root = document.documentElement;
    return { x: root.scrollWidth - root.clientWidth, y: root.scrollHeight - root.clientHeight };
  });
  expect(overflow).toEqual({ x: 0, y: 0 });

  // Nobody watches unseen: the sharer sees the friend's name, and the friend sees themselves counted.
  await expect(sharePanel(page)).toContainText('1 watching');
  await sharePanel(page).getByRole('button', { name: 'Share settings' }).click();
  await expect(sharePanel(page).getByRole('listitem').filter({ hasText: friend.account.username })).toBeVisible();
  await stage.getByRole('button', { name: '1 watching. Show who is watching' }).click();
  await expect(friend.page.getByRole('dialog', { name: 'Watching now' })).toContainText('you');

  expect(sharerErrors).toEqual([]);
  expect(friendErrors).toEqual([]);
});

test('a reload watches again; stopping ends the share for everyone; the page shares again, also for a latecomer', async ({
  startServer,
  context,
  page,
  asUser,
  headless,
}) => {
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  await prepareSource(context, page, server, headless);
  const sharerErrors = uncaughtErrors(page);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  const friend = await asUser(server);
  const friendErrors = uncaughtErrors(friend.page);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  await clickShare(page);
  const firstShare = await waitForOwnShare(page);
  await waitForPlayback(friend.page, firstShare.id);
  const watching = await readState(friend.page);

  // The friend reloads the page: a new connection (01 §10.3), which watches again by itself. The sharer still
  // counts one person watching, not the page of before as a second one.
  await friend.page.reload();
  const reloaded = await waitForRoom(friend.page, 'lounge');
  expect(reloaded.connection.connectionId).not.toBe(watching.connection.connectionId);
  await waitForPlayback(friend.page, firstShare.id);
  await expect(sharePanel(page)).toContainText('1 watching');
  expect((await readState(page)).room.participants).toBe(2);

  // Stop: the bar goes, the Share button is back, and the friend's stage is empty again.
  await sharePanel(page).getByRole('button', { name: 'Stop sharing' }).click();
  await expect(sharePanel(page)).toHaveCount(0);
  await waitForState(page, (s) => s.viewer.shares.length === 0, { message: "no share on the sharer's page" });
  await expect(page.getByRole('banner').getByRole('button', { name: 'Share', exact: true })).toBeEnabled();
  await expectNoShare(friend.page, AT_ONCE_MS);
  await expect(toasts(friend.page)).toContainText(`${admin.username} stopped sharing`);

  // The same page shares again: a new share, with a new id, which the friend watches like the first.
  await clickShare(page);
  const secondShare = await waitForOwnShare(page);
  expect(secondShare.id).not.toBe(firstShare.id);
  await waitForPlayback(friend.page, secondShare.id);

  // Someone who opens the room now finds the share live, on their stage, playing.
  const latecomer = await asUser(server);
  const latecomerErrors = uncaughtErrors(latecomer.page);
  await latecomer.page.goto(`${server.url}/`);
  await waitForRoom(latecomer.page, 'lounge');
  await expect(latecomer.page.getByRole('region', { name: `Now watching: ${admin.username}'s tab` })) //
    .toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  const sample = await waitForPlayback(latecomer.page, secondShare.id);
  expect(videoOf(sample, secondShare.id)?.frameHeight ?? 0).toBeGreaterThan(0);
  await expect(sharePanel(page)).toContainText('2 watching');
  expect(sharerErrors).toEqual([]);

  // The sharer's tab closes: the page says goodbye (close 1000, 01 §4.2), so the share is gone at once for both,
  // not after a grace period with a frozen picture.
  await page.close();
  await expectNoShare(friend.page, AT_ONCE_MS);
  await expectNoShare(latecomer.page, AT_ONCE_MS);
  expect(friendErrors).toEqual([]);
  expect(latecomerErrors).toEqual([]);
});

test('headful: the share ends when the shared tab is closed', async ({
  startServer,
  context,
  page,
  asUser,
  headless,
}) => {
  test.skip(
    headless,
    'the fake display of a headless run never ends by itself; CI runs headful, a local run with --headed',
  );

  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  const tone = await prepareSource(context, page, server, headless);
  if (!tone) throw new Error('a headful run shares the tone tab');
  const sharerErrors = uncaughtErrors(page);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  await clickShare(page);
  const own = await waitForOwnShare(page);
  await waitForPlayback(friend.page, own.id);

  // What is shared goes away, as when the sharer closes the window they share or clicks the browser's own "Stop
  // sharing": the capture ends by itself, and the page ends the share. Nothing failed, so nothing is reported.
  await tone.close();
  await expect(sharePanel(page)).toHaveCount(0, { timeout: AT_ONCE_MS });
  await waitForState(page, (s) => s.viewer.shares.length === 0, { message: "no share on the sharer's page" });
  await expect(page.getByRole('banner').getByRole('button', { name: 'Share', exact: true })).toBeEnabled();
  await expect(page.getByRole('alert')).toHaveCount(0);
  await expectNoShare(friend.page, AT_ONCE_MS);
  await expect(toasts(friend.page)).toContainText(`${admin.username} stopped sharing`);
  expect(sharerErrors).toEqual([]);
});
