// reconnect.spec (05 §19.3, 01 §19 "Reconnect" and "Restart"; README S81): a page that loses the server and gets it
// back, through the real server.
// (a) The socket drops while the media is fine (01 §11.5 A): the page resumes its connection, with the same
//     connection id, the video keeps decoding, and the server offers the sub PC nothing new.
// (b) The network is gone for 5 s: "Reconnecting…" shows after 2 s, not before, and goes once the page is back
//     (05 §7.1).
// (c) The server restarts during a share (01 §11.6): the sharer publishes its share again with `replaces`, without a
//     new pick, and the friend who watched it decodes the new share within 15 s of the new server being up, on the
//     stage, with its sound, and still as the pick it was (01 §10.6).
// (d) The server ends a share while its sharer's socket is down, and the connection is resumed: the sharer publishes
//     it again with `replaces` (01 §10.5) on a new pub PC of the next gen, which the server takes in place of the
//     one it kept, and the friend decodes the new share. (Not on the PC it has: the re-offer that rejects the old
//     m-sections moves Chrome's BUNDLE group to a new transport, and the SFU's PC never connected again. The share
//     ends by "Stop it" in another tab of the same person, the quickest way to end it on the server while its media
//     flows: a media_timeout takes 32 s, more than the grace of 30 s that the socket may stay away.)
//
// The network is cut with the harness's `link`, a SOCKS proxy that the page's browser context reaches the server
// through (fixtures.ts), not with context.setOffline: Chrome's offline emulation holds an open WebSocket's frames
// back and leaves its TCP connection alone, so the page would not notice a cut of 5 s (05 §19.3). The link carries
// only TCP: the media, on UDP, is not cut with it. For (a) the link is cut and restored at once: the socket's TCP
// connection ends under it without a close frame, which is how a carrier NAT timeout or a proxy reset drops it, and
// the page's next try gets through. (05 §19.3 names `__isshoni.dropSocket()` for (a); the SignalClient has no public
// hook for it yet, and the link drops the socket more like a network does.)
//
// What is shared: the fake display of 05 §19.3 (a canvas, with a 1 kHz tone), in a headful run too. Every test has a
// server of its own: (c) restarts it, and the others count what a room's pages do.
import type { Page } from '@playwright/test';

import { enableDebug, expect, fakeDisplay, test, type E2EServer } from './fixtures.ts';
import { audioOf, readState, videoOf, waitForRoom, waitForState, waitForStats } from './stats.ts';

// Two pages that encode and decode, and a server restart: room for a CI machine that runs them in software.
test.describe.configure({ timeout: 120_000 });

/** A share that started may take this long to reach the server and a viewer: two PeerConnections. */
const MEDIA_TIMEOUT_MS = 20_000;
/** 05 §7.1: "Reconnecting…" shows once the connection has been down this long. */
const RECONNECTING_AFTER_MS = 2_000;
/** (b): how long the network is gone. */
const OFFLINE_MS = 5_000;
/** (c): the viewer decodes the re-published share within this of the new server being up (01 §19). */
const REPUBLISHED_WITHIN_MS = 15_000;
/** (a): the server's Resync() after a resume would have offered by now (01 §10.5): it offers nothing. */
const RESYNC_QUIET_MS = 2_000;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Notes a measured time in the test's report, next to the limit it was held against. */
function measured(what: string, ms: number): void {
  test.info().annotations.push({ type: 'measured', description: `${what}: ${String(Math.round(ms))} ms` });
}

/** The admin of a server: every e2e server of these tests is set up. */
function adminOf(server: E2EServer): NonNullable<E2EServer['admin']> {
  if (!server.admin) throw new Error('the server has no admin');
  return server.admin;
}

/**
 * Clicks Share in the header and Share in the sheet, with the page in front (05 §19.3); returns the share's id once
 * it is live.
 */
async function share(page: Page): Promise<string> {
  await page.bringToFront();
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
  const own = await waitForState(page, (s) => s.viewer.shares.find((x) => x.local && x.status === 'live'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the share is live',
  });
  return own.id;
}

/** What a page's signaling sockets carried, counted from Node (Playwright's view of the page's WebSockets). */
interface SignalingWire {
  /** Signaling sockets the page opened. */
  readonly sockets: number;
  /** pc.offer {pc: 'sub'} messages the page received, on all of them. */
  readonly subOffers: number;
}

