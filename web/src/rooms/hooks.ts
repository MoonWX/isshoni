// React's side of the room runtime (05 §3 "Layering rule": components read the stores and call the controllers,
// never the WebSocket). Pages and components inside <AppProviders> use these hooks.
import { useEffect } from 'react';
import { useNavigate } from 'react-router';
import { useStore } from 'zustand';

import { useApp } from '../app/context';
import type { ConnectionState } from './connection';
import type { RoomStoreState } from './roomStore';
import { getRoomRuntime, type RoomRuntime } from './runtime';

/** The app's room runtime: the connection, the session and their stores. */
export function useRoomRuntime(): RoomRuntime {
  return getRoomRuntime(useApp());
}

/** Reads connectionStore; re-renders when the selected value changes. */
export function useConnection<T>(selector: (s: ConnectionState) => T): T {
  return useStore(useRoomRuntime().stores.connection, selector);
}

/**
 * Reads roomStore; re-renders when the selected value changes. Select stored values (s.state, s.joinState), not
 * arrays built in the selector: a new array on every call re-renders forever.
 */
export function useRoom<T>(selector: (s: RoomStoreState) => T): T {
  return useStore(useRoomRuntime().stores.room, selector);
}

/** The path of a room's page (05 §5). */
export function roomPath(roomId: string): string {
  return `/r/${encodeURIComponent(roomId)}`;
}

/**
 * The room page's wiring (05 §7, §11.1). With the route's room id it makes that room the desired one and starts the
 * tab's connection (the first call does; later ones find it running), and it follows the session when the server
 * sends the user elsewhere (roomStore.redirect: the room was deleted or doesn't exist) by navigating to that room.
 *
 * It never leaves the room on unmount: the session is app-level and survives navigation to /account or /admin.
 * A refused join is in roomStore (joinState failed, joinError) for the page to render.
 */
export function useRoomSession(roomId: string | undefined): RoomRuntime {
  const runtime = useRoomRuntime();
  const navigate = useNavigate();

  useEffect(() => {
    if (roomId === undefined || roomId === '') return;
    // The desired room first, then the connection: the first welcome's resync joins it (01 §10.5).
    runtime.session.join(roomId).catch(() => undefined);
    runtime.start();
  }, [runtime, roomId]);

  const redirect = useStore(runtime.stores.room, (s) => s.redirect);
  useEffect(() => {
    if (redirect === null) return;
    runtime.stores.room.getState().clearRedirect();
    void navigate(roomPath(redirect.roomId), { replace: true });
  }, [redirect, navigate, runtime]);

  return runtime;
}
