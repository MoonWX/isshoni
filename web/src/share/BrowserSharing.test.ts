// BrowserSharing.start and the ActiveShare it returns (BrowserShare), against the real SignalClient on the fake
// WebSocket server and the fake RTCPeerConnection (05 §13.4–§13.7): share.start then the pub offer, the share state
// machine's second half, stop and browser-stop, the server ending the share, presets, hints.
import { afterEach, beforeEach, describe, expect, expectTypeOf, it, vi } from 'vitest';

import { LocalError } from '../lib/errors';
import { clearLog } from '../lib/log';
import type {
  ActiveShare,
  LocalEndReason,
  LocalShareState,
  PickedSource,
  Platform,
  ShareHint,
} from '../platform/types';
import { isProtocolError } from '../protocol/errors';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer, makeError } from '../protocol/testing';
import type { PCClose, ShareStart, ShareStop, ShareUpdate } from '../protocol/types.gen';
import type { ShareRecovery } from '../rooms/RoomSession';
import {
  FAKE_AUDIO_CODECS,
  FAKE_VIDEO_CODECS,
  FakeRTCPeerConnection,
  parseSdpSections,
} from '../test/FakeRTCPeerConnection';
import { createTestPlatform } from '../test/platform';
import {
  BrowserShare,
  END_NOTICE_GRACE_MS,
  PubNegotiationFailedError,
  ShareEndedError,
  STATS_SAMPLE_MS,
} from './BrowserShare';
import { BrowserSharing, type BrowserSharingDeps } from './BrowserSharing';
import { createShareStore, ShareCancelledError, type ShareStore } from './shareStore';
import { fakePick, type FakePick } from './testing/fakeCapture';
import { FakeShareHub, lastPc, pubOffers, shareInfo, shareParams } from './testing/publish';

let server: FakeSignalServer;
let hub: FakeShareHub;
let signal: SignalClient;
let platform: Platform;
let store: ShareStore;
let sharing: BrowserSharing;
/** Every share a test started: stopped after it, so none keeps its timers and page listeners. */
const started: ActiveShare[] = [];

const tick = async (ms = 0): Promise<void> => {
  await vi.advanceTimersByTimeAsync(ms);
};

function makeSharing(overrides: Partial<BrowserSharingDeps> = {}): BrowserSharing {
  return new BrowserSharing({
    capture: () => Promise.resolve(null),
    platform,
    store,
    capabilities: (kind) => (kind === 'video' ? FAKE_VIDEO_CODECS : FAKE_AUDIO_CODECS),
    ...overrides,
  });
}

interface Started {
  share: ActiveShare;
  src: FakePick;
  states: LocalShareState[];
  ended: LocalEndReason[];
  hints: (ShareHint | null)[];
}

/** Starts a share of a window (with sound unless told otherwise) in the lounge and records its events. */
async function start(
  opts: { preset?: 'auto' | 'game' | 'movie' | 'text'; withAudio?: boolean; src?: FakePick } = {},
): Promise<Started> {
  const src = opts.src ?? fakePick('window', true);
  const share = await sharing.start(
    src,
    { preset: opts.preset ?? 'auto', withAudio: opts.withAudio ?? true },
    { signal, roomId: 'lounge' },
  );
  started.push(share);
  const states: LocalShareState[] = [];
  const ended: LocalEndReason[] = [];
  const hints: (ShareHint | null)[] = [];
  share.on('state', (s) => states.push(s));
  share.on('ended', (r) => ended.push(r));
  share.on('hint', (h) => hints.push(h));
  return { share, src, states, ended, hints };
}

/** The server answers the offer, the PC connects, and room.state lists the share as live. */
async function goLive(shareId = 's_1'): Promise<void> {
  hub.answer();
  await tick();
  lastPc().setIceConnectionState('connected');
  lastPc().setConnectionState('connected');
  hub.roomState('lounge', [shareInfo(shareId)]);
  await tick();
}

/** The types of the client's messages after the handshake, in the order they were sent. */
const wire = (): string[] =>
  server
    .messages()
    .map((m) => m.type)
    .filter((t) => t !== 'hello' && t !== 'ping');