/** Starts counting a page's signaling sockets and the sub offers they bring. Call it before the page loads. */
function watchSignaling(page: Page): SignalingWire {
  const wire = { sockets: 0, subOffers: 0 };
  page.on('websocket', (ws) => {
    // The signaling endpoint (01 §3.1); nothing else of the app opens a WebSocket, but a dev tool might.
    if (new URL(ws.url()).pathname !== '/ws') return;
    wire.sockets++;
    ws.on('framereceived', ({ payload }) => {
      if (typeof payload !== 'string') return;
      try {
        const msg = JSON.parse(payload) as { type?: unknown; data?: { pc?: unknown } };
        if (msg.type === 'pc.offer' && msg.data?.pc === 'sub') wire.subOffers++;
      } catch {
        // Not JSON: nothing of the protocol.
      }
    });
  });
  return wire;
}

/** A pub offer or answer as the page's signaling socket carried it (01 §9). */
interface PubNegotiation {
  readonly gen: number;
  readonly neg: number;
  readonly sdp: string;
}

/** What a page's signaling sockets carried about its pub PC, counted from Node. */
interface PubWire {
  readonly offers: PubNegotiation[];
  readonly answers: PubNegotiation[];
  /** The codes of the error notifications in scope `pc` (sdp_invalid, …). */
  readonly errors: string[];
}

/** Starts recording a page's pub offers and answers. Call it before the page loads. */
function watchPub(page: Page): PubWire {
  const wire: PubWire = { offers: [], answers: [], errors: [] };
  page.on('websocket', (ws) => {
    if (new URL(ws.url()).pathname !== '/ws') return;
    const note = (payload: string | Buffer): void => {
      if (typeof payload !== 'string') return;
      let msg: {
        type?: unknown;
        data?: { pc?: unknown; gen?: unknown; neg?: unknown; sdp?: unknown; scope?: unknown; code?: unknown };
      };
      try {
        msg = JSON.parse(payload) as typeof msg;
      } catch {
        return;
      }
      const d = msg.data;
      if (d === undefined) return;
      if ((msg.type === 'pc.offer' || msg.type === 'pc.answer') && d.pc === 'pub') {
        const n = { gen: Number(d.gen), neg: Number(d.neg), sdp: String(d.sdp) };
        (msg.type === 'pc.offer' ? wire.offers : wire.answers).push(n);
      } else if (msg.type === 'error' && d.scope === 'pc') {
        wire.errors.push(String(d.code));
      }
    };
    ws.on('framesent', ({ payload }) => {
      note(payload);
    });
    ws.on('framereceived', ({ payload }) => {
      note(payload);
    });
  });
  return wire;
}

/** The port of each m-section of an SDP (0: rejected). */
function mPorts(sdp: string): number[] {
  return sdp
    .split(/\r?\nm=/)
    .slice(1)
    .map((section) => Number(section.split(' ')[1]));
}

/** When the connection banner showed what, as the page saw it: `kind` is its data-kind, '' while empty. */
interface BannerMark {
  readonly kind: string;
  readonly at: number;
}

/** An init script: notes every change of the connection banner's kind, with the page's Date.now(). */
function noteBanner(): void {
  const marks: BannerMark[] = [];
  (window as unknown as { __bannerMarks: BannerMark[] }).__bannerMarks = marks;
  let last: string | null = null;
  new MutationObserver(() => {
    const banner = document.querySelector('[data-testid="connection-banner"]');
    if (banner === null) return;
    const kind = banner.getAttribute('data-kind') ?? '';
    if (kind === last) return;
    last = kind;
    marks.push({ kind, at: Date.now() });
  }).observe(document, { subtree: true, childList: true, attributes: true, attributeFilter: ['data-kind'] });
}

function bannerMarks(page: Page): Promise<BannerMark[]> {
  return page.evaluate(() => (window as unknown as { __bannerMarks: BannerMark[] }).__bannerMarks);
}

