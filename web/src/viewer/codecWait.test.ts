// The codec wait (05 §10.6, §19.1; 01 §8.10, §11.7): while a subscription has the reason `codec`, the page reads
// its capabilities every 5 s and sends caps.update when its H.264 decoders changed, and nothing else: never a
// pc.restart for codecs. After 3 minutes it asks for a reload; a browser other than Firefox without any H.264
// decoder is told that it can't play the video.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createUiStore } from '../app/uiStore';
import { clearLog, createLogger, logLines } from '../lib/log';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer } from '../protocol/testing';
import type { Caps, CapsUpdate, PCOffer, SubscriptionStatus } from '../protocol/types.gen';
import { buildSdp, FakeRTCPeerConnection } from '../test/FakeRTCPeerConnection';
import { createTestPlatform } from '../test/platform';
import {
  attachCodecWait,
  CAPS_POLL_MS,
  CAPS_UPDATE_SPACING_MS,
  CODEC_STUCK_MS,
  CODEC_WAIT_GAP_MS,
  codecNotice,
  type CodecWaitSignal,
} from './codecWait';
import { attachViewer, createViewer, type ViewerServices } from './services';
import { SubscriberPC } from './SubscriberPC';
import { room, SELF, shareInfo, status } from './testing';
import { createViewerStore, type ViewerStore } from './viewerStore';

const NO_H264: Caps = { decode: ['opus'] };
const CB: Caps = { decode: ['h264/42e0', 'h264/4200', 'opus'] };
const CODEC: Partial<SubscriptionStatus> = { video: 'off', requestedVideo: 'high', reason: 'codec' };

/** A browser whose capabilities the test changes, and that counts how often they are read. */
function browser(initial: Caps): { caps: () => Caps; set(next: Caps): void; reads(): number } {
  let now = initial;
  let reads = 0;
  return {
    caps: () => {
      reads++;
      return { ...now, decode: [...now.decode] };
    },
    set(next) {
      now = next;
    },
    reads: () => reads,
  };
}

describe('codecNotice', () => {
  const wait = { since: 0, stuck: false, noDecoder: false, firefox: false };
  it('maps the wait to what the page says', () => {
    expect(codecNotice(null)).toBeNull();
    expect(codecNotice(wait)).toBe('waiting');
    expect(codecNotice({ ...wait, firefox: true, noDecoder: true })).toBe('waiting');
    expect(codecNotice({ ...wait, stuck: true })).toBe('stuck');
    expect(codecNotice({ ...wait, stuck: true, firefox: true })).toBe('stuckFirefox');
    expect(codecNotice({ ...wait, noDecoder: true })).toBe('missing');
    expect(codecNotice({ ...wait, noDecoder: true, stuck: true })).toBe('missing');
  });
});

