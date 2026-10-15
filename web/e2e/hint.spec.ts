// hint.spec (05 §19.3, W9; 01 §8.10, §11.7; 02 §8.3, §8.5; README S86): the room's codec policy and the codec wait,
// through the real server and real Chrome pages.
// - A quality.hint{codec} makes the sharer re-offer, and the viewers keep decoding. A share goes out in High while
//   everybody can decode it; then a friend joins whose browser decodes only Constrained Baseline (a Firefox does,
//   S4 finding 3). The server's policy drops to `cb` at once (02 §8.3) and sends the sharer quality.hint {reason:
//   codec, codec: h264/42e0}; the sharer re-offers its pub PC in the same gen with neg + 1 and 42e0 first, the share
//   goes on in 42e0 (room.state's codec), and both friends decode it without anything sent for it.
// - The codec wait of a fresh Firefox (05 §10.6; the manual M-FF-1 is the real one): a friend whose browser has no
//   H.264 yet hears the share, and its stage says the decoder is still coming; when H.264 appears the page sends
//   caps.update, never pc.restart, the server rebuilds its sub PC with the video, and it decodes.
//
// "Only Constrained Baseline" and "no H.264 yet" are what the friend's RTCRtpReceiver.getCapabilities('video')
// reports, changed by an init script: the caps of hello and caps.update come from it (01's detectCaps()). The browser
// still decodes everything, so these tests are about what the pages and the server do, not about a decoder. The
// "Firefox" of the second test is its user agent, which picks the page's help text only (lib/ua.ts).
//
// What is shared: the fake display of 05 §19.3, in a headful run too. Every test has a server of its own: the codec
// policy is the room's.
import type { Page } from '@playwright/test';

import { expect, fakeDisplay, test, type E2EServer, type TestFixtures } from './fixtures.ts';
import { audioOf, videoOf, waitForRoom, waitForState, waitForStats, type DebugShare } from './stats.ts';

// Three pages that encode or decode, and a codec switch: room for a CI machine that runs them in software.
test.describe.configure({ timeout: 120_000 });

/** A share that started may take this long to reach the server and a viewer: two PeerConnections. */
const MEDIA_TIMEOUT_MS = 20_000;
/** The page reads its capabilities every 5 s while it waits for a decoder (05 §18); this allows for the trip. */
const CAPS_UPDATE_WITHIN_MS = 8_000;
/** Decoding counts as going on when this many more frames were decoded (the fake display draws 30 per second). */
const MORE_FRAMES = 15;
/** The user agent of a desktop Firefox: the help text of the codec wait is Firefox's. */
const FIREFOX_UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:150.0) Gecko/20100101 Firefox/150.0';

// ---- The friends' browsers ----

/** An init script: RTCRtpReceiver lists only the H.264 profiles that `keep` names (none for []), unless `__h264`. */
function limitDecoders(keep: readonly string[]): void {
  const receiver = RTCRtpReceiver as unknown as { getCapabilities(kind: string): RTCRtpCapabilities | null };
  const original = receiver.getCapabilities.bind(RTCRtpReceiver);
  const w = window as unknown as { __h264?: boolean };
  receiver.getCapabilities = (kind: string) => {
    const caps = original(kind);
    if (kind !== 'video' || caps === null || w.__h264 === true) return caps;
    const allowed = (c: RTCRtpCodec): boolean => {
      if (c.mimeType.toLowerCase() !== 'video/h264') return true;
      const profile = /profile-level-id=([0-9a-f]{4})/i.exec(c.sdpFmtpLine ?? '')?.[1]?.toLowerCase();
      return profile !== undefined && keep.includes(profile);
    };
    return { ...caps, codecs: caps.codecs.filter(allowed) };
  };
}

/** An init script: the page sees a desktop Firefox (navigator.userAgent, and no Chromium userAgentData). */
function looksLikeFirefox(ua: string): void {
  Object.defineProperty(Navigator.prototype, 'userAgent', { configurable: true, get: () => ua });
  Object.defineProperty(Navigator.prototype, 'userAgentData', { configurable: true, get: () => undefined });
}

// ---- What the pages' signaling carries ----

/** One protocol message on a page's signaling socket, as Playwright saw it. */
interface Frame {
  readonly dir: 'sent' | 'received';
  readonly type: string;
  readonly data: Record<string, unknown>;
}