test('(a) a dropped socket resumes with the same connection id: the video keeps decoding, no sub offer again', async ({
  startServer,
  context,
  page,
  browser,
  link,
}) => {
  const server = await startServer();
  await server.signIn(context, adminOf(server));
  await fakeDisplay(page, 'window+audio');
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  // The friend reaches the server through the link.
  const account = await server.createUser();
  const friendContext = await browser.newContext({ proxy: { server: link.proxy } });
  try {
    await enableDebug(friendContext);
    await server.signIn(friendContext, account);
    const friend = await friendContext.newPage();
    const wire = watchSignaling(friend);
    await friend.goto(`${server.url}/`);
    await waitForRoom(friend, 'lounge');

    const shareId = await share(page);
    await waitForState(friend, (s) => s.viewer.focusedShareId === shareId && s.viewer.media === 'connected', {
      timeout: MEDIA_TIMEOUT_MS,
      message: 'the friend watches the share',
    });
    const decoding = await waitForStats(friend, (s) => (videoOf(s, shareId)?.framesDecoded ?? 0) > 0 && s, {
      timeout: MEDIA_TIMEOUT_MS,
      message: 'the friend decodes the share',
    });
    const before = await readState(friend);
    const offers = wire.subOffers;
    expect(wire.sockets).toBe(1);
    expect(offers).toBeGreaterThan(0);

    // The socket's TCP connection ends under it, without a close frame; the network is back at once.
    const droppedAt = Date.now();
    link.cut();
    link.restore();

    const after = await waitForState(friend, (s) => wire.sockets >= 2 && s.connection.state === 'ready' && s, {
      timeout: 10_000,
      message: 'the friend is connected again on a new socket',
    });
    measured('the friend is connected again after the drop', Date.now() - droppedAt);
    expect(after.connection.resumed).toBe(true);
    expect(after.connection.connectionId).toBe(before.connection.connectionId);
    expect(after.room).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
    expect(after.viewer.subGen).toBe(before.viewer.subGen);

    // The media never went through the link: the video keeps decoding.
    const framesBefore = videoOf(decoding, shareId)?.framesDecoded ?? 0;
    await waitForStats(friend, (s) => (videoOf(s, shareId)?.framesDecoded ?? 0) > framesBefore + 30, {
      timeout: 10_000,
      message: 'the friend keeps decoding the share',
    });
    // The server's Resync() offered nothing: the sub PC was connected all along (01 §10.5, §11.5 A).
    await sleep(RESYNC_QUIET_MS);
    expect(wire.subOffers).toBe(offers);
    const later = await readState(friend);
    expect(later.viewer.media).toBe('connected');
    expect(later.viewer.subGen).toBe(before.viewer.subGen);
    expect(later.viewer.focusedShareId).toBe(shareId);
    await expect(friend.getByTestId('connection-banner')).toHaveText('');
  } finally {
    await friendContext.close();
  }
});

test('(b) 5 s offline: "Reconnecting…" shows after 2 s, and goes once the page is back', async ({
  startServer,
  browser,
  link,
}) => {
  const server = await startServer();
  const account = await server.createUser();
  const offlineContext = await browser.newContext({ proxy: { server: link.proxy } });
  try {
    await enableDebug(offlineContext);
    await server.signIn(offlineContext, account);
    const page = await offlineContext.newPage();
    await page.addInitScript(noteBanner);
    await page.goto(`${server.url}/`);
    const before = await waitForRoom(page, 'lounge');
    const banner = page.getByTestId('connection-banner');
    await expect(banner).toHaveText('');

    const cutAt = Date.now();
    link.cut();
    await expect(banner).toContainText('Reconnecting…', { timeout: RECONNECTING_AFTER_MS + 3_000 });
    const left = cutAt + OFFLINE_MS - Date.now();
    if (left > 0) await sleep(left);
    const restoredAt = Date.now();
    link.restore();
    // The page's next try is at most 10 s away (01 §10.2); after 5 s down it is about 2.5 s away.
    await expect(banner).toHaveText('', { timeout: 15_000 });

    const marks = await bannerMarks(page);
    const shownAt = marks.find((m) => m.kind === 'reconnecting')?.at;
    if (shownAt === undefined) throw new Error(`the banner never said "Reconnecting…": ${JSON.stringify(marks)}`);
    const gone = marks.find((m) => m.kind === '' && m.at > shownAt);
    if (gone === undefined) throw new Error(`the banner never went: ${JSON.stringify(marks)}`);
    measured('"Reconnecting…" shows after the cut', shownAt - cutAt);
    measured('the banner goes after the network is back', gone.at - restoredAt);
    // Not before 2 s: the page knows of the cut at once (its socket closes), and the banner waits (05 §7.1). Both
    // clocks are this machine's; a page that heard of the cut late would show it later, never earlier.
    expect(shownAt - cutAt).toBeGreaterThanOrEqual(RECONNECTING_AFTER_MS);
    expect(shownAt - cutAt).toBeLessThan(RECONNECTING_AFTER_MS + 1_500);
    // One banner, steady through the tries, and only "Reconnecting…": no restart, and 5 s are far from the 30 s
    // of "Can't reach the server".
    expect(marks.filter((m) => m.at >= cutAt).map((m) => m.kind)).toEqual(['reconnecting', '']);
    expect(gone.at).toBeGreaterThanOrEqual(restoredAt);

    // Back within the grace period: the same connection.
    const after = await waitForState(page, (s) => s.connection.state === 'ready' && s);
    expect(after.connection.resumed).toBe(true);
    expect(after.connection.connectionId).toBe(before.connection.connectionId);
    expect(after.room).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
  } finally {
    await offlineContext.close();
  }
});

