// The in-page sharer under the real room session (rooms/RoomSession, 05 §11.1): BrowserSharing as platform.sharing
// of a room runtime on the fake signaling server. The session starts and stops the share, tells it when the server
// ended it, and takes it across reconnects (01 §10.5, §10.6: resync, re-publish with replaces, the capture hold);
// everything about the pub PC goes between BrowserSharing and the server.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { LocalShareState } from '../platform/types';
import { BackoffMaxMs } from '../protocol/signal-client';
import { makeError } from '../protocol/testing';
import type { PCClose, ShareStart, ShareStop } from '../protocol/types.gen';
import { createHarness, refuseConnections, tick, track, type Harness } from '../rooms/testing/harness';
import { FAKE_AUDIO_CODECS, FAKE_VIDEO_CODECS, FakeRTCPeerConnection } from '../test/FakeRTCPeerConnection';
import { BrowserShare, END_NOTICE_GRACE_MS, ShareEndedError } from './BrowserShare';
import { BrowserSharing } from './BrowserSharing';
import { CAPTURE_HOLD_MS } from './recovery';
import { createShareStore, type ShareStore } from './shareStore';
import { linkShareUi, shareErrorMessage } from './shareUi';
import { fakePick, type FakePick } from './testing/fakeCapture';
import { FakeShareHub, lastPc, pubOffers, shareInfo, shareParams } from './testing/publish';

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

/** The server goes out of reach: the socket drops, and new ones are refused until the returned function is called. */
function loseServer(): () => void {
  const back = refuseConnections(h.server);
  h.server.drop();
  return back;
}

/** The client's messages on its newest socket, by type, without the heartbeat. */
const onSocket = (): string[] =>
  h.server
    .messages(undefined, h.server.socket)
    .map((m) => m.type)
    .filter((t) => t !== 'ping');

