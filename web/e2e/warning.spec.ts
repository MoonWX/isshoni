// warning.spec (05 §19.3; 05 §13.1, §13.3): a whole screen picked together with the computer's sound stops at the
// ScreenAudioWarning, and nothing is shared until the sharer has answered it.
// - "Share without sound" publishes the picture alone: no audio track is offered, and the friend gets none.
// - "Share with sound anyway" publishes both.
// - "Pick something else" opens the picker again, and what is picked there is shared.
//
// No flag makes Chrome's picker choose a whole screen with system audio, so the first pick is always the fake
// display of 05 §19.3 (`monitor+audio`). The second pick, behind "Pick something else", is the real picker with
// the tone tab in a headful Chrome (CI), and another fake display in a headless one, which can't accept a picker.
//
// Every test has a server of its own: the tests say what a whole room shows.
import type { Locator, Page } from '@playwright/test';

import { expect, fakeDisplay, openToneTab, test } from './fixtures.ts';
import { audioOf, readState, videoOf, waitForRoom, waitForState, waitForStats, type DebugShare } from './stats.ts';

/** The key of the fake-display seam (05 §19.3), for the test that changes it between two picks. */
const FAKE_DISPLAY_KEY = 'isshoni.e2e.fakeDisplay';

/** A share that started may take this long to reach the server and a viewer. */
const MEDIA_TIMEOUT_MS = 20_000;

/** A signaling message as it crossed the page's WebSocket (01 §5). */
interface Frame {
  readonly type: string;
  readonly data?: unknown;
}

/** Collects the signaling messages a page sends, from its next navigation on. */
function sentFrames(page: Page): Frame[] {
  const frames: Frame[] = [];
  page.on('websocket', (socket) => {
    socket.on('framesent', ({ payload }) => {
      if (typeof payload === 'string') frames.push(JSON.parse(payload) as Frame);
    });
  });
  return frames;
}

/** Clicks Share in the header and Share in the sheet, with the page in front (05 §19.3). */
async function clickShare(page: Page): Promise<void> {
  await page.bringToFront();
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  const sheet = page.getByRole('dialog', { name: 'Share your screen' });
  await sheet.getByRole('button', { name: 'Share', exact: true }).click();
}

function warning(page: Page): Locator {
  return page.getByRole('dialog', { name: 'Share all sound on this computer?' });
}

/** The sharer's bar at the bottom of the page (05 §13.7). */
function sharePanel(page: Page): Locator {
  return page.getByRole('region', { name: 'Your share' });
}

/** The share this page publishes, once room.state has it live. */
function waitForOwnShare(page: Page): Promise<DebugShare> {
  return waitForState(page, (s) => s.viewer.shares.find((share) => share.local && share.status === 'live'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the page's own share is live",
  });
}

/** The share.start requests among a page's messages. */
function shareStarts(frames: readonly Frame[]): { kind: string; audio: boolean }[] {
  return frames.filter((f) => f.type === 'share.start').map((f) => f.data as { kind: string; audio: boolean });
}

/** The kinds of the tracks that the page's pub offers bound to a share: `video`, and `audio` when sound is sent. */
function publishedKinds(frames: readonly Frame[], shareId: string): string[] {
  const kinds = new Set<string>();
  for (const frame of frames) {
    if (frame.type !== 'pc.offer') continue;
    const offer = frame.data as { pc: string; tracks: { shareId: string; kind: string }[] };
    if (offer.pc !== 'pub') continue;
    for (const track of offer.tracks) if (track.shareId === shareId) kinds.add(track.kind);
  }
  return [...kinds].sort();
}

