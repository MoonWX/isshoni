// Picture-in-picture (05 §12.5): the stage's video in the browser's floating window, on desktops. Whether there is
// a button at all is the platform's answer (PlatformCapabilities.pip, 05 §8): false where the browser has no PiP,
// and on iOS in M1, where it is unreliable (plan). The controller checks again that the video has the call.
//
// viewerStore.pipShareId is what the app reads: the layer policy keeps the PiP share on the high layer, also while
// the page is hidden (rule 2: that is when the window is watched). The store follows the browser
// (`enterpictureinpicture`, `leavepictureinpicture`: the window's own close button), and the browser follows the
// store: when the share ends or the page leaves its room, the window closes.
//
// The window shows one <video> element, the stage's. When the stage moves to another share that element goes, and
// the window with it: the caller says so with refresh(). No React here (05 §3).
import type { Logger } from '../lib/log';
import type { ViewerStore } from './viewerStore';

/** The stage's video and the share it shows. Read when it is needed. */
export interface PipTarget {
  readonly video: HTMLVideoElement | null;
  readonly shareId: string | null;
}

export interface PipDeps {
  store: ViewerStore;
  /** PlatformCapabilities.pip. False: enter() does nothing. */
  supported: boolean;
  target: () => PipTarget;
  /** Default: the global document. */
  doc?: Document;
  log?: Logger;
}

export interface PipController {
  /**
   * Starts following the browser and the store. Returns the function that stops it, which also closes a window
   * that is open. Nothing happens before attach().
   */
  attach(): () => void;
  /** Call it inside the user gesture. */
  enter(): void;
  exit(): void;
  toggle(): void;
  /** The stage's video changed or went away: a window that showed the old one closes. */
  refresh(): void;
}

/** The PiP calls as browsers without them (and jsdom) have them: not at all. */
interface PipVideoLike {
  requestPictureInPicture?: () => Promise<unknown>;
}

interface PipDocumentLike {
  readonly pictureInPictureElement?: Element | null;
  exitPictureInPicture?: () => Promise<void>;
}

export function createPip(deps: PipDeps): PipController {
  const { store, supported, target, log } = deps;
  const doc = deps.doc ?? document;
  /** The video in the window; null while there is none. */
  let element: Element | null = null;
  let attached = false;

  const setStore = (shareId: string | null): void => {
    store.getState().setPip(shareId);
  };

  /** Closes the browser's window when it shows this controller's video. The store is the caller's business. */
  const leave = (): void => {
    const el = element;
    element = null;
    const d = doc as unknown as PipDocumentLike;
    if (el === null || d.pictureInPictureElement !== el || typeof d.exitPictureInPicture !== 'function') return;
    d.exitPictureInPicture().catch((err: unknown) => {
      log?.debug('exitPictureInPicture was refused', { error: err });
    });
  };

  const onEnter = (e: Event): void => {
    const { video, shareId } = target();
    if (video === null || shareId === null || e.target !== video) return;
    element = video;
    setStore(shareId);
  };

  const onLeave = (e: Event): void => {
    if (element === null || e.target !== element) return;
    // The window's own close button, or the browser closed it.
    element = null;
    setStore(null);
  };

  const enter = (): void => {
    if (!attached || !supported || store.getState().pipShareId !== null) return;
    const { video, shareId } = target();
    const request = (video as PipVideoLike | null)?.requestPictureInPicture;
    if (video === null || shareId === null || typeof request !== 'function') return;
    // The store follows the enter event; a refusal (no metadata yet, no gesture) changes nothing.
    request.call(video).catch((err: unknown) => {
      log?.debug('requestPictureInPicture was refused', { error: err });
    });
  };

  const exit = (): void => {
    // The store's subscription below closes the window.
    setStore(null);
  };

  return {
    attach() {
      if (attached) return () => undefined;
      attached = true;
      // Caught on the way down: whether these events bubble differs between browsers.
      doc.addEventListener('enterpictureinpicture', onEnter, true);
      doc.addEventListener('leavepictureinpicture', onLeave, true);
      const offStore = store.subscribe((s, prev) => {
        if (prev.pipShareId !== null && s.pipShareId === null) leave();
      });
      return () => {
        if (!attached) return;
        attached = false;
        doc.removeEventListener('enterpictureinpicture', onEnter, true);
        doc.removeEventListener('leavepictureinpicture', onLeave, true);
        // The stage is going away (the user left the room page), and its video with it.
        setStore(null);
        offStore();
      };
    },
    enter,
    exit,
    toggle() {
      if (store.getState().pipShareId !== null) exit();
      else enter();
    },
    refresh() {
      if (element !== null && element !== target().video) setStore(null);
    },
  };
}
