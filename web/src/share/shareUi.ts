// The share state's part in the app-wide UI (uiStore), and the texts of a share that ended.
//
// linkShareUi keeps uiStore.sharing equal to "this page shares a screen right now", so the UpdatePill offers no
// Reload meanwhile (05 §16.2: a reload would end the share), and shows the toast of a share that the server ended
// without a failure ("Sharing was stopped from another tab or device", 05 §13.1).
//
// Why a link and not a component: the share outlives the room page (the session is app-level, 05 §11.1), so both
// have to work on /account and /admin too, where nothing of share/ is mounted. A share can only start from a Share
// button, so each button makes the link when it mounts; from then on the link stays, whatever page is shown.
import type { TFunction } from 'i18next';

import type { UiStore } from '../app/uiStore';
import { i18n } from '../i18n';
import { errorMessage } from '../lib/errorText';
import { ShareEndedError } from './BrowserShare';
import { isSharingPhase, type ShareStore } from './shareStore';

const linked = new WeakMap<ShareStore, WeakSet<UiStore>>();

/**
 * Links a share store to the app's uiStore, for good: ui.sharing follows the store's phase (starting, live,
 * reconnecting and stopping count as sharing), and a notice of the store becomes a toast. Linking the same pair
 * again does nothing.
 */
export function linkShareUi(store: ShareStore, ui: UiStore): void {
  let uis = linked.get(store);
  if (uis === undefined) {
    uis = new WeakSet();
    linked.set(store, uis);
  }
  if (uis.has(ui)) return;
  uis.add(ui);

  const sync = (): void => {
    const sharing = isSharingPhase(store.getState().phase);
    if (ui.getState().sharing !== sharing) ui.getState().setSharing(sharing);
  };
  sync();
  store.subscribe((state, before) => {
    sync();
    if (state.notice !== null && state.notice !== before.notice) {
      // One kind so far; a switch when there are more.
      ui.getState().toast({ kind: 'info', message: i18n.t('share.ended.elsewhere') });
    }
  });
}

/**
 * The message for the error of a failed share (shareStore.error): the texts of a share the server ended (05 §13.1),
 * else the error's own (errors.<code>, errors.local.<code>).
 */
export function shareErrorMessage(err: unknown, t: TFunction = i18n.t): string {
  if (err instanceof ShareEndedError) {
    return err.kind === 'media_timeout' ? t('share.ended.mediaTimeout') : t('share.ended.generic');
  }
  return errorMessage(err, t);
}
