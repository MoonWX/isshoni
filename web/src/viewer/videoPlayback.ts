// The tiles' <video> elements and whether the browser lets them play (05 §10.3). Tile videos are muted, so they
// autoplay almost everywhere; iOS Low Power Mode blocks even muted autoplay. ShareVideo starts each element through
// play() here: a refusal marks it blocked (viewerStore.videoBlocked), TapToStart shows "Tap to start video", and
// its one tap handler calls retry(), which plays every refused video again inside the user gesture.
import type { ViewerStore } from './viewerStore';

/**
 * Whether play() was rejected because the browser wants a user gesture. It goes by the error's name alone, not by
 * `instanceof Error`: a DOMException from another realm (an iframe, or jsdom's in the tests) is not an instance of
 * this realm's Error.
 */
export function isNotAllowed(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as { name?: unknown }).name === 'NotAllowedError';
}

export interface VideoPlayback {
  /** A tile's <video> has a (new) source: play it. A NotAllowedError marks it blocked. */
  play(el: HTMLVideoElement): void;
  /** The element lost its source or is gone. */
  forget(el: HTMLVideoElement): void;
  /** The tap: play() on every video that was refused. Call it inside the user gesture. */
  retry(): void;
  /** Plays every video that is paused: when the page comes back on iOS (05 §12.8). */
  resume(): void;
}

export function createVideoPlayback(deps: { store: ViewerStore }): VideoPlayback {
  const { store } = deps;
  const videos = new Set<HTMLVideoElement>();
  const blocked = new Set<HTMLVideoElement>();

  const publish = (): void => {
    const any = blocked.size > 0;
    if (store.getState().videoBlocked !== any) store.getState().setVideoBlocked(any);
  };

  const start = (el: HTMLVideoElement): void => {
    // A DOM without playback (jsdom) returns nothing from play(); Promise.resolve() below covers that.
    let started: Promise<void> | undefined;
    try {
      started = el.play();
    } catch {
      return; // no playback here at all (a test DOM): nothing a tap could fix
    }
    Promise.resolve(started).then(
      () => {
        if (blocked.delete(el)) publish();
      },
      (err: unknown) => {
        // AbortError: a newer source replaced this one; its own play() decides.
        if (!videos.has(el) || !isNotAllowed(err)) return;
        blocked.add(el);
        publish();
      },
    );
  };

  return {
    play(el) {
      videos.add(el);
      start(el);
    },
    forget(el) {
      videos.delete(el);
      if (blocked.delete(el)) publish();
    },
    retry() {
      for (const el of [...blocked]) start(el);
    },
    resume() {
      for (const el of videos) {
        if (el.paused) start(el);
      }
    },
  };
}