describe('attachCodecWait', () => {
  let store: ViewerStore;
  let sent: { type: string; data: unknown }[];
  let ready: boolean;
  let signal: CodecWaitSignal;
  let detach: (() => void) | undefined;

  const capsUpdates = (): CapsUpdate[] => sent.filter((m) => m.type === 'caps.update').map((m) => m.data as CapsUpdate);
  const codecStatus = (shareId = 's_a'): void => {
    store.getState().applyStatus([status(shareId, CODEC)]);
  };
  const videoBack = (shareId = 's_a'): void => {
    store.getState().applyStatus([status(shareId)]);
  };

  beforeEach(() => {
    vi.useFakeTimers();
    clearLog();
    store = createViewerStore();
    sent = [];
    ready = true;
    signal = {
      notify: (type, data) => {
        if (!ready) return false;
        sent.push({ type, data });
        return true;
      },
    };
  });

  afterEach(() => {
    detach?.();
    detach = undefined;
    vi.useRealTimers();
  });

  it('does nothing while no subscription has the reason codec', async () => {
    const b = browser(NO_H264);
    detach = attachCodecWait(signal, store, { caps: b.caps, firefox: true });
    const readsAtStart = b.reads();
    store.getState().applyStatus([status('s_a', { video: 'low', reason: 'bandwidth' })]);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(b.reads()).toBe(readsAtStart);
    expect(sent).toEqual([]);
    expect(store.getState().codecWait).toBeNull();
  });

  it('polls every 5 s while it waits, and sends caps.update once when H.264 arrives', async () => {
    const b = browser(NO_H264);
    detach = attachCodecWait(signal, store, { caps: b.caps, firefox: true });
    codecStatus();
    const start = Date.now();
    expect(store.getState().codecWait).toEqual({ since: start, stuck: false, noDecoder: true, firefox: true });
    expect(codecNotice(store.getState().codecWait)).toBe('waiting');
    const reads = b.reads();

    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS * 4);
    expect(b.reads()).toBe(reads + 4);
    expect(sent).toEqual([]);

    // OpenH264 is installed: the next poll tells the server, once.
    b.set(CB);
    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS);
    expect(capsUpdates()).toEqual([{ caps: CB }]);
    expect(store.getState().codecWait?.noDecoder).toBe(false);
    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS * 3);
    expect(capsUpdates()).toHaveLength(1);
    expect(sent.map((m) => m.type)).toEqual(['caps.update']);
    expect(logLines().some((l) => l.msg.includes('caps.update sent'))).toBe(true);

    // The server rebuilt the sub PC with the video: the wait is over, and so is the poll.
    videoBack();
    expect(store.getState().codecWait).toBeNull();
    const after = b.reads();
    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS * 4);
    expect(b.reads()).toBe(after);
  });

  it('reads the capabilities at once when the wait begins', () => {
    const b = browser(NO_H264);
    detach = attachCodecWait(signal, store, { caps: b.caps, firefox: true });
    // The decoder came between the hello and the first status.
    b.set(CB);
    codecStatus();
    expect(capsUpdates()).toEqual([{ caps: CB }]);
  });

  it('compares the decoders as sets, and only the H.264 ones', async () => {
    const b = browser({ decode: ['h264/42e0', 'h264/4200', 'opus'] });
    detach = attachCodecWait(signal, store, { caps: b.caps, firefox: true });
    codecStatus();
    b.set({ decode: ['h264/4200', 'h264/42e0'], encode: ['h264/42e0'], simulcast: true });
    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS * 3);
    expect(sent).toEqual([]);
  });

  it('tries again at the next poll when signaling was not ready', async () => {
    const b = browser(NO_H264);
    detach = attachCodecWait(signal, store, { caps: b.caps, firefox: true });
    codecStatus();
    ready = false;
    b.set(CB);
    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS);
    expect(sent).toEqual([]);
    ready = true;
    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS);
    expect(capsUpdates()).toEqual([{ caps: CB }]);
  });

  it('sends at most one caps.update per 5 s', async () => {
    const b = browser(NO_H264);
    detach = attachCodecWait(signal, store, { caps: b.caps, firefox: true });
    b.set(CB);
    codecStatus(); // at once
    expect(capsUpdates()).toHaveLength(1);
    // The wait ends and begins again at once: the decoders changed back meanwhile, but the spacing holds.
    videoBack();
    b.set(NO_H264);
    codecStatus();
    expect(capsUpdates()).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(CAPS_UPDATE_SPACING_MS);
    expect(capsUpdates()).toEqual([{ caps: CB }, { caps: NO_H264 }]);
  });

  it('asks for a reload after 3 minutes, Firefox with its own text', async () => {
    detach = attachCodecWait(signal, store, { caps: () => CB, firefox: true });
    codecStatus();
    await vi.advanceTimersByTimeAsync(CODEC_STUCK_MS - 1);
    expect(codecNotice(store.getState().codecWait)).toBe('waiting');
    await vi.advanceTimersByTimeAsync(1);
    expect(store.getState().codecWait?.stuck).toBe(true);
    expect(codecNotice(store.getState().codecWait)).toBe('stuckFirefox');
    // It keeps polling: a caps change still helps (02 §8.5).
    expect(sent).toEqual([]);
  });

  it('another browser that has H.264 gets the plain reload text', async () => {
    detach = attachCodecWait(signal, store, { caps: () => CB, firefox: false });
    codecStatus();
    await vi.advanceTimersByTimeAsync(CODEC_STUCK_MS);
    expect(codecNotice(store.getState().codecWait)).toBe('stuck');
  });

  it('a browser other than Firefox without any H.264 decoder is told it cannot play the video', () => {
    detach = attachCodecWait(signal, store, { caps: () => NO_H264, firefox: false });
    codecStatus();
    expect(codecNotice(store.getState().codecWait)).toBe('missing');
  });

  it('a short gap goes on with the same 3 minutes; a long one starts them again', async () => {
    detach = attachCodecWait(signal, store, { caps: () => NO_H264, firefox: true });
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1)), SELF);
    codecStatus('s_a');
    const since = store.getState().codecWait?.since;
    await vi.advanceTimersByTimeAsync(60_000);
    // The share ends (its status goes with it), and another one starts.
    store.getState().syncRoom(room(), SELF);
    expect(store.getState().status).toEqual({});
    expect(store.getState().codecWait).toBeNull();
    await vi.advanceTimersByTimeAsync(CODEC_WAIT_GAP_MS);
    store.getState().syncRoom(room(shareInfo('s_b', 'u_cy', 2)), SELF);
    codecStatus('s_b');
    expect(store.getState().codecWait?.since).toBe(since);
    await vi.advanceTimersByTimeAsync(CODEC_STUCK_MS - 60_000 - CODEC_WAIT_GAP_MS);
    expect(store.getState().codecWait?.stuck).toBe(true);

    videoBack('s_b');
    await vi.advanceTimersByTimeAsync(CODEC_WAIT_GAP_MS + 1);
    codecStatus('s_b');
    expect(store.getState().codecWait).toMatchObject({ since: Date.now(), stuck: false });
  });

  it('survives capabilities that throw', async () => {
    let fail = true;
    detach = attachCodecWait(signal, store, {
      caps: () => {
        if (fail) throw new Error('no RTCRtpReceiver');
        return CB;
      },
      firefox: true,
      log: createLogger('test'),
    });
    codecStatus();
    expect(store.getState().codecWait).not.toBeNull();
    fail = false;
    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS);
    expect(capsUpdates()).toEqual([{ caps: CB }]);
  });

  it('stops when detached', async () => {
    const b = browser(NO_H264);
    const off = attachCodecWait(signal, store, { caps: b.caps, firefox: true });
    codecStatus();
    off();
    expect(store.getState().codecWait).toBeNull();
    const reads = b.reads();
    await vi.advanceTimersByTimeAsync(CODEC_STUCK_MS);
    expect(b.reads()).toBe(reads);
    expect(store.getState().codecWait).toBeNull();
  });
});

