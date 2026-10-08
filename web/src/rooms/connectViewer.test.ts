// The viewer's side of the room runtime (05 §10–§12): the runtime makes viewer/'s store and registry once and
// keeps them in step with the session: room.state → tiles and focus, subscribe.status, the "bo started sharing
// [Watch]" toast (owner decision, 05 §24.1), and by default viewer/'s SubscriberPC as the session's sub PC.
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { buildSdp, FakeRTCPeerConnection } from '../test/FakeRTCPeerConnection';
import { FakeSignalServer } from '../protocol/testing';
import type { PCOffer, SubscriptionStatus } from '../protocol/types.gen';
import { createTestServices } from '../test/render';
import { createRoomRuntime, getRoomRuntime, type RoomRuntime } from './runtime';
import { createHarness, FakeHub, participant, shareInfo, tick, type Harness } from './testing/harness';
import { PRELOAD_TIMEOUT_MS, preloadLazyChunks } from './testing/page';

/** The signed-in user of the fake server's welcome. */
const ME = 'k3m9p2qxw7ht';
const people = [participant(ME, 'alex'), participant('u_bo', 'bo'), participant('u_cy', 'cy')];
const boShare = shareInfo('s_bo', 'u_bo', 'c_bo', { startedAt: '2026-10-12T19:02:30.000Z' });
const cyShare = shareInfo('s_cy', 'u_cy', 'c_cy', { startedAt: '2026-10-12T19:05:00.000Z' });

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5); // no backoff jitter
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('the viewer follows the session', () => {
  let h: Harness;
  const viewer = () => h.runtime.viewer.store.getState();
  const shareIds = () => viewer().shares.map((s) => s.id);

  beforeEach(async () => {
    h = createHarness();
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
  });

  afterEach(() => {
    h.close();
  });

  it('is made once per runtime, with the volume the device remembers', () => {
    expect(getRoomRuntime(h.services).viewer).toBe(h.runtime.viewer);
    expect(viewer().volume).toBe(1);

    const services = createTestServices();
    services.prefs.getState().setVolume(0.4);
    const other = createRoomRuntime(services);
    expect(other.viewer).not.toBe(h.runtime.viewer);
    expect(other.viewer.store.getState().volume).toBe(0.4);
    other.dispose();
  });

  it('gets every room.state: tiles, whose share is this page’s, and the newest share on the stage', () => {
    const own = h.runtime.stores.room.getState().connectionId ?? '';
    const mine = shareInfo('s_mine', ME, own, { startedAt: '2026-10-12T19:06:00.000Z' });
    const laptop = shareInfo('s_laptop', ME, 'c_laptop', { startedAt: '2026-10-12T19:01:00.000Z' });
    h.hub.sendState('lounge', { participants: people, shares: [laptop, boShare, mine] });

    expect(viewer().shares.map((s) => [s.id, s.ownerName, s.own, s.local])).toEqual([
      ['s_mine', 'alex', true, true],
      ['s_bo', 'bo', false, false],
      // The same user from another device: theirs, but watched like anyone's.
      ['s_laptop', 'alex', true, false],
    ]);
    // Auto-focus never takes a share of this user (05 §12.2).
    expect(viewer()).toMatchObject({ focusedShareId: 's_bo', focusMode: 'auto', audibleShareId: 's_bo' });

    h.hub.sendState('lounge', { participants: people, shares: [laptop, boShare, mine, cyShare] });
    expect(viewer().focusedShareId).toBe('s_cy');
    h.hub.sendState('lounge', { participants: people, shares: [boShare] });
    expect(shareIds()).toEqual(['s_bo']);
    expect(viewer().focusedShareId).toBe('s_bo');
  });

  it('drops the tracks of shares that are gone from room.state', () => {
    const track = (id: string) => ({ id }) as unknown as MediaStreamTrack;
    h.hub.sendState('lounge', { participants: people, shares: [boShare, cyShare] });
    h.runtime.viewer.registry.set('s_bo', 'video', track('v-bo'));
    h.runtime.viewer.registry.set('s_cy', 'video', track('v-cy'));
    h.hub.sendState('lounge', { participants: people, shares: [cyShare] });
    expect(h.runtime.viewer.registry.shareIds()).toEqual(['s_cy']);
  });

  it('goes back to its empty state when the user leaves the room', async () => {
    h.hub.sendState('lounge', { participants: people, shares: [boShare, cyShare] });
    viewer().focusShare('s_bo');
    h.runtime.viewer.registry.set('s_bo', 'video', {} as MediaStreamTrack);
    expect(viewer()).toMatchObject({ focusedShareId: 's_bo', focusMode: 'manual' });

    await h.runtime.session.leave();
    expect(viewer()).toMatchObject({
      shares: [],
      focusedShareId: null,
      focusMode: 'auto',
      audibleShareId: null,
      ended: [],
    });
    expect(h.runtime.viewer.registry.shareIds()).toEqual([]);
  });

  it('starts empty in another room: a pick in the room before holds nothing there', async () => {
    h.hub.sendState('lounge', { participants: people, shares: [boShare, cyShare] });
    viewer().focusShare('s_bo');

    h.hub.shares = [shareInfo('s_dee', 'u_dee', 'c_dee')];
    const joined = h.runtime.session.join('games');
    // At once, before the server answered: nothing of the Lounge is left to show under the new room's name.
    expect(viewer()).toMatchObject({ shares: [], focusMode: 'auto', ended: [] });
    await tick();
    await joined;
    expect(shareIds()).toEqual(['s_dee']);
    expect(viewer()).toMatchObject({ focusedShareId: 's_dee', focusMode: 'auto' });
  });

  it('keeps the tiles through a reconnect, and a re-published share takes over the pick (01 §10.6)', async () => {
    h.hub.sendState('lounge', { participants: people, shares: [boShare, cyShare] });
    viewer().focusShare('s_bo');

    // The server restarted: a new connection, and the first room.state lists no shares.
    h.server.resume = false;
    h.hub.shares = [];
    h.server.drop();
    expect(shareIds()).toEqual(['s_cy', 's_bo']);
    await tick(1_000);
    expect(h.server.welcomes.at(-1)?.resumed).toBe(false);
    expect(h.runtime.stores.room.getState().joinState).toBe('joined');
    expect(shareIds()).toEqual([]);

    // bo's page publishes its share again, under a new id that names the old one.
    h.hub.sendState('lounge', {
      participants: people,
      shares: [shareInfo('s_bo2', 'u_bo', 'c_bo2', { replaces: 's_bo' }), shareInfo('s_cy2', 'u_cy', 'c_cy2')],
    });
    expect(viewer()).toMatchObject({ focusedShareId: 's_bo2', focusMode: 'manual' });
  });

  it('gives the store the server’s subscribe.status', () => {
    h.hub.sendState('lounge', { participants: people, shares: [boShare] });
    const status: SubscriptionStatus = {
      shareId: 's_bo',
      requestedVideo: 'high',
      video: 'low',
      audio: 'on',
      reason: 'bandwidth',
    };
    h.server.send('subscribe.status', { subs: [status] });
    expect(viewer().status).toEqual({ s_bo: status });
  });

  describe('a share that starts (owner decision, 05 §24.1)', () => {
    const started = (userId: string, name: string, shareId: string) =>
      ({ roomId: 'lounge', kind: 'share.started', userId, name, shareId, at: '2026-10-12T19:05:00.000Z' }) as const;
    const toasts = () =>
      h.services.ui.getState().toasts.map((t) => ({ message: t.message, action: t.action?.label ?? null }));

    it('takes the stage while nothing is picked, and the room only says so', () => {
      h.hub.sendState('lounge', { participants: people, shares: [boShare] });
      h.server.send('room.event', started('u_cy', 'cy', 's_cy'));
      h.hub.sendState('lounge', { participants: people, shares: [boShare, cyShare] });
      expect(viewer().focusedShareId).toBe('s_cy');
      expect(toasts()).toEqual([{ message: 'cy started sharing', action: null }]);
    });

    it('after a pick, only shows a toast, whose Watch picks the new share', () => {
      h.hub.sendState('lounge', { participants: people, shares: [boShare] });
      viewer().focusShare('s_bo');
      h.server.send('room.event', started('u_cy', 'cy', 's_cy'));
      h.hub.sendState('lounge', { participants: people, shares: [boShare, cyShare] });

      // The pick holds: neither the stage nor the sound moved.
      expect(viewer()).toMatchObject({ focusedShareId: 's_bo', audibleShareId: 's_bo', focusMode: 'manual' });
      // One toast, the viewer's: the session's plain announcement is not shown as well.
      expect(toasts()).toEqual([{ message: 'cy started sharing', action: 'Watch' }]);

      h.services.ui.getState().toasts[0]?.action?.run();
      expect(viewer()).toMatchObject({ focusedShareId: 's_cy', focusMode: 'manual' });
    });

    it('never announces this user’s own share', () => {
      h.hub.sendState('lounge', { participants: people, shares: [boShare] });
      viewer().focusShare('s_bo');
      h.server.send('room.event', started(ME, 'alex', 's_laptop'));
      expect(toasts()).toEqual([]);
    });
  });

  it('dispose() disconnects the viewer', () => {
    h.hub.sendState('lounge', { participants: people, shares: [boShare] });
    h.runtime.dispose();
    h.runtime.stores.room.setState({ state: null });
    expect(shareIds()).toEqual(['s_bo']);
  });
});

