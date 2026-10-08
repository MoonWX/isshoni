// The stats' side of the room runtime (05 §10.7): the collector samples the viewer's sub PC while the session is in
// a room, and window.__isshoni exists in a tab with the debug flag, with a fresh sample per tile and the page's
// state. The viewer's freeze watch reads the same samples (05 §12.6). The runtime's own media is used (no fakes for
// the sub PC), so this also checks that the viewer knows the session's sub PC.
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEBUG_FLAG_KEY } from '../lib/stats/debugHandle';
import { FakeSignalServer } from '../protocol/testing';
import type { PCOffer } from '../protocol/types.gen';
import { buildSdp, FakeRTCPeerConnection } from '../test/FakeRTCPeerConnection';
import { createTestServices } from '../test/render';
import type { RoomDebugState } from './connectStats';
import { createRoomRuntime, type RoomRuntime } from './runtime';
import { FakeHub, participant, shareInfo, tick } from './testing/harness';
import { PRELOAD_TIMEOUT_MS, preloadLazyChunks } from './testing/page';

/** The signed-in user of the fake server's welcome. */
const ME = 'k3m9p2qxw7ht';
const people = [participant(ME, 'alex'), participant('u_bo', 'bo')];
const boShare = shareInfo('s_bo', 'u_bo', 'c_bo');

/** A sub offer with one video m-section that carries s_bo. */
const offer: PCOffer = {
  pc: 'sub',
  gen: 1,
  neg: 1,
  sdp: buildSdp({
    ufrag: 'srv1',
    sections: [{ kind: 'video', mid: '0', direction: 'sendonly', msid: { stream: 's_bo', track: 'v-s_bo' } }],
  }),
  tracks: [{ mid: '0', shareId: 's_bo', kind: 'video' }],
};

/** What getStats() reports for the sub PC after `seconds` of watching s_bo. */
function report(seconds: number): Map<string, Record<string, unknown>> {
  const stats: Record<string, unknown>[] = [
    {
      id: 'IV0',
      type: 'inbound-rtp',
      kind: 'video',
      mid: '0',
      timestamp: seconds * 1000,
      bytesReceived: seconds * 1_000_000,
      packetsLost: 0,
      frameWidth: 1920,
      frameHeight: 1080,
      framesPerSecond: 60,
      framesDecoded: seconds * 60,
    },
  ];
  return new Map(stats.map((s) => [s['id'] as string, s]));
}

let server: FakeSignalServer;
let hub: FakeHub;
let runtime: RoomRuntime;

/** A runtime on the fake server, in the lounge; `debug` sets the tab's flag before the runtime is made. */
async function start(debug: boolean): Promise<void> {
  const services = createTestServices();
  if (debug) services.platform.storage.session.set(DEBUG_FLAG_KEY, '1');
  runtime = createRoomRuntime(services);
  const joined = runtime.session.join('lounge');
  runtime.start();
  await tick();
  await joined;
}

/** bo shares, and the server's sub PC is negotiated and connected. */
async function watching(): Promise<FakeRTCPeerConnection> {
  hub.sendState('lounge', { participants: people, shares: [boShare] });
  server.send('pc.offer', offer);
  await tick();
  const pc = FakeRTCPeerConnection.last;
  if (!pc) throw new Error('no RTCPeerConnection was created');
  pc.setConnectionState('connected');
  return pc;
}

// The media chunk (the collector, the sub PC), loaded before the clock is faked.
beforeAll(preloadLazyChunks, PRELOAD_TIMEOUT_MS);

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5); // no backoff jitter
  server = FakeSignalServer.install();
  hub = new FakeHub(server);
});

