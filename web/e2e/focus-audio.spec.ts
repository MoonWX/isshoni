// focus-audio.spec (05 §19.3, 01 §19; 05 §12.2–§12.3, §12.5): people share, a friend watches, through the real
// server. What is on the friend's stage, and what they hear:
// - The newest share takes the stage by itself, with its sound, and only its sound reaches the friend: the server
//   forwards no audio of the other share, whose bytes stand still on the friend's side.
// - A click on the other tile moves the stage and the sound to that share within 2 s.
// - A tile's speaker button moves the sound alone: the stage stays, and its "Listen here" brings the sound back.
// - A share without sound takes the stage like any other, and then nothing is heard: no sound is asked for, and the
//   stage has no Mute; the speaker button of another share still brings that one's sound (README S72).
// - After a pick, a new share takes neither the stage nor the sound: a toast says so, with Watch. In a fullscreen
//   stage the toast is inside it (a browser draws nothing outside the fullscreen element), and Watch works there.
//
// What is shared: the fake display of 05 §19.3 (a canvas, with a 1 kHz tone for `+audio`), in a headful run too.
// These tests are about what the viewer does with several shares; the real picker's tab capture is watch.spec's.
//
// Every test has a server of its own: the tests say what a whole room shows.
import type { Locator, Page } from '@playwright/test';

import {
  expect,
  fakeDisplay,
  test,
  type Account,
  type E2EServer,
  type FakeDisplay,
  type TestFixtures,
} from './fixtures.ts';
import {
  audioOf,
  readState,
  readStats,
  waitForRoom,
  waitForState,
  type DebugShare,
  type DebugState,
  type StatsSample,
} from './stats.ts';

// Three pages that encode and decode: room for a CI machine that runs them all in software.
test.describe.configure({ timeout: 120_000 });

/** A share that started may take this long to reach the server and a viewer: two PeerConnections. */
const MEDIA_TIMEOUT_MS = 20_000;
/** A click on a tile moves the stage and the sound within this (05 §19.3). */
const FLIP_MS = 2_000;
/**
 * A share's sound counts as not forwarded when its bytes stand still for this long: far more than the 20 ms between
 * two Opus packets of a tone, and long enough for the packets that were on their way when it stopped.
 */
const QUIET_MS = 1_000;
const STATS_INTERVAL_MS = 250;

// ---- People and shares ----

/** One more person in the lounge: their account and their page. */
interface Person {
  readonly account: Account;
  readonly page: Page;
}

/** Someone who shares: and the share their page publishes. */
interface Sharer extends Person {
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

/** One more person opens the lounge, ready to share the fake display `display`. */
async function joinLounge(server: E2EServer, asUser: TestFixtures['asUser'], display: FakeDisplay): Promise<Person> {
  const user = await asUser(server);
  await fakeDisplay(user.page, display);
  await user.page.goto(`${server.url}/`);
  await waitForRoom(user.page, 'lounge');
  return { account: user.account, page: user.page };
}

/**
 * Clicks Share in the header and Share in the sheet, with the page in front (05 §19.3); returns once the share is
 * live.
 */
async function share(person: Person): Promise<Sharer> {
  const { page } = person;
  await page.bringToFront();
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  const sheet = page.getByRole('dialog', { name: 'Share your screen' });
  await sheet.getByRole('button', { name: 'Share', exact: true }).click();
  const own = await waitForState(page, (s) => s.viewer.shares.find((x) => x.local && x.status === 'live'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${person.account.username}'s share is live`,
  });
  return { ...person, shareId: own.id };
}

/** One more person opens the lounge and shares; returns once the share is live. */
async function startSharing(server: E2EServer, asUser: TestFixtures['asUser'], display: FakeDisplay): Promise<Sharer> {
  return share(await joinLounge(server, asUser, display));
}

// ---- The viewer's page ----

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** The stage's accessible name while it shows the person's share (a fake display is a window, 05 §19.3). */
function stageName(person: Person): string {
  return `Now watching: ${person.account.username}'s window`;
}

/** The stage while it shows the person's share. */
function stage(page: Page, person: Person): Locator {
  return page.getByRole('region', { name: stageName(person) });
}

/** The main button of the person's tile: "Watch bo's window, 1 watching". */
function tile(page: Page, person: Person): Locator {
  return page.getByRole('button', { name: new RegExp(`^Watch ${escapeRegExp(person.account.username)}'s window,`) });
}

/** The speaker button of the person's tile (05 §12.3). */
function speaker(page: Page, person: Person): Locator {
  return page.getByRole('button', { name: `Listen to ${person.account.username}`, exact: true });
}

/** The toasts: what a room.event says (05 §11.1). */
function toasts(scope: Page | Locator): Locator {
  return scope.locator('section[aria-label="Notifications"]');
}

/** The toast that says the person started sharing. */
function startedToast(scope: Page | Locator, person: Person): Locator {
  return toasts(scope)
    .getByRole('listitem')
    .filter({ hasText: `${person.account.username} started sharing` });
}

/** The share as the page's state has it. */
function shareIn(state: DebugState, sharer: Sharer): DebugShare | undefined {
  return state.viewer.shares.find((s) => s.id === sharer.shareId);
}

/** Notes a measured time in the test's report, next to the limit it was held against. */
function measured(what: string, ms: number): void {
  test.info().annotations.push({ type: 'measured', description: `${what}: ${String(Math.round(ms))} ms` });
}

/** The exceptions nobody caught on a page, from now on: a test expects none. */
function uncaughtErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(String(error.stack ?? error)));
  return errors;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// ---- What is heard ----

