// What the layout makes the server send (05 §12.4), end to end in the page: the layout with its tiles, the
// intersection observer and the page's visibility feed the layer policy, and the session's real SubscriptionSync
// turns its answers into subscribe.update messages. The focused share gets the high layer, thumbnails the low one,
// and a tile out of view, a hidden tab after 10 s and the tiles under a fullscreen stage get no video.
// (layerPolicy.test.ts has the rules themselves; the e2e layers.spec measures the bytes.)
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createLogger } from '../lib/log';
import type { SignalState } from '../protocol/signal-client';
import type { SubscribeResult, SubscriptionWant } from '../protocol/types.gen';
import { SUBSCRIBE_DEBOUNCE_MS, SUBSCRIBE_OFF_DELAY_MS, SubscriptionSync } from '../rooms/subscriptionSync';
import { installFakeMedia, installFakeMediaElement, type FakeMediaElementControl } from '../test/fakeMedia';
import { PAGE_HIDDEN_OFF_MS } from './layerPolicy';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { attachSubscriptions } from './subscriptions';
import { installFakeIntersectionObserver, room, SELF, shareInfo, type FakeIntersections } from './testing';
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
const BEA2 = shareInfo('s_bea2', 'u_bea', 2, { kind: 'tab' });
const CY = shareInfo('s_cy', 'u_cy', 3);

const want = (shareId: string, video: 'high' | 'low' | 'off', audio: 'on' | 'off' = 'off'): SubscriptionWant => ({
  shareId,
  video,
  audio,
});
const sorted = (subs: readonly SubscriptionWant[]) => [...subs].sort((a, b) => a.shareId.localeCompare(b.shareId));

let media: FakeMediaElementControl;
let viewer: ViewerServices;
let signal: FakeSignal;
let sync: SubscriptionSync;
let detach: () => void;
let io: FakeIntersections;
let hidden = false;

beforeEach(() => {
  vi.useFakeTimers();
  installFakeMedia();
  media = installFakeMediaElement();
  io = installFakeIntersectionObserver();
  hidden = false;
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => (hidden ? 'hidden' : 'visible'),
  });
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
  Reflect.deleteProperty(document, 'visibilityState');
  Reflect.deleteProperty(document, 'pictureInPictureElement');
  Reflect.deleteProperty(HTMLVideoElement.prototype, 'requestPictureInPicture');
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const flush = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));
const state = () => viewer.store.getState();
const setHidden = (next: boolean): void => {
  act(() => {
    hidden = next;
    document.dispatchEvent(new Event('visibilitychange'));
  });
};
/** The tile (<li>) of a share. */
const tileOf = (name: RegExp): HTMLElement => {
  const li = screen.getByRole('button', { name }).closest('li');
  if (!li) throw new Error('no tile');
  return li;
};
const scroll = (el: Element, ratio: number): void => {
  act(() => {
    io.show(el, ratio);
  });
};
/** What the server holds after every message so far: the last word per share. */
const held = (): SubscriptionWant[] => {
  const last = new Map<string, SubscriptionWant>();
  for (const sub of signal.updates.flat()) last.set(sub.shareId, sub);
  return sorted([...last.values()]);
};

/** Cy's window on the stage, Bea's tab and Bea's window as tiles, all in view, and the first update sent. */
async function watching(): Promise<void> {
  syncRoom(viewer, room(BEA, BEA2, CY), SELF);
  render(<ViewerLayout viewer={viewer} />);
  await flush(SUBSCRIBE_DEBOUNCE_MS);
}

