// What the viewer asks the server for (05 §12.3–§12.4), through the session's real SubscriptionSync and a stand-in
// for the SignalClient's request(): audio only for the focused share, and every change of focus or of what is
// heard as ONE subscribe.update with all the shares it touches.
import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createLogger } from '../lib/log';
import type { SignalState } from '../protocol/signal-client';
import type { SubscribeResult, SubscriptionWant } from '../protocol/types.gen';
import { SUBSCRIBE_DEBOUNCE_MS, SUBSCRIBE_OFF_DELAY_MS, SubscriptionSync } from '../rooms/subscriptionSync';
import { installFakeMedia, installFakeMediaElement, type FakeMediaElementControl } from '../test/fakeMedia';
import { PAGE_HIDDEN_OFF_MS } from './layerPolicy';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { attachSubscriptions, layerInputs, viewerWants, type SubscriptionSink } from './subscriptions';
import { room, SELF, shareInfo } from './testing';
import { markShown } from './useVisibility';
import { ViewerLayout } from './ViewerLayout';

/** The SignalClient's request(), recorded: the subs of every subscribe.update, oldest first. */
class FakeSignal {
  state: SignalState = 'ready';
  readonly updates: SubscriptionWant[][] = [];

  request = (type: string, data: { subs: SubscriptionWant[] }): Promise<SubscribeResult> => {
    expect(type).toBe('subscribe.update');
    this.updates.push(data.subs);
    return Promise.resolve({ ignored: [] });
  };
}

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2);
const DEE = shareInfo('s_dee', 'u_dee', 3);
const MINE = shareInfo('s_mine', 'u_alex', 4, { connectionId: 'c_me' });

const want = (shareId: string, video: 'high' | 'low' | 'off', audio: 'on' | 'off' = 'off'): SubscriptionWant => ({
  shareId,
  video,
  audio,
});
/** Order doesn't matter on the wire. */
const sorted = (subs: readonly SubscriptionWant[]) => [...subs].sort((a, b) => a.shareId.localeCompare(b.shareId));

let media: FakeMediaElementControl;
let viewer: ViewerServices;
let signal: FakeSignal;
let sync: SubscriptionSync;
let detach: () => void;

beforeEach(() => {
  vi.useFakeTimers();
  installFakeMedia();
  media = installFakeMediaElement();
  viewer = createViewer();
  signal = new FakeSignal();
  sync = new SubscriptionSync({ signal, log: createLogger('test') });
  detach = attachSubscriptions(viewer, sync);
});

afterEach(() => {
  detach();
  sync.dispose();
  viewer.dispose();
  media.restore();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const sync_ = (...shares: Parameters<typeof room>): void => {
  act(() => {
    syncRoom(viewer, room(...shares), SELF);
  });
};
const flush = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));
const state = () => viewer.store.getState();

describe('layerInputs', () => {
  it('reads the store: remote shares, the stage, the sound, what is shown, the page and the modes', () => {
    syncRoom(viewer, room(BEA, CY, MINE), SELF);
    markShown(viewer.store, 's_cy');
    markShown(viewer.store, 's_bea');
    state().setFullscreen(true);
    state().setPip('s_bea');
    state().setPageHidden(1_000);
    expect(layerInputs(state(), 4_500)).toEqual({
      remoteShares: ['s_cy', 's_bea'], // tile order, without the share this page publishes
      focused: 's_cy',
      audible: 's_cy',
      visible: { s_cy: true, s_bea: true },
      pageHiddenForMs: 3_500,
      fullscreen: true,
      pip: 's_bea',
    });
    expect(layerInputs({ ...state(), pageHiddenSince: null }, 4_500).pageHiddenForMs).toBe(0);
  });

  it('counts the stage’s share as focused only while the stage shows it', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    // The room page is not mounted (the user is on /account): nothing is shown, the sound goes on (05 §11.1).
    expect(layerInputs(state(), 0).focused).toBeNull();
    expect(viewerWants(state(), 0)).toEqual([want('s_cy', 'off', 'on'), want('s_bea', 'off')]);
    const hide = markShown(viewer.store, 's_cy');
    expect(layerInputs(state(), 0).focused).toBe('s_cy');
    expect(viewerWants(state(), 0)).toEqual([want('s_cy', 'high', 'on'), want('s_bea', 'off')]);
    hide();
    expect(layerInputs(state(), 0).focused).toBeNull();
  });
});