describe('the session’s sub PC (05 §10.1)', () => {
  let server: FakeSignalServer;
  let runtime: RoomRuntime;

  /** A sub offer with one video m-section that carries s_bo. */
  const offer = (gen: number, neg: number): PCOffer => ({
    pc: 'sub',
    gen,
    neg,
    sdp: buildSdp({
      ufrag: 'srv1',
      sections: [{ kind: 'video', mid: '0', direction: 'sendonly', msid: { stream: 's_bo', track: 'v-s_bo' } }],
    }),
    tracks: [{ mid: '0', shareId: 's_bo', kind: 'video' }],
  });

  // The media chunk, loaded before the clock is faked.
  beforeAll(preloadLazyChunks, PRELOAD_TIMEOUT_MS);

  beforeEach(async () => {
    server = FakeSignalServer.install();
    new FakeHub(server);
    // No media override: the runtime's own.
    runtime = createRoomRuntime(createTestServices());
    const joined = runtime.session.join('lounge');
    runtime.start();
    await tick();
    await joined;
  });

  afterEach(() => {
    runtime.dispose();
    server.uninstall();
  });

  it('is viewer/’s SubscriberPC, made with the first sub offer, on the page’s registry and store', async () => {
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
    expect(runtime.viewer.store.getState().media).toBe('idle');

    server.send('pc.offer', offer(1, 1));
    await tick();
    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    const pc = FakeRTCPeerConnection.last;
    expect(server.messages('pc.answer').map((m) => m.data)).toEqual([
      { pc: 'sub', gen: 1, neg: 1, sdp: pc?.localDescription?.sdp },
    ]);
    // The PC's state is in viewerStore, and its tracks are in the registry the tiles read.
    expect(runtime.viewer.store.getState().media).toBe('connecting');
    expect(runtime.viewer.registry.shareIds()).toEqual(['s_bo']);
    expect(runtime.viewer.registry.get('s_bo').video).toBeDefined();
  });

  it('gets the candidates that came right behind the offer, after it', async () => {
    server.send('pc.offer', offer(1, 1));
    server.send('pc.ice', {
      pc: 'sub',
      gen: 1,
      candidate: { candidate: 'candidate:1 1 udp 1 192.0.2.1 7882 typ host' },
    });
    await tick();
    expect(FakeRTCPeerConnection.last?.remoteCandidates).toHaveLength(1);
  });

  it('is closed when the session leaves the room, and the next room starts a new one at gen 1', async () => {
    server.send('pc.offer', offer(1, 1));
    await tick();
    const first = FakeRTCPeerConnection.last;
    await runtime.session.leave();
    expect(first?.connectionState).toBe('closed');
    expect(runtime.viewer.store.getState().media).toBe('idle');

    const joined = runtime.session.join('lounge');
    await tick();
    await joined;
    server.send('pc.offer', offer(1, 1));
    await tick();
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    expect(FakeRTCPeerConnection.last?.connectionState).not.toBe('closed');
  });
});
