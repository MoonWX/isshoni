// Fullscreen (05 §12.5): the stage alone, as large as the screen. Toggled with F, a double click or double tap on
// the stage, or the stage's button. Which way it goes is the platform's answer (PlatformCapabilities.fullscreen,
// 05 §8), never a guess from the user agent:
//
// | Mode         | How                                                                                           |
// |--------------|-----------------------------------------------------------------------------------------------|
// | `element`    | requestFullscreen() on the viewer's layout, the stage's container, so everything on the stage |
// |              | stays visible: the state, the bar with its controls, TapToStart, the watchers, the banner     |
// | `video-only` | iPhone: video.webkitEnterFullscreen(), the native player with only the picture; the sound     |
// |              | keeps coming from the page's <audio> element                                                  |
// | `none`       | a CSS pseudo-fullscreen: the layout covers the page (ViewerLayout.module.css)                 |
//
// A way that doesn't work when it is asked for (the browser rejects the request, the stage has no video yet) falls
// back to the pseudo-fullscreen, so F always does something.
//
// viewerStore.fullscreen is what the app reads: ViewerLayout renders only the stage, and the layer policy turns
// every other share's video off (rule 5: IntersectionObserver can't see that the tiles are covered). The store
// follows the browser (`fullscreenchange`, the iPhone's `webkitbeginfullscreen` and `webkitendfullscreen`), so Esc
// and the native player's own "Done" are seen; and the browser follows the store: when the page leaves its room
// (viewerStore.reset()) the fullscreen ends. No React here (05 §3).
import type { Logger } from '../lib/log';
import type { PlatformCapabilities } from '../platform/types';
import type { ViewerStore } from './viewerStore';

export type FullscreenMode = PlatformCapabilities['fullscreen'];

/** What goes fullscreen. Read when it is needed: the stage's video changes with the focused share. */
export interface FullscreenTarget {
  /** The viewer's layout: the stage's container. */
  readonly container: HTMLElement | null;
  /** The stage's <video>, for the iPhone's native player; null on an empty stage. */
  readonly video: HTMLVideoElement | null;
}

export interface FullscreenDeps {
  store: ViewerStore;
  mode: FullscreenMode;
  target: () => FullscreenTarget;
  /**
   * The iPhone's native player closed. iOS pauses the video with it, so the caller plays the tiles' videos again
   * (VideoPlayback.resume()). Called at the event and once more NATIVE_EXIT_RETRY_MS later: the pause can come
   * after the event, and playing what already plays does nothing.
   */
  onNativeExit?: () => void;
  /** Default: the global document. */
  doc?: Document;
  log?: Logger;
}

export interface FullscreenController {
  /**
   * Starts following the browser and the store. Returns the function that stops it, which also leaves a
   * fullscreen that is on. Nothing happens before attach(): making the controller starts nothing.
   */
  attach(): () => void;
  /** Call it inside the user gesture: browsers refuse a fullscreen request that no key press or click asked for. */
  enter(): void;
  exit(): void;
  toggle(): void;
  /** The stage's video changed or went away: a native player that showed the old one is gone with it. */
  refresh(): void;
}

/** The vendor-prefixed Fullscreen API of Safari before 16.4, next to the standard one. */
interface FullscreenDocumentLike {
  readonly fullscreenElement?: Element | null;
  readonly webkitFullscreenElement?: Element | null;
  exitFullscreen?: () => Promise<void> | undefined;
  webkitExitFullscreen?: () => Promise<void> | undefined;
}

interface FullscreenElementLike {
  requestFullscreen?: () => Promise<void> | undefined;
  webkitRequestFullscreen?: () => Promise<void> | undefined;
}

/** How long after the native player closed its video is played once more. */
export const NATIVE_EXIT_RETRY_MS = 400;

function fullscreenElementOf(doc: Document): Element | null {
  const d = doc as unknown as FullscreenDocumentLike;
  return d.fullscreenElement ?? d.webkitFullscreenElement ?? null;
}

