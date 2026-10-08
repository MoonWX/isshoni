// Who watches this page's share (05 §13.7: the sharer always sees who is watching). The names come from room.state:
// a share's `watchers` (01 §8.5; the owner is never among them) joined with the room's participants.
import type { RoomState } from '../protocol/types.gen';

export interface ShareWatcher {
  readonly userId: string;
  /** The participant's name; '' when the snapshot doesn't list them (the panel says "Someone"). */
  readonly name: string;
}

/** The watchers of one share in a room.state, in the snapshot's order; [] without the snapshot or the share. */
export function shareWatchers(
  state: Pick<RoomState, 'shares' | 'participants'> | null | undefined,
  shareId: string | null | undefined,
): ShareWatcher[] {
  const share = shareId == null ? undefined : state?.shares.find((s) => s.id === shareId);
  if (state == null || share === undefined) return [];
  const names = new Map(state.participants.map((p) => [p.userId, p.name]));
  return share.watchers.map((w) => ({ userId: w.userId, name: names.get(w.userId) ?? '' }));
}