beforeEach(async () => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  clearLog();
  server = FakeSignalServer.install();
  hub = new FakeShareHub(server);
  platform = createTestPlatform();
  signal = new SignalClient({
    url: platform.signaling().url,
    client: platform.client,
    role: 'full',
    caps: () => platform.capsNow(),
  });
  store = createShareStore();
  sharing = makeSharing();
  signal.start();
  await tick();
  expect(signal.state).toBe('ready');
});

afterEach(async () => {
  await Promise.all(started.splice(0).map((share) => share.stop()));
  signal.stop();
  server.uninstall();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('BrowserSharing (05 §8)', () => {
  it('is the in-page provider', () => {
    expect(sharing.mode).toBe('in-page');
  });

  it('pick() is the capture it was given: called at once, same result', async () => {
    const src = fakePick('window', true);
    const capture = vi.fn<BrowserSharingDeps['capture']>(() => Promise.resolve(src));
    const picking = makeSharing({ capture }).pick({ preset: 'text' });
    expect(capture).toHaveBeenCalledExactlyOnceWith({ preset: 'text' });
    await expect(picking).resolves.toBe(src);
  });

  it('pick() passes a cancel (null) and a failed capture through', async () => {
    await expect(makeSharing().pick({ preset: 'auto' })).resolves.toBeNull();
    const failure = new Error('capture failed');
    await expect(makeSharing({ capture: () => Promise.reject(failure) }).pick({ preset: 'auto' })).rejects.toBe(
      failure,
    );
  });

  it('its ActiveShare has the session’s serverEnded seam (rooms/RoomSession ShareRecovery)', () => {
    expectTypeOf<BrowserShare>().toExtend<ActiveShare & Pick<ShareRecovery, 'serverEnded'>>();
  });
});

describe('start: share.start, then the pub offer (05 §13.4)', () => {
  it('sends share.start {kind, preset, audio, ref}, then a pub offer with tracks, gen 1 and neg 1', async () => {
    const { share, src } = await start({ src: fakePick('browser', true), preset: 'movie' });

    expect(wire()).toEqual(['share.start', 'pc.offer']);
    const [request] = hub.requests<ShareStart>('share.start');
    // Only what was picked, by kind: no label, so no window title.
    expect(request).toEqual({
      kind: 'tab',
      preset: 'movie',
      audio: true,
      ref: expect.stringMatching(/^[0-9a-f]{24}$/) as string,
    });
    expect(pubOffers(server)).toEqual([
      {
        pc: 'pub',
        gen: 1,
        neg: 1,
        sdp: lastPc().localDescription?.sdp,
        tracks: [
          { mid: '0', shareId: 's_1', kind: 'video' },
          { mid: '1', shareId: 's_1', kind: 'audio' },
        ],
      },
    ]);

    expect(share.shareId).toBe('s_1');
    expect(share.kind).toBe('tab');
    expect(share.state).toBe('starting');
    expect(share.params).toEqual(shareParams('s_1'));
    expect(share.preview).toBe(src.preview);
    expect(src.release).not.toHaveBeenCalled();
  });

  it('builds the transceivers from the ShareParams of the answer: encodings q then f, the codec first', async () => {
    hub.nextParams = { codec: 'h264/42e0' };
    await start();
    const [video, audio] = lastPc().transceivers;
    expect(video?.sender.parameters.encodings).toEqual([
      { rid: 'q', active: true, maxBitrate: 300_000, maxFramerate: 15, scaleResolutionDownBy: 3 },
      { rid: 'f', active: true, maxBitrate: 8_000_000, maxFramerate: 60, scaleResolutionDownBy: 1 },
    ]);
    expect(video?.codecPreferences[0]?.sdpFmtpLine).toContain('profile-level-id=42e01f');
    expect(audio?.sender.parameters.encodings).toEqual([{ maxBitrate: 128_000 }]);
  });

  it('every start has a fresh ref', async () => {
    const first = await start();
    await first.share.stop();
    await start();
    const refs = hub.requests<ShareStart>('share.start').map((r) => r.ref);
    expect(new Set(refs).size).toBe(2);
  });

  it('without sound: the audio tracks are stopped and left out, and share.start says audio: false', async () => {
    const src = fakePick('monitor', true);
    const [audio] = src.stream.getAudioTracks();
    await start({ src, withAudio: false });
    expect(hub.requests<ShareStart>('share.start')[0]).toMatchObject({ kind: 'screen', audio: false });
    expect(audio?.readyState).toBe('ended');
    expect(src.stream.getAudioTracks()).toEqual([]);
    expect(pubOffers(server)[0]?.tracks).toEqual([{ mid: '0', shareId: 's_1', kind: 'video' }]);
    expect(store.getState()).toMatchObject({ withAudio: false, soundOn: false });
  });

  it('a pick without an audio track says audio: false', async () => {
    await start({ src: fakePick('window', false) });
    expect(hub.requests<ShareStart>('share.start')[0]).toMatchObject({ audio: false });
    expect(lastPc().transceivers).toHaveLength(1);
  });

  it('fills the share store: starting, with what is shared and the params', async () => {
    await start({ preset: 'game' });
    expect(store.getState()).toMatchObject({
      phase: 'starting',
      preset: 'game',
      withAudio: true,
      soundOn: true,
      picked: { kind: 'window', audioScope: 'window', warning: null },
      params: shareParams('s_1'),
      error: null,
    });
  });

  it('a capture that already ended: capture_failed, and nothing is sent', async () => {
    const src = fakePick('window', true);
    src.stream.getVideoTracks()[0]?.end();
    await expect(start({ src })).rejects.toMatchObject({ code: 'capture_failed' });
    expect(wire()).toEqual([]);
    expect(store.getState().phase).toBe('idle');
  });

  it('no H.264 encoder: h264_unavailable before the server is asked (05 §13.4 step 8)', async () => {
    sharing = makeSharing({ capabilities: () => [{ mimeType: 'video/VP8', clockRate: 90000 }] });
    const err: unknown = await start().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LocalError);
    expect(err).toMatchObject({ code: 'h264_unavailable' });
    expect(wire()).toEqual([]);
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
  });

  it('share.start refused (share_limit): rejects with the server’s error; no PC, and the store is the caller’s', async () => {
    server.handle('share.start', () => ({
      error: makeError('share_limit', 'request', { params: { limit: 1, per: 'user' } }),
    }));
    const err: unknown = await start().catch((e: unknown) => e);
    expect(isProtocolError(err) && err.code).toBe('share_limit');
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
    expect(store.getState().phase).toBe('idle');
  });

  it('the tracks can’t be offered: the share the server made is stopped again, and the store says failed', async () => {
    platform = createTestPlatform({
      createPeerConnection: (config) => {
        const pc = new FakeRTCPeerConnection(config);
        pc.failNext('setLocalDescription');
        return pc as unknown as RTCPeerConnection;
      },
    });
    sharing = makeSharing();
    const src = fakePick('window', true);
    const err: unknown = await start({ src }).catch((e: unknown) => e);
    expect(err).toMatchObject({ name: 'LocalError', code: 'webrtc_failed' });
    // A start that failed once is a failed share, not the end of media on this page.
    expect(err).not.toBeInstanceOf(PubNegotiationFailedError);
    await tick();
    expect(wire()).toEqual(['share.start', 'share.stop']);
    expect(hub.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(store.getState()).toMatchObject({ phase: 'failed', error: err });
    // The source stays the caller's to release (the share store's pick flow does).
    expect(src.release).not.toHaveBeenCalled();
  });

  it('"Stop sharing" in the browser while share.start is on its way: given up, not failed; the share is stopped', async () => {
    const src = fakePick('window', true);
    const starting = start({ src }).catch((e: unknown) => e);
    src.stream.getVideoTracks()[0]?.end();
    const err = await starting;
    expect(err).toBeInstanceOf(ShareCancelledError);
    await tick();
    expect(wire()).toEqual(['share.start', 'share.stop']);
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
    expect(store.getState()).toMatchObject({ phase: 'idle', error: null });
  });

  it('"Stop sharing" while the tracks are being added: the share stops itself, and the start is given up', async () => {
    const src = fakePick('window', true);
    platform = createTestPlatform({
      createPeerConnection: (config) => {
        src.stream.getVideoTracks()[0]?.end();
        return new FakeRTCPeerConnection(config) as unknown as RTCPeerConnection;
      },
    });
    sharing = makeSharing();
    const err: unknown = await start({ src }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ShareCancelledError);
    await tick();
    expect(wire()).toEqual(['share.start', 'pc.offer', 'share.stop', 'pc.close']);
    expect(src.release).toHaveBeenCalledOnce();
    expect(store.getState()).toMatchObject({ phase: 'idle', error: null });
  });

  it('an error about the share that comes while its tracks are being added fails the start with that error', async () => {
    platform = createTestPlatform({
      createPeerConnection: (config) => {
        // The PC is made after share.start was answered: the server's error arrives right then.
        server.error(makeError('codec_not_supported', 'share', { shareId: 's_1' }));
        return new FakeRTCPeerConnection(config) as unknown as RTCPeerConnection;
      },
    });
    sharing = makeSharing();
    const src = fakePick('window', true);
    const err: unknown = await start({ src }).catch((e: unknown) => e);
    expect(isProtocolError(err) && err.code).toBe('codec_not_supported');
    await tick();
    expect(store.getState().phase).toBe('failed');
    expect(store.getState().error).toBe(err);
    // The share cleaned up after itself: the capture too, since it had taken the source.
    expect(src.release).toHaveBeenCalledOnce();
    expect(lastPc().signalingState).toBe('closed');
  });
});

describe('the share goes live (05 §13.1)', () => {
  it('routes the server’s pc.answer {pub} to the pub PC', async () => {
    await start();
    expect(lastPc().signalingState).toBe('have-local-offer');
    hub.answer();
    await tick();
    expect(lastPc().signalingState).toBe('stable');
  });

  it('starting → live once room.state lists the share as live', async () => {
    const { share, states } = await start();
    hub.answer();
    await tick();
    // Still starting in room.state, another room, another share: nothing.
    hub.roomState('lounge', [shareInfo('s_1', { status: 'starting', layers: [] })]);
    hub.roomState('games', [shareInfo('s_1')]);
    hub.roomState('lounge', [shareInfo('s_other')]);
    await tick();
    expect(share.state).toBe('starting');
    expect(store.getState().phase).toBe('starting');

    hub.roomState('lounge', [shareInfo('s_1')]);
    await tick();
    expect(share.state).toBe('live');
    expect(states).toEqual(['live']);
    expect(store.getState().phase).toBe('live');
  });

  it('live ⇄ reconnecting with the pub PC, and unreachable is told to the store', async () => {
    const { share, states } = await start();
    await goLive();
    lastPc().setIceConnectionState('disconnected');
    expect(share.state).toBe('reconnecting');
    expect(store.getState().phase).toBe('reconnecting');
    lastPc().setIceConnectionState('connected');
    expect(share.state).toBe('live');
    expect(store.getState().phase).toBe('live');
    expect(states).toEqual(['live', 'reconnecting', 'live']);
  });

  it('routes the server’s pc.restart {pub, rebuild}: a new PC with gen 2 and the same share', async () => {
    const { share } = await start();
    await goLive();
    server.send('pc.restart', { pc: 'pub', gen: 1, mode: 'rebuild', reason: 'failed' });
    await tick();
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 2,
      neg: 1,
      tracks: [{ shareId: 's_1' }, { shareId: 's_1' }],
    });
    expect(share.state).toBe('reconnecting');
    await goLive();
    expect(share.state).toBe('live');
  });

  it('asks before the page is left while sharing, and not afterwards (05 §13.6)', async () => {
    const leaving = (): boolean => !window.dispatchEvent(new Event('beforeunload', { cancelable: true }));
    expect(leaving()).toBe(false);
    const { share } = await start();
    expect(leaving()).toBe(true);
    await share.stop();
    expect(leaving()).toBe(false);
  });
});

