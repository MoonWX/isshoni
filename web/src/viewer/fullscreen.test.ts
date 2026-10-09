// Fullscreen (05 §12.5): each of the platform's three ways (element, the iPhone's native player, the CSS
// pseudo-fullscreen), the fallbacks between them, the store following the browser and the browser following the
// store; and the double click or double tap that toggles it.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  createFullscreen,
  DOUBLE_TAP_MS,
  DOUBLE_TAP_PX,
  NATIVE_EXIT_RETRY_MS,
  onDoublePress,
  type FullscreenController,
  type FullscreenMode,
} from './fullscreen';
import { createViewerStore, type ViewerStore } from './viewerStore';

let store: ViewerStore;
let container: HTMLDivElement;
let video: HTMLVideoElement;
let detach: (() => void) | undefined;

/** The Fullscreen API that jsdom doesn't have, as a browser with (or without) the standard names has it. */
interface FakeFullscreenApi {
  /** The element that is fullscreen now. */
  element: Element | null;
  request: ReturnType<typeof vi.fn<() => Promise<void> | undefined>>;
  exit: ReturnType<typeof vi.fn<() => Promise<void> | undefined>>;
  /** The browser changed it by itself: Esc (null), or another element. */
  set(el: Element | null): void;
}

let cleanups: (() => void)[] = [];

function installFullscreenApi(prefix: '' | 'webkit' = ''): FakeFullscreenApi {
  const names =
    prefix === ''
      ? { el: 'fullscreenElement', request: 'requestFullscreen', exit: 'exitFullscreen', change: 'fullscreenchange' }
      : {
          el: 'webkitFullscreenElement',
          request: 'webkitRequestFullscreen',
          exit: 'webkitExitFullscreen',
          change: 'webkitfullscreenchange',
        };
  const api: FakeFullscreenApi = {
    element: null,
    request: vi.fn(),
    exit: vi.fn(),
    set(el) {
      api.element = el;
      document.dispatchEvent(new Event(names.change));
    },
  };
  // The standard calls answer with a promise, the prefixed ones with nothing.
  api.request.mockImplementation(function (this: Element) {
    api.set(this);
    return prefix === '' ? Promise.resolve() : undefined;
  });
  api.exit.mockImplementation(() => {
    api.set(null);
    return prefix === '' ? Promise.resolve() : undefined;
  });
  Object.defineProperty(document, names.el, { configurable: true, get: () => api.element });
  Object.defineProperty(document, names.exit, { configurable: true, value: api.exit });
  Object.defineProperty(HTMLElement.prototype, names.request, { configurable: true, value: api.request });
  cleanups.push(() => {
    Reflect.deleteProperty(document, names.el);
    Reflect.deleteProperty(document, names.exit);
    Reflect.deleteProperty(HTMLElement.prototype, names.request);
  });
  return api;
}

/** The iPhone's native player on a <video>: the calls, and the events the browser fires at the element. */
function nativePlayer(el: HTMLVideoElement, supported = true) {
  const state = { displaying: false };
  const enter = vi.fn(() => {
    state.displaying = true;
    el.dispatchEvent(new Event('webkitbeginfullscreen'));
  });
  const exit = vi.fn(() => {
    state.displaying = false;
    el.dispatchEvent(new Event('webkitendfullscreen'));
  });
  Object.defineProperties(el, {
    webkitEnterFullscreen: { configurable: true, value: enter },
    webkitExitFullscreen: { configurable: true, value: exit },
    webkitSupportsFullscreen: { configurable: true, get: () => supported },
    webkitDisplayingFullscreen: { configurable: true, get: () => state.displaying },
  });
  return {
    enter,
    exit,
    /** The player's own "Done". */
    done() {
      state.displaying = false;
      el.dispatchEvent(new Event('webkitendfullscreen'));
    },
  };
}

function controller(mode: FullscreenMode, over: { onNativeExit?: () => void } = {}): FullscreenController {
  const fullscreen = createFullscreen({
    store,
    mode,
    target: () => ({
      container: container.isConnected ? container : null,
      video: container.querySelector('video'),
    }),
    ...over,
  });
  detach = fullscreen.attach();
  return fullscreen;
}

const on = (): boolean => store.getState().fullscreen;

beforeEach(() => {
  store = createViewerStore();
  container = document.createElement('div');
  video = document.createElement('video');
  container.append(video);
  document.body.append(container);
});