describe('attachSubscriptions: audio only for the focused share', () => {
  it('asks for the newest share in high with audio and for the other tiles in low without', async () => {
    render(<ViewerLayout viewer={viewer} />);
    sync_(BEA, CY, DEE);
    expect(state().focusedShareId).toBe('s_dee');
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    expect(sorted(signal.updates[0] ?? [])).toEqual([
      want('s_bea', 'low'),
      want('s_cy', 'low'),
      want('s_dee', 'high', 'on'),
    ]);
    expect(signal.updates[0]?.filter((w) => w.audio === 'on')).toEqual([want('s_dee', 'high', 'on')]);
  });

  it('never subscribes to a share this page publishes', async () => {
    render(<ViewerLayout viewer={viewer} />);
    sync_(BEA, MINE);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toEqual([[want('s_bea', 'high', 'on')]]);

    // Enlarging the own preview moves neither the sound nor any subscription's audio.
    fireEvent.click(screen.getByRole('button', { name: /^Show your preview on the stage/ }));
    expect(state().focusedShareId).toBe('s_mine');
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates.at(-1)).toEqual([want('s_bea', 'low', 'on')]);
    expect(signal.updates.flat().some((w) => w.shareId === 's_mine')).toBe(false);
  });

  it('subscribes a share that auto-focus puts on the stage, with the audio moving in the same update', async () => {
    render(<ViewerLayout viewer={viewer} />);
    sync_(BEA);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toEqual([[want('s_bea', 'high', 'on')]]);

    sync_(BEA, CY);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(2);
    expect(sorted(signal.updates[1] ?? [])).toEqual([want('s_bea', 'low'), want('s_cy', 'high', 'on')]);
  });

  it('holds a pick: a share that starts afterwards gets a low tile and no audio', async () => {
    render(<ViewerLayout viewer={viewer} />);
    sync_(BEA, CY);
    fireEvent.click(screen.getByRole('button', { name: /^Watch Bea/ }));
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    signal.updates.length = 0;

    sync_(BEA, CY, DEE);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toEqual([[want('s_dee', 'low')]]);

    // The picked share ends: auto-focus resumes with the newest, in one update (which also turns the ended
    // share off; the server lists it under `ignored`).
    sync_(CY, DEE);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(2);
    expect(sorted(signal.updates[1] ?? [])).toEqual([want('s_bea', 'off'), want('s_dee', 'high', 'on')]);
  });
});

