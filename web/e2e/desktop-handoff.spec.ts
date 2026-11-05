// desktop-handoff.spec (M2 03 §12.2, §22.4 A; README WA83): the web → desktop handoff, with the desktop app's real
// Go parts (internal/desktop/e2ehost: C2 on 01's core, 05's audio service on the fake engine) and its page in Chrome
// at http://wails.localhost, and a friend who shares a window with sound from a browser. The same user has
// - a browser tab that can share (role full), which reports itself as Chrome-engine "other" on Windows, so that its
//   playback.state is one 05 acts on: a tab of a browser family without a rule shows the viewer-unknown-browser
//   banner in the app's What friends hear (05 §5.9, §14.7) while the tab plays, which is how this spec sees that
//   the report reached the app (the core's EventPlaybackState → SetViewerBrowser);
// - a phone-emulated tab (an Android UA: role viewer).
//
// Then:
// - the tab plays the friend's share: its playback.state {playing: true} reaches the app once C2 is online;
// - the app's page joins the room: its playback.stop (toRole full) stops the tab, which says "Now watching in the
//   isshoni app" and subscribes to nothing, and the app hears playing: false; the phone keeps playing;
// - "Watch here instead" subscribes again, and playing: true reaches the app;
// - closing the tab takes its connection out of user.connections; the app's page tells Go (App.OwnConnections), which
//   counts the tab as no longer playing.
import type { BrowserContext, Page } from '@playwright/test';

import { expect, fakeDisplay, test, type DesktopApp } from './fixtures.ts';
import { audioOf, readStats, waitForRoom, waitForState, waitForStats } from './stats.ts';

test.describe.configure({ timeout: 180_000 });

const ROOM = 'lounge';
const MEDIA_TIMEOUT_MS = 20_000;
/** A relay message, a user.connections and Go's report: well under a second each; CI gets room. */
const RELAY_TIMEOUT_MS = 15_000;
/** Closing a page closes its socket (pagehide); a socket that isn't closed cleanly waits out the 30 s grace. */
const GONE_TIMEOUT_MS = 45_000;

/** Opera on Windows: os windows, browser family other (lib/ua.ts), so 05 shows viewer-unknown-browser. */
const WINDOWS_OTHER_UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 ' +
  'OPR/124.0.0.0';
/** A phone: os android, a handheld, so the page connects as a viewer (05 §8). */
const PHONE_UA =
  'Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile ' +
  'Safari/537.36';

/** Makes the context's pages report `ua` (and no Client Hints), before any script of theirs runs. */
async function reportAs(context: BrowserContext, ua: string, platform: string): Promise<void> {
  await context.addInitScript(
    ([userAgent, navPlatform]) => {
      Object.defineProperty(Navigator.prototype, 'userAgent', { get: () => userAgent, configurable: true });
      Object.defineProperty(Navigator.prototype, 'platform', { get: () => navPlatform, configurable: true });
      Object.defineProperty(Navigator.prototype, 'userAgentData', { get: () => undefined, configurable: true });
    },
    [ua, platform] as const,
  );
}

/** What friends hear's banners in the app (Audio.FriendsHear). */
async function banners(desktop: DesktopApp): Promise<string[]> {
  const fh = await desktop.call<{ banners: { code: string }[] }>('Audio', 'FriendsHear');
  return fh.banners.map((b) => b.code);
}

/** Whether 05 shows that a tab of an unknown browser plays: the tab's playback.state {playing: true} reached it. */
async function expectTabPlaying(desktop: DesktopApp, playing: boolean, what: string, timeout = RELAY_TIMEOUT_MS) {
  await expect
    .poll(async () => (await banners(desktop)).includes('viewer-unknown-browser'), { timeout, message: what })
    .toBe(playing);
}

/** The page hears the share: its audio energy grows. */
async function expectHearing(page: Page, shareId: string, what: string): Promise<void> {
  const before = audioOf(await readStats(page), shareId)?.totalAudioEnergy ?? 0;
  await waitForStats(page, (s) => (audioOf(s, shareId)?.totalAudioEnergy ?? 0) > before && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: what,
  });
}

/** The page watches the share with its sound on. */
async function expectWatching(page: Page, shareId: string, what: string): Promise<void> {
  await waitForState(page, (s) => s.viewer.audibleShareId === shareId && s.viewer.audio === 'playing' && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${what}: the share is heard`,
  });
  await waitForState(page, (s) => s.viewer.shares.find((sh) => sh.id === shareId && sh.subscription?.audio === 'on'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${what}: its audio is subscribed`,
  });
  await expectHearing(page, shareId, `${what}: the sound arrives`);
}

