// audioOut (05 §10.2–§10.3): all audio plays through ONE <audio> element, made once per page load and appended to
// document.body, so the unlock survives room switches. Its srcObject is a MediaStream with the audible share's
// audio track; audio-follows-focus is a srcObject swap. Tile videos are always muted, so they autoplay everywhere,
// and iOS needs only one gesture: the one that starts this element.
//
// The unlock state machine (viewerStore.audio):
//
//   locked ──assign track, play() ok──► playing ◄──tap (user gesture): audio.play() + video.play()── blocked
//      │                                  │  ▲                                                          ▲
//      └──── play() NotAllowedError ──────┼──┼──────────────────────────────────────────────────────────┘
//                                         │  └── unmute ── muted ◄── user mutes (M key / button)
//                                         └── later play() rejects (iOS interruption) → blocked
//
// - The element is made when it is first needed (a track, or a tap), not by createAudioOut: a page that never
//   plays sound has none.
// - A swap keeps the state: `playing` stays `playing` while the new stream starts.
// - A muted element keeps playing, so unmuting is instant; the audio subscription stays on (05 §12.4 rule 1).
// - navigator.audioSession.type is NOT changed: 'playback' could interrupt a voice call on the same phone.
// createViewer() (services.ts) makes the page's AudioOut and gives it the audible share's track and the volume.
import type { Logger } from '../lib/log';
import { isNotAllowed } from './videoPlayback';
import type { AudioUnlockState, ViewerStore } from './viewerStore';

export interface AudioOut {
  /** The audible share's audio track, or null for silence. Swapping tracks keeps playing (05 §10.2). */
  setTrack(track: MediaStreamTrack | null): void;
  /**
   * The tap of "Tap to unmute": calls audio.play() synchronously, inside the user gesture (05 §10.3). Resolves once
   * it plays (at once when there is nothing to play); rejects when the browser still refuses.
   */
  unlock(): Promise<void>;
  /**
   * Plays again after the browser paused the element (an iOS interruption, the page coming back): a refusal means
   * `blocked`, and the user taps. It needs no gesture and does nothing while the element plays.
   */
  resume(): void;
  /** The user's mute (M key or button): playing ⇄ muted. Unmuting plays, so call it inside the user gesture. */
  setMuted(muted: boolean): void;
  /** Sets the volume (0–1) and returns what the element reports back: iOS ignores it (05 §10.3). */
  setVolume(volume: number): number;
  /** The element, once it exists (tests and the debug overlay). */
  readonly element: HTMLAudioElement | null;
  /** Removes the element. Tests only: the page keeps its element for its lifetime. */
  dispose(): void;
}

export interface AudioOutDeps {
  /** viewerStore.audio gets the unlock state. */
  store: ViewerStore;
  /** Where the element is appended; default document.body. */
  parent?: HTMLElement;
  log?: Logger;
}

/** The element is played again by itself after a pause at most once per this long. */
export const AUTO_RESUME_SPACING_MS = 1_000;

function clamp01(v: number): number {
  return Number.isFinite(v) ? Math.min(1, Math.max(0, v)) : 1;
}

/**
 * Whether this browser lets a page set an element's volume. iOS doesn't: `volume` always reads 1 there, and the
 * hardware buttons are the volume (05 §10.3: the slider is hidden).
 */
export function canSetVolume(doc: Document = document): boolean {
  const probe = doc.createElement('audio');
  try {
    probe.volume = 0.5;
  } catch {
    return false;
  }
  return probe.volume === 0.5;
}

