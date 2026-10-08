// roomStore (05 §6.1): the room this tab is in, as the UI reads it: the desired room and how far joining it got,
// the last room.state snapshot (participants, shares, rev), and the connection's own ids. Only RoomSession writes
// it (05 §11.1); React reads it with useRoom() (rooms/hooks.ts). A vanilla Zustand store (05 §3 "Layering rule").
import { createStore, type StoreApi } from 'zustand/vanilla';

import type { ProtocolError } from '../protocol/errors';
import type { ParticipantInfo, RoomInfo, RoomState, ShareInfo } from '../protocol/types.gen';

/**
 * How far joining the desired room got:
 * - idle: no desired room (before the first join, after leave(), after the server took the session out of its room);
 * - joining: a desired room, and room.join isn't answered yet, or signaling isn't ready (the next resync joins);
 * - joined: room.join was answered ok; `state` follows with the first room.state;
 * - failed: the server refused the room (joinError says why). The session doesn't try again by itself.
 */
export type JoinState = 'idle' | 'joining' | 'joined' | 'failed';

/**
 * The session left its room because the server said so, and the UI should go to another one (05 §6.3): the room
 * doesn't exist (room_not_found on a join), an admin deleted it (room_closed), or a room-scope error this build
 * doesn't know (01 §12.3). The room page navigates to roomId and calls clearRedirect().
 */
export interface RoomRedirect {
  /** Where to go: welcome.defaultRoomId. */
  readonly roomId: string;
  /** The error code that caused it. */
  readonly code: string;
}

export interface RoomStoreState {
  /** The desired room (the "memory" of 01 §10.5): set by join() before anything else, null after leave(). */
  readonly roomId: string | null;
  readonly joinState: JoinState;
  /** failed: the server's error (room_full, forbidden, …); render it with lib/errorText. */
  readonly joinError: ProtocolError | null;
  /** The room's id and name, from room.join's ok; null until then. */
  readonly room: RoomInfo | null;
  /** The last room.state of the desired room (01 §8.5); null until the first one after a join. */
  readonly state: RoomState | null;
  /** The connection's own id (welcome.connectionId): the one in ShareInfo.connectionId of this tab's shares. */
  readonly connectionId: string | null;
  /** The signed-in user's id (welcome.user.id). */
  readonly userId: string | null;
  readonly redirect: RoomRedirect | null;
  /** The UI followed the redirect (or dropped it). */
  clearRedirect(): void;
}

export type RoomStore = StoreApi<RoomStoreState>;

export function createRoomStore(): RoomStore {
  return createStore<RoomStoreState>()((set) => ({
    roomId: null,
    joinState: 'idle',
    joinError: null,
    room: null,
    state: null,
    connectionId: null,
    userId: null,
    redirect: null,
    clearRedirect() {
      set({ redirect: null });
    },
  }));
}

/** The shares this connection publishes, by the snapshot (01 §8.5: ShareInfo.connectionId). */
export function sharesOfConnection(state: RoomState | null, connectionId: string | null): ShareInfo[] {
  if (state === null || connectionId === null) return [];
  return state.shares.filter((s) => s.connectionId === connectionId);
}

/** A participant of the snapshot by user id. */
export function participantOf(state: RoomState | null, userId: string): ParticipantInfo | undefined {
  return state?.participants.find((p) => p.userId === userId);
}
