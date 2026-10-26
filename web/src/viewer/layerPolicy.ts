// The layer policy (05 §12.4, "05 policy" in 01 §8.9): the only place that decides what to subscribe to. A pure
// function from what the viewer shows to one SubscriptionWant per remote share.
//
// Rules, in order (05 §12.4):
// 1. Audio: `on` only for `audible`, whatever the page visibility (background tabs keep playing sound on desktop
//    and Android).
// 2. The PiP share → `high`, even when the page is hidden.
// 3. Page hidden for ≥ 10 s → every other share's video `off`.
// 4. The focused share → `high`.
// 5. In fullscreen, every non-focused share → `off` (IntersectionObserver can't see that they're covered).
// 6. Other shares: visible → `low`, not visible (off-screen, scrolled away, unmounted) → `off`.
// 7. M2 (docs/m2/03 §10.4): a local share without a preview of the page's own (the desktop app's share, published
//    by its C2) is subscribed by the same rules, but with `low` at most and audio always `off`: its tile and the stage
//    show the preview layer (360p15, about 0.3 Mbps), and the app never plays its own sound.
//
// subscriptions.ts builds the inputs from viewerStore and gives the result to the session's SubscriptionSync, which
// sends the changes in one subscribe.update (05 §12.3). The rules are written out with this slice (S47), which
// needs the focus and audio ones; what feeds `visible` beyond "the tile is mounted", `pageHiddenForMs`,
// `fullscreen` and `pip` arrives with S56 (05 W8), until which those inputs keep their defaults.
import {
  AudioStateOff,
  AudioStateOn,
  VideoLayerHigh,
  VideoLayerLow,
  VideoLayerOff,
  type SubscriptionWant,
  type VideoLayer,
} from '../protocol/types.gen';

/** A page hidden for this long gets no video (05 §18); the sound goes on. */
export const PAGE_HIDDEN_OFF_MS = 10_000;

export interface LayerInputs {
  /** Live or stalled shares, excluding the ones this page publishes (viewerStore: the shares that aren't `local`). */
  remoteShares: string[];
  /**
   * M2: the local shares that have no preview of this page's own (viewerStore: `local` but not `localPreview`; the
   * desktop app's share, docs/m2/03 §10.4). Default none.
   */
  localShares?: string[];
  focused: string | null;
  audible: string | null;
  /** IntersectionObserver, ≥ 10% visible. */
  visible: Record<string, boolean>;
  /** 0 when visible. */
  pageHiddenForMs: number;
  fullscreen: boolean;
  pip: string | null;
}

function videoLayer(shareId: string, i: LayerInputs): VideoLayer {
  if (shareId === i.pip) return VideoLayerHigh; // rule 2
  if (i.pageHiddenForMs >= PAGE_HIDDEN_OFF_MS) return VideoLayerOff; // rule 3
  if (shareId === i.focused) return VideoLayerHigh; // rule 4
  if (i.fullscreen) return VideoLayerOff; // rule 5
  return i.visible[shareId] === true ? VideoLayerLow : VideoLayerOff; // rule 6
}

/**
 * The desired video layer and audio of every remote share, in the order of remoteShares, then of every local share
 * without a preview (rule 7): {shareId, video, audio}.
 */
export function desiredSubscriptions(i: LayerInputs): SubscriptionWant[] {
  const remote = i.remoteShares.map((shareId): SubscriptionWant => ({
    shareId,
    video: videoLayer(shareId, i),
    audio: shareId === i.audible ? AudioStateOn : AudioStateOff, // rule 1
  }));
  const local = (i.localShares ?? []).map((shareId): SubscriptionWant => {
    const video = videoLayer(shareId, i);
    return { shareId, video: video === VideoLayerHigh ? VideoLayerLow : video, audio: AudioStateOff }; // rule 7
  });
  return [...remote, ...local];
}
