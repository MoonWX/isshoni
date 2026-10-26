// desktop-share.spec (M2 03 §22.4 A, README WA65): the desktop app shares natively, with its real Go parts
// (internal/desktop/e2ehost: the share controller, C2 on 01's core, 05's audio service) on the fake engine's 640×360
// Decodable pattern with its tone, and the desktop SPA in Chrome at http://wails.localhost:
// - Share → the desktop ShareSheet → the fake pick → the first-share sheet ("Friends will see: Fake window") → Go
//   live → the panel says "You're live · Fake window · …";
// - a friend's web viewer decodes the share's frames and its audio energy (M1's stats.ts);
// - the desktop's own tile is local: subscribed with the low layer and no audio, shown as "Preview: what friends
//   see", never heard;
// - the tray says "Live, 1 watching" once the harness has the tray (README WA68; a null tray view is noted);
// - the harness's debug-level log names no window title and no share label (03 §17);
// - the panel's Stop ends the share for the friend; a second share needs no first-share sheet (tips.firstShareShown).
//
// Pause, the late joiner, the hotkeys and the audio alert case come with README WA80.
import AxeBuilder from '@axe-core/playwright';
import fsp from 'node:fs/promises';
import type { Page } from '@playwright/test';

import { expect, test, type DesktopApp, type E2EServer } from './fixtures.ts';
import { audioOf, videoOf, waitForRoom, waitForState, waitForStats } from './stats.ts';

/** C2 connects, publishes and goes live; a viewer's sub PC follows. */
const LIVE_TIMEOUT_MS = 30_000;
const MEDIA_TIMEOUT_MS = 20_000;
/** The fake engine's picker names its window this way (internal/media/fake): the label that stays on this PC. */
const FAKE_LABEL = 'Fake window';

test.describe.configure({ timeout: 150_000 });

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

  // The panel counts the friend; the tray says the same once the harness has one (README WA68).
  await expect(panel.getByText(`You're live · ${FAKE_LABEL} · Auto · 1 watching`)).toBeVisible();
  const { tray } = await desktop.state();
  if (tray === null) {
    test.info().annotations.push({ type: 'tray', description: 'the harness has no tray yet (README WA68)' });
  } else {
    await expect
      .poll(async () => JSON.stringify((await desktop.state()).tray))
      .toMatch(/tray-live[\s\S]*1 watching|1 watching[\s\S]*tray-live/);
  }

  // Details (Ctrl+Shift+D): the encoder and the layers, no source name.
  await page.keyboard.press('Control+Shift+D');
  const details = panel.locator('[data-share-details]');
  await expect(details.locator('[data-detail="encoder"]')).toContainText('fake');
  await expect(details.locator('[data-detail="layer-f"]')).toContainText('640 × 360');
  await expect(details).not.toContainText(FAKE_LABEL);
  await expectAccessible(page, 'the native share panel', 'section[aria-label="Your share"]');

  // Stop: the share ends for the friend, and the panel goes.
  await panel.getByRole('button', { name: 'Stop sharing' }).click();
  await expect(panel).toBeHidden({ timeout: MEDIA_TIMEOUT_MS });
  await waitForState(friend.page, (s) => !s.viewer.shares.some((sh) => sh.id === share.id) && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the share is gone from the friend's page",
  });

  // A second share: the first-share sheet was shown once (tips.firstShareShown), so it goes live at once.
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await sheet.getByRole('button', { name: 'Share', exact: true }).click();
  await expect(panel.getByText(`You're live · ${FAKE_LABEL}`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });
  await expect(first).toBeHidden();
  await panel.getByRole('button', { name: 'Stop sharing' }).click();
  await expect(panel).toBeHidden({ timeout: MEDIA_TIMEOUT_MS });

  // 03 §17: the harness's log, at debug level, names neither the window nor the share's label.
  const log = await fsp.readFile(desktop.logPath, 'utf8');
  expect(log).not.toContain(FAKE_LABEL);
  expect(log).not.toMatch(/\.exe\b/i);
});