test('a whole screen with system audio asks first, and "Share without sound" publishes no audio track', async ({
  startServer,
  context,
  page,
  asUser,
}) => {
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  await fakeDisplay(page, 'monitor+audio');
  const sent = sentFrames(page);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  // The pick stops at the warning. Nothing is shared while it is open: no request, no share in the room.
  await clickShare(page);
  await expect(warning(page)).toBeVisible();
  await expect(warning(page)).toContainText('Friends will hear everything on this computer');
  await expect(sharePanel(page)).toHaveCount(0);
  expect(shareStarts(sent)).toEqual([]);
  expect((await readState(friend.page)).viewer.shares).toEqual([]);

  await warning(page).getByRole('button', { name: 'Share without sound' }).click();
  await expect(warning(page)).toHaveCount(0);

  // Live, as a screen. The share's settings have no sound switch: they say that no sound is shared.
  const own = await waitForOwnShare(page);
  expect(own.kind).toBe('screen');
  await expect(sharePanel(page)).toContainText("You're live · Screen · Auto");
  await sharePanel(page).getByRole('button', { name: 'Share settings' }).click();
  await expect(sharePanel(page)).toContainText('No sound is shared');
  await expect(sharePanel(page).getByRole('switch', { name: 'Share sound' })).toHaveCount(0);
  // On the wire: the share was announced without audio, and its offer carries a video track only.
  expect(shareStarts(sent)).toEqual([expect.objectContaining({ kind: 'screen', audio: false })]);
  expect(publishedKinds(sent, own.id)).toEqual(['video']);

  // The friend watches the picture, and receives no sound for it: no audio in the stats, nothing to listen to.
  await expect(friend.page.getByRole('region', { name: `Now watching: ${admin.username}'s screen` })) //
    .toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  const sample = await waitForStats(friend.page, (s) => (videoOf(s, own.id)?.framesDecoded ?? 0) > 10 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the friend decodes the share',
  });
  expect(audioOf(sample, own.id)).toBeUndefined();
  const watching = await readState(friend.page);
  expect(watching.viewer.focusedShareId).toBe(own.id);
  // The server says the same of the friend's subscription: the stage's layer was asked for, and there is no sound.
  expect(watching.viewer.shares[0]?.subscription).toMatchObject({
    shareId: own.id,
    requestedVideo: 'high',
    audio: 'off',
  });
});

test('"Share with sound anyway" publishes the screen with its sound', async ({
  startServer,
  context,
  page,
  asUser,
}) => {
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  await fakeDisplay(page, 'monitor+audio');
  const sent = sentFrames(page);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  await clickShare(page);
  await warning(page).getByRole('button', { name: 'Share with sound anyway' }).click();
  await expect(warning(page)).toHaveCount(0);

  const own = await waitForOwnShare(page);
  expect(own.kind).toBe('screen');
  await expect(sharePanel(page)).toContainText("You're live · Screen · Auto");
  await sharePanel(page).getByRole('button', { name: 'Share settings' }).click();
  await expect(sharePanel(page).getByRole('switch', { name: 'Share sound' })).toBeChecked();
  await expect(sharePanel(page)).not.toContainText('No sound is shared');
  expect(shareStarts(sent)).toEqual([expect.objectContaining({ kind: 'screen', audio: true })]);
  expect(publishedKinds(sent, own.id)).toEqual(['audio', 'video']);

  // The friend hears it.
  await waitForStats(
    friend.page,
    (s) => (videoOf(s, own.id)?.framesDecoded ?? 0) > 0 && (audioOf(s, own.id)?.audioLevel ?? 0) > 0.05,
    { timeout: MEDIA_TIMEOUT_MS, message: 'the friend decodes the share and hears its sound' },
  );
  expect((await readState(friend.page)).viewer).toMatchObject({ audibleShareId: own.id, audioPlaying: true });
});

test('"Pick something else" opens the picker again, and what is picked there is shared', async ({
  startServer,
  context,
  page,
  headless,
}) => {
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  await fakeDisplay(page, 'monitor+audio');
  // Headful: the tone tab is there for the real picker to select.
  if (!headless) await openToneTab(context, server);
  const sent = sentFrames(page);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  await clickShare(page);
  await expect(warning(page)).toBeVisible();

  // The next pick is another one: the real picker (headful), or a window with its own sound (headless).
  const second = headless ? 'window+audio' : null;
  await page.evaluate(
    ([key, value]) => {
      if (value === null) sessionStorage.removeItem(key);
      else sessionStorage.setItem(key, value);
    },
    [FAKE_DISPLAY_KEY, second] as const,
  );
  await page.bringToFront();
  await warning(page).getByRole('button', { name: 'Pick something else' }).click();

  // That pick needs no warning, so it is shared at once: a tab, or a window, with its sound. The screen of the
  // first pick was never announced.
  await expect(warning(page)).toHaveCount(0);
  const kind = headless ? 'window' : 'tab';
  const own = await waitForOwnShare(page);
  expect(own.kind).toBe(kind);
  await expect(sharePanel(page)).toContainText(`You're live · ${headless ? 'Window' : 'Tab'} · Auto`);
  await expect(sharePanel(page)).not.toContainText('sound');
  expect(shareStarts(sent)).toEqual([expect.objectContaining({ kind, audio: true })]);
  expect(publishedKinds(sent, own.id)).toEqual(['audio', 'video']);
  expect((await readState(page)).viewer.shares).toHaveLength(1);
});
