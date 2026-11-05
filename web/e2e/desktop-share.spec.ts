// desktop-share.spec (M2 03 §22.4 A, README WA65): the desktop app shares natively, with its real Go parts
// (internal/desktop/e2ehost: the share controller, C2 on 01's core, 05's audio service) on the fake engine's 640×360
// Decodable pattern with its tone, and the desktop SPA in Chrome at http://wails.localhost:
// - Share → the desktop ShareSheet → the fake pick → the first-share sheet ("Friends will see: Fake window") → Go
//   live → the panel says "You're live · Fake window · …";
// - a friend's web viewer decodes the share's frames and its audio energy (M1's stats.ts);
// - the desktop's own tile is local: subscribed with the low layer and no audio, shown as "Preview: what friends
//   see", never heard;
// - the tray says "Live, 1 watching"; the hotkeys are registered while live and named in the panel's tooltips; the
//   `normal` scenario's mic prompt shows its card in the panel (README WA80);
// - Pause from the panel, then by hotkey: the friend's tile says "Paused" with no new frames and no sound, a friend
//   who joins while paused gets no frame, Resume (button, hotkey) brings frames back, and the Stop hotkey ends the
//   share without asking, unregisters the hotkeys and releases keep-awake (03 §11.2, §11.3, §22.4; README WA80);
// - the harness's debug-level log names no window title, no share label, no exe and none of the `normal` audio
//   scenario's apps (03 §17, §22.4);
// - a second share needs no first-share sheet (tips.firstShareShown), and the panel's Stop ends it.
//
// Closing the window while live (03 §11.6, README WF17; the harness's POST /__e2e/window calls App.WindowClosing and
// the tray's Open): the window hides to the tray and the share goes on; the first close shows the toast "isshoni is
// still sharing…", a hidden window shows no notice and tips.trayNoticeShown stays unset; the tray's Open shows the
// same line as a notice at the top of the window, the page reports it shown and only then is the flag set; after
// that neither the toast nor the notice comes again.
//
// The audio alert case (05 §14.7 "Outside the window", README WA80): with ISSHONI_FAKE_AUDIO=degrade-live, a mic
// prompt is pending when the share goes live (the tray's "1 question waiting", the pill's marker), and 5 s into the
// share the fake loses process loopback: the tray warns with "Friends hear voice apps" first, the pill's state says
// it in place of the count, and exactly one voiceAppsHeard toast is recorded. In this scenario the degrade also ends
// the prompt (Chrome is heard then, so its mic row is gone, 05 §14.6), so the two show one after the other here;
// internal/desktop/app's TestAudioAlertOutsideWindow has them together.
import AxeBuilder from '@axe-core/playwright';
import fsp from 'node:fs/promises';
import type { Page } from '@playwright/test';

import { expect, test, type DesktopApp, type E2EServer } from './fixtures.ts';
import { audioOf, readStats, videoOf, waitForRoom, waitForState, waitForStats } from './stats.ts';

/** C2 connects, publishes and goes live; a viewer's sub PC follows. */
const LIVE_TIMEOUT_MS = 30_000;
const MEDIA_TIMEOUT_MS = 20_000;
/** The fake engine's picker names its window this way (internal/media/fake): the label that stays on this PC. */
const FAKE_LABEL = 'Fake window';
/** The display names of the apps in 05's `normal` fake audio scenario (internal/media/fake/audio_ctl.go). */
const NORMAL_SCENARIO_APPS = ['Spotify', 'Discord', 'Steam', 'Google Chrome'];

/** The default hotkeys, as the harness records them while they are registered (03 §11.2). */
const HOTKEYS = { pause: 'Ctrl+Alt+Shift+F9', stop: 'Ctrl+Alt+Shift+F10' };
/** The panel's paused bar (03 §10.3). */
const PAUSED_BAR = 'Paused · nothing new leaves your PC; friends see "Paused" and hear nothing';
/** How long a check of "nothing new arrives" watches (the stats are read on demand). */
const QUIET_MS = 3_000;
/** What may still be on its way when a pause lands: frames and audio sent just before it. */
const SETTLE_MS = 1_500;

test.describe.configure({ timeout: 240_000 });

/** The tray's view as the harness recorded it (tray.View: icon, tooltip, menu), or null before the first. */
interface TrayView {
  icon: string;
  tooltip: string;
  menu: { label?: string; action?: string; enabled: boolean }[];
}