// ---- 05 §19.1 with the real parts: the SignalClient on the fake server, the SubscriberPC on the fake PC ----

/** A server's sub offer (01 §11.7): the share's audio, and its video once the viewer can decode it. */
function subOffer(gen: number, neg: number, withVideo: boolean): PCOffer {
  const sections = withVideo
    ? [
        { kind: 'audio' as const, mid: '0' },
        { kind: 'video' as const, mid: '1' },
      ]
    : [{ kind: 'audio' as const, mid: '0' }];
  return {
    pc: 'sub',
    gen,
    neg,
    sdp: buildSdp({
      ufrag: `srv${String(gen)}`,
      sections: sections.map((s) => ({
        kind: s.kind,
        mid: s.mid,
        direction: 'sendonly',
        msid: { stream: 's_a', track: `${s.kind.charAt(0)}-s_a` },
      })),
    }),
    tracks: sections.map((s) => ({ mid: s.mid, shareId: 's_a', kind: s.kind })),
  };
}

describe('a fresh Firefox against the server (01 §11.7)', () => {
  let server: FakeSignalServer;
  let signal: SignalClient;
  let caps: Caps;
  let viewer: ViewerServices;
  let sub: SubscriberPC;
  let offs: (() => void)[];

  /** What the client sent after its hello, by type, without the heartbeat. */
  const wire = (): string[] =>
    server
      .messages()
      .map((m) => m.type)
      .filter((t) => t !== 'hello' && t !== 'ping');

  beforeEach(async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    clearLog();
    caps = NO_H264;
    server = FakeSignalServer.install();
    const platform = createTestPlatform({ capsNow: () => caps });
    signal = new SignalClient({
      url: platform.signaling().url,
      client: { ...platform.client, browser: 'firefox' },
      role: 'viewer',
      caps: () => platform.capsNow(),
    });
    viewer = createViewer();
    sub = viewer.createSubscriber({ platform, signal, log: createLogger('sub-pc'), ui: createUiStore() }, SubscriberPC);
    offs = [
      // The room session's part (05 §11.1): the server's sub offers go to the sub PC.
      signal.on('pc.offer', (o) => {
        if (o.pc === 'sub')
          void sub.handleOffer(o).then(() => FakeRTCPeerConnection.last?.setConnectionState('connected'));
      }),
      attachViewer(signal, viewer, { caps: () => caps, firefox: true }),
    ];
    signal.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(server.hellos[0]?.caps).toEqual(NO_H264);
  });

  afterEach(() => {
    for (const off of offs) off();
    sub.close();
    signal.stop();
    server.uninstall();
    viewer.dispose();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('polls its caps and sends only caps.update, never pc.restart, through the server’s own rebuilds', async () => {
    // Audio only, and the status that says why (01 §11.7).
    server.send('pc.offer', subOffer(1, 1, false));
    server.send('subscribe.status', { subs: [status('s_a', CODEC)] });
    await vi.advanceTimersByTimeAsync(0);
    expect(sub.gen).toBe(1);

    // A minute of waiting. The server rebuilds the sub PC on its own meanwhile (02 §8.5: a Firefox that lists H.264
    // before it can use it); the page just answers.
    await vi.advanceTimersByTimeAsync(20_000);
    server.send('pc.offer', subOffer(2, 1, false));
    await vi.advanceTimersByTimeAsync(20_000);
    server.send('pc.offer', subOffer(3, 1, false));
    server.send('subscribe.status', { subs: [status('s_a', CODEC)] });
    await vi.advanceTimersByTimeAsync(20_000);
    expect(sub.gen).toBe(3);
    expect(wire().filter((t) => t !== 'pc.answer')).toEqual([]);

    expect(viewer.store.getState().codecWait).toMatchObject({ stuck: false, noDecoder: true });

    // OpenH264 arrives: within 5 s the page tells the server, once.
    caps = CB;
    await vi.advanceTimersByTimeAsync(CAPS_POLL_MS);
    expect(server.messages('caps.update').map((m) => m.data)).toEqual([{ caps: CB }]);

    // The server rebuilds the sub PC with the video (gen + 1), and the reason clears.
    server.send('pc.offer', subOffer(4, 1, true));
    server.send('subscribe.status', { subs: [status('s_a')] });
    await vi.advanceTimersByTimeAsync(CODEC_STUCK_MS);
    expect(sub.gen).toBe(4);
    expect(viewer.store.getState().codecWait).toBeNull();
    expect(viewer.registry.get('s_a').video).toBeDefined();

    expect(server.messages('pc.restart')).toEqual([]);
    expect(wire().filter((t) => t !== 'pc.answer')).toEqual(['caps.update']);
    expect(server.messages('pc.answer').map((m) => (m.data as { gen: number }).gen)).toEqual([1, 2, 3, 4]);
  });

  it('asks for a reload after 3 minutes, and still sends nothing else', async () => {
    server.send('pc.offer', subOffer(1, 1, false));
    server.send('subscribe.status', { subs: [status('s_a', CODEC)] });
    await vi.advanceTimersByTimeAsync(CODEC_STUCK_MS);
    expect(codecNotice(viewer.store.getState().codecWait)).toBe('stuckFirefox');
    expect(wire().filter((t) => t !== 'pc.answer')).toEqual([]);
  });
});

describe('attachViewer', () => {
  it('runs no codec wait for a signal that cannot send', () => {
    const viewer = createViewer();
    let onStatus: ((s: { subs: SubscriptionStatus[] }) => void) | undefined;
    const off = attachViewer(
      {
        on: ((_type: string, fn: (s: { subs: SubscriptionStatus[] }) => void) => {
          onStatus = fn;
          return () => undefined;
        }) as never,
      },
      viewer,
      { caps: () => NO_H264, firefox: false },
    );
    onStatus?.({ subs: [status('s_a', CODEC)] });
    expect(viewer.store.getState().status['s_a']?.reason).toBe('codec');
    expect(viewer.store.getState().codecWait).toBeNull();
    off();
    viewer.dispose();
  });
});
