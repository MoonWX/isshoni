// "You're already sharing from another tab or device" (05 §13.1): the M1 UI starts at most one share per user, so
// the ShareSheet has to know about a share this user started on another connection. The room page finds it in
// room.state with this and gives the sheet a ShareElsewhere whose onStop sends share.stop for it (allowed for the
// same user, 01 §4.1; the desktop app's page, whose role is viewer, asks its C2 to: platform.sharing.stopOther).
//
// The page's own connections are a list (docs/m2/03 §10.2): its own one, plus the platform's other local connections
// (platform.sharing.localConnectionIds(): the desktop app's C2), whose share is this page's and not "elsewhere".
import type { ShareInfo } from '../protocol/types.gen';

/**
 * The share in room.state that belongs to this user but to none of this page's connections; null when there is none.
 * Every status counts (starting and stalled too): the share exists, and a second one would not replace it.
 */
export function findShareElsewhere(
  shares: readonly ShareInfo[],
  own: { readonly userId: string; readonly connectionIds: readonly string[] },
): ShareInfo | null {
  return shares.find((s) => s.userId === own.userId && !own.connectionIds.includes(s.connectionId)) ?? null;
}
