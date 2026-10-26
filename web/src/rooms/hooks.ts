// React's side of the room runtime (05 §3 "Layering rule": components read the stores and call the controllers,
// never the WebSocket). Pages and components inside <AppProviders> use these hooks.
import { useCallback, useEffect, useReducer, useState, useSyncExternalStore } from 'react';
import { useNavigate } from 'react-router';
import { useStore } from 'zustand';

import { useApp } from '../app/context';
import type { ActiveShare, Platform } from '../platform/types';
import type { ConnectionState } from './connection';
import type { RoomSession } from './RoomSession';
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

/** Whether a location's path is a room's page: there the page itself shows the session, elsewhere InRoomBar does. */
export function isRoomPath(pathname: string): boolean {
  return /^\/r\/[^/]+\/?$/.test(pathname);
}

/**
 * The share this tab publishes (session.share), null while it shares nothing. Re-renders when the share starts or
 * ends, and when it changes in place: a re-publish gives it a new shareId (01 §10.6), which ActiveShare reports as
 * a state change (05 §8).
 */
export function useLocalShare(session: RoomSession): ActiveShare | null {
  const subscribe = useCallback((onChange: () => void) => session.on('share', onChange), [session]);
  const share = useSyncExternalStore(subscribe, () => session.share);
  const [, changed] = useReducer((n: number) => n + 1, 0);
  useEffect(() => share?.on('state', changed), [share]);
  return share;
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

/** Whether this page can share (docs/m2/03 §7.6), and why not: what the room page's Share gates read. */
export interface ShareAvailability {
  /** platform.capabilities().canShare. */
  readonly canShare: boolean;
  /** Native (the desktop app): Share.Capabilities' reason when canShare is false, a media code; else undefined. */
  readonly reason?: string;
}

/**
 * Whether to offer Share (docs/m2/03 §7.6): capabilities().canShare, never `role` or `sharing !== null` alone (the
 * desktop is role viewer and can share; its engine can say it can't: unsupported_os, no_encoder). Read
 * asynchronously, as viewer/useControls.ts does: null until the platform answered, and for a platform without a
 * sharing provider. In the desktop a refusal brings Share.Capabilities' reason for the note in its place.
 */
export function useShareAvailability(platform: Platform): ShareAvailability | null {
  const [known, setKnown] = useState<{ platform: Platform; value: ShareAvailability } | null>(null);
  useEffect(() => {
    if (platform.sharing === null) return undefined;
    let current = true;
    const settle = (value: ShareAvailability): void => {
      if (current) setKnown({ platform, value });
    };
    platform.capabilities().then(
      (caps) => {
        const bridge = platform.desktop?.bridge;
        if (caps.canShare || platform.sharing?.mode !== 'native' || bridge === undefined) {
          settle({ canShare: caps.canShare });
          return;
        }
        bridge.share.capabilities().then(
          (c) => {
            settle(
              c.reason !== undefined && c.reason !== '' ? { canShare: false, reason: c.reason } : { canShare: false },
            );
          },
          () => {
            settle({ canShare: false });
          },
        );
      },
      () => {
        settle({ canShare: false });
      },
    );
    return () => {
      current = false;
    };
  }, [platform]);
  return known !== null && known.platform === platform ? known.value : null;
}