/** Starts recording a page's signaling messages, both ways. Call it before the page loads. */
function record(page: Page): Frame[] {
  const frames: Frame[] = [];
  page.on('websocket', (ws) => {
    // The signaling endpoint (01 §3.1); nothing else of the app opens a WebSocket.
    if (new URL(ws.url()).pathname !== '/ws') return;
    const note = (dir: Frame['dir'], payload: string | Buffer): void => {
      if (typeof payload !== 'string') return;
      try {
        const msg = JSON.parse(payload) as { type?: unknown; data?: unknown };
        if (typeof msg.type !== 'string') return;
        const data = typeof msg.data === 'object' && msg.data !== null ? (msg.data as Record<string, unknown>) : {};
        frames.push({ dir, type: msg.type, data });
      } catch {
        // Not JSON: nothing of the protocol.
      }
    };
    ws.on('framesent', ({ payload }) => {
      note('sent', payload);
    });
    ws.on('framereceived', ({ payload }) => {
      note('received', payload);
    });
  });
  return frames;
}

function sent(frames: readonly Frame[], type: string): Record<string, unknown>[] {
  return frames.filter((f) => f.dir === 'sent' && f.type === type).map((f) => f.data);
}

function received(frames: readonly Frame[], type: string): Record<string, unknown>[] {
  return frames.filter((f) => f.dir === 'received' && f.type === type).map((f) => f.data);
}

/** The pub offers a page sent (01 §9: gen, neg, sdp). */
function pubOffers(frames: readonly Frame[]): { gen: number; neg: number; sdp: string }[] {
  return sent(frames, 'pc.offer')
    .filter((d) => d['pc'] === 'pub')
    .map((d) => ({ gen: Number(d['gen']), neg: Number(d['neg']), sdp: String(d['sdp']) }));
}

/**
 * The H.264 profiles of an SDP's first video m-section, in the order of its payload types: what the offerer
 * prefers first ("42e0", "6400", …).
 */
function videoProfiles(sdp: string): string[] {
  const section = sdp.split(/\r?\nm=/).find((s) => s.startsWith('video '));
  if (section === undefined) return [];
  const order = section.split(/\r?\n/)[0]?.split(' ').slice(3) ?? [];
  const h264 = new Set(
    [...section.matchAll(/^a=rtpmap:(\d+) H264\//gim)].map((m) => m[1]).filter((pt) => pt !== undefined),
  );
  const profile = new Map(
    [...section.matchAll(/^a=fmtp:(\d+) .*profile-level-id=([0-9a-f]{4})/gim)].map((m) => [m[1], m[2]?.toLowerCase()]),
  );
  return order.filter((pt) => h264.has(pt)).map((pt) => profile.get(pt) ?? '?');
}

// ---- People and shares ----

/** The admin's page in the lounge: the first friend, a browser that decodes every profile. */
async function openViewer(server: E2EServer, page: Page): Promise<void> {
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(page.context(), admin);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');
}

/** One more person opens the lounge; `prepare` runs before the page loads the app. */
async function joinLounge(
  server: E2EServer,
  asUser: TestFixtures['asUser'],
  prepare?: (page: Page) => Promise<void>,
): Promise<Page> {
  const user = await asUser(server);
  await prepare?.(user.page);
  await user.page.goto(`${server.url}/`);
  await waitForRoom(user.page, 'lounge');
  return user.page;
}

/** Clicks Share in the header and Share in the sheet, with the page in front; returns the share's id once live. */
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

/**
 * The share's H.264 profile as the page's last room.state has it: viewer/stats.ts puts `codec` into each share of
 * state() (README S86), a field stats.ts's DebugShare doesn't declare.
 */
function codecOf(share: DebugShare | undefined): string | null {
  const codec: unknown = share === undefined ? undefined : Reflect.get(share, 'codec');
  return typeof codec === 'string' ? codec : null;
}

/** The exceptions nobody caught on a page, from now on: a test expects none. */
function uncaughtErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(String(error.stack ?? error)));
  return errors;
}