test('a same-user tab hands off to the app and back; a phone keeps playing; a closed tab stops counting', async ({
  startDesktop,
  server,
  asUser,
}) => {
  // The friend shares a window with sound.
  const friend = await asUser(server);
  await fakeDisplay(friend.page, 'window+audio');
  await friend.page.goto(`${server.url}/r/${ROOM}`);
  await waitForRoom(friend.page, ROOM);
  await friend.page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await friend.page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
  const share = await waitForState(friend.page, (s) => s.viewer.shares.find((x) => x.local && x.status === 'live'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the friend's share is live",
  });

  // The user's tab (Windows, a browser without a rule) and phone watch it.
  const account = await server.createUser();
  const tab = await asUser(server, account);
  await reportAs(tab.context, WINDOWS_OTHER_UA, 'Win32');
  await tab.page.goto(`${server.url}/r/${ROOM}`);
  await waitForRoom(tab.page, ROOM);
  await expectWatching(tab.page, share.id, 'the tab');
  const phone = await asUser(server, account);
  await reportAs(phone.context, PHONE_UA, 'Linux armv81');
  await phone.page.goto(`${server.url}/r/${ROOM}`);
  await waitForRoom(phone.page, ROOM);
  await expectWatching(phone.page, share.id, 'the phone');
  // The phone has no Share: it is a viewer, which playback.stop's toRole full doesn't reach.
  await expect(phone.page.getByRole('banner').getByRole('button', { name: 'Share', exact: true })).toHaveCount(0);

  // The app is linked: C2 comes online, the tab tells it that it plays.
  const desktop = await startDesktop({ server: server.url });
  const st = await desktop.call<{ status: string }>('Auth', 'LinkWithPassword', account.username, account.password);
  expect(st.status).toBe('linked');
  await expectTabPlaying(desktop, true, "the tab's playback.state {playing: true} reaches the app");
  // (No "Open in the isshoni app" here: the e2e server is http://, which has no deep link, 03 §12.3; OpenInApp.test
  // covers the entry.)

  // The app's page joins the room: the tab stops, the phone doesn't.
  await desktop.page.goto(`${desktop.origin}/r/${ROOM}`);
  await waitForRoom(desktop.page, ROOM);
  await expect(tab.page.getByText('Now watching in the isshoni app')).toBeVisible({ timeout: RELAY_TIMEOUT_MS });
  await waitForState(
    tab.page,
    // 03 §22.4: every subscription off; the video goes after the off delay (SUBSCRIBE_OFF_DELAY_MS, well inside the
    // timeout).
    (s) =>
      s.viewer.shares.find(
        (sh) => sh.id === share.id && sh.subscription?.audio === 'off' && sh.subscription.video === 'off',
      ),
    { timeout: MEDIA_TIMEOUT_MS, message: 'the handed-off tab subscribes to nothing' },
  );
  await expectTabPlaying(desktop, false, "the handed-off tab's playback.state {playing: false} reaches the app");
  await expectHearing(phone.page, share.id, 'the phone keeps playing');
  await expect(phone.page.getByText('Now watching in the isshoni app')).toHaveCount(0);
  // The app watches the share itself.
  await expectWatching(desktop.page, share.id, 'the app');

  // Watch here instead: the tab plays again, and says so to the app.
  await tab.page.getByRole('button', { name: 'Watch here instead' }).click();
  await expect(tab.page.getByText('Now watching in the isshoni app')).toHaveCount(0);
  await expectWatching(tab.page, share.id, 'the tab, after Watch here instead');
  await expectTabPlaying(desktop, true, "the tab's playback.state {playing: true} reaches the app again");

  // A network blip of the app's page: its resync re-joins the room, which is no join the user asked for, so the tab
  // isn't stopped again (03 §12.2).
  await desktop.page.evaluate(() => {
    window.__isshoni?.dropSocket();
  });
  await waitForState(desktop.page, (s) => s.connection.resumed && s.room.joinState === 'joined' && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the app's page is back in the room after the drop",
  });
  await tab.page.waitForTimeout(2_000);
  await expect(tab.page.getByText('Now watching in the isshoni app')).toHaveCount(0);

  // Closing the tab: it leaves user.connections, and the app counts it as no longer playing.
  await tab.page.close();
  await expectTabPlaying(desktop, false, 'the closed tab counts as stopped (App.OwnConnections)', GONE_TIMEOUT_MS);
  await expectHearing(phone.page, share.id, 'the phone still plays');
});
