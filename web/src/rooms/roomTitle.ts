// The document title of the room page (05 §11.2): the room's live state, readable from the tab strip.
//   ● Sharing · Lounge · isshoni     while this page shares
//   ● 2 live · Lounge · isshoni      while others share
//   Lounge · isshoni                 otherwise
import type { TFunction } from 'i18next';
import { useEffect } from 'react';

import { ShareStatusStarting, type RoomState } from '../protocol/types.gen';

/** The shares that count as live in the title: the ones with a tile (not `starting`, 01 §4.4). */
export function liveShareCount(state: RoomState | null): number {
  return state?.shares.filter((s) => s.status !== ShareStatusStarting).length ?? 0;
}

export interface RoomTitleInput {
  /** The room's name. */
  room: string;
  /** How many shares are live in it. */
  live: number;
  /** This page shares. */
  sharing: boolean;
}

export function roomTitle(t: TFunction, { room, live, sharing }: RoomTitleInput): string {
  const app = t('common.appName');
  if (sharing) return t('room.title.sharing', { room, app });
  if (live > 0) return t('room.title.live', { count: live, room, app });
  return t('room.title.idle', { room, app });
}

/** Shows title as the document's title while the component is mounted; the title of before comes back after. */
export function useDocumentTitle(title: string): void {
  useEffect(() => {
    const before = document.title;
    document.title = title;
    return () => {
      document.title = before;
    };
  }, [title]);
}