/** Waits until the page has decoded MORE_FRAMES more frames of the share than it had, and returns that sample. */
async function keepsDecoding(page: Page, shareId: string, who: string) {
  const first = await waitForStats(page, (s) => (videoOf(s, shareId)?.framesDecoded ?? 0) > 0 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${who} decodes the share`,
  });
  const before = videoOf(first, shareId)?.framesDecoded ?? 0;
  return waitForStats(page, (s) => (videoOf(s, shareId)?.framesDecoded ?? 0) >= before + MORE_FRAMES && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `${who} keeps decoding the share`,
  });
}

// ---- The tests ----

test('a quality.hint{codec} makes the sharer re-offer, and the viewers keep decoding', async ({
  startServer,
  page,
  asUser,
}) => {
  const server = await startServer();
  await openViewer(server, page);
  const viewerErrors = uncaughtErrors(page);

  let alexWire: Frame[] = [];
  const alex = await joinLounge(server, asUser, async (p) => {
    alexWire = record(p);
    await fakeDisplay(p, 'window');
  });
  const sharerErrors = uncaughtErrors(alex);
  const shareId = await share(alex);

  // Everybody decodes High: the room's policy is `high`, and the share goes out in it (02 §8.3). A Chrome that
  // can't encode High (a software encoder only, as on CI's Linux runners) puts its next profile first, Constrained
  // Baseline (05 §13.4), and the share starts in that one.
  const encodesHigh = await alex.evaluate(
    () =>
      RTCRtpSender.getCapabilities('video')?.codecs.some(
        (c) => c.mimeType.toLowerCase() === 'video/h264' && /profile-level-id=6400/i.test(c.sdpFmtpLine ?? ''),
      ) ?? false,
  );
  const startProfile = encodesHigh ? '6400' : '42e0';
  const params = received(alexWire, 'ok').find((d) => d['shareId'] === shareId);
  expect(params?.['codec'], "share.start's ShareParams.codec").toBe(`h264/${startProfile}`);
  if (!encodesHigh) {
    test.info().annotations.push({ type: 'note', description: "the sharer's Chrome can't encode High here" });
  }
  await waitForState(page, (s) => codecOf(s.viewer.shares.find((x) => x.id === shareId)) === `h264/${startProfile}`, {
    timeout: MEDIA_TIMEOUT_MS,
    message: `room.state says the share is h264/${startProfile}`,
  });
  const firstOffers = pubOffers(alexWire);
  expect(firstOffers.length, 'the pub offers before the hint').toBeGreaterThan(0);
  const before = firstOffers.at(-1);
  if (!before) throw new Error('no pub offer');
  expect(videoProfiles(before.sdp)[0], 'the first profile of the first offer').toBe(startProfile);
  await keepsDecoding(page, shareId, 'the admin');

  // The rest needs a switch away from High. A sharer whose Chrome can't encode High (CI's Linux runners) starts in
  // Constrained Baseline already, so a friend who decodes only that changes nothing and no hint comes (02 §8.3).
  test.skip(!encodesHigh, "the sharer's Chrome can't encode High, so there is no codec switch to test");

  // A friend whose browser decodes only Constrained Baseline joins.
  let danWire: Frame[] = [];
  const dan = await joinLounge(server, asUser, async (p) => {
    danWire = record(p);
    await p.addInitScript(limitDecoders, ['42e0', '4200']);
  });
  const danErrors = uncaughtErrors(dan);
  const danHello = sent(danWire, 'hello')[0]?.['caps'] as { decode?: string[] } | undefined;
  expect(
    danHello?.decode?.filter((k) => k.startsWith('h264/')).sort(),
    "the friend's hello: Constrained Baseline and Baseline only",
  ).toEqual(['h264/4200', 'h264/42e0']);

  // The server tells the sharer to switch (01 §8.10) …
  await expect
    .poll(() => received(alexWire, 'quality.hint').filter((h) => h['shareId'] === shareId), {
      message: 'the sharer gets quality.hint {codec: h264/42e0}',
      timeout: MEDIA_TIMEOUT_MS,
    })
    .toContainEqual({ shareId, reason: 'codec', codec: 'h264/42e0' });

  // … and the sharer re-offers its pub PC: the same gen, neg + 1, 42e0 first (01 §9, §8.10).
  await expect
    .poll(() => pubOffers(alexWire).length, { message: 'the sharer re-offers', timeout: 10_000 })
    .toBeGreaterThan(firstOffers.length);
  const reoffer = pubOffers(alexWire)[firstOffers.length];
  if (!reoffer) throw new Error('no re-offer');
  expect({ gen: reoffer.gen, neg: reoffer.neg }, 'the re-offer').toEqual({ gen: before.gen, neg: before.neg + 1 });
  expect(videoProfiles(reoffer.sdp)[0], 'the first profile of the re-offer').toBe('42e0');
  await expect
    .poll(
      () =>
        received(alexWire, 'pc.answer').some(
          (a) => a['pc'] === 'pub' && a['gen'] === reoffer.gen && a['neg'] === reoffer.neg,
        ),
      { message: 'the server answers the re-offer', timeout: 10_000 },
    )
    .toBe(true);

  // The share goes on in Constrained Baseline: room.state says so once the SFU sees its first keyframe in it.
  await waitForState(page, (s) => codecOf(s.viewer.shares.find((x) => x.id === shareId)) === 'h264/42e0', {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'room.state says the share is Constrained Baseline now',
  });

  // The first friend keeps decoding, now in 42e0; the new friend decodes it too.
  const after = await keepsDecoding(page, shareId, 'the admin');
  expect(videoOf(after, shareId)?.codec, "the admin's inbound codec").toBe('h264/42e0');
  await keepsDecoding(dan, shareId, 'the friend who decodes only Constrained Baseline');
  await waitForState(dan, (s) => s.viewer.shares.find((x) => x.id === shareId)?.subscription?.reason === undefined, {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the friend's subscription has no reason left",
  });

  // Nobody asked for anything to get there: no caps.update, no pc.restart, and the share stayed live.
  expect(sent(danWire, 'caps.update'), "the friend's caps.update").toEqual([]);
  expect(sent(danWire, 'pc.restart'), "the friend's pc.restart").toEqual([]);
  expect(sent(alexWire, 'pc.restart'), "the sharer's pc.restart").toEqual([]);
  expect(received(alexWire, 'error'), "the sharer's errors").toEqual([]);
  const own = await waitForState(alex, (s) => s.viewer.shares.find((x) => x.id === shareId));
  expect(own.status).toBe('live');
  expect(viewerErrors).toEqual([]);
  expect(sharerErrors).toEqual([]);
  expect(danErrors).toEqual([]);
});

test('a fresh Firefox hears the share while it waits for its decoder, and sends only caps.update', async ({
  startServer,
  page,
  asUser,
}) => {
  const server = await startServer();
  // The admin shares.
  await fakeDisplay(page, 'window+audio');
  await openViewer(server, page);
  const shareId = await share(page);

  let wire: Frame[] = [];
  const dan = await joinLounge(server, asUser, async (p) => {
    wire = record(p);
    await p.addInitScript(limitDecoders, []);
    await p.addInitScript(looksLikeFirefox, FIREFOX_UA);
  });
  const errors = uncaughtErrors(dan);
  const hello = sent(wire, 'hello')[0]?.['caps'] as { decode?: string[] } | undefined;
  expect(hello?.decode, "the friend's hello: no H.264 yet").toEqual(['opus']);

  // The server sends the audio and says why there is no video (01 §11.7); the stage says the decoder is coming.
  await waitForState(dan, (s) => s.viewer.shares.find((x) => x.id === shareId)?.subscription?.reason === 'codec', {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the friend's subscription has the reason codec",
  });
  const stage = dan.getByRole('region', { name: /^Now watching:/ });
  await expect(stage.getByText(/still getting its video decoder/)).toBeVisible();
  await waitForStats(
    dan,
    (s) => (audioOf(s, shareId)?.bytesReceived ?? 0) > 0 && (audioOf(s, shareId)?.audioLevel ?? 0) > 0.05,
    {
      timeout: MEDIA_TIMEOUT_MS,
      message: 'the friend hears the share',
    },
  );
  const subGen = (await waitForState(dan, (s) => s.viewer.subGen > 0 && s)).viewer.subGen;

  // Waiting: the capabilities are read every 5 s, and nothing changed, so nothing is sent.
  await dan.waitForTimeout(6_000);
  expect(sent(wire, 'caps.update'), 'caps.update while nothing changed').toEqual([]);

  // OpenH264 is there: the page tells the server, once.
  await dan.evaluate(() => {
    (window as unknown as { __h264?: boolean }).__h264 = true;
  });
  await expect
    .poll(() => sent(wire, 'caps.update').length, {
      message: 'caps.update after the decoder came',
      timeout: CAPS_UPDATE_WITHIN_MS,
    })
    .toBe(1);
  const update = sent(wire, 'caps.update')[0]?.['caps'] as { decode?: string[] } | undefined;
  // The browser's own decoders, which every Chrome has Constrained Baseline among.
  expect(update?.decode, 'the decoders in caps.update').toContain('h264/42e0');

  // The server rebuilds the sub PC with the video (02 §8.5): a new gen, and the stage plays it.
  await waitForState(dan, (s) => s.viewer.subGen > subGen, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the server rebuilt the sub PC',
  });
  await keepsDecoding(dan, shareId, 'the friend');
  await expect(stage.getByText(/still getting its video decoder/)).toBeHidden();

  // The page's whole part was that caps.update: never a pc.restart for codecs (01 §11.7).
  expect(sent(wire, 'pc.restart'), "the friend's pc.restart").toEqual([]);
  expect(sent(wire, 'caps.update')).toHaveLength(1);
  expect(errors).toEqual([]);
});
