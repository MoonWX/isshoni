// The viewer's wiring: syncRoom keeps the store and the registry in line with room.state, attachViewer takes the
// server's subscribe.status (with the real SignalClient on the fake server), and the parts that later slices fill
// say so.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { NotImplementedError } from '../lib/errors';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer } from '../protocol/testing';
import { FakeMediaStreamTrack } from '../test/fakeMedia';
import { createTestPlatform } from '../test/platform';
import { createAudioOut } from './audioOut';
import { desiredSubscriptions } from './layerPolicy';
import { attachViewer, createViewer, syncRoom } from './services';
import { room, SELF, shareInfo, status } from './testing';

const track = (kind: 'audio' | 'video') => new FakeMediaStreamTrack(kind) as unknown as MediaStreamTrack;

describe('syncRoom', () => {
  it('updates the tiles and drops the tracks of shares that left room.state', () => {
    const viewer = createViewer({ volume: 0.3 });
    expect(viewer.store.getState().volume).toBe(0.3);
    viewer.registry.set('s_a', 'video', track('video'));
    viewer.registry.set('s_b', 'video', track('video'));
    viewer.registry.set('s_b', 'audio', track('audio'));

    syncRoom(viewer, room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2)), SELF);
    expect(viewer.store.getState().shares.map((s) => s.id)).toEqual(['s_b', 's_a']);
    expect(viewer.registry.shareIds()).toEqual(['s_a', 's_b']);

    syncRoom(viewer, room(shareInfo('s_a', 'u_bea', 1)), SELF);
    expect(viewer.store.getState().focusedShareId).toBe('s_a');
    expect(viewer.registry.shareIds()).toEqual(['s_a']);

    syncRoom(viewer, null, SELF);
    expect(viewer.store.getState().shares).toEqual([]);
    expect(viewer.registry.shareIds()).toEqual([]);
  });

  it('keeps the tracks of a share that is still `starting` in the snapshot', () => {
    const viewer = createViewer();
    viewer.registry.set('s_a', 'video', track('video'));
    syncRoom(viewer, room(shareInfo('s_a', 'u_bea', 1, { status: 'starting' })), SELF);
    expect(viewer.store.getState().shares).toEqual([]);
    expect(viewer.registry.shareIds()).toEqual(['s_a']);
  });
});

describe('attachViewer', () => {
  let server: FakeSignalServer;
  let signal: SignalClient;

  beforeEach(async () => {
    vi.useFakeTimers();
    server = FakeSignalServer.install();
    const platform = createTestPlatform();
    signal = new SignalClient({
      url: platform.signaling().url,
      client: platform.client,
      role: 'viewer',
      caps: () => platform.capsNow(),
    });
    signal.start();
    await vi.advanceTimersByTimeAsync(0);
  });

  afterEach(() => {
    signal.stop();
    server.uninstall();
    vi.useRealTimers();
  });

  it('puts subscribe.status into the store until it is detached', () => {
    const viewer = createViewer();
    syncRoom(viewer, room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2)), SELF);
    const detach = attachViewer(signal, viewer);

    server.send('subscribe.status', {
      subs: [status('s_a', { video: 'low', reason: 'bandwidth' }), status('s_b', { video: 'off', reason: 'waiting' })],
    });
    expect(viewer.store.getState().status['s_a']?.reason).toBe('bandwidth');
    expect(viewer.store.getState().status['s_b']?.reason).toBe('waiting');
    // Only what changed is sent (01 §8.9): the other entry stays.
    server.send('subscribe.status', { subs: [status('s_a')] });
    expect(viewer.store.getState().status['s_a']?.reason).toBeUndefined();
    expect(viewer.store.getState().status['s_b']?.reason).toBe('waiting');

    detach();
    server.send('subscribe.status', { subs: [status('s_b')] });
    expect(viewer.store.getState().status['s_b']?.reason).toBe('waiting');
  });
});

describe('parts that later slices fill in (interfaces first)', () => {
  it('desiredSubscriptions says it arrives with S56', () => {
    const call = () =>
      desiredSubscriptions({
        remoteShares: ['s_a'],
        focused: 's_a',
        audible: 's_a',
        visible: {},
        pageHiddenForMs: 0,
        fullscreen: false,
        pip: null,
      });
    expect(call).toThrow(NotImplementedError);
    expect(call).toThrow('viewer.desiredSubscriptions is not implemented yet (S56)');
  });

  it('createAudioOut says it arrives with S47', () => {
    const call = () => createAudioOut({ store: createViewer().store });
    expect(call).toThrow(NotImplementedError);
    expect(call).toThrow('viewer.createAudioOut is not implemented yet (S47)');
  });
});