describe('the share across reconnects (01 §10.5, §10.6)', () => {
  it('a restarted server: the share is published again with replaces, on a new pub PC of gen 1, and goes live', async () => {
    const src = await startShare();
    await goLive();
    const share = h.runtime.session.share;
    if (!share) throw new Error('no share');
    const changes: string[] = [];
    share.on('state', (s: LocalShareState) => changes.push(`${s} ${share.shareId}`));
    const before = h.runtime.stores.room.getState().connectionId;

    // The server goes away, and its side of the pub PC with it; while signaling is down that is only recorded.
    const back = loseServer();
    lastPc().setConnectionState('failed');
    await tick(5_000);
    expect(store.getState().phase).toBe('reconnecting');
    expect(src.release).not.toHaveBeenCalled();
    // A new process, which knows no connection (01 §11.6).
    h.server.restart();
    back();
    await tick(BackoffMaxMs);
    expect(h.runtime.signal.welcome?.resumed).toBe(false);
    expect(h.runtime.stores.room.getState().connectionId).not.toBe(before);

    // The room first, then the share again: share.start {replaces, ref: new}, and its offer on a pub PC of gen 1.
    const sent = onSocket();
    expect(sent.indexOf('room.join')).toBeLessThan(sent.indexOf('share.start'));
    expect(sent.indexOf('share.start')).toBeLessThan(sent.indexOf('pc.offer'));
    const starts = shares.requests<ShareStart>('share.start');
    expect(starts).toHaveLength(2);
    expect(starts[1]).toEqual({
      kind: 'window',
      preset: 'auto',
      audio: true,
      ref: expect.stringMatching(/^[0-9a-f]{24}$/) as string,
      replaces: 's_1',
    });
    expect(starts[1]?.ref).not.toBe(starts[0]?.ref);
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    expect(pubOffers(h.server).at(-1)).toMatchObject({
      gen: 1,
      neg: 1,
      tracks: [{ shareId: 's_2' }, { shareId: 's_2' }],
    });
    expect(h.server.messages('pc.close')).toEqual([]);

    // The same share, under its new id: the capture stays, and the page hears of the change.
    expect(h.runtime.session.share).toBe(share);
    expect(share.shareId).toBe('s_2');
    expect(changes).toEqual(['reconnecting s_1', 'reconnecting s_2']);
    expect(store.getState()).toMatchObject({ phase: 'reconnecting', params: { shareId: 's_2' } });
    expect(h.services.ui.getState().sharing).toBe(true);

    await goLive('s_2');
    expect(share.state).toBe('live');
    expect(store.getState().phase).toBe('live');
    expect(src.release).not.toHaveBeenCalled();
    expect(h.toasts()).toEqual([]);
  });

  it('a resumed connection whose share the server lost meanwhile: published again on the same pub PC', async () => {
    const src = await startShare();
    await goLive();
    const share = h.runtime.session.share;
    const pc = lastPc();
    h.server.welcomeDefaults = { roomId: 'lounge' };
    h.server.drop();
    await tick(1000);
    expect(h.runtime.signal.welcome?.resumed).toBe(true);

    // The snapshot after the welcome has no s_1, nor does the next one, which comes before share.start is answered:
    // neither is "the server ended it" (05 §13.1's fallback).
    h.hub.sendState('lounge', { shares: [] });
    h.hub.sendState('lounge', { shares: [] });
    await tick();
    expect(shares.requests<ShareStart>('share.start').at(-1)).toMatchObject({ replaces: 's_1' });
    expect(h.runtime.session.share).toBe(share);
    expect(share?.shareId).toBe('s_2');
    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    expect(pc.transceivers.map((t) => t.stopped)).toEqual([true, true, false, false]);
    expect(pubOffers(h.server).at(-1)).toMatchObject({
      gen: 1,
      neg: 2,
      tracks: [
        { mid: '2', shareId: 's_2' },
        { mid: '3', shareId: 's_2' },
      ],
    });
    expect(store.getState().phase).toBe('reconnecting');

    // The answer, and the server lists the new share as live: the PC never stopped being connected.
    shares.answer();
    await tick();
    h.hub.sendState('lounge', { shares: [own('s_2')] });
    expect(share?.state).toBe('live');
    expect(store.getState().phase).toBe('live');
    expect(src.release).not.toHaveBeenCalled();
    expect(h.server.messages('pc.close')).toEqual([]);
  });

  it('60 s without a ready connection: the share ends, "the server was unreachable", and the capture is released', async () => {
    const src = await startShare();
    await goLive();
    const back = loseServer();
    await tick(CAPTURE_HOLD_MS - 1);
    expect(src.release).not.toHaveBeenCalled();
    expect(h.runtime.session.share?.state).toBe('live');
    await tick(1);
    expect(src.release).toHaveBeenCalledOnce();
    expect(h.runtime.session.share).toBeNull();
    expect(store.getState()).toMatchObject({
      phase: 'failed',
      error: { name: 'ShareEndedError', kind: 'unreachable' },
    });
    expect(shareErrorMessage(store.getState().error)).toBe('Sharing stopped: the server was unreachable');
    expect(h.services.ui.getState().sharing).toBe(false);

    // The server is back, past its grace period: a new connection, which has no share, and none is published.
    h.server.restart();
    back();
    await tick(BackoffMaxMs);
    expect(h.runtime.signal.welcome?.resumed).toBe(false);
    expect(shares.requests<ShareStart>('share.start')).toHaveLength(1);
    expect(shares.requests<ShareStop>('share.stop')).toEqual([]);
    expect(h.server.messages('pc.close')).toEqual([]);
  });

  it('…and a server that kept the connection is told: the share it still has is stopped, its pub PC closed', async () => {
    await startShare();
    await goLive();
    const back = loseServer();
    await tick(CAPTURE_HOLD_MS);
    expect(h.runtime.session.share).toBeNull();
    h.server.welcomeDefaults = { roomId: 'lounge' };
    back();
    await tick(BackoffMaxMs);
    expect(h.runtime.signal.welcome?.resumed).toBe(true);
    expect(shares.requests<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 1 }]);
    h.hub.sendState('lounge', { shares: [own('s_1')] });
    expect(shares.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
  });

  it('a connection back within the hold keeps the share, and the next outage gets a whole hold again', async () => {
    const src = await startShare();
    await goLive();
    h.server.welcomeDefaults = { roomId: 'lounge' };
    let back = loseServer();
    await tick(CAPTURE_HOLD_MS - BackoffMaxMs - 1_000);
    back();
    await tick(BackoffMaxMs);
    expect(h.runtime.signal.state).toBe('ready');

    back = loseServer();
    await tick(CAPTURE_HOLD_MS - 1_000);
    expect(src.release).not.toHaveBeenCalled();
    expect(h.runtime.session.share?.state).toBe('live');
    // Back a second before the hold ends ("Retry now": no wait for the next try).
    back();
    h.runtime.signal.retryNow();
    await tick();
    expect(h.runtime.signal.state).toBe('ready');
    await tick(CAPTURE_HOLD_MS);
    expect(src.release).not.toHaveBeenCalled();
    expect(store.getState().phase).toBe('live');
  });

  it('a share.start whose answer the socket took goes again with the same ref after the resume, and the share starts', async () => {
    h.server.welcomeDefaults = { roomId: 'lounge' };
    let calls = 0;
    h.server.handle('share.start', () => {
      // The server makes the share, but the socket drops before its answer gets out.
      if (calls++ === 0) h.server.drop();
      return { ok: shareParams('s_1') };
    });
    const started = track(h.runtime.session.startShare(fakePick('window', true), { preset: 'auto', withAudio: true }));
    await tick(1000);
    expect(h.runtime.signal.welcome?.resumed).toBe(true);
    expect(started.done).toBe(true);
    expect(started.error).toBeUndefined();
    const starts = shares.requests<ShareStart>('share.start');
    expect(starts).toHaveLength(2);
    expect(starts[1]).toEqual(starts[0]);
    expect(h.runtime.session.share?.shareId).toBe('s_1');
    expect(pubOffers(h.server)).toHaveLength(1);
    expect(store.getState().phase).toBe('starting');
  });

  it('…but on a new connection the start fails with connection_lost, and nothing is sent again', async () => {
    h.server.handle('share.start', () => {
      h.server.restart();
      h.server.drop();
      return { ok: shareParams('s_1') };
    });
    const started = track(h.runtime.session.startShare(fakePick('window', true), { preset: 'auto', withAudio: true }));
    await tick(1000);
    expect(h.runtime.signal.welcome?.resumed).toBe(false);
    expect(started.error).toMatchObject({ code: 'connection_lost' });
    expect(shares.requests<ShareStart>('share.start')).toHaveLength(1);
    expect(pubOffers(h.server)).toEqual([]);
    expect(h.runtime.session.share).toBeNull();
  });

  it('a re-publish whose answer the socket took goes again with the same ref once a welcome keeps the connection', async () => {
    await startShare();
    await goLive();
    const share = h.runtime.session.share;
    let calls = 0;
    h.server.handle('share.start', () => {
      // The re-publish's first try: the server makes s_2, and the socket drops before the answer.
      if (calls++ === 0) h.server.drop();
      return { ok: shareParams('s_2') };
    });
    h.server.restart();
    h.server.drop();
    await tick(600);
    expect(h.server.welcomes.at(-1)?.resumed).toBe(false);
    expect(shares.requests<ShareStart>('share.start')).toHaveLength(2);
    expect(share?.shareId).toBe('s_1');

    // The next welcome keeps the connection: its snapshot lists s_2, which the page doesn't know by that id.
    h.server.welcomeDefaults = { roomId: 'lounge' };
    await tick(BackoffMaxMs);
    expect(h.runtime.signal.welcome?.resumed).toBe(true);
    h.hub.sendState('lounge', { shares: [own('s_2', { status: 'starting', replaces: 's_1' })] });
    await tick();
    const starts = shares.requests<ShareStart>('share.start');
    expect(starts).toHaveLength(3);
    expect(starts[1]).toMatchObject({ replaces: 's_1' });
    expect(starts[2]).toEqual(starts[1]);
    expect(share?.shareId).toBe('s_2');
    expect(pubOffers(h.server).at(-1)).toMatchObject({
      gen: 1,
      neg: 1,
      tracks: [{ shareId: 's_2' }, { shareId: 's_2' }],
    });
  });

  it('a capture that ended while the server was away is not published again: the share ends quietly', async () => {
    const src = await startShare();
    await goLive();
    const back = loseServer();
    // A stopped track fires no `ended`: the page finds out when it would publish the share again.
    src.stream.getVideoTracks()[0]?.stop();
    h.server.restart();
    back();
    await tick(BackoffMaxMs);
    expect(h.runtime.signal.welcome?.resumed).toBe(false);
    expect(shares.requests<ShareStart>('share.start')).toHaveLength(1);
    expect(h.runtime.session.share).toBeNull();
    expect(store.getState().phase).toBe('idle');
    expect(src.release).toHaveBeenCalledOnce();
    expect(pubOffers(h.server)).toHaveLength(1);
  });

  it('a re-publish that the server refuses ends the share with its error', async () => {
    const src = await startShare();
    await goLive();
    h.server.handle('share.start', () => ({ error: makeError('share_limit', 'request') }));
    h.server.restart();
    h.server.drop();
    await tick(1000);
    expect(h.runtime.session.share).toBeNull();
    expect(store.getState()).toMatchObject({ phase: 'failed', error: { code: 'share_limit' } });
    expect(src.release).toHaveBeenCalledOnce();
    expect(shares.requests<ShareStop>('share.stop')).toEqual([]);
    expect(pubOffers(h.server)).toHaveLength(1);
  });
});