describe('layers: what is in view', () => {
  it('asks for the focused share in high and for the thumbnails in low', async () => {
    await watching();
    expect(signal.updates).toHaveLength(1);
    expect(held()).toEqual([want('s_bea', 'low'), want('s_bea2', 'low'), want('s_cy', 'high', 'on')]);
  });

  it('turns a tile’s video off a second after it scrolled out of view, and on again at once when it is back', async () => {
    await watching();
    const tile = tileOf(/^Watch Bea's window/);
    scroll(tile, 0);
    expect(state().visible['s_bea']).toBe(false);
    // Not at once: scrolling past a tile must not flap its subscription.
    await flush(SUBSCRIBE_OFF_DELAY_MS - 1);
    expect(signal.updates).toHaveLength(1);
    await flush(1 + SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(2);
    expect(signal.updates[1]).toEqual([want('s_bea', 'off')]);
    expect(held()).toEqual([want('s_bea', 'off'), want('s_bea2', 'low'), want('s_cy', 'high', 'on')]);

    scroll(tile, 0.5);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(3);
    expect(signal.updates[2]).toEqual([want('s_bea', 'low')]);
  });

  it('sends nothing for a tile that is back in view within the second', async () => {
    await watching();
    const tile = tileOf(/^Watch Bea's window/);
    scroll(tile, 0);
    await flush(SUBSCRIBE_OFF_DELAY_MS / 2);
    scroll(tile, 1);
    await flush(SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
  });

  it('counts a sliver as out of view: less than 10% of the tile', async () => {
    await watching();
    scroll(tileOf(/^Watch Bea's window/), 0.05);
    scroll(tileOf(/^Watch Bea's tab/), 0.1);
    await flush(SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    expect(held()).toEqual([want('s_bea', 'off'), want('s_bea2', 'low'), want('s_cy', 'high', 'on')]);
  });

  it('keeps the sound of a share whose tile scrolled away', async () => {
    await watching();
    fireEvent.click(within(tileOf(/^Watch Bea's window/)).getByRole('button', { name: 'Listen to Bea' }));
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    scroll(tileOf(/^Watch Bea's window/), 0);
    await flush(SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    expect(held()).toEqual([want('s_bea', 'off', 'on'), want('s_bea2', 'low'), want('s_cy', 'high', 'off')]);
  });

  it('gives a picked tile the high layer in one update, wherever it was scrolled', async () => {
    await watching();
    scroll(tileOf(/^Watch Bea's window/), 0);
    await flush(SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    signal.updates.length = 0;

    act(() => {
      state().focusShare('s_bea'); // the people panel's "Watch": no tile was touched
    });
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    expect(sorted(signal.updates[0] ?? [])).toEqual([want('s_bea', 'high', 'on'), want('s_cy', 'low')]);
  });
});

describe('layers: a hidden tab', () => {
  it('turns every video off after 10 s and keeps the sound; back on when the tab is visible again', async () => {
    await watching();
    setHidden(true);
    await flush(PAGE_HIDDEN_OFF_MS - 1);
    expect(signal.updates).toHaveLength(1);
    // The 10 s, then the second that a video going off waits (01 §11.4's example).
    await flush(1 + SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(2);
    expect(held()).toEqual([want('s_bea', 'off'), want('s_bea2', 'off'), want('s_cy', 'off', 'on')]);

    setHidden(false);
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(3);
    expect(held()).toEqual([want('s_bea', 'low'), want('s_bea2', 'low'), want('s_cy', 'high', 'on')]);
  });

  it('sends nothing for a tab that is back within the 10 s', async () => {
    await watching();
    setHidden(true);
    await flush(PAGE_HIDDEN_OFF_MS - 1_000);
    setHidden(false);
    await flush(PAGE_HIDDEN_OFF_MS + SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
  });

  it('keeps the picture-in-picture share on the high layer while the tab is hidden', async () => {
    Object.defineProperty(document, 'pictureInPictureElement', { configurable: true, get: () => null });
    await watching();
    act(() => {
      state().setPip('s_cy'); // the floating window shows the stage's share
    });
    setHidden(true);
    await flush(PAGE_HIDDEN_OFF_MS + SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    expect(held()).toEqual([want('s_bea', 'off'), want('s_bea2', 'off'), want('s_cy', 'high', 'on')]);
  });
});

describe('layers: fullscreen', () => {
  it('turns the thumbnails off, which the observer can’t see are covered, and on again afterwards', async () => {
    await watching();
    fireEvent.click(within(screen.getByRole('toolbar')).getByRole('button', { name: 'Fullscreen' }));
    expect(state().fullscreen).toBe(true);
    await flush(SUBSCRIBE_OFF_DELAY_MS + SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(2);
    expect(held()).toEqual([want('s_bea', 'off'), want('s_bea2', 'off'), want('s_cy', 'high', 'on')]);

    fireEvent.click(within(screen.getByRole('toolbar')).getByRole('button', { name: 'Exit fullscreen' }));
    await flush(SUBSCRIBE_DEBOUNCE_MS);
    expect(held()).toEqual([want('s_bea', 'low'), want('s_bea2', 'low'), want('s_cy', 'high', 'on')]);
  });
});
