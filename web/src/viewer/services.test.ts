// The viewer's wiring: syncRoom keeps the store and the registry in line with room.state, attachViewer takes the
// server's subscribe.status (with the real SignalClient on the fake server), the page's one <audio> element
// plays the audible share and only that one (audio follows focus, 05 §12.3), and createSubscriber makes the
// session's sub PC.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createUiStore } from '../app/uiStore';
import { createLogger } from '../lib/log';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer } from '../protocol/testing';
import {
  FakeMediaStream,
  FakeMediaStreamTrack,
  installFakeMedia,
  installFakeMediaElement,
  type FakeMediaElementControl,
} from '../test/fakeMedia';
import { FakeRTCPeerConnection } from '../test/FakeRTCPeerConnection';
import { createTestPlatform } from '../test/platform';
import { attachViewer, createViewer, syncRoom, type ViewerServices } from './services';
import { SubscriberPC } from './SubscriberPC';
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

describe('createViewer: audio follows focus (05 §12.3)', () => {
  let media: FakeMediaElementControl;
  let viewer: ViewerServices;
  const audio: Record<string, MediaStreamTrack> = {};

  /** Lets the promise of a play() settle. */
  const settle = () => Promise.resolve().then(() => Promise.resolve());
  /** The audio tracks the page's one <audio> element plays. */
  const heard = () => (viewer.audio.element?.srcObject as unknown as FakeMediaStream | null)?.getTracks() ?? [];
  /** Gives the shares their tracks, as the sub PC does. */
  const receive = (...shareIds: string[]): void => {
    for (const id of shareIds) {
      const received = track('audio');
      audio[id] = received;
      viewer.registry.set(id, 'video', track('video'));
      viewer.registry.set(id, 'audio', received);
    }
  };
  const BEA = shareInfo('s_bea', 'u_bea', 1);
  const CY = shareInfo('s_cy', 'u_cy', 2);

  beforeEach(() => {
    installFakeMedia();
    media = installFakeMediaElement();
    viewer = createViewer();
  });

  afterEach(() => {
    viewer.dispose();
    media.restore();
    vi.unstubAllGlobals();
  });

  it('plays the audio of the focused share, and only that one', async () => {
    expect(viewer.audio.element).toBeNull();
    syncRoom(viewer, room(BEA, CY), SELF);
    receive('s_bea', 's_cy');
    await settle();
    expect(viewer.store.getState()).toMatchObject({ focusedShareId: 's_cy', audibleShareId: 's_cy' });
    expect(heard()).toEqual([audio['s_cy']]);
    expect(document.querySelectorAll('audio')).toHaveLength(1);
    expect(viewer.store.getState().audio).toBe('playing');
    // The other share's audio track is received by nobody's element: tile videos hold video only (05 §10.2).
    expect(media.played).toEqual([viewer.audio.element]);
  });

  it('moves with the focus as a swap of the element’s stream', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    receive('s_bea', 's_cy');
    await settle();
    const el = viewer.audio.element;

    viewer.store.getState().focusShare('s_bea'); // a click on Bea's tile
    expect(heard()).toEqual([audio['s_bea']]);
    expect(viewer.audio.element).toBe(el);
    expect(viewer.store.getState().audio).toBe('playing');
    await settle();
    expect(el?.paused).toBe(false);

    // The picked share ends: auto-focus, and the sound with it, goes to the newest one left.
    syncRoom(viewer, room(CY), SELF);
    expect(heard()).toEqual([audio['s_cy']]);
  });

  it('follows the speaker button without the focus', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    receive('s_bea', 's_cy');
    viewer.store.getState().toggleAudible('s_bea');
    await settle();
    expect(viewer.store.getState()).toMatchObject({ focusedShareId: 's_cy', audibleShareId: 's_bea' });
    expect(heard()).toEqual([audio['s_bea']]);
    viewer.store.getState().setAudible(null);
    expect(heard()).toEqual([]);
  });

  it('starts when the track arrives after the focus, and takes a replaced track', async () => {
    syncRoom(viewer, room(BEA), SELF);
    expect(viewer.audio.element).toBeNull(); // focused, nothing received yet
    receive('s_bea');
    await settle();
    expect(heard()).toEqual([audio['s_bea']]);

    // The sub PC was rebuilt: a new track for the same share.
    const next = track('audio');
    viewer.registry.set('s_bea', 'audio', next);
    expect(heard()).toEqual([next]);
    viewer.registry.delete('s_bea', 'audio');
    expect(heard()).toEqual([]);
  });

  it('goes silent when the audible share ends, and keeps the element for the next one', async () => {
    syncRoom(viewer, room(BEA), SELF);
    receive('s_bea');
    await settle();
    const el = viewer.audio.element;
    syncRoom(viewer, room(), SELF);
    expect(heard()).toEqual([]);
    expect(el?.isConnected).toBe(true);

    // The unlock survives leaving the room and joining another (05 §10.2).
    viewer.store.getState().reset();
    syncRoom(viewer, room(CY), SELF);
    receive('s_cy');
    await settle();
    expect(viewer.audio.element).toBe(el);
    expect(heard()).toEqual([audio['s_cy']]);
    expect(viewer.store.getState().audio).toBe('playing');
  });

  it('never plays a share this page publishes', async () => {
    syncRoom(viewer, room(shareInfo('s_mine', 'u_alex', 1, { connectionId: 'c_me' })), SELF);
    receive('s_mine'); // there is none in practice
    viewer.store.getState().focusShare('s_mine');
    viewer.store.getState().toggleAudible('s_mine');
    await settle();
    expect(viewer.store.getState().audibleShareId).toBeNull();
    expect(viewer.audio.element).toBeNull();
  });

  it('gives the element the volume, and the preferences each change', async () => {
    const onVolume = vi.fn();
    viewer.dispose();
    viewer = createViewer({ volume: 0.4, onVolume });
    syncRoom(viewer, room(BEA), SELF);
    receive('s_bea');
    await settle();
    expect(viewer.audio.element?.volume).toBe(0.4);
    expect(onVolume).not.toHaveBeenCalled();

    viewer.store.getState().setVolume(0.8);
    expect(viewer.audio.element?.volume).toBe(0.8);
    expect(onVolume).toHaveBeenCalledExactlyOnceWith(0.8);
  });

  it('unlock() starts the audio and every refused video inside the caller’s gesture', async () => {
    media.policy = 'block';
    syncRoom(viewer, room(BEA), SELF);
    receive('s_bea');
    const video = document.createElement('video');
    viewer.videos.play(video);
    await settle();
    expect(viewer.store.getState()).toMatchObject({ audio: 'blocked', videoBlocked: true });

    media.policy = 'allow';
    const before = media.played.length;
    viewer.unlock();
    expect(media.played.slice(before)).toEqual([viewer.audio.element, video]);
    await settle();
    expect(viewer.store.getState()).toMatchObject({ audio: 'playing', videoBlocked: false, focusMode: 'auto' });
  });

  it('dispose() stops the following and removes the element', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    receive('s_bea', 's_cy');
    await settle();
    viewer.dispose();
    expect(document.querySelectorAll('audio')).toHaveLength(0);
    viewer.store.getState().focusShare('s_bea');
    viewer.registry.set('s_bea', 'audio', track('audio'));
    expect(viewer.audio.element).toBeNull();
  });
});

describe('viewer.createSubscriber (05 §10.1)', () => {
  it('makes the session’s sub PC with the viewer’s registry and store, and remembers the newest', () => {
    const viewer = createViewer();
    expect(viewer.subscriber).toBeNull();
    const platform = createTestPlatform();
    const signal = new SignalClient({
      url: platform.signaling().url,
      client: platform.client,
      role: 'viewer',
      caps: () => platform.capsNow(),
    });
    const deps = { platform, signal, log: createLogger('sub-pc'), ui: createUiStore() };

    const first = viewer.createSubscriber(deps, SubscriberPC);
    expect(first).toBeInstanceOf(SubscriberPC);
    expect(viewer.subscriber).toBe(first);
    expect(first.gen).toBe(0);
    expect(first.connectionState).toBe('closed');
    expect(first.trackOf('0')).toBeUndefined();
    expect(FakeRTCPeerConnection.instances).toHaveLength(0); // the PC is made on the first sub offer

    // After a welcome that was not resumed the session makes a new one.
    const second = viewer.createSubscriber(deps, SubscriberPC);
    expect(viewer.subscriber).toBe(second);
    first.close();
    second.close();
  });
});