test('(c) a server restart: the friend decodes the re-published share within 15 s, still picked, with its sound', async ({
  startServer,
  context,
  page,
  asUser,
}) => {
  const server = await startServer();
  const admin = adminOf(server);
  await server.signIn(context, admin);
  await fakeDisplay(page, 'window+audio');
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');
  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  const first = await share(page);
  await waitForState(
    friend.page,
    (s) => s.viewer.focusedShareId === first && s.viewer.audibleShareId === first && s.viewer.media === 'connected',
    { timeout: MEDIA_TIMEOUT_MS, message: 'the friend watches and hears the share' },
  );
  // The friend picks it ("1": the newest share): a pick is what the re-published share has to keep (01 §10.6),
  // while a new share would only be a toast then (05 §12.2).
  await friend.page.keyboard.press('1');
  await waitForState(friend.page, (s) => s.viewer.focusMode === 'manual' && s.viewer.focusedShareId === first, {
    message: 'the friend picked the share',
  });
  await waitForStats(
    friend.page,
    (s) => (videoOf(s, first)?.framesDecoded ?? 0) > 0 && (audioOf(s, first)?.audioLevel ?? 0) > 0.05,
    { timeout: MEDIA_TIMEOUT_MS, message: 'the friend decodes the share and hears its tone' },
  );

  // The server stops (server.shutdown, close 1012) and a new process starts on the same data and ports.
  const stoppingAt = Date.now();
  await server.restart();
  const upAt = Date.now();
  measured('the server restart, from SIGTERM to ready', upAt - stoppingAt);
  const deadline = upAt + REPUBLISHED_WITHIN_MS;
  const left = (): number => Math.max(deadline - Date.now(), 1);

  const now = await waitForState(
    friend.page,
    (s) => {
      const next = s.viewer.shares.find((x) => x.id !== first && x.userId === admin.id && x.status === 'live');
      return (
        next !== undefined &&
        s.viewer.focusedShareId === next.id &&
        s.viewer.audibleShareId === next.id && { state: s, shareId: next.id }
      );
    },
    { timeout: left(), message: 'the friend has the re-published share on the stage, with its sound' },
  );
  await waitForStats(
    friend.page,
    (s) => (videoOf(s, now.shareId)?.framesDecoded ?? 0) > 0 && (audioOf(s, now.shareId)?.audioLevel ?? 0) > 0.05,
    { timeout: left(), message: 'the friend decodes the re-published share and hears its tone' },
  );
  measured('the friend decodes the re-published share after the new server was up', Date.now() - upAt);
  measured('… and after the old server began to stop', Date.now() - stoppingAt);

  // A new connection, and the share is the one that replaces the share the friend picked: still a pick.
  expect(now.state.connection.resumed).toBe(false);
  expect(now.state.viewer.focusMode).toBe('manual');
  expect(now.state.viewer.shares.map((x) => x.id)).toEqual([now.shareId]);

  // The sharer was never asked to pick again: its page publishes the same capture under the new id.
  const sharer = await waitForState(
    page,
    (s) => s.viewer.shares.find((x) => x.local && x.id === now.shareId && x.status === 'live') !== undefined && s,
    { message: "the sharer's page has the new share" },
  );
  expect(sharer.connection.resumed).toBe(false);
  expect(sharer.viewer.shares).toHaveLength(1);
  await expect(page.getByRole('region', { name: 'Your share' })).toContainText("You're live");
  await expect(page.getByTestId('connection-banner')).toHaveText('');
  await expect(friend.page.getByTestId('connection-banner')).toHaveText('');
});

