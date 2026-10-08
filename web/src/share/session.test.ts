// The in-page sharer under the real room session (rooms/RoomSession, 05 §11.1): BrowserSharing as platform.sharing
// of a room runtime on the fake signaling server. The session starts and stops the share and tells it when the
// server ended it; everything about the pub PC goes between BrowserSharing and the server.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { PCClose, ShareStart, ShareStop } from '../protocol/types.gen';
import { createHarness, tick, type Harness } from '../rooms/testing/harness';
import { FAKE_AUDIO_CODECS, FAKE_VIDEO_CODECS, FakeRTCPeerConnection } from '../test/FakeRTCPeerConnection';
import { BrowserShare, END_NOTICE_GRACE_MS, ShareEndedError } from './BrowserShare';
import { BrowserSharing } from './BrowserSharing';
import { createShareStore, type ShareStore } from './shareStore';
import { linkShareUi } from './shareUi';
import { fakePick, type FakePick } from './testing/fakeCapture';
import { FakeShareHub, lastPc, pubOffers, shareInfo } from './testing/publish';

let h: Harness;
let shares: FakeShareHub;
let store: ShareStore;

const sentTypes = (): string[] =>
  h.server
    .messages()
    .map((m) => m.type)
    .filter((t) => t.startsWith('share.') || t.startsWith('pc.') || t === 'room.leave');

/** A share of this page's connection as room.state lists it. */
const own = (id: string, overrides: Parameters<typeof shareInfo>[1] = {}) =>
  shareInfo(id, { connectionId: h.runtime.stores.room.getState().connectionId ?? '', ...overrides });

async function startShare(src: FakePick = fakePick('window', true)): Promise<FakePick> {
  await h.runtime.session.startShare(src, { preset: 'auto', withAudio: true });
  return src;
}

/** The SFU answers, the PC connects, and the hub lists the share as live. */
async function goLive(shareId = 's_1'): Promise<void> {
  shares.answer();
  await tick();
  lastPc().setIceConnectionState('connected');
  lastPc().setConnectionState('connected');
  h.hub.sendState('lounge', { shares: [own(shareId)] });
  await tick();
}

beforeEach(async () => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  store = createShareStore();
  const sharing = new BrowserSharing({
    capture: () => Promise.resolve(null),
    platform: { createPeerConnection: (config) => new FakeRTCPeerConnection(config) as unknown as RTCPeerConnection },
    store,
    capabilities: (kind) => (kind === 'video' ? FAKE_VIDEO_CODECS : FAKE_AUDIO_CODECS),
  });
  h = createHarness({ platform: { sharing } });
  shares = new FakeShareHub(h.server);
  linkShareUi(store, h.services.ui);
  await h.connect();
  await h.runtime.session.join('lounge');
  await tick();
});