function trayOf(state: { tray: unknown }): TrayView | null {
  return state.tray as TrayView | null;
}

/**
 * The share is paused on `page` (03 §11.3): its status is `paused`, its tile or stage says "Paused", and once what
 * was in flight has arrived no new frame is decoded and no sound is heard.
 */
async function expectPaused(page: Page, shareId: string, what: string): Promise<void> {
  await waitForState(page, (s) => s.viewer.shares.find((sh) => sh.id === shareId && sh.status === 'paused'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${what}: the share's status is paused`,
  });
  await expect(page.locator(`[data-share-id="${shareId}"]`).getByText('Paused', { exact: true }).first()).toBeVisible();
  await page.waitForTimeout(SETTLE_MS);
  const before = await readStats(page);
  await page.waitForTimeout(QUIET_MS);
  const after = await readStats(page);
  expect(videoOf(after, shareId)?.framesDecoded ?? 0, `${what}: no new frames`).toBe(
    videoOf(before, shareId)?.framesDecoded ?? 0,
  );
  expect(audioOf(after, shareId)?.totalAudioEnergy ?? 0, `${what}: no sound`).toBe(
    audioOf(before, shareId)?.totalAudioEnergy ?? 0,
  );
}

/** Frames of the share are decoded on `page` again, and its status is live. */
async function expectFrames(page: Page, shareId: string, what: string): Promise<void> {
  await waitForState(page, (s) => s.viewer.shares.find((sh) => sh.id === shareId && sh.status === 'live'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${what}: the share is live again`,
  });
  const before = videoOf(await readStats(page), shareId)?.framesDecoded ?? 0;
  await waitForStats(page, (s) => (videoOf(s, shareId)?.framesDecoded ?? 0) > before + 5 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${what}: new frames`,
  });
}

/** A desktop app linked to the worker's server as a new member, with its page in `roomId`. */
async function linkedDesktop(
  startDesktop: (o?: { server?: string }) => Promise<DesktopApp>,
  server: E2EServer,
  roomId: string,
): Promise<DesktopApp> {
  const account = await server.createUser();
  const desktop = await startDesktop({ server: server.url });
  const st = await desktop.call<{ status: string }>('Auth', 'LinkWithPassword', account.username, account.password);
  expect(st.status).toBe('linked');
  await desktop.page.goto(`${desktop.origin}/r/${roomId}`);
  await waitForRoom(desktop.page, roomId);
  return desktop;
}

/** The axe check of 05 §19.3 (WCAG 2.0 A and AA, 2.1 AA), in both colour schemes, on what `include` holds. */
async function expectAccessible(page: Page, what: string, include: string): Promise<void> {
  for (const colorScheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme, reducedMotion: 'reduce' });
    await page.waitForTimeout(200);
    const { violations, passes } = await new AxeBuilder({ page })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21aa'])
      .include(include)
      .analyze();
    expect(passes.length, `axe checked ${what} (${colorScheme})`).toBeGreaterThan(0);
    const serious = violations
      .filter((v) => v.impact === 'serious' || v.impact === 'critical')
      .map((v) => `${v.id}: ${v.help} (${v.nodes.map((n) => n.target.join(' ')).join(', ')})`);
    expect(serious, `axe on ${what} (${colorScheme})`).toEqual([]);
  }
  await page.emulateMedia({ colorScheme: null, reducedMotion: null });
}