describe('stop and browser-stop send share.stop (05 §13.1 "stopping")', () => {
  it('stop(): share.stop, then pc.close for the last share, and the capture tracks stop', async () => {
    const { share, src, states, ended } = await start();
    await goLive();
    const before = wire().length;
    const phases: string[] = [];
    store.subscribe((s, prev) => {
      if (s.phase !== prev.phase) phases.push(s.phase);
    });

    await share.stop();

    expect(wire().slice(before)).toEqual(['share.stop', 'pc.close']);
    expect(hub.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(hub.requests<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 1 }]);
    expect(lastPc().signalingState).toBe('closed');
    expect(src.release).toHaveBeenCalledOnce();
    expect(src.stream.getTracks().map((t) => t.readyState)).toEqual(['ended', 'ended']);
    expect(share.state).toBe('ended');
    expect(share.preview).toBeNull();
    expect(states.at(-1)).toBe('ended');
    expect(ended).toEqual(['user']);
    expect(phases).toEqual(['stopping', 'idle']);
    expect(store.getState()).toMatchObject({ phase: 'idle', picked: null, params: null, notice: null, error: null });
  });

  it('the browser’s own "Stop sharing" (the video track ends): share.stop and the same cleanup', async () => {
    const { share, src, ended } = await start();
    await goLive();
    const before = wire().length;

    src.stream.getVideoTracks()[0]?.end();
    await tick();

    expect(wire().slice(before)).toEqual(['share.stop', 'pc.close']);
    expect(hub.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(src.release).toHaveBeenCalledOnce();
    expect(share.state).toBe('ended');
    expect(ended).toEqual(['browser-stopped']);
    expect(store.getState().phase).toBe('idle');
  });

  it('browser-stop while the share is still starting stops it too', async () => {
    const { src, ended } = await start();
    src.stream.getVideoTracks()[0]?.end();
    await tick();
    expect(hub.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(ended).toEqual(['browser-stopped']);
    expect(store.getState().phase).toBe('idle');
  });

  it('stop() again, or after the capture ended, waits for the same end: one share.stop', async () => {
    const { share, src } = await start();
    await goLive();
    const first = share.stop();
    const second = share.stop();
    src.stream.getVideoTracks()[0]?.end();
    await Promise.all([first, second]);
    await share.stop();
    expect(hub.requests<ShareStop>('share.stop')).toHaveLength(1);
    expect(hub.requests<PCClose>('pc.close')).toHaveLength(1);
  });

  it('stop() never rejects: a refused share.stop is logged and the share is gone all the same', async () => {
    const { share, src } = await start();
    await goLive();
    server.handle('share.stop', () => ({ error: makeError('internal', 'request') }));
    await expect(share.stop()).resolves.toBeUndefined();
    expect(src.release).toHaveBeenCalledOnce();
    expect(store.getState().phase).toBe('idle');
  });

  it('stop() without a connection cleans up locally; the pc.close goes out after the resume', async () => {
    const { share, src } = await start();
    await goLive();
    server.drop();
    await tick();
    await share.stop();
    expect(src.release).toHaveBeenCalledOnce();
    expect(share.state).toBe('ended');
    expect(hub.requests<ShareStop>('share.stop')).toEqual([]);
    await tick(1000);
    expect(hub.requests<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 1 }]);
  });

  it('after a stop the next share starts on a new PC with the next gen', async () => {
    const first = await start();
    await goLive();
    await first.share.stop();
    const second = await start();
    expect(second.share.shareId).toBe('s_2');
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 2,
      neg: 1,
      tracks: [{ shareId: 's_2' }, { shareId: 's_2' }],
    });
    expect(store.getState().phase).toBe('starting');
  });
});