export function createFullscreen(deps: FullscreenDeps): FullscreenController {
  const { store, mode, target, log } = deps;
  const doc = deps.doc ?? document;
  /** How the fullscreen that is on was entered; null while there is none. */
  let how: 'element' | 'video' | 'pseudo' | null = null;
  /** The element that is fullscreen in `element` mode, and the video in the native player. */
  let element: Element | null = null;
  let nativeVideo: HTMLVideoElement | null = null;
  /** An element request the browser has not answered yet. */
  let pending = false;
  let attached = false;
  let retry: ReturnType<typeof setTimeout> | undefined;

  const setStore = (fullscreen: boolean): void => {
    store.getState().setFullscreen(fullscreen);
  };

  const pseudo = (): void => {
    how = 'pseudo';
    setStore(true);
  };

  /** Asks the browser for element fullscreen. False when it has no such call: the caller falls back. */
  const requestElement = (container: HTMLElement): boolean => {
    const el = container as unknown as FullscreenElementLike;
    const request = el.requestFullscreen ?? el.webkitRequestFullscreen;
    if (typeof request !== 'function') return false;
    let asked: Promise<void> | undefined;
    try {
      asked = request.call(container);
    } catch (err) {
      log?.debug('requestFullscreen threw', { error: err });
      return false;
    }
    pending = true;
    // The prefixed call returns nothing: its outcome is the change or the error event alone.
    Promise.resolve(asked).catch((err: unknown) => {
      log?.debug('requestFullscreen was refused', { error: err });
      onRefused();
    });
    return true;
  };

  /** The browser said no (a rejected promise, the error event, or both): the pseudo-fullscreen instead, once. */
  const onRefused = (): void => {
    if (!pending) return;
    pending = false;
    if (attached && how === null && !store.getState().fullscreen) pseudo();
  };

  /** Opens the iPhone's native player. False when this video can't: no metadata yet, or no such call. */
  const requestVideo = (video: HTMLVideoElement | null): boolean => {
    if (!video || typeof video.webkitEnterFullscreen !== 'function' || video.webkitSupportsFullscreen === false) {
      return false;
    }
    try {
      video.webkitEnterFullscreen();
    } catch (err) {
      log?.debug('webkitEnterFullscreen threw', { error: err });
      return false;
    }
    return true;
  };

  /** Ends the browser's side of the fullscreen that is on. The store is the caller's business. */
  const leave = (): void => {
    const was = how;
    how = null;
    if (was === 'element') {
      const el = element;
      element = null;
      // Only the fullscreen this controller asked for: never one that something else on the page holds.
      if (el === null || fullscreenElementOf(doc) !== el) return;
      const d = doc as unknown as FullscreenDocumentLike;
      const exit = d.exitFullscreen ?? d.webkitExitFullscreen;
      if (typeof exit !== 'function') return;
      try {
        Promise.resolve(exit.call(doc)).catch((err: unknown) => {
          log?.debug('exitFullscreen was refused', { error: err });
        });
      } catch (err) {
        log?.debug('exitFullscreen threw', { error: err });
      }
    } else if (was === 'video') {
      // nativeVideo stays until its end event, which restarts the video that iOS pauses.
      const video = nativeVideo;
      try {
        video?.webkitExitFullscreen?.();
      } catch (err) {
        log?.debug('webkitExitFullscreen threw', { error: err });
      }
    }
  };

  const onFullscreenChange = (): void => {
    pending = false;
    const el = fullscreenElementOf(doc);
    if (el !== null && el === target().container) {
      how = 'element';
      element = el;
      setStore(true);
    } else if (how === 'element') {
      // Esc, the browser's own exit, or the element left the page.
      how = null;
      element = null;
      setStore(false);
    }
  };

  const isStageVideo = (e: Event): e is Event & { target: HTMLVideoElement } => {
    const { container, video } = target();
    return e.target instanceof HTMLVideoElement && (e.target === video || container?.contains(e.target) === true);
  };

  const onNativeBegin = (e: Event): void => {
    if (!isStageVideo(e)) return;
    how = 'video';
    nativeVideo = e.target;
    setStore(true);
  };

  const onNativeEnd = (e: Event): void => {
    if (e.target !== nativeVideo && !isStageVideo(e)) return;
    nativeVideo = null;
    if (how === 'video') {
      // The player's own "Done". After exit() the store is off already, and only the video is left to restart.
      how = null;
      setStore(false);
    }
    const resume = deps.onNativeExit;
    if (!resume) return;
    resume();
    clearTimeout(retry);
    retry = setTimeout(resume, NATIVE_EXIT_RETRY_MS);
  };

  const enter = (): void => {
    if (!attached || store.getState().fullscreen) return;
    const { container, video } = target();
    if (mode === 'element' && container !== null && requestElement(container)) return;
    if (mode === 'video-only' && requestVideo(video)) return;
    pseudo();
  };

  const exit = (): void => {
    // The store's subscription below ends the browser's side.
    setStore(false);
  };

  return {
    attach() {
      if (attached) return () => undefined;
      attached = true;
      doc.addEventListener('fullscreenchange', onFullscreenChange);
      doc.addEventListener('webkitfullscreenchange', onFullscreenChange);
      doc.addEventListener('fullscreenerror', onRefused);
      doc.addEventListener('webkitfullscreenerror', onRefused);
      // The iPhone's events are fired at the <video> and don't bubble: caught on the way down.
      doc.addEventListener('webkitbeginfullscreen', onNativeBegin, true);
      doc.addEventListener('webkitendfullscreen', onNativeEnd, true);
      const offStore = store.subscribe((s, prev) => {
        if (prev.fullscreen && !s.fullscreen) leave();
      });
      return () => {
        if (!attached) return;
        attached = false;
        doc.removeEventListener('fullscreenchange', onFullscreenChange);
        doc.removeEventListener('webkitfullscreenchange', onFullscreenChange);
        doc.removeEventListener('fullscreenerror', onRefused);
        doc.removeEventListener('webkitfullscreenerror', onRefused);
        doc.removeEventListener('webkitbeginfullscreen', onNativeBegin, true);
        doc.removeEventListener('webkitendfullscreen', onNativeEnd, true);
        clearTimeout(retry);
        pending = false;
        // The layout is going away (the user left the room page): nothing is fullscreen without it.
        setStore(false);
        offStore();
        nativeVideo = null;
      };
    },
    enter,
    exit,
    toggle() {
      if (store.getState().fullscreen) exit();
      else enter();
    },
    refresh() {
      if (how !== 'video') return;
      if (nativeVideo !== null && nativeVideo === target().video && nativeVideo.webkitDisplayingFullscreen !== false) {
        return;
      }
      how = null;
      nativeVideo = null;
      setStore(false);
    },
  };
}

