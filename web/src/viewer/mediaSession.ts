// What is playing, for the system (05 §12.8): navigator.mediaSession.metadata is {title: "bo's window", artist:
// "<room>"} while a share is heard, so Android's notification shade, a lock screen or a keyboard's media overlay
// name the sound that keeps playing in the background (the layer policy's rule 1). Nothing is shown about a share
// that is only watched: the notification belongs to the audio.
//
// The Media Session API is feature-detected: without it (or without MediaMetadata) this does nothing. No action
// handlers are set: a live share can't be paused or skipped, and the page's own mute stays the only control.
// No React here (05 §3).
import type { TFunction } from 'i18next';

import { shareTitle } from './shareView';
import type { ViewerServices } from './services';
import type { ViewerState } from './viewerStore';

export interface MediaSessionDeps {
  t: TFunction;
  /** The room's name, the `artist` line. Read with every change. */
  artist: () => string;
  /** Default: navigator.mediaSession, where the browser has one. */
  session?: Pick<MediaSession, 'metadata'> | null;
  /** Default: the global MediaMetadata. */
  Metadata?: typeof MediaMetadata;
}

/** The audible share's title, or null while nothing is heard. */
function nowPlaying(s: ViewerState, t: TFunction): string | null {
  const share = s.audibleShareId === null ? undefined : s.shares.find((x) => x.id === s.audibleShareId);
  return share ? shareTitle(t, share) : null;
}

/**
 * Keeps the system's "now playing" on the audible share. Returns the function that stops it and clears the
 * metadata.
 */
export function attachMediaSession(viewer: Pick<ViewerServices, 'store'>, deps: MediaSessionDeps): () => void {
  const nav = typeof navigator === 'undefined' ? undefined : (navigator as { mediaSession?: MediaSession });
  const session = deps.session === undefined ? (nav?.mediaSession ?? null) : deps.session;
  const Metadata = deps.Metadata ?? (typeof MediaMetadata === 'function' ? MediaMetadata : undefined);
  if (session === null || Metadata === undefined) return () => undefined;

  /** What the metadata says now: "title\nartist", or null for none. A change of anything else is not written. */
  let shown: string | null = null;
  const apply = (): void => {
    const title = nowPlaying(viewer.store.getState(), deps.t);
    const artist = deps.artist();
    const next = title === null ? null : `${title}\n${artist}`;
    if (next === shown) return;
    shown = next;
    session.metadata = title === null ? null : new Metadata({ title, artist });
  };

  const off = viewer.store.subscribe((s, prev) => {
    if (s.audibleShareId !== prev.audibleShareId || s.shares !== prev.shares) apply();
  });
  apply();
  return () => {
    off();
    if (shown !== null) session.metadata = null;
    shown = null;
  };
}