afterEach(() => {
  h.close();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('RoomSession with the in-page sharer', () => {
  it('startShare: share.start in the session’s room, then the pub offer; the session holds the share', async () => {
    const src = await startShare();
    expect(sentTypes()).toEqual(['share.start', 'pc.offer']);
    expect(shares.requests<ShareStart>('share.start')[0]).toMatchObject({
      kind: 'window',
      preset: 'auto',
      audio: true,
    });
    expect(pubOffers(h.server)[0]).toMatchObject({
      pc: 'pub',
      gen: 1,
      neg: 1,
      tracks: [{ shareId: 's_1' }, { shareId: 's_1' }],
    });

    const share = h.runtime.session.share;
    expect(share).toBeInstanceOf(BrowserShare);
    expect(share).toMatchObject({ shareId: 's_1', state: 'starting', preview: src.preview });
    expect(store.getState().phase).toBe('starting');
    expect(h.services.ui.getState().sharing).toBe(true);

    await goLive();
    expect(share?.state).toBe('live');
    expect(store.getState().phase).toBe('live');
  });

  it('stopShare: share.stop, pc.close, the capture released, and nothing left of the share', async () => {
    const src = await startShare();
    await goLive();
    await h.runtime.session.stopShare();
    expect(sentTypes()).toEqual(['share.start', 'pc.offer', 'share.stop', 'pc.close']);
    expect(shares.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(src.release).toHaveBeenCalledOnce();
    expect(h.runtime.session.share).toBeNull();
    expect(store.getState().phase).toBe('idle');
    expect(h.services.ui.getState().sharing).toBe(false);
    // The server's share.stopped for a stop of our own changes nothing, and is no toast.
    h.server.send('room.event', {
      kind: 'share.stopped',
      roomId: 'lounge',
      userId: 'k3m9p2qxw7ht',
      name: 'Alex',
      shareId: 's_1',
      reason: 'stopped',
      at: '2026-10-12T19:05:00.000Z',
    });
    await tick(END_NOTICE_GRACE_MS);
    expect(store.getState()).toMatchObject({ phase: 'idle', notice: null });
    expect(h.toasts()).toEqual([]);
  });

  it('the browser’s "Stop sharing": the share ends itself, and the session lets go of it', async () => {
    const src = await startShare();
    await goLive();
    src.stream.getVideoTracks()[0]?.end();
    await tick();
    expect(shares.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(shares.requests<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 1 }]);
    expect(h.runtime.session.share).toBeNull();
    expect(store.getState().phase).toBe('idle');
  });

  it('leave(): the share stops before the room is left', async () => {
    await startShare();
    await goLive();
    await h.runtime.session.leave();
    expect(sentTypes()).toEqual(['share.start', 'pc.offer', 'share.stop', 'pc.close', 'room.leave']);
    expect(store.getState().phase).toBe('idle');
  });

  it('room.event share.stopped {media_timeout} for the own share: failed, nothing sent for it, never published again', async () => {
    const src = await startShare();
    h.server.send('room.event', {
      kind: 'share.stopped',
      roomId: 'lounge',
      userId: 'k3m9p2qxw7ht',
      name: 'Alex',
      shareId: 's_1',
      reason: 'media_timeout',
      at: '2026-10-12T19:05:00.000Z',
    });
    await tick();
    expect(src.release).toHaveBeenCalledOnce();
    expect(h.runtime.session.share).toBeNull();
    expect(store.getState().phase).toBe('failed');
    expect(store.getState().error).toBeInstanceOf(ShareEndedError);
    expect(store.getState().error).toMatchObject({ kind: 'media_timeout' });
    expect(sentTypes()).toEqual(['share.start', 'pc.offer', 'pc.close']);
    expect(h.services.ui.getState().sharing).toBe(false);
    expect(h.toasts()).toEqual([]);
  });

  it('share.stopped {stopped} (from another tab or device): idle, with the toast', async () => {
    await startShare();
    await goLive();
    h.server.send('room.event', {
      kind: 'share.stopped',
      roomId: 'lounge',
      userId: 'k3m9p2qxw7ht',
      name: 'Alex',
      shareId: 's_1',
      reason: 'stopped',
      at: '2026-10-12T19:05:00.000Z',
    });
    await tick(END_NOTICE_GRACE_MS);
    expect(store.getState().phase).toBe('idle');
    expect(h.toasts()).toEqual(['Sharing was stopped from another tab or device']);
    expect(shares.requests<ShareStop>('share.stop')).toEqual([]);
  });

  it('a newer room.state without the share (the fallback): failed, "stopped by the server"', async () => {
    await startShare();
    await goLive();
    h.hub.sendState('lounge', { shares: [] });
    await tick();
    expect(h.runtime.session.share).toBeNull();
    expect(store.getState().phase).toBe('failed');
    expect(store.getState().error).toMatchObject({ name: 'ShareEndedError', kind: 'server' });
  });

  it('a welcome that is not resumed: the session stops the share, and the pub PC is dropped without a pc.close', async () => {
    const src = await startShare();
    await goLive();
    h.server.restart();
    h.server.drop();
    await tick(1000);
    expect(h.runtime.signal.state).toBe('ready');
    await tick();
    expect(src.release).toHaveBeenCalledOnce();
    expect(h.runtime.session.share).toBeNull();
    expect(store.getState().phase).toBe('idle');
    expect(lastPc().signalingState).toBe('closed');
    expect(h.server.messages('pc.close')).toEqual([]);

    // Sharing again starts over: a new share, a pub PC of gen 1.
    await startShare();
    expect(pubOffers(h.server).at(-1)).toMatchObject({
      gen: 1,
      neg: 1,
      tracks: [{ shareId: 's_2' }, { shareId: 's_2' }],
    });
  });

  it('a resumed welcome keeps the share: the offer that was out goes again, and the share stays', async () => {
    await startShare();
    // The server kept the connection in its room through the drop (01 §10.3).
    h.server.welcomeDefaults = { roomId: 'lounge' };
    h.server.drop();
    await tick(1000);
    expect(h.runtime.signal.state).toBe('ready');
    expect(h.runtime.signal.welcome?.resumed).toBe(true);
    await tick();
    expect(h.runtime.session.share?.shareId).toBe('s_1');
    const offers = pubOffers(h.server);
    expect(offers).toHaveLength(2);
    expect(offers[1]).toEqual(offers[0]);
    await goLive();
    expect(store.getState().phase).toBe('live');
  });
});