test('a fake pick goes live through the first-share sheet; a browser watches; the own tile is the low layer', async ({
  startDesktop,
  server,
  asUser,
}) => {
  const room = 'lounge';
  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/r/${room}`);
  await waitForRoom(friend.page, room);
  const desktop = await linkedDesktop(startDesktop, server, room);
  const { page } = desktop;

  // Share → the desktop ShareSheet: Optimize for, Share sound (on), Share.
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  const sheet = page.getByRole('dialog', { name: 'Share your screen' });
  await expect(sheet.getByRole('radio', { name: /^Auto/ })).toBeChecked();
  await expect(sheet.getByRole('checkbox', { name: 'Share sound' })).toBeChecked();
  await expectAccessible(page, 'the desktop ShareSheet', 'dialog[open]');
  await sheet.getByRole('button', { name: 'Share', exact: true }).click();

  // The fake picker answers at once; the first share of this install shows the first-share sheet.
  const first = page.getByRole('dialog', { name: 'Ready to go live?' });
  await expect(first).toBeVisible();
  await expect(first.getByText(`Friends will see: ${FAKE_LABEL}`)).toBeVisible();
  await expect(sheet).toBeHidden();
  await expectAccessible(page, 'the first-share sheet', 'dialog[open]');
  await first.getByRole('button', { name: 'Go live' }).click();
  await expect(first).toBeHidden();

  // Live: the panel says so, with the picker's name, which only this PC shows.
  const panel = page.getByRole('region', { name: 'Your share' });
  await expect(panel.getByText(`You're live · ${FAKE_LABEL} · Auto ·`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });

  // A friend's browser watches: frames of the Decodable pattern and the tone's energy.
  const share = await waitForState(
    friend.page,
    (s) => s.viewer.shares.find((sh) => !sh.own && sh.status === 'live' && sh.layers.includes('high')),
    { timeout: LIVE_TIMEOUT_MS, message: "the desktop's share is live on the friend's page" },
  );
  await waitForState(friend.page, (s) => s.viewer.focusedShareId === share.id && s, {
    message: "the desktop's share is on the friend's stage",
  });
  await waitForStats(friend.page, (s) => (videoOf(s, share.id)?.framesDecoded ?? 0) > 10 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the friend decodes frames of the share',
  });
  await waitForStats(friend.page, (s) => (audioOf(s, share.id)?.totalAudioEnergy ?? 0) > 0 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the friend hears the share's tone",
  });

  // The desktop's own tile: local, the low layer only, never its audio (03 §10.4).
  const own = await waitForState(
    page,
    (s) => s.viewer.shares.find((sh) => sh.id === share.id && sh.local && sh.subscription?.video === 'low'),
    { timeout: MEDIA_TIMEOUT_MS, message: "the desktop's own share is local and subscribed at the low layer" },
  );
  expect(own.own).toBe(true);
  expect(own.subscription?.audio).toBe('off');
  expect(own.subscription?.requestedVideo).toBe('low');
  const ownStats = await waitForStats(page, (s) => (videoOf(s, share.id)?.framesDecoded ?? 0) > 5 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the desktop decodes its own preview layer',
  });
  expect(audioOf(ownStats, share.id)?.totalAudioEnergy ?? 0).toBe(0);
  await waitForState(page, (s) => s.viewer.audibleShareId === null && s, { message: 'the desktop hears nothing' });
  await expect(page.getByText('Preview: what friends see').first()).toBeVisible();

  // The panel counts the friend, and so does the tray: Live, "1 watching" (03 §11.1, WA68's names).
  await expect(panel.getByText(`You're live · ${FAKE_LABEL} · Auto · 1 watching`)).toBeVisible();
  await expect
    .poll(async () => trayOf(await desktop.state()))
    .toMatchObject({ icon: 'tray-live', tooltip: `Sharing ${FAKE_LABEL} · 1 watching` });

  // The hotkeys are registered while the share is live (03 D9, §11.2), and the panel's buttons name them.
  await expect.poll(async () => (await desktop.state()).hotkeys).toEqual(HOTKEYS);
  await expect(panel.getByRole('button', { name: 'Pause' })).toHaveAttribute('title', 'Pause (Ctrl+Alt+Shift+F9)');
  await expect(panel.getByRole('button', { name: 'Stop sharing' })).toHaveAttribute(
    'title',
    'Stop (Ctrl+Alt+Shift+F10)',
  );

  // The `normal` scenario's mic prompt (Chrome uses the mic 5 s in, 05 §13.3): its card at the top of the panel
  // (WA77's card). The harness's window never hides, so the toast a hidden window gets is the app's Go test
  // (internal/desktop/app: TestMicPromptToast).
  await expect(panel.locator('[data-prompt-kind="browser-mic"]')).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });

  // Details (Ctrl+Shift+D): the encoder and the layers, no source name.
  await page.keyboard.press('Control+Shift+D');
  const details = panel.locator('[data-share-details]');
  await expect(details.locator('[data-detail="encoder"]')).toContainText('fake');
  await expect(details.locator('[data-detail="layer-f"]')).toContainText('640 × 360');
  await expect(details).not.toContainText(FAKE_LABEL);
  await expectAccessible(page, 'the native share panel', 'section[aria-label="Your share"]');

  // Pause from the panel (03 §11.3): nothing new leaves the PC; the friend's tile says "Paused" over the last frame,
  // with no new frames and no sound. Resume brings both back.
  await panel.getByRole('button', { name: 'Pause' }).click();
  await expect(panel.getByText(PAUSED_BAR)).toBeVisible();
  await expectPaused(friend.page, share.id, 'the friend, paused from the panel');
  await panel.getByRole('button', { name: 'Resume' }).click();
  await expectFrames(friend.page, share.id, 'the friend, resumed from the panel');

  // Pause by hotkey; a friend who joins while paused gets no frame, only the "Paused" tile.
  await desktop.hotkey('pause');
  await expect(panel.getByText(PAUSED_BAR)).toBeVisible();
  await expect
    .poll(async () => trayOf(await desktop.state()))
    .toMatchObject({ icon: 'tray-paused', tooltip: 'Sharing paused · 1 watching' });
  await expectPaused(friend.page, share.id, 'the friend, paused by the hotkey');
  const late = await asUser(server);
  await late.page.goto(`${server.url}/r/${room}`);
  await waitForRoom(late.page, room);
  await expectPaused(late.page, share.id, 'a friend who joined while paused');
  expect(videoOf(await readStats(late.page), share.id)?.framesDecoded ?? 0, 'the late joiner got a frame').toBe(0);

  // Resume by hotkey: both get frames again (the resume starts from a keyframe).
  await desktop.hotkey('pause');
  await expect(panel.getByText(`You're live · ${FAKE_LABEL}`)).toBeVisible();
  await expectFrames(friend.page, share.id, 'the friend, resumed by the hotkey');
  await expectFrames(late.page, share.id, 'the late joiner, resumed by the hotkey');

  // Stop by hotkey, without asking: the share ends for both, the hotkeys are unregistered and keep-awake is off.
  await desktop.hotkey('stop');
  await expect(panel).toBeHidden({ timeout: MEDIA_TIMEOUT_MS });
  for (const viewer of [friend.page, late.page]) {
    await waitForState(viewer, (s) => !s.viewer.shares.some((sh) => sh.id === share.id) && s, {
      timeout: MEDIA_TIMEOUT_MS,
      message: "the share is gone from a friend's page",
    });
  }
  await expect.poll(async () => (await desktop.state()).hotkeys).toEqual({});
  await expect.poll(async () => (await desktop.state()).keepAwake).toBe(false);
  await expect.poll(async () => trayOf(await desktop.state())).toMatchObject({ icon: 'tray-idle' });

  // A second share: the first-share sheet was shown once (tips.firstShareShown), so it goes live at once.
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await sheet.getByRole('button', { name: 'Share', exact: true }).click();
  await expect(panel.getByText(`You're live · ${FAKE_LABEL}`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });
  await expect(first).toBeHidden();
  await panel.getByRole('button', { name: 'Stop sharing' }).click();
  await expect(panel).toBeHidden({ timeout: MEDIA_TIMEOUT_MS });

  // 03 §17, §22.4: the harness's log, at debug level, names neither the window nor the share's label, no exe and
  // no app of the audio scenario the share ran on.
  const log = await fsp.readFile(desktop.logPath, 'utf8');
  expect(log).not.toContain(FAKE_LABEL);
  expect(log).not.toMatch(/\.exe\b/i);
  for (const name of NORMAL_SCENARIO_APPS) expect(log, name).not.toContain(name);
});