/** A second tap counts as a double tap within this long after the first … */
export const DOUBLE_TAP_MS = 300;
/** … and this close to it (CSS px). A finger that moved further than this between down and up was no tap. */
export const DOUBLE_TAP_PX = 40;
/** A dblclick this soon after a touch is the browser's echo of a double tap that was already counted. */
const TOUCH_ECHO_MS = 700;

/** The stage's own controls: a double press on one of them is two presses of it. */
const CONTROLS = 'button, a, input, select, textarea, label, [role="dialog"], [role="toolbar"]';

/**
 * Calls fn for a double click, and for a double tap, anywhere on el that is not one of its controls (05 §12.5).
 * Double taps are counted here: with `touch-action: manipulation`, which the stage sets so that a double tap
 * doesn't zoom the page, not every browser sends dblclick for touch. It is called inside the second press, so fn
 * may ask for fullscreen. Returns the function that stops it.
 */
export function onDoublePress(
  el: HTMLElement,
  fn: () => void,
  now: () => number = () => performance.now(),
): () => void {
  let down: { x: number; y: number } | null = null;
  let lastTap: { at: number; x: number; y: number } | null = null;
  let lastTouchAt = -Infinity;

  const onControl = (e: Event): boolean => e.target instanceof Element && e.target.closest(CONTROLS) !== null;
  const near = (a: { x: number; y: number }, b: { x: number; y: number }): boolean =>
    Math.hypot(a.x - b.x, a.y - b.y) <= DOUBLE_TAP_PX;

  const onPointerDown = (e: PointerEvent): void => {
    if (e.pointerType !== 'touch') return;
    // A second finger: a pinch, not a tap.
    down = e.isPrimary ? { x: e.clientX, y: e.clientY } : null;
    if (!e.isPrimary) lastTap = null;
  };

  const onPointerUp = (e: PointerEvent): void => {
    if (e.pointerType !== 'touch') return;
    const at = now();
    lastTouchAt = at;
    const up = { x: e.clientX, y: e.clientY };
    const tapped = down !== null && e.isPrimary && near(down, up) && !onControl(e);
    down = null;
    if (!tapped) {
      lastTap = null;
      return;
    }
    if (lastTap !== null && at - lastTap.at <= DOUBLE_TAP_MS && near(lastTap, up)) {
      lastTap = null;
      fn();
      return;
    }
    lastTap = { at, ...up };
  };

  const onPointerCancel = (): void => {
    down = null;
    lastTap = null;
  };

  const onDblClick = (e: MouseEvent): void => {
    if (now() - lastTouchAt < TOUCH_ECHO_MS || onControl(e)) return;
    fn();
  };

  el.addEventListener('pointerdown', onPointerDown);
  el.addEventListener('pointerup', onPointerUp);
  el.addEventListener('pointercancel', onPointerCancel);
  el.addEventListener('dblclick', onDblClick);
  return () => {
    el.removeEventListener('pointerdown', onPointerDown);
    el.removeEventListener('pointerup', onPointerUp);
    el.removeEventListener('pointercancel', onPointerCancel);
    el.removeEventListener('dblclick', onDblClick);
  };
}
