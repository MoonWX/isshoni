// The layer policy (05 §12.4, "05 policy" in 01 §8.9): the only place that decides what to subscribe to. A pure
// function from what the viewer shows to one SubscriptionWant per remote share.
//
// The interface is declared here, with the viewer's first slice ("interfaces first", docs/m1/README.md §4); the
// rules arrive with S56 (05 W8). Until then desiredSubscriptions throws NotImplementedError.
//
// Rules, in order (05 §12.4):
// 1. Audio: `on` only for `audible`, whatever the page visibility.
// 2. The PiP share → `high`, even when the page is hidden.
// 3. Page hidden for ≥ 10 s → every other share's video `off`.
// 4. The focused share → `high`.
// 5. In fullscreen, every non-focused share → `off`.
// 6. Other shares: visible → `low`, not visible (off-screen, scrolled away, unmounted) → `off`.
import { NotImplementedError } from '../lib/errors';
import type { SubscriptionWant } from '../protocol/types.gen';

export interface LayerInputs {
  /** Live or stalled shares, excluding the ones this page publishes (viewerStore: the shares that aren't `local`). */
  remoteShares: string[];
  focused: string | null;
  audible: string | null;
  /** IntersectionObserver, ≥ 10% visible. */
  visible: Record<string, boolean>;
  /** 0 when visible. */
  pageHiddenForMs: number;
  fullscreen: boolean;
  pip: string | null;
}

/** The desired video layer and audio of every remote share: {shareId, video, audio}. */
export const desiredSubscriptions: (inputs: LayerInputs) => SubscriptionWant[] = () => {
  throw new NotImplementedError('viewer.desiredSubscriptions', 'S56');
};