/** The window's X (the shell's close hook, App.WindowClosing) or the tray's Open (App.ShowWindow). */
async function windowAction(desktop: DesktopApp, action: 'close' | 'open'): Promise<void> {
  const res = await fetch(`${desktop.hostUrl}/__e2e/window?action=${action}`, { method: 'POST' });
  expect(res.status, `POST /__e2e/window?action=${action}`).toBe(204);
}

/** settings.json's tips.trayNoticeShown, through Settings.Get. */
async function trayNoticeShown(desktop: DesktopApp): Promise<boolean> {
  return (await desktop.call<{ tips: { trayNoticeShown: boolean } }>('Settings', 'Get')).tips.trayNoticeShown;
}

/** The close-to-tray line, as the toast and the window notice say it (03 §11.6). */
const STILL_SHARING = 'isshoni is still sharing. Stop it from the tray icon (^).';

test('closing the window while live hides it to the tray; the notice follows the toast once (03 §11.6)', async ({
  startDesktop,
  server,
}) => {
  const room = 'lounge';
  const desktop = await linkedDesktop(startDesktop, server, room);
  const { page } = desktop;
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
  await page.getByRole('dialog', { name: 'Ready to go live?' }).getByRole('button', { name: 'Go live' }).click();
  const panel = page.getByRole('region', { name: 'Your share' });
  await expect(panel.getByText(`You're live · ${FAKE_LABEL}`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });
  await expect.poll(async () => (await desktop.state()).hotkeys).toEqual(HOTKEYS);
  const notice = page.locator('[data-window-notice="still-sharing"]');
  const stillSharing = async (): Promise<Toast[]> =>
    ((await desktop.state()).toasts as Toast[]).filter((t) => t.Kind === 'still-sharing');
  expect(await trayNoticeShown(desktop)).toBe(false);

  // X while live: the window hides to the tray and the app goes on sharing (the hotkeys stay, the tray says Live);
  // the first time, the toast "isshoni is still sharing…".
  await windowAction(desktop, 'close');
  let st = await desktop.state();
  expect(st.windowVisible, 'the window is hidden').toBe(false);
  expect(st.quit, 'the app quit').toBe(false);
  expect((await desktop.call<{ state: string } | null>('Share', 'Current'))?.state).toBe('live');
  expect(st.hotkeys).toEqual(HOTKEYS);
  await expect
    .poll(async () => trayOf(await desktop.state()))
    .toMatchObject({ icon: 'tray-live', tooltip: `Sharing ${FAKE_LABEL} · 0 watching` });
  await expect.poll(async () => (await stillSharing()).length).toBe(1);
  const [toast] = await stillSharing();
  expect(toast?.Body).toBe(STILL_SHARING);

  // While the window is hidden, nothing is shown in it and the flag stays unset (03 §11.6: set only once the window
  // notice was shown); App.WindowNotice answers null for a hidden window.
  expect(await desktop.call('App', 'WindowNotice')).toBeNull();
  await page.waitForTimeout(500);
  await expect(notice).toHaveCount(0);
  expect(await trayNoticeShown(desktop)).toBe(false);
  // A second close in the same run shows no second toast.
  await windowAction(desktop, 'close');
  await page.waitForTimeout(500);
  expect(await stillSharing()).toHaveLength(1);

  // The tray's Open: the window shows the same line as a dismissible notice (window.notice), the page reports it
  // shown, and only then is tips.trayNoticeShown set.
  await windowAction(desktop, 'open');
  expect((await desktop.state()).windowVisible).toBe(true);
  await expect(notice).toBeVisible({ timeout: MEDIA_TIMEOUT_MS });
  await expect(notice).toContainText(STILL_SHARING);
  await expect.poll(() => trayNoticeShown(desktop), { message: 'tips.trayNoticeShown after the notice' }).toBe(true);
  await expectAccessible(page, 'the close-to-tray notice', '[data-window-notice]');
  await notice.getByRole('button', { name: 'Dismiss' }).click();
  await expect(notice).toHaveCount(0);

  // Once shown, never again: another close hides the window without a toast, and opening it shows no notice.
  await windowAction(desktop, 'close');
  st = await desktop.state();
  expect(st.windowVisible).toBe(false);
  expect(st.quit).toBe(false);
  await windowAction(desktop, 'open');
  expect(await desktop.call('App', 'WindowNotice')).toBeNull();
  await page.reload();
  await waitForRoom(page, room);
  await expect(panel.getByText(`You're live · ${FAKE_LABEL}`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });
  await page.waitForTimeout(500);
  await expect(notice).toHaveCount(0);
  expect(await stillSharing()).toHaveLength(1);

  // The share ran throughout; Stop ends it.
  expect((await desktop.call<{ state: string } | null>('Share', 'Current'))?.state).toBe('live');
  await desktop.hotkey('stop');
  await expect(panel).toBeHidden({ timeout: MEDIA_TIMEOUT_MS });
});

