// What a tile and the stage say about a share: its names (05 §12.6, §16.6) and the state it is in (05 §10.4, the
// rows of §12.6 that need no stats). Pure functions of the store's data, with every message key written out so
// check:i18n sees it.
import type { TFunction } from 'i18next';

import {
  ShareKindTab,
  ShareKindWindow,
  ShareStatusStalled,
  StatusReasonBandwidth,
  StatusReasonCodec,
  StatusReasonUnavailable,
  StatusReasonWaiting,
  type ShareKind,
  type SubscriptionStatus,
} from '../protocol/types.gen';
import type { ViewerShare } from './viewerStore';

/**
 * "Screen", "Window" or "Tab": the catalog's share.label.<kind> (05 §12.6, §16.5), the key family the sharer's
 * pages and the desktop tray use too. A kind this build doesn't know reads as a screen (01 §8.13).
 */
export function kindLabel(t: TFunction, kind: ShareKind): string {
  switch (kind as string) {
    case ShareKindWindow:
      return t('share.label.window');
    case ShareKindTab:
      return t('share.label.tab');
    default:
      return t('share.label.screen');
  }
}

/** The sharer as the tile names them: "You" for this user's shares. */
export function sharerName(t: TFunction, share: ViewerShare): string {
  if (share.own) return t('viewer.you');
  return share.ownerName !== '' ? share.ownerName : t('viewer.someone');
}

/** What is shared, in a word: the label the sharer typed, else the kind. */
export function shareWhat(t: TFunction, share: ViewerShare): string {
  const label = share.info.label?.trim();
  return label !== undefined && label !== '' ? label : kindLabel(t, share.info.kind);
}

/** "Alex's window", or "Alex: Movie night" when the sharer typed a label. */
export function shareTitle(t: TFunction, share: ViewerShare): string {
  const name = sharerName(t, share);
  const label = share.info.label?.trim();
  if (label !== undefined && label !== '') return t('viewer.title.labeled', { name, label });
  if (share.own) {
    switch (share.info.kind as string) {
      case ShareKindWindow:
        return t('viewer.title.ownWindow');
      case ShareKindTab:
        return t('viewer.title.ownTab');
      default:
        return t('viewer.title.ownScreen');
    }
  }
  switch (share.info.kind as string) {
    case ShareKindWindow:
      return t('viewer.title.window', { name });
    case ShareKindTab:
      return t('viewer.title.tab', { name });
    default:
      return t('viewer.title.screen', { name });
  }
}

/** "3 watching": share.watchers.length (01 §8.5); the owner is never among them. */
export function watchingText(t: TFunction, share: ViewerShare): string {
  return t('viewer.watching', { count: share.info.watchers.length });
}

/** What covers the video: a spinner while connecting, a notice while the sharer's connection is down, the codec wait. */
export type ShareOverlay = 'connecting' | 'stalled' | 'codec';
/** A small label on a playing video: why it isn't the quality that was asked for. */
export type ShareBadge = 'bandwidth' | 'unavailable' | 'reduced';

export interface ShareView {
  readonly overlay: ShareOverlay | null;
  readonly badge: ShareBadge | null;
}

/**
 * The state a share's tile shows (05 §10.4, §12.6):
 * - the server's `codec` reason: this browser can't decode the video yet; audio keeps playing (05 §10.6);
 * - the share is `stalled`: the sharer's connection dropped; the last frame stays;
 * - no video track yet, or the `waiting` reason: connecting;
 * - the `bandwidth` or `unavailable` reason: a badge; a reason this build doesn't know: a badge without a specific
 *   reason (01 §8.13).
 * A share this page publishes shows its preview; only `stalled` applies to it.
 */
export function shareView(share: ViewerShare, status: SubscriptionStatus | undefined, hasVideo: boolean): ShareView {
  const stalled = share.info.status === ShareStatusStalled;
  if (share.local) return { overlay: stalled ? 'stalled' : null, badge: null };
  const reason: string | undefined = status?.reason;
  if (reason === StatusReasonCodec) return { overlay: 'codec', badge: null };
  let badge: ShareBadge | null = null;
  if (reason === StatusReasonBandwidth) badge = 'bandwidth';
  else if (reason === StatusReasonUnavailable) badge = 'unavailable';
  else if (reason !== undefined && reason !== '' && reason !== StatusReasonWaiting) badge = 'reduced';
  if (stalled) return { overlay: 'stalled', badge };
  if (!hasVideo || reason === StatusReasonWaiting) return { overlay: 'connecting', badge: null };
  return { overlay: null, badge };
}

export function overlayText(t: TFunction, overlay: ShareOverlay): string {
  switch (overlay) {
    case 'connecting':
      return t('viewer.state.connecting');
    case 'stalled':
      return t('viewer.state.stalled');
    case 'codec':
      return t('viewer.state.codec');
  }
}

export function badgeText(t: TFunction, badge: ShareBadge): string {
  switch (badge) {
    case 'bandwidth':
      return t('viewer.state.bandwidth');
    case 'unavailable':
      return t('viewer.state.unavailable');
    case 'reduced':
      return t('viewer.state.reduced');
  }
}