describe('the server ends the share (05 §13.1)', () => {
  const serverEnded = (share: ActiveShare, reason: Parameters<ShareRecovery['serverEnded']>[0]) =>
    (share as BrowserShare).serverEnded(reason);

  it.each(['stopped', 'left'] as const)(
    '%s: the capture stops, the pub PC is closed, no share.stop; idle with the "elsewhere" notice',
    async (reason) => {
      const { share, src, ended } = await start();
      await goLive();
      const before = wire().length;
      const done = serverEnded(share, reason);
      expect(src.release).toHaveBeenCalledOnce();
      expect(share.state).toBe('ended');
      expect(ended).toEqual(['server']);
      await tick(END_NOTICE_GRACE_MS);
      await done;
      expect(wire().slice(before)).toEqual(['pc.close']);
      expect(store.getState()).toMatchObject({ phase: 'idle', notice: { kind: 'elsewhere' }, error: null });
    },
  );

  it('media_timeout: failed, "no video reached the server"', async () => {
    const { share, src } = await start();
    await serverEnded(share, 'media_timeout');
    expect(src.release).toHaveBeenCalledOnce();
    const { phase, error, notice } = store.getState();
    expect(phase).toBe('failed');
    expect(error).toBeInstanceOf(ShareEndedError);
    expect(error).toMatchObject({ kind: 'media_timeout', reason: 'media_timeout' });
    expect(notice).toBeNull();
    expect(hub.requests<ShareStop>('share.stop')).toEqual([]);
  });

  it('room_closed: idle without a word (the session shows the room’s end)', async () => {
    const { share } = await start();
    await goLive();
    await serverEnded(share, 'room_closed');
    expect(store.getState()).toMatchObject({ phase: 'idle', notice: null, error: null });
  });

  it.each([undefined, 'disconnected', 'kicked'] as const)(
    'any other reason (%s), and room.state without the share: failed, "stopped by the server"',
    async (reason) => {
      const { share } = await start();
      await goLive();
      await serverEnded(share, reason);
      expect(store.getState().phase).toBe('failed');
      expect(store.getState().error).toMatchObject({ name: 'ShareEndedError', kind: 'server', reason });
    },
  );

  it('with two shares on the PC, the one that ended is taken off with a re-offer', async () => {
    const first = await start();
    hub.answer();
    await tick();
    const second = await start({ src: fakePick('browser', true) });
    hub.answer();
    await tick();
    // The page's state machine follows the first share only.
    expect(store.getState()).toMatchObject({
      phase: 'starting',
      picked: { kind: 'window' },
      params: { shareId: 's_1' },
    });
    await second.share.setAudioEnabled(false);
    expect(store.getState().soundOn).toBe(true);

    await serverEnded(first.share, 'media_timeout');
    expect(store.getState().phase).toBe('failed');
    expect(server.messages('pc.close')).toEqual([]);
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 1,
      neg: 3,
      tracks: [{ shareId: 's_2' }, { shareId: 's_2' }],
    });
    expect(second.share.state).toBe('starting');
    expect(second.src.release).not.toHaveBeenCalled();
    // The second share ending changes nothing in the store either.
    await second.share.stop();
    expect(store.getState().phase).toBe('failed');
  });

  it('an error in scope share (codec_not_supported) fails the share with that error', async () => {
    const { share, src, ended } = await start();
    hub.answer();
    await tick();
    server.error(makeError('codec_not_supported', 'share', { shareId: 's_1' }));
    await tick();
    expect(share.state).toBe('ended');
    expect(ended).toEqual(['error']);
    expect(src.release).toHaveBeenCalledOnce();
    const { phase, error } = store.getState();
    expect(phase).toBe('failed');
    expect(isProtocolError(error) && error.code).toBe('codec_not_supported');
  });

  it('share.stopped {stopped} followed by that error: the error wins over the "elsewhere" notice', async () => {
    const { share } = await start();
    hub.answer();
    await tick();
    const done = serverEnded(share, 'stopped');
    await tick(END_NOTICE_GRACE_MS / 2);
    expect(store.getState().phase).toBe('stopping');
    server.error(makeError('codec_not_supported', 'share', { shareId: 's_1' }));
    await tick();
    await done;
    const { phase, error, notice } = store.getState();
    expect(phase).toBe('failed');
    expect(isProtocolError(error) && error.code).toBe('codec_not_supported');
    expect(notice).toBeNull();
  });

  it('errors about other shares and scopes are not this share’s', async () => {
    const { share } = await start();
    await goLive();
    server.error(makeError('codec_not_supported', 'share', { shareId: 's_other' }));
    server.error(makeError('internal', 'room', { shareId: 's_1' }));
    await tick();
    expect(share.state).toBe('live');
  });

  it('a pub PC whose negotiation failed twice within a minute ends the share with PubNegotiationFailedError', async () => {
    const { share, src, ended } = await start();
    server.error(makeError('sdp_invalid', 'pc', { pc: 'pub', gen: 1, neg: 1 }));
    await tick();
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 2, neg: 1 });
    expect(share.state).toBe('starting');
    server.error(makeError('sdp_invalid', 'pc', { pc: 'pub', gen: 2, neg: 1 }));
    await tick();
    expect(share.state).toBe('ended');
    expect(ended).toEqual(['error']);
    // Not a plain failed share: the app shows "Can't connect media" for this one (05 §9; shareUi.test.tsx).
    const { phase, error } = store.getState();
    expect(phase).toBe('failed');
    expect(error).toBeInstanceOf(PubNegotiationFailedError);
    expect(error).toMatchObject({ name: 'LocalError', code: 'webrtc_failed' });
    expect(hub.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(hub.requests<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 2 }]);
    expect(src.release).toHaveBeenCalledOnce();
  });
});