afterEach(() => {
  detach?.();
  detach = undefined;
  for (const cleanup of cleanups) cleanup();
  cleanups = [];
  document.body.replaceChildren();
  vi.useRealTimers();
});

describe('createFullscreen: the CSS pseudo-fullscreen (mode none)', () => {
  it('is only the store’s flag, which ViewerLayout turns into the covering layout', () => {
    const fullscreen = controller('none');
    expect(on()).toBe(false);
    fullscreen.enter();
    expect(on()).toBe(true);
    fullscreen.enter(); // already on
    expect(on()).toBe(true);
    fullscreen.exit();
    expect(on()).toBe(false);
  });

  it('toggles', () => {
    const fullscreen = controller('none');
    fullscreen.toggle();
    expect(on()).toBe(true);
    fullscreen.toggle();
    expect(on()).toBe(false);
  });

  it('never asks the browser, even where it could', () => {
    const api = installFullscreenApi();
    const player = nativePlayer(video);
    controller('none').enter();
    expect(on()).toBe(true);
    expect(api.request).not.toHaveBeenCalled();
    expect(player.enter).not.toHaveBeenCalled();
  });

  it('starts nothing before attach(), and ends with the detach', () => {
    const fullscreen = createFullscreen({ store, mode: 'none', target: () => ({ container, video }) });
    fullscreen.enter();
    expect(on()).toBe(false);

    const off = fullscreen.attach();
    fullscreen.enter();
    expect(on()).toBe(true);
    off(); // the layout is gone
    expect(on()).toBe(false);
    fullscreen.enter();
    expect(on()).toBe(false);
    off(); // twice is once
  });
});

describe('createFullscreen: element fullscreen', () => {
  it('asks for the layout, the stage’s container, and follows the browser’s answer', () => {
    const api = installFullscreenApi();
    const fullscreen = controller('element');
    fullscreen.enter();
    expect(api.request).toHaveBeenCalledOnce();
    expect(api.request.mock.contexts[0]).toBe(container);
    expect(on()).toBe(true);

    fullscreen.exit();
    expect(api.exit).toHaveBeenCalledOnce();
    expect(on()).toBe(false);
  });

  it('waits for the browser before it says so', () => {
    const api = installFullscreenApi();
    api.request.mockImplementation(() => new Promise<void>(() => undefined));
    const fullscreen = controller('element');
    fullscreen.enter();
    expect(on()).toBe(false);
    api.set(container);
    expect(on()).toBe(true);
  });

  it('sees Esc: the browser leaves fullscreen by itself', () => {
    const api = installFullscreenApi();
    const fullscreen = controller('element');
    fullscreen.enter();
    api.set(null);
    expect(on()).toBe(false);
    expect(api.exit).not.toHaveBeenCalled();
    // And again.
    fullscreen.toggle();
    expect(on()).toBe(true);
  });

  it('works with Safari’s prefixed calls, which answer with events only', () => {
    const api = installFullscreenApi('webkit');
    const fullscreen = controller('element');
    fullscreen.enter();
    expect(api.request.mock.contexts[0]).toBe(container);
    expect(on()).toBe(true);
    fullscreen.exit();
    expect(api.exit).toHaveBeenCalledOnce();
    expect(on()).toBe(false);
  });

  it('falls back to the pseudo-fullscreen when the browser refuses', async () => {
    const api = installFullscreenApi();
    api.request.mockImplementation(() => Promise.reject(new TypeError('fullscreen error')));
    const fullscreen = controller('element');
    fullscreen.enter();
    expect(on()).toBe(false);
    await Promise.resolve();
    await Promise.resolve();
    expect(on()).toBe(true);
    // Both the rejection and the error event say no: one fallback.
    document.dispatchEvent(new Event('fullscreenerror'));
    fullscreen.exit();
    expect(on()).toBe(false);
    expect(api.exit).not.toHaveBeenCalled();
  });

  it('falls back on the error event of a call that returns nothing', () => {
    const api = installFullscreenApi('webkit');
    api.request.mockImplementation(() => undefined);
    const fullscreen = controller('element');
    fullscreen.enter();
    expect(on()).toBe(false);
    document.dispatchEvent(new Event('webkitfullscreenerror'));
    expect(on()).toBe(true);
  });

  it('falls back when the call throws, and where the element has no such call', () => {
    const api = installFullscreenApi();
    api.request.mockImplementation(() => {
      throw new TypeError('not allowed');
    });
    const first = controller('element');
    first.enter();
    expect(on()).toBe(true);
    detach?.();
    for (const cleanup of cleanups.splice(0)) cleanup();

    // jsdom as it is: no Fullscreen API at all.
    controller('element').enter();
    expect(on()).toBe(true);
  });

  it('leaves the browser’s fullscreen when the store is reset (the page left its room)', () => {
    const api = installFullscreenApi();
    controller('element').enter();
    store.getState().reset();
    expect(api.exit).toHaveBeenCalledOnce();
    expect(api.element).toBeNull();
  });

  it('leaves it when the layout goes away', () => {
    const api = installFullscreenApi();
    controller('element').enter();
    detach?.();
    expect(api.exit).toHaveBeenCalledOnce();
    expect(on()).toBe(false);
  });

  it('never ends a fullscreen that is not its own', () => {
    const api = installFullscreenApi();
    const other = document.createElement('div');
    document.body.append(other);
    const fullscreen = controller('element');
    api.set(other);
    expect(on()).toBe(false);
    fullscreen.exit();
    detach?.();
    expect(api.exit).not.toHaveBeenCalled();
    expect(api.element).toBe(other);
  });
});