export function createAudioOut(deps: AudioOutDeps): AudioOut {
  const { store, log } = deps;
  let el: HTMLAudioElement | null = null;
  let track: MediaStreamTrack | null = null;
  let volume = clamp01(store.getState().volume);
  let muted = store.getState().audio === 'muted';
  /** The element has played once: the browser lets it play again without a tap. */
  let unlocked = store.getState().audio === 'playing';
  /** Grows with every play() and every change of source: the result of an older play() is ignored. */
  let attempt = 0;
  let lastAutoResume = -Infinity;
  let disposed = false;

  const setState = (audio: AudioUnlockState): void => {
    if (store.getState().audio !== audio) store.getState().setAudio(audio);
  };

  /**
   * The browser paused the element by itself (this module never calls pause()): an interruption. Try to go on, but
   * not in a loop: something that pauses it again within a second leaves it to the user's tap.
   */
  const onPause = (): void => {
    if (disposed || track === null) return;
    const now = Date.now();
    if (now - lastAutoResume < AUTO_RESUME_SPACING_MS) {
      if (!muted) setState('blocked');
      return;
    }
    lastAutoResume = now;
    void play().catch(() => undefined);
  };

  const element = (): HTMLAudioElement => {
    if (el) return el;
    const parent = deps.parent ?? document.body;
    el = parent.ownerDocument.createElement('audio');
    // For the tests and the debug overlay to find it; it has no controls, so it is not rendered.
    el.dataset['isshoniAudio'] = '';
    el.volume = volume;
    el.muted = muted;
    el.addEventListener('pause', onPause);
    parent.append(el);
    return el;
  };

  /**
   * Starts the element. Resolves when it plays, rejects with the browser's NotAllowedError when it wants a tap
   * (state `blocked`). Anything else, like the AbortError of a play() that a newer source cut short, resolves: a
   * newer attempt decides.
   */
  const play = (): Promise<void> => {
    const a = element();
    const mine = ++attempt;
    // Very old browsers return nothing from play(); Promise.resolve() below covers that.
    let started: Promise<void> | undefined;
    try {
      // Synchronous: inside a tap this is what the gesture allows.
      started = a.play();
    } catch (err) {
      started = Promise.reject(err instanceof Error ? err : new Error(String(err)));
    }
    return Promise.resolve(started).then(
      () => {
        if (mine !== attempt || disposed) return;
        unlocked = true;
        setState(muted ? 'muted' : 'playing');
      },
      (err: unknown) => {
        if (mine !== attempt || disposed) return;
        if (!isNotAllowed(err)) {
          log?.debug('audio play() ended early', { error: err });
          return;
        }
        // A muted element isn't heard anyway: it stays `muted`, and unmuting tries again inside its gesture.
        if (!muted) setState('blocked');
        throw err;
      },
    );
  };

  return {
    setTrack(next) {
      if (disposed || next === track) return;
      track = next;
      if (next === null) {
        attempt++;
        if (el) el.srcObject = null;
        // Nothing to unmute anymore: the next track tries by itself again.
        if (store.getState().audio === 'blocked') setState('locked');
        return;
      }
      if (typeof MediaStream !== 'function') {
        // Not a browser (boot-check.js requires WebRTC, 05 §4): a test's DOM without media. Nothing can play.
        log?.warn('no MediaStream here: the audible share stays silent');
        track = null;
        return;
      }
      element().srcObject = new MediaStream([next]);
      void play().catch(() => undefined);
    },

    unlock() {
      if (disposed || track === null) return Promise.resolve();
      return play();
    },

    resume() {
      if (disposed || track === null || !el?.paused) return;
      void play().catch(() => undefined);
    },

    setMuted(next) {
      if (disposed) return;
      muted = next;
      if (el) el.muted = next;
      if (next) {
        setState('muted');
        return;
      }
      if (track !== null && el?.paused !== false) {
        // It never started, or was refused before the user muted: this gesture may start it.
        setState(unlocked ? 'playing' : 'locked');
        void play().catch(() => undefined);
        return;
      }
      setState(unlocked ? 'playing' : 'locked');
    },

    setVolume(next) {
      volume = clamp01(next);
      if (!el) return volume;
      el.volume = volume;
      return el.volume;
    },

    get element() {
      return el;
    },

    dispose() {
      disposed = true;
      attempt++;
      track = null;
      if (el) {
        el.removeEventListener('pause', onPause);
        el.srcObject = null;
        el.remove();
        el = null;
      }
    },
  };
}