test('(d) a share the server ended while the socket was down: published again on a new pub PC, and decoded', async ({
  startServer,
  browser,
  context,
  page,
  asUser,
  link,
}) => {
  const server = await startServer();
  const admin = adminOf(server);
  // The sharer reaches the server through the link; another tab of the same person (the default context) doesn't.
  const sharerContext = await browser.newContext({ proxy: { server: link.proxy } });
  try {
    await enableDebug(sharerContext);
    await fakeDisplay(sharerContext, 'window+audio');
    await server.signIn(sharerContext, admin);
    const sharer = await sharerContext.newPage();
    const pub = watchPub(sharer);
    await sharer.goto(`${server.url}/`);
    const before = await waitForRoom(sharer, 'lounge');
    await server.signIn(context, admin);
    await page.goto(`${server.url}/`);
    await waitForRoom(page, 'lounge');
    const friend = await asUser(server);
    await friend.page.goto(`${server.url}/`);
    await waitForRoom(friend.page, 'lounge');

    const first = await share(sharer);
    await waitForStats(friend.page, (s) => (videoOf(s, first)?.framesDecoded ?? 0) > 0, {
      timeout: MEDIA_TIMEOUT_MS,
      message: 'the friend decodes the share',
    });
    const published = pub.offers.at(-1);
    if (published === undefined) throw new Error('the sharer sent no pub offer');
    const oldPorts = mPorts(published.sdp);

    // The sharer's socket goes. Its connection waits out the grace on the server, with the share and the pub PC,
    // whose media doesn't take the link.
    link.cut();
    // The other tab stops the share (01 §8.7: its user may): the server ends it, and the sharer doesn't hear of it.
    await page.bringToFront();
    await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
    const sheet = page.getByRole('dialog', { name: 'Share your screen' });
    await expect(sheet).toContainText("You're already sharing from another tab or device");
    await sheet.getByRole('button', { name: 'Stop it' }).click();
    await waitForState(friend.page, (s) => s.viewer.shares.every((x) => x.id !== first), {
      message: 'the server ended the share',
    });
    await page.keyboard.press('Escape');
    // The sharer in front again, as when it shared: its capture is drawn on a timer.
    await sharer.bringToFront();
    const restoredAt = Date.now();
    link.restore();

    // The resumed connection's room.state lacks the share: the sharer publishes it again, with `replaces`.
    const again = await waitForState(
      sharer,
      (s) => {
        const next = s.viewer.shares.find((x) => x.local && x.id !== first && x.status === 'live');
        return next !== undefined && { state: s, shareId: next.id };
      },
      { timeout: MEDIA_TIMEOUT_MS, message: 'the sharer has its share live again under a new id' },
    );
    expect(again.state.connection.resumed).toBe(true);
    expect(again.state.connection.connectionId).toBe(before.connection.connectionId);
    await waitForStats(friend.page, (s) => (videoOf(s, again.shareId)?.framesDecoded ?? 0) > 0, {
      timeout: MEDIA_TIMEOUT_MS,
      message: 'the friend decodes the share published again',
    });
    measured('the friend decodes the share published again after the network was back', Date.now() - restoredAt);

    // The share went onto a new pub PC, the next gen (and no later one), whose first offer carries only it; the SFU
    // answered that offer, and nothing about the pub PC failed.
    const offer = pub.offers.find((o) => o.gen === published.gen + 1);
    if (offer === undefined) throw new Error(`no pub offer of gen ${String(published.gen + 1)}`);
    expect(offer.neg).toBe(1);
    expect(Math.max(...pub.offers.map((o) => o.gen))).toBe(offer.gen);
    expect(mPorts(offer.sdp)).toHaveLength(oldPorts.length);
    expect(mPorts(offer.sdp).every((port) => port !== 0)).toBe(true);
    expect(pub.answers.some((a) => a.gen === offer.gen && a.neg === offer.neg)).toBe(true);
    expect(pub.errors).toEqual([]);
    await expect(sharer.getByRole('region', { name: 'Your share' })).toContainText("You're live");
  } finally {
    await sharerContext.close();
  }
});