/** The audio bytes the page has received for a share: cumulative, 0 while it has no audio stream. */
function audioBytes(sample: StatsSample, shareId: string): number {
  return audioOf(sample, shareId)?.bytesReceived ?? 0;
}

interface SoundOptions {
  /** How far apart the two samples are, at least; 0 compares each sample with the one before. Default QUIET_MS. */
  windowMs?: number;
  /** Default MEDIA_TIMEOUT_MS. */
  timeout?: number;
}

/**
 * Waits until the page receives the sound of `heard` and of no share in `silent`: between two samples at least
 * `windowMs` apart, the audio bytes of `heard` grew and those of the others did not. With `heard` null, only that
 * nothing grew. Returns the later sample. The error of a wait that times out has the last differences.
 */
async function waitForSoundOf(
  page: Page,
  heard: Sharer | null,
  silent: readonly Sharer[],
  options: SoundOptions = {},
): Promise<StatsSample> {
  const { windowMs = QUIET_MS, timeout = MEDIA_TIMEOUT_MS } = options;
  const deadline = Date.now() + timeout;
  const samples: StatsSample[] = [];
  let last = 'no two samples';
  for (;;) {
    const sample = await readStats(page, Math.max(deadline - Date.now(), 1_000));
    const before = samples.findLast((earlier) => sample.at - earlier.at >= windowMs);
    samples.push(sample);
    if (before) {
      const grown = (s: Sharer): number => audioBytes(sample, s.shareId) - audioBytes(before, s.shareId);
      if ((heard === null || grown(heard) > 0) && silent.every((s) => grown(s) === 0)) return sample;
      const all = heard ? [heard, ...silent] : silent;
      last = `${all.map((s) => `${s.account.username} +${String(grown(s))} B`).join(', ')} in ${String(sample.at - before.at)} ms`;
    }
    if (Date.now() >= deadline) {
      const names = silent.map((s) => s.account.username).join(', ') || 'anyone else';
      const what = heard ? `the sound of ${heard.account.username} alone, not of ${names}` : `no sound of ${names}`;
      throw new Error(`${what}: not within ${String(timeout)} ms. Last audio received: ${last}`);
    }
    await sleep(STATS_INTERVAL_MS);
  }
}

/** Waits until the page's state has `focused` on the stage, in `mode`, and `heard` as the share that is heard. */
function waitForFocus(page: Page, focused: Sharer, mode: 'auto' | 'manual', heard: Sharer | null): Promise<DebugState> {
  const sound = heard ? `${heard.account.username}'s sound heard` : 'nothing heard';
  return waitForState(
    page,
    (s) =>
      s.viewer.focusedShareId === focused.shareId &&
      s.viewer.focusMode === mode &&
      s.viewer.audibleShareId === (heard?.shareId ?? null) &&
      s,
    { message: `${focused.account.username}'s share on the stage (${mode}), ${sound}` },
  );
}

/**
 * Waits until the server's subscribe.status says what the page receives of the share (01 §8.9): the video layer it
 * asked for, and whether the audio is forwarded.
 */
function waitForSubscription(page: Page, sharer: Sharer, video: 'high' | 'low' | 'off', audio: 'on' | 'off') {
  return waitForState(
    page,
    (s) => {
      const sub = shareIn(s, sharer)?.subscription;
      return sub?.requestedVideo === video && sub.audio === audio && sub;
    },
    { message: `the server's status for ${sharer.account.username}'s share: video ${video}, audio ${audio}` },
  );
}

// ---- The tests ----