afterEach(() => {
  runtime.dispose();
  server.uninstall();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('window.__isshoni (05 §10.7)', () => {
  it('does not exist without the tab’s debug flag', async () => {
    await start(false);
    expect(window.__isshoni).toBeUndefined();
  });

  it('exists with the flag, and goes with the runtime', async () => {
    await start(true);
    expect(window.__isshoni).toBeDefined();
    runtime.dispose();
    expect(window.__isshoni).toBeUndefined();
  });

  it('state() is the connection, the room and the viewer: ids and states, no names', async () => {
    await start(true);
    hub.sendState('lounge', { participants: people, shares: [boShare] });
    const state = window.__isshoni?.state() as RoomDebugState;
    expect(state.connection).toEqual({
      state: 'ready',
      resumed: false,
      connectionId: runtime.stores.room.getState().connectionId,
    });
    expect(state.connection.connectionId).not.toBeNull();
    expect(state.room).toEqual({
      roomId: 'lounge',
      joinState: 'joined',
      rev: runtime.stores.room.getState().state?.rev,
      participants: 2,
    });
    expect(state.room.rev).not.toBeNull();
    expect(state.viewer).toMatchObject({ focusedShareId: 's_bo', audibleShareId: 's_bo', subGen: 0 });
    expect(JSON.stringify(state)).not.toMatch(/alex|"bo"/);
  });

  it('stats() is a fresh sample with the tile of each share the sub PC receives', async () => {
    await start(true);
    const pc = await watching();
    pc.stats = report(10);
    const sample = await window.__isshoni?.stats();
    // The viewer made the session's sub PC, so the collector finds it and maps the stream to its share.
    expect(runtime.viewer.subscriber?.gen).toBe(1);
    expect(sample?.pcs).toMatchObject([{ pc: 'sub', gen: 1, state: 'connected' }]);
    expect(sample?.shares['s_bo']?.video).toMatchObject({ frameWidth: 1920, frameHeight: 1080, framesDecoded: 600 });
  });

  it('dropSocket() is not there yet', async () => {
    await start(true);
    expect(() => window.__isshoni?.dropSocket()).toThrow(/S81/);
  });
});

describe('the stats collector follows the room (05 §10.7)', () => {
  it('samples the sub PC every 2 s while the session is in a room, and stops when it leaves', async () => {
    await start(false);
    const pc = await watching();
    const getStats = vi.spyOn(pc, 'getStats');
    await tick(2_000);
    expect(getStats).toHaveBeenCalledTimes(1);
    await tick(4_000);
    expect(getStats).toHaveBeenCalledTimes(3);

    await runtime.session.leave();
    await tick(10_000);
    expect(getStats).toHaveBeenCalledTimes(3);
  });

  it('starts again with the next room', async () => {
    await start(false);
    await runtime.session.leave();
    await tick();
    const joined = runtime.session.join('lounge');
    await tick();
    await joined;
    const pc = await watching();
    const getStats = vi.spyOn(pc, 'getStats');
    await tick(2_000);
    expect(getStats).toHaveBeenCalled();
  });
});

describe('the tiles’ freeze watch reads the collector (05 §12.6)', () => {
  it('a shown share whose frame count stands still for 3 s is frozen, until frames arrive again', async () => {
    await start(false);
    const pc = await watching();
    const viewer = runtime.viewer.store;
    viewer.getState().setVisible('s_bo', true); // the stage shows it, as the mounted layout says
    pc.stats = report(10);
    await tick(2_000);
    expect(viewer.getState().frozen).toEqual({});
    await tick(4_000);
    expect(viewer.getState().frozen).toEqual({ s_bo: true });

    pc.stats = report(20);
    await tick(2_000);
    expect(viewer.getState().frozen).toEqual({});
  });

  it('a share that nothing shows is never frozen', async () => {
    await start(false);
    const pc = await watching();
    pc.stats = report(10);
    await tick(10_000);
    expect(runtime.viewer.store.getState().frozen).toEqual({});
  });

  it('ends with the runtime', async () => {
    await start(false);
    const pc = await watching();
    const viewer = runtime.viewer.store;
    viewer.getState().setVisible('s_bo', true);
    pc.stats = report(10);
    await tick(6_000);
    expect(viewer.getState().frozen).toEqual({ s_bo: true });

    runtime.dispose();
    expect(viewer.getState().frozen).toEqual({});
  });
});
