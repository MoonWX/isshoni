// The page's own visibility (05 §12.4 rule 3, §12.8): viewerStore.pageHiddenSince, and what happens when the page
// comes back. createViewer() attaches it for the life of the viewer, so it also works while the room page is not
// mounted (the audible share plays on, 05 §11.1). No React here (05 §3).
//
// - Hidden: the store gets the time. After 10 s the layer policy turns every video off (subscriptions.ts has the
//   timer); the sound goes on (rule 1).
// - Visible again: the time is cleared, so video is asked for again at once. iOS suspended the page meanwhile
//   (WebRTC stops in the background and when the screen locks), and its media elements are paused: every tile's
//   video is played again, and so is the <audio> element. If the browser refuses that one, its state is `blocked`
//   and TapToStart asks for the tap (05 §10.3). On a desktop nothing was paused, and both calls do nothing.
import type { AudioOut } from './audioOut';
import type { VideoPlayback } from './videoPlayback';
import type { ViewerStore } from './viewerStore';

export interface PageDeps {
  store: ViewerStore;
  videos: Pick<VideoPlayback, 'resume'>;
  audio: Pick<AudioOut, 'resume'>;
  /** Default: the global document. */
  doc?: Document;
  /** Default Date.now. */
  now?: () => number;
}

/** Follows the document's visibility. Returns the function that stops it. */
export function attachPage(deps: PageDeps): () => void {
  const { store, videos, audio } = deps;
  const doc = deps.doc ?? (typeof document === 'undefined' ? undefined : document);
  // Not a page (a test without a DOM): nothing to follow, and the store keeps "visible".
  if (!doc) return () => undefined;
  const now = deps.now ?? Date.now;

  const onChange = (): void => {
    if (doc.visibilityState === 'hidden') {
      // The first time counts: a second `hidden` without a `visible` between them doesn't restart the 10 s.
      if (store.getState().pageHiddenSince === null) store.getState().setPageHidden(now());
      return;
    }
    const wasHidden = store.getState().pageHiddenSince !== null;
    store.getState().setPageHidden(null);
    if (!wasHidden) return;
    videos.resume();
    audio.resume();
  };

  doc.addEventListener('visibilitychange', onChange);
  // A page that is made in the background (a restored tab, a link opened in a new tab) starts hidden.
  if (doc.visibilityState === 'hidden') onChange();
  return () => {
    doc.removeEventListener('visibilitychange', onChange);
  };
}