/** A recorded toast (app.Notification as the harness's JSON has it). */
interface Toast {
  ID: string;
  Kind: string;
  Body: string;
  Actions: { ID: string; Title: string }[] | null;
}

/** The pill's state as the harness recorded it (app.PillState). */
interface PillState {
  Visible: boolean;
  Mode: string;
  VoiceAppsHeard: boolean;
  Questions: number;
  Text: { Line: string; Questions: string };
}

test('degrade-live: friends hear voice apps, said outside the window (tray, pill, one toast)', async ({
  startDesktop,
  server,
}) => {
  const room = 'lounge';
  const desktop = await linkedDesktop((o) => startDesktop({ ...o, fakeAudio: 'degrade-live' }), server, room);
  const { page } = desktop;

  // The scripted prompt: the fake's Chrome uses the mic 5 s after 05's watcher starts. A watch of the test's own (as
  // Settings → Audio holds one) starts it before the share, so the prompt is pending when the share goes live; the
  // degrade then comes 5 s into the share (the fake's clock for it is the session's).
  await desktop.call('Audio', 'Watch', true);
  await expect
    .poll(async () => (await desktop.call<{ prompts: unknown[] }>('Audio', 'FriendsHear')).prompts.length, {
      timeout: MEDIA_TIMEOUT_MS,
    })
    .toBe(1);

  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  const sheet = page.getByRole('dialog', { name: 'Share your screen' });
  await sheet.getByRole('button', { name: 'Share', exact: true }).click();
  const first = page.getByRole('dialog', { name: 'Ready to go live?' });
  // The first-share sheet names the hotkeys the share will have (03 §10.2).
  await expect(
    first.getByText('Pause: Ctrl+Alt+Shift+F9 · Stop: Ctrl+Alt+Shift+F10, also from the tray icon'),
  ).toBeVisible();
  await first.getByRole('button', { name: 'Go live' }).click();
  const panel = page.getByRole('region', { name: 'Your share' });
  await expect(panel.getByText(`You're live · ${FAKE_LABEL}`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });

  // Live with the question pending: "1 question waiting" right after the share's line, and the pill's marker.
  const fast = { intervals: [100], timeout: MEDIA_TIMEOUT_MS };
  await expect
    .poll(async () => trayOf(await desktop.state())?.menu.slice(0, 2), fast)
    .toEqual([
      { label: `Sharing ${FAKE_LABEL} · 0 watching`, enabled: false },
      { label: '1 question waiting', action: 'questions', enabled: true },
    ]);
  await expect
    .poll(async () => (await desktop.state()).pill as PillState, fast)
    .toMatchObject({ Mode: 'live', Questions: 1, Text: { Questions: '1 question waiting' } });

  // 5 s in, the fake loses process loopback (engine endpoint: voice apps are heard, Chrome too, so its question is
  // gone with its mic row, 05 §14.6). The tray warns, with "Friends hear voice apps" first, and the pill says it in
  // place of the count.
  const ALERT_TIMEOUT_MS = 20_000;
  await expect
    .poll(async () => trayOf(await desktop.state()), { timeout: ALERT_TIMEOUT_MS })
    .toMatchObject({ icon: 'tray-warn', tooltip: `Friends hear voice apps · Sharing ${FAKE_LABEL} · 0 watching` });
  expect(trayOf(await desktop.state())?.menu.slice(0, 2)).toEqual([
    { label: 'Friends hear voice apps', enabled: false },
    { label: `Sharing ${FAKE_LABEL} · 0 watching`, enabled: false },
  ]);
  await expect
    .poll(async () => (await desktop.state()).pill as PillState)
    .toMatchObject({ Mode: 'live', VoiceAppsHeard: true, Text: { Line: 'Sharing · Friends hear voice apps' } });

  // Exactly one toast, with Open, whether or not the window is in front; none again while the banner stays.
  const alerts = async (): Promise<Toast[]> =>
    ((await desktop.state()).toasts as Toast[]).filter((t) => t.Kind === 'voice-apps-heard');
  await expect.poll(async () => (await alerts()).length).toBe(1);
  const [toast] = await alerts();
  expect(toast?.Body).toBe(
    "Voice apps can't be kept out right now, so friends hear everything, including your calls. Open isshoni to turn sound off.",
  );
  expect(toast?.Actions?.map((a) => a.ID)).toEqual(['open']);
  await page.waitForTimeout(QUIET_MS * 2);
  expect(await alerts(), 'a second voiceAppsHeard toast in the same share').toHaveLength(1);

  await desktop.hotkey('stop');
  await expect(panel).toBeHidden({ timeout: MEDIA_TIMEOUT_MS });
  await expect.poll(async () => trayOf(await desktop.state())).toMatchObject({ icon: 'tray-idle' });
  await expect.poll(async () => ((await desktop.state()).pill as PillState).Mode).toBe('');
});