describe('createFullscreen: the iPhone’s native player (mode video-only)', () => {
  it('opens the stage’s video in the native player and follows its events', () => {
    const player = nativePlayer(video);
    const fullscreen = controller('video-only');
    fullscreen.enter();
    expect(player.enter).toHaveBeenCalledOnce();
    expect(on()).toBe(true);

    fullscreen.exit();
    expect(player.exit).toHaveBeenCalledOnce();
    expect(on()).toBe(false);
  });

  it('sees the player’s own "Done", and plays the videos again: iOS pauses them with the player', () => {
    vi.useFakeTimers();
    const onNativeExit = vi.fn();
    const player = nativePlayer(video);
    controller('video-only', { onNativeExit }).enter();
    player.done();
    expect(on()).toBe(false);
    expect(onNativeExit).toHaveBeenCalledTimes(1);
    // Once more a moment later: the pause can come after the event.
    vi.advanceTimersByTime(NATIVE_EXIT_RETRY_MS);
    expect(onNativeExit).toHaveBeenCalledTimes(2);
  });

  it('plays the videos again after its own exit too', () => {
    vi.useFakeTimers();
    const onNativeExit = vi.fn();
    nativePlayer(video);
    const fullscreen = controller('video-only', { onNativeExit });
    fullscreen.enter();
    fullscreen.exit();
    expect(onNativeExit).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(NATIVE_EXIT_RETRY_MS);
    expect(onNativeExit).toHaveBeenCalledTimes(2);
  });

  it('stops the second try with the detach', () => {
    vi.useFakeTimers();
    const onNativeExit = vi.fn();
    const player = nativePlayer(video);
    controller('video-only', { onNativeExit }).enter();
    player.done();
    detach?.();
    vi.advanceTimersByTime(NATIVE_EXIT_RETRY_MS);
    expect(onNativeExit).toHaveBeenCalledTimes(1);
  });

  it('falls back to the pseudo-fullscreen for a video that can’t: no metadata yet, no video, a call that throws', () => {
    const notYet = nativePlayer(video, false);
    const fullscreen = controller('video-only');
    fullscreen.enter();
    expect(notYet.enter).not.toHaveBeenCalled();
    expect(on()).toBe(true);
    fullscreen.exit();

    video.remove(); // an empty stage
    fullscreen.enter();
    expect(on()).toBe(true);
    fullscreen.exit();

    const broken = document.createElement('video');
    container.append(broken);
    nativePlayer(broken).enter.mockImplementation(() => {
      throw new DOMException('no metadata', 'InvalidStateError');
    });
    fullscreen.enter();
    expect(on()).toBe(true);
  });

  it('ends when the stage moves to another share: the old video is gone, and the player with it', () => {
    const player = nativePlayer(video);
    const fullscreen = controller('video-only');
    fullscreen.enter();
    fullscreen.refresh(); // the same video still plays
    expect(on()).toBe(true);

    const next = document.createElement('video');
    video.replaceWith(next);
    fullscreen.refresh();
    expect(on()).toBe(false);
    expect(player.exit).not.toHaveBeenCalled();
  });

  it('ignores the native player of a video that is not the stage’s', () => {
    const elsewhere = document.createElement('video');
    document.body.append(elsewhere);
    controller('video-only');
    nativePlayer(elsewhere).enter();
    expect(on()).toBe(false);
  });
});