describe('attachSubscriptions: one subscribe.update per change (05 §12.3)', () => {
  it('sends a click on a tile as one message with both shares: the old one down, the new one up', async () => {
    render(<ViewerLayout viewer={viewer} />);
    sync_(BEA, CY);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    signal.updates.length = 0;

    fireEvent.click(screen.getByRole('button', { name: /^Watch Bea/ }));
    expect(state().focusedShareId).toBe('s_bea');
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    expect(sorted(signal.updates[0] ?? [])).toEqual([want('s_bea', 'high', 'on'), want('s_cy', 'low')]);

    // Nothing trails behind it.
    await flush(SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
  });

  it('moves the audio alone with a speaker button: one message, the video layers stay', async () => {
    render(<ViewerLayout viewer={viewer} />);
    sync_(BEA, CY);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    signal.updates.length = 0;

    fireEvent.click(screen.getByRole('button', { name: 'Listen to Bea' }));
    expect(state().focusedShareId).toBe('s_cy');
    expect(state().focusMode).toBe('auto');
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    expect(sorted(signal.updates[0] ?? [])).toEqual([want('s_bea', 'low', 'on'), want('s_cy', 'high', 'off')]);

    // And back to the stage's share.
    fireEvent.click(screen.getByRole('button', { name: 'Listen to Bea' }));
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(2);
    expect(sorted(signal.updates[1] ?? [])).toEqual([want('s_bea', 'low', 'off'), want('s_cy', 'high', 'on')]);
  });

  it('merges quick changes into one message', async () => {
    render(<ViewerLayout viewer={viewer} />);
    sync_(BEA, CY, DEE);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    signal.updates.length = 0;

    fireEvent.click(screen.getByRole('button', { name: /^Watch Bea/ }));
    await flush(SUBSCRIBE_DEBOUNCE_MS - 50);
    fireEvent.click(screen.getByRole('button', { name: /^Watch Cy/ }));
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    expect(sorted(signal.updates[0] ?? [])).toEqual([want('s_cy', 'high', 'on'), want('s_dee', 'low', 'off')]);
  });

  it('says nothing for changes the policy doesn’t read', () => {
    const set = vi.fn<SubscriptionSink['set']>();
    const off = attachSubscriptions(viewer, { set });
    expect(set).toHaveBeenCalledTimes(1); // the set as it is when attached
    state().setVolume(0.3);
    state().setAudio('blocked');
    state().setMedia('connected');
    state().applyStatus([{ shareId: 's_x', video: 'low', audio: 'off', requestedVideo: 'high' }]);
    expect(set).toHaveBeenCalledTimes(1);
    syncRoom(viewer, room(BEA), SELF);
    expect(set).toHaveBeenLastCalledWith([want('s_bea', 'off', 'on')]);
    off();
    syncRoom(viewer, room(), SELF);
    expect(set).toHaveBeenLastCalledWith([want('s_bea', 'off', 'on')]);
  });
});

describe('attachSubscriptions: what is not shown', () => {
  it('turns every video off when the room page goes away, and the sound stays', async () => {
    const { unmount } = render(<ViewerLayout viewer={viewer} />);
    sync_(BEA, CY);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    signal.updates.length = 0;

    unmount(); // navigation to /account: the session and the viewer live on (05 §11.1)
    // A tile that left the screen waits a second, so scrolling doesn't flap.
    await flush(SUBSCRIBE_OFF_DELAY_MS - 1);
    expect(signal.updates).toEqual([]);
    await flush(1 + SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    expect(sorted(signal.updates[0] ?? [])).toEqual([want('s_bea', 'off'), want('s_cy', 'off', 'on')]);

    // Back on the room page: at once.
    render(<ViewerLayout viewer={viewer} />);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(sorted(signal.updates.at(-1) ?? [])).toEqual([want('s_bea', 'low'), want('s_cy', 'high', 'on')]);
  });

  it('looks again when a hidden page reaches 10 s (rule 3)', async () => {
    const set = vi.fn<SubscriptionSink['set']>();
    let now = 50_000;
    const off = attachSubscriptions(viewer, { set }, { now: () => now });
    syncRoom(viewer, room(BEA), SELF);
    markShown(viewer.store, 's_bea');
    expect(set).toHaveBeenLastCalledWith([want('s_bea', 'high', 'on')]);
    const calls = set.mock.calls.length;

    state().setPageHidden(now);
    expect(set).toHaveBeenLastCalledWith([want('s_bea', 'high', 'on')]);
    now += PAGE_HIDDEN_OFF_MS;
    await vi.advanceTimersByTimeAsync(PAGE_HIDDEN_OFF_MS - 1);
    expect(set).toHaveBeenCalledTimes(calls + 1);
    await vi.advanceTimersByTimeAsync(1);
    expect(set).toHaveBeenCalledTimes(calls + 2);
    expect(set).toHaveBeenLastCalledWith([want('s_bea', 'off', 'on')]);

    // Visible again before the next look: the timer is gone with the detach.
    state().setPageHidden(null);
    expect(set).toHaveBeenLastCalledWith([want('s_bea', 'high', 'on')]);
    state().setPageHidden(now);
    off();
    const after = set.mock.calls.length;
    await vi.advanceTimersByTimeAsync(2 * PAGE_HIDDEN_OFF_MS);
    expect(set).toHaveBeenCalledTimes(after);
  });
});
