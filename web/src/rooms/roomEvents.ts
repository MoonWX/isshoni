// room.event → words (05 §11.1, 01 §8.6). Events only announce (a toast, which the Announcer's polite live region
// also reads, 05 §16.6); the UI renders from room.state. Not announced: the user's own events, and the
// share.started of a re-publish (replaces is set, 01 §10.6).
import type { TFunction } from 'i18next';

import { i18n } from '../i18n';
import {
  EndReasonDisconnected,
  EndReasonLeft,
  EndReasonStopped,
  RoomEventKindParticipantJoined,
  RoomEventKindParticipantLeft,
  RoomEventKindShareStarted,
  RoomEventKindShareStopped,
  type RoomEvent,
} from '../protocol/types.gen';

/** Whether a room.event is announced at all to the user with this id. */
export function isAnnounced(e: RoomEvent, ownUserId: string | null): boolean {
  if (e.userId === ownUserId) return false;
  if (e.kind === RoomEventKindShareStarted && e.replaces !== undefined && e.replaces !== '') return false;
  return true;
}

/**
 * The announcement for a room.event, with the person's name from the event (it works after they left the snapshot);
 * null for a kind this build doesn't know (01 §8.13: ignored).
 */
export function roomEventMessage(e: RoomEvent, t: TFunction = i18n.t): string | null {
  const name = e.name;
  // A string here: a newer server may send kinds this build doesn't have.
  const kind: string = e.kind;
  // Literal keys, so check:i18n sees each one.
  switch (kind) {
    case RoomEventKindParticipantJoined:
      return t('room.event.joined', { name });
    case RoomEventKindParticipantLeft:
      // Grace expired (01 §4.2): they didn't choose to go.
      return e.reason === EndReasonDisconnected
        ? t('room.event.disconnected', { name })
        : t('room.event.left', { name });
    case RoomEventKindShareStarted:
      return t('room.event.shareStarted', { name });
    case RoomEventKindShareStopped:
      // The sharer stopped it (or left with it); every other reason is the share ending by itself.
      return e.reason === undefined || e.reason === EndReasonStopped || e.reason === EndReasonLeft
        ? t('room.event.shareStopped', { name })
        : t('room.event.shareEnded', { name });
    default:
      return null;
  }
}