describe('onDoublePress', () => {
  let now = 0;
  let stage: HTMLElement;
  let fn: ReturnType<typeof vi.fn<() => void>>;
  let off: () => void;

  /** A touch pointer event at a point of the stage; jsdom's events get the pointer's fields added. */
  const touch = (
    type: 'pointerdown' | 'pointerup' | 'pointercancel',
    x = 100,
    y = 100,
    over: { pointerType?: string; isPrimary?: boolean } = {},
  ): void => {
    const e = new MouseEvent(type, { bubbles: true, clientX: x, clientY: y });
    Object.defineProperties(e, {
      pointerType: { value: over.pointerType ?? 'touch' },
      isPrimary: { value: over.isPrimary ?? true },
    });
    stage.dispatchEvent(e);
  };
  const tap = (x = 100, y = 100): void => {
    touch('pointerdown', x, y);
    touch('pointerup', x, y);
  };

  beforeEach(() => {
    now = 1_000;
    stage = document.createElement('section');
    stage.innerHTML = '<video></video><div role="toolbar"><button id="mute">Mute</button></div>';
    document.body.append(stage);
    fn = vi.fn();
    off = onDoublePress(stage, fn, () => now);
  });

  afterEach(() => {
    off();
  });

  it('calls for a double click on the stage, and not on one of its controls', () => {
    stage.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    expect(fn).toHaveBeenCalledTimes(1);
    stage.querySelector('button')?.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('calls for two taps close together in time and place', () => {
    tap();
    expect(fn).not.toHaveBeenCalled();
    now += DOUBLE_TAP_MS;
    tap(100 + DOUBLE_TAP_PX, 100);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('counts a third tap as the first of a new pair', () => {
    tap();
    now += 100;
    tap();
    now += 100;
    tap();
    expect(fn).toHaveBeenCalledTimes(1);
    now += 100;
    tap();
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('ignores the dblclick that a browser adds to a double tap', () => {
    tap();
    now += 150;
    tap();
    now += 10;
    stage.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    expect(fn).toHaveBeenCalledTimes(1);
    // A real double click later counts again.
    now += 5_000;
    stage.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('doesn’t call for taps that are too slow or too far apart', () => {
    tap();
    now += DOUBLE_TAP_MS + 1;
    tap();
    expect(fn).not.toHaveBeenCalled();
    now += 50;
    tap(100 + DOUBLE_TAP_PX + 1, 100);
    expect(fn).not.toHaveBeenCalled();
  });

  it('doesn’t take a swipe, a pinch or a cancelled touch for a tap', () => {
    tap();
    now += 50;
    touch('pointerdown', 100, 100);
    touch('pointerup', 100, 300); // the finger moved: a scroll
    now += 50;
    tap();
    expect(fn).not.toHaveBeenCalled();

    now += 1_000;
    tap();
    now += 50;
    touch('pointerdown', 100, 100);
    touch('pointerdown', 200, 100, { isPrimary: false }); // a second finger
    touch('pointerup', 100, 100);
    expect(fn).not.toHaveBeenCalled();

    now += 1_000;
    tap();
    now += 50;
    touch('pointerdown');
    touch('pointercancel');
    touch('pointerup');
    expect(fn).not.toHaveBeenCalled();
  });

  it('leaves taps on the stage’s controls to them', () => {
    const button = stage.querySelector('button');
    const onButton = (type: string): void => {
      const e = new MouseEvent(type, { bubbles: true, clientX: 100, clientY: 100 });
      Object.defineProperties(e, { pointerType: { value: 'touch' }, isPrimary: { value: true } });
      button?.dispatchEvent(e);
    };
    for (let i = 0; i < 2; i++) {
      onButton('pointerdown');
      onButton('pointerup');
      now += 100;
    }
    expect(fn).not.toHaveBeenCalled();
  });

  it('leaves the mouse’s pointer events to dblclick, and stops when asked', () => {
    for (let i = 0; i < 2; i++) {
      touch('pointerdown', 100, 100, { pointerType: 'mouse' });
      touch('pointerup', 100, 100, { pointerType: 'mouse' });
    }
    expect(fn).not.toHaveBeenCalled();
    off();
    stage.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    tap();
    tap();
    expect(fn).not.toHaveBeenCalled();
  });
});
