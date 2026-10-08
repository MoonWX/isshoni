// "You're already sharing from another tab or device" (05 §13.1): the M1 UI starts at most one share per user, so
// the ShareSheet has to know about a share this user started on another connection. The room page finds it in
// room.state with this and gives the sheet a ShareElsewhere whose onStop sends share.stop for it (allowed for the
// same user, 01 §4.1).
import type { ShareInfo } from '../protocol/types.gen';

/**
 * The share in room.state that belongs to this user but to another of their connections; null when there is none.
 * Every status counts (starting and stalled too): the share exists, and a second one would not replace it.
 */
export function findShareElsewhere(
  shares: readonly ShareInfo[],
  own: { readonly userId: string; readonly connectionId: string },
): ShareInfo | null {
  return shares.find((s) => s.userId === own.userId && s.connectionId !== own.connectionId) ?? null;
}
