// "bo started sharing [Watch]" (05 §12.2; owner decision, 05 §24.1): once the user has picked a tile, a newly
// started share takes neither the stage nor the sound (autoFocus.ts holds the pick). The room says so with a toast
// whose action picks the new share. While nothing is picked the new share is on the stage already, and the room's
// plain "bo started sharing" announcement is enough.
//
// The room session announces room.events (05 §11.1) and lets the viewer take one over: this module is that hook.
// It doesn't import React (05 §3).
import type { TFunction } from 'i18next';

import type { UiStore } from '../app/uiStore';
import { i18n } from '../i18n';
import { RoomEventKindShareStarted, type RoomEvent } from '../protocol/types.gen';
import type { ViewerServices } from './services';

/** How long the toast's "Watch" waits for a share that room.state hasn't shown yet (05 §18: the ?focus= wait). */
export const WATCH_WAIT_MS = 5_000;

export interface WatchToastDeps {
  viewer: Pick<ViewerServices, 'store'>;
  ui: UiStore;
  /** This user's id (welcome.user.id): their own shares are not announced (05 §11.1). */
  selfUserId: () => string | null | undefined;
  /** Default: the app's i18next instance. */
  t?: TFunction;
}

/**
 * Makes the viewer's room.event handler. It returns true when it announced the event itself (the caller then skips
 * its own toast), and undefined when the event isn't its business: anything but share.started, a re-publish
 * (`replaces`, 01 §10.6), the user's own share, or a share that auto-focus puts on the stage.
 */
export function createWatchToast(deps: WatchToastDeps): (e: RoomEvent) => boolean | undefined {
  const { viewer, ui } = deps;

  return (e) => {
    const shareId = e.shareId;
    if ((e.kind as string) !== RoomEventKindShareStarted || shareId === undefined || shareId === '') return undefined;
    if (e.replaces !== undefined && e.replaces !== '') return undefined;
    if (e.userId === deps.selfUserId()) return undefined;
    if (viewer.store.getState().focusMode !== 'manual') return undefined;

    const t = deps.t ?? i18n.t.bind(i18n);
    ui.getState().toast({
      kind: 'info',
      message: t('viewer.toast.started', { name: e.name }),
      action: {
        label: t('viewer.toast.watch'),
        run: () => {
          // A pick of that share. room.event can be ahead of the room.state that lists the share as live, so the
          // pick waits for it like ?focus= does, and is dropped when the share never shows up.
          viewer.store.getState().dispatch({ type: 'focusParam', shareId });
          setTimeout(() => {
            if (viewer.store.getState().pendingFocusParam === shareId) {
              viewer.store.getState().dispatch({ type: 'focusParam', shareId: null });
            }
          }, WATCH_WAIT_MS);
        },
      },
    });
    return true;
  };
}