describe('changes while live (05 §13.5, §13.6)', () => {
  it('setPreset: share.update, the new params and content hint at once, a re-offer for the new audio bitrate', async () => {
    const { share, src } = await start({ preset: 'auto' });
    await goLive();
    await share.setPreset('movie');

    expect(hub.requests<ShareUpdate>('share.update')).toEqual([{ shareId: 's_1', preset: 'movie' }]);
    expect(share.params?.audioBitrate).toBe(256_000);
    expect(store.getState()).toMatchObject({ preset: 'movie', params: { audioBitrate: 256_000 } });
    expect(src.stream.getVideoTracks()[0]?.contentHint).toBe('motion');
    const [video, audio] = lastPc().transceivers;
    expect(video?.sender.parameters.degradationPreference).toBe('maintain-framerate');
    expect(audio?.sender.parameters.encodings).toEqual([{ maxBitrate: 256_000 }]);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 1, neg: 2 });
  });

  it('a preset with the same audio bitrate needs no offer', async () => {
    const { share } = await start({ preset: 'auto' });
    await goLive();
    await share.setPreset('text');
    expect(lastPc().transceivers[0]?.sender.parameters.degradationPreference).toBe('maintain-resolution');
    expect(pubOffers(server)).toHaveLength(1);
  });

  it('setPreset rejects with the server’s error and leaves the share as it was', async () => {
    const { share } = await start({ preset: 'auto' });
    await goLive();
    server.handle('share.update', () => ({ error: makeError('share_not_found', 'request') }));
    const err: unknown = await share.setPreset('game').catch((e: unknown) => e);
    expect(isProtocolError(err) && err.code).toBe('share_not_found');
    expect(store.getState().preset).toBe('auto');
    expect(share.state).toBe('live');
  });

  it('quality.hint {encodings}: setParameters at once, by rid; the params are the latest', async () => {
    const { share } = await start();
    await goLive();
    const encodings = shareParams('s_1').encodings.map((e) => (e.rid === 'f' ? { ...e, active: false } : e));
    server.send('quality.hint', { shareId: 's_1', reason: 'viewers', encodings });
    await tick();
    const byRid = new Map(lastPc().transceivers[0]?.sender.parameters.encodings.map((e) => [e.rid, e.active]));
    expect(byRid.get('f')).toBe(false);
    expect(byRid.get('q')).toBe(true);
    expect(share.params?.encodings).toEqual(encodings);
    expect(store.getState().params?.encodings).toEqual(encodings);
    expect(pubOffers(server)).toHaveLength(1);
  });

  it('quality.hint {codec}: that profile first and a re-offer; a hint for another share is ignored', async () => {
    const { share } = await start();
    await goLive();
    server.send('quality.hint', { shareId: 's_other', reason: 'codec', codec: 'h264/42e0' });
    await tick();
    expect(pubOffers(server)).toHaveLength(1);

    server.send('quality.hint', { shareId: 's_1', reason: 'codec', codec: 'h264/42e0' });
    await tick();
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 1, neg: 2 });
    expect(parseSdpSections(pubOffers(server).at(-1)?.sdp ?? '')[0]?.codecLines?.[1]).toContain('42e01f');
    expect(share.params?.codec).toBe('h264/42e0');
  });

  it('setAudioEnabled: the panel’s sound switch mutes the track and tells the store', async () => {
    const { share, src } = await start();
    await goLive();
    await share.setAudioEnabled(false);
    expect(src.stream.getAudioTracks()[0]?.enabled).toBe(false);
    expect(store.getState().soundOn).toBe(false);
    await share.setAudioEnabled(true);
    expect(src.stream.getAudioTracks()[0]?.enabled).toBe(true);
    expect(store.getState().soundOn).toBe(true);
  });

  it('setPaused is not in the M1 web sharer', async () => {
    const { share } = await start();
    await expect(share.setPaused(true)).rejects.toMatchObject({ name: 'NotSupportedError' });
  });
});

