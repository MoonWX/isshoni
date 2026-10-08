// What the viewer wants from the server (05 §12.3–§12.4): this module turns viewerStore into the inputs of the
// layer policy and gives the policy's result, the complete desired set, to the session's SubscriptionSync
// (rooms/subscriptionSync.ts, `session.subscriptions`). That one diffs the set against what the server holds and
// sends ONE subscribe.update with every share that changed, so the server stops the old audio and starts the new
// one in the same step (01 §8.9). A focus change is therefore one message: the old share goes to {low, off} and the
// new one to {high, on} together. No React here (05 §3).
//
// What is watched:
// - Audio is on only for viewerStore.audibleShareId (rule 1).
// - The share on the stage gets the high layer while the stage shows it, the other tiles the low one while they
//   are shown, and what is not shown gets no video: away from the room page nothing is mounted, so every video
//   goes off while the audible share keeps playing (05 §11.1). "Shown" is viewerStore.visible, which the stage and
//   the tiles write (useVisibility.ts).
// - A share this page publishes is never subscribed (05 §12.2).
import type { SubscriptionWant } from '../protocol/types.gen';
import { desiredSubscriptions, PAGE_HIDDEN_OFF_MS, type LayerInputs } from './layerPolicy';
import type { ViewerServices } from './services';
import type { ViewerData } from './viewerStore';

/** Where the desired set goes: the session's SubscriptionSync (`session.subscriptions`). */
export interface SubscriptionSink {
  /** The complete desired set; every share not listed is {off, off}. An unchanged set must change nothing. */
  set(wants: readonly SubscriptionWant[]): void;
}

/** The policy's inputs (05 §12.4) from the store, at `now` (Date.now()). */
export function layerInputs(s: ViewerData, now: number): LayerInputs {
  // The stage's share counts as focused only while the stage shows it: with the room page away it is one more
  // share that nobody sees.
  const shown = s.focusedShareId !== null && s.visible[s.focusedShareId] === true;
  return {
    remoteShares: s.shares.filter((share) => !share.local).map((share) => share.id),
    focused: shown ? s.focusedShareId : null,
    audible: s.audibleShareId,
    visible: s.visible,
    pageHiddenForMs: s.pageHiddenSince === null ? 0 : Math.max(0, now - s.pageHiddenSince),
    fullscreen: s.fullscreen,
    pip: s.pipShareId,
  };
}

/** The complete desired set for what the store holds now. */
export function viewerWants(s: ViewerData, now: number = Date.now()): SubscriptionWant[] {
  return desiredSubscriptions(layerInputs(s, now));
}

/**
 * Keeps the sink's desired set equal to what the viewer shows and plays: once now, and after every change of the
 * store that the policy reads. It also looks again when a hidden page reaches the 10 s of rule 3. Returns the
 * function that stops it.
 */
export function attachSubscriptions(
  viewer: Pick<ViewerServices, 'store'>,
  sink: SubscriptionSink,
  opts: { now?: () => number } = {},
): () => void {
  const { store } = viewer;
  const now = opts.now ?? Date.now;
  let hiddenTimer: ReturnType<typeof setTimeout> | undefined;

  const apply = (): void => {
    const s = store.getState();
    const at = now();
    sink.set(viewerWants(s, at));
    clearTimeout(hiddenTimer);
    hiddenTimer = undefined;
    if (s.pageHiddenSince !== null && at - s.pageHiddenSince < PAGE_HIDDEN_OFF_MS) {
      hiddenTimer = setTimeout(apply, s.pageHiddenSince + PAGE_HIDDEN_OFF_MS - at);
    }
  };

  const off = store.subscribe((s, prev) => {
    if (
      s.shares !== prev.shares ||
      s.focusedShareId !== prev.focusedShareId ||
      s.audibleShareId !== prev.audibleShareId ||
      s.visible !== prev.visible ||
      s.pageHiddenSince !== prev.pageHiddenSince ||
      s.fullscreen !== prev.fullscreen ||
      s.pipShareId !== prev.pipShareId
    ) {
      apply();
    }
  });
  apply();

  return () => {
    off();
    clearTimeout(hiddenTimer);
  };
}