test('the newest share takes the stage by itself, and only its sound reaches the viewer', async ({
  startServer,
  page,
  asUser,
}) => {
  const server = await startServer();
  await openViewer(server, page);
  const viewerErrors = uncaughtErrors(page);

  // One share: on the stage by itself, and heard.
  const alex = await startSharing(server, asUser, 'window+audio');
  await expect(stage(page, alex)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await waitForFocus(page, alex, 'auto', alex);
  await waitForSoundOf(page, alex, []);

  // A newer one: it takes the stage and the sound by itself, and the first one moves to a tile. The room says who
  // started (a toast stays 5 s, so it is looked for from the start).
  const boInLounge = await joinLounge(server, asUser, 'window+audio');
  const told = expect(startedToast(page, boInLounge)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  // Awaited below; a test that fails before that must not report it as a second error.
  told.catch(() => undefined);
  const bo = await share(boInLounge);
  await told;
  await expect(stage(page, bo)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await expect(tile(page, alex)).toBeVisible();
  await waitForFocus(page, bo, 'auto', bo);

  // Only its sound reaches the page: the server stopped forwarding the first share's audio in the same update that
  // started the new one's (01 §11.4), and says so.
  const sample = await waitForSoundOf(page, bo, [alex]);
  expect(audioOf(sample, bo.shareId)?.audioLevel ?? 0).toBeGreaterThan(0.05);
  await waitForSubscription(page, bo, 'high', 'on');
  await waitForSubscription(page, alex, 'low', 'off');
  expect((await readState(page)).viewer).toMatchObject({ audio: 'playing', audioPlaying: true });
  // And it stays that way.
  await waitForSoundOf(page, bo, [alex], { windowMs: 2 * QUIET_MS });
  expect(viewerErrors).toEqual([]);
});

test('a click on the other tile moves the stage and the sound within 2 s; a speaker button moves the sound alone', async ({
  startServer,
  page,
  asUser,
}) => {
  const server = await startServer();
  await openViewer(server, page);
  const viewerErrors = uncaughtErrors(page);
  const alex = await startSharing(server, asUser, 'window+audio');
  await waitForFocus(page, alex, 'auto', alex);
  const bo = await startSharing(server, asUser, 'window+audio');
  await waitForFocus(page, bo, 'auto', bo);
  await waitForSoundOf(page, bo, [alex]);

  // The click on the first share's tile: the stage and the sound move to it, the server forwards its audio and no
  // longer the other one's, within 2 s.
  const clickedAt = Date.now();
  await tile(page, alex).click();
  const flipped = await waitForSoundOf(page, alex, [bo], { windowMs: 0 });
  measured("the clicked share's sound alone reaches the page after the click", flipped.at - clickedAt);
  expect(flipped.at - clickedAt, "from the click to the clicked share's sound alone (ms)").toBeLessThanOrEqual(FLIP_MS);
  await waitForFocus(page, alex, 'manual', alex);
  await expect(stage(page, alex)).toBeVisible();
  await expect(tile(page, bo)).toBeVisible();
  // It holds.
  await waitForSoundOf(page, alex, [bo]);
  await waitForSubscription(page, alex, 'high', 'on');
  await waitForSubscription(page, bo, 'low', 'off');

  // The speaker button of the other tile: the sound moves to that share, and the stage stays where it is.
  await speaker(page, bo).click();
  await expect(speaker(page, bo)).toHaveAttribute('aria-pressed', 'true');
  await waitForFocus(page, alex, 'manual', bo);
  await waitForSoundOf(page, bo, [alex]);
  await waitForSubscription(page, alex, 'high', 'off');
  await waitForSubscription(page, bo, 'low', 'on');
  await expect(stage(page, alex)).toBeVisible();

  // The stage says that its share's sound is elsewhere, and brings it back.
  const listenHere = stage(page, alex).getByRole('button', {
    name: `Listen here. You're listening to ${bo.account.username} now.`,
  });
  await listenHere.click();
  await waitForFocus(page, alex, 'manual', alex);
  await waitForSoundOf(page, alex, [bo]);
  await expect(listenHere).toHaveCount(0);
  await expect(speaker(page, bo)).toHaveAttribute('aria-pressed', 'false');
  expect(viewerErrors).toEqual([]);
});

test('a share without sound takes the stage, and then nothing is heard and the stage has no Mute', async ({
  startServer,
  page,
  asUser,
}) => {
  const server = await startServer();
  await openViewer(server, page);
  const viewerErrors = uncaughtErrors(page);
  const alex = await startSharing(server, asUser, 'window+audio');
  await waitForFocus(page, alex, 'auto', alex);
  await waitForSoundOf(page, alex, []);
  await expect(stage(page, alex).getByRole('button', { name: 'Mute', exact: true })).toBeVisible();

  // A window without its sound: the newest share, so it takes the stage. It has nothing to be heard, so nothing is:
  // the sound of the share before it stops with the focus (05 §12.3), and the server is asked for no audio.
  const bo = await startSharing(server, asUser, 'window');
  await expect(stage(page, bo)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await waitForFocus(page, bo, 'auto', null);
  await waitForSubscription(page, bo, 'high', 'off');
  await waitForSubscription(page, alex, 'low', 'off');
  const quiet = await waitForSoundOf(page, null, [alex]);
  expect(audioOf(quiet, bo.shareId)).toBeUndefined();
  // The stage has no sound controls: nothing to mute, nothing to bring here. The tile of the share with sound has
  // its speaker button.
  const silentStage = stage(page, bo);
  await expect(silentStage.getByRole('button', { name: 'Mute', exact: true })).toHaveCount(0);
  await expect(silentStage.getByRole('slider', { name: 'Volume' })).toHaveCount(0);
  await expect(silentStage.getByRole('button', { name: /^Listen here/ })).toHaveCount(0);
  await expect(silentStage.getByRole('button', { name: 'Fullscreen', exact: true })).toBeVisible();
  await expect(speaker(page, alex)).toHaveAttribute('aria-pressed', 'false');

  // The other share's speaker button: its sound plays while the silent share stays on the stage, which now has a
  // Mute for what is heard.
  await speaker(page, alex).click();
  await waitForFocus(page, bo, 'auto', alex);
  await waitForSoundOf(page, alex, []);
  await expect(silentStage.getByRole('button', { name: 'Mute', exact: true })).toBeVisible();
  await expect(silentStage.getByRole('button', { name: /^Listen here/ })).toHaveCount(0);

  // A pick moves the sound with the stage: to the share that has sound, and away again with a pick of the silent
  // one.
  await tile(page, alex).click();
  await waitForFocus(page, alex, 'manual', alex);
  await tile(page, bo).click();
  await waitForFocus(page, bo, 'manual', null);
  await waitForSoundOf(page, null, [alex]);
  await expect(silentStage.getByRole('button', { name: 'Mute', exact: true })).toHaveCount(0);
  expect(viewerErrors).toEqual([]);
});

test('after a pick a new share is a toast with Watch, also inside a fullscreen stage', async ({
  startServer,
  page,
  asUser,
}) => {
  const server = await startServer();
  await openViewer(server, page);
  const viewerErrors = uncaughtErrors(page);
  const alex = await startSharing(server, asUser, 'window+audio');
  await expect(stage(page, alex)).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await waitForFocus(page, alex, 'auto', alex);

  // A pick: the number key of the first share, the one on the stage (05 §12.7). The page holds it from now on.
  await page.keyboard.press('1');
  await waitForFocus(page, alex, 'manual', alex);

  // The stage goes fullscreen: the browser's own, on the viewer's layout (05 §12.5).
  await stage(page, alex).getByRole('button', { name: 'Fullscreen', exact: true }).click();
  await waitForState(page, (s) => s.viewer.fullscreen, { message: 'the stage is fullscreen' });
  const layout = page.locator(':fullscreen');
  await expect(layout).toHaveCount(1);
  await expect(layout.getByRole('region', { name: stageName(alex) })).toBeVisible();

  // A new share. The toast that says who started is inside the fullscreen element, where the friend sees it, with
  // Watch. It stays 5 s, so it is looked for from the start, and the pointer rests on it, which holds it there.
  const boInLounge = await joinLounge(server, asUser, 'window+audio');
  const toast = startedToast(layout, boInLounge);
  const told = expect(toast).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  told.catch(() => undefined);
  const bo = await share(boInLounge);
  await told;
  await toast.hover();
  await expect(toast).toBeInViewport({ ratio: 1 });
  const watch = toast.getByRole('button', { name: 'Watch', exact: true });
  await expect(watch).toBeVisible();
  // The stage and the sound stay with the pick.
  await waitForFocus(page, alex, 'manual', alex);
  await waitForSoundOf(page, alex, [bo]);
  await expect(toast).toBeVisible();

  // Watch, from inside the fullscreen stage: the new share takes the stage and the sound, and the stage stays
  // fullscreen.
  await watch.click();
  await waitForFocus(page, bo, 'manual', bo);
  await expect(layout.getByRole('region', { name: stageName(bo) })).toBeVisible();
  await waitForSoundOf(page, bo, [alex]);
  expect((await readState(page)).viewer.fullscreen).toBe(true);

  // The toasts are in the fullscreen layout, inside the room's main area. Out of fullscreen they are back in their
  // own place, outside it.
  await expect(toasts(page.locator('main'))).toHaveCount(1);
  await stage(page, bo).getByRole('button', { name: 'Exit fullscreen', exact: true }).click();
  await waitForState(page, (s) => !s.viewer.fullscreen, { message: 'the stage is not fullscreen' });
  await expect(page.locator(':fullscreen')).toHaveCount(0);
  await expect(toasts(page)).toHaveCount(1);
  await expect(toasts(page.locator('main'))).toHaveCount(0);
  expect(viewerErrors).toEqual([]);
});