describe('stats and hints (05 §13.7)', () => {
  /** What the video sender reports: the full layer at a height, limited or not, with bytes that grow. */
  function report(seconds: number, height: number, limit: string): Map<string, Record<string, unknown>> {
    return new Map([
      [
        'f',
        {
          type: 'outbound-rtp',
          kind: 'video',
          rid: 'f',
          frameWidth: Math.round((height * 16) / 9),
          frameHeight: height,
          framesPerSecond: 30,
          bytesSent: seconds * 250_000,
          timestamp: seconds * 1000,
          qualityLimitationReason: limit,
          encoderImplementation: 'VideoToolbox',
          powerEfficientEncoder: true,
        },
      ],
      [
        'q',
        {
          type: 'outbound-rtp',
          kind: 'video',
          rid: 'q',
          frameHeight: 360,
          bytesSent: seconds * 25_000,
          timestamp: seconds * 1000,
        },
      ],
      ['codec', { type: 'codec', mimeType: 'video/H264' }],
    ]);
  }

  it('stats(): a sample per layer, with the bitrate since the previous one', async () => {
    const { share } = await start();
    await goLive();
    const [video, audio] = lastPc().transceivers;
    if (!video || !audio) throw new Error('no senders');
    let seconds = 10;
    video.sender.getStats = () => Promise.resolve(report(seconds, 1080, 'none'));
    audio.sender.getStats = () =>
      Promise.resolve(
        new Map([
          ['a', { type: 'outbound-rtp', kind: 'audio', bytesSent: seconds * 16_000, timestamp: seconds * 1000 }],
        ]),
      );

    const first = await share.stats();
    expect(first?.layers.map((l) => [l.rid, l.kbps])).toEqual([
      ['f', 0],
      ['q', 0],
    ]);
    seconds = 12;
    const second = await share.stats();
    expect(second?.layers).toEqual([
      { rid: 'f', kbps: 2000, width: 1920, height: 1080, fps: 30, limit: 'none', encoder: 'VideoToolbox', hw: true },
      { rid: 'q', kbps: 200, height: 360 },
    ]);
    expect(second?.audioKbps).toBe(128);
  });

  it('stats() is null once the share ended', async () => {
    const { share } = await start();
    await goLive();
    await share.stop();
    await expect(share.stats()).resolves.toBeNull();
  });

  it('the upload hint: after 3 bandwidth-limited samples below the target size, and gone when the share ends', async () => {
    const { share, hints } = await start();
    await goLive();
    const sender = lastPc().transceivers[0]?.sender;
    if (!sender) throw new Error('no video sender');
    let seconds = 0;
    sender.getStats = () => Promise.resolve(report((seconds += 2), 720, 'bandwidth'));

    await tick(STATS_SAMPLE_MS * 2);
    expect(hints).toEqual([]);
    await tick(STATS_SAMPLE_MS);
    expect(hints).toEqual([{ kind: 'upload-limited', approxHeight: 720 }]);
    expect(store.getState().hint).toEqual({ kind: 'upload-limited', approxHeight: 720 });

    await share.stop();
    expect(hints.at(-1)).toBeNull();
    expect(store.getState().hint).toBeNull();
  });

  it('the CPU hint', async () => {
    const { hints } = await start();
    await goLive();
    const sender = lastPc().transceivers[0]?.sender;
    if (!sender) throw new Error('no video sender');
    let seconds = 0;
    sender.getStats = () => Promise.resolve(report((seconds += 2), 1080, 'cpu'));
    await tick(STATS_SAMPLE_MS * 3);
    expect(hints).toEqual([{ kind: 'cpu-limited' }]);
  });
});

describe('a PickedSource is all start() needs', () => {
  it('takes the source by its interface', async () => {
    const pick = fakePick('window', true);
    const src: PickedSource = { ...pick };
    const share = await sharing.start(src, { preset: 'auto', withAudio: true }, { signal, roomId: 'lounge' });
    expect(share.kind).toBe('window');
    await share.stop();
    expect(pick.release).toHaveBeenCalledOnce();
  });
});
