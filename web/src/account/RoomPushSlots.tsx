// Where the room page plugs in Web Push (05 §11.2, §16.3): the two places rooms/ mounts, which load the real
// components (RoomPush.tsx, with the `account` namespace) as a chunk of their own when they first render.
// - <RoomPushPrompts /> on the room page: the push state with the start-up re-post, the watch time, notification
//   clicks and the iOS Home Screen sheet;
// - <RoomNotificationsCard /> in the stage's empty state: the one-time card.
//
// This file has no texts and stays small: it is part of the room's chunk. Until the push chunk is there, and if it
// can't be loaded (offline, a server updated under the page), both render nothing: a push prompt is never worth the
// room's error screen, which is what a failed React.lazy would show (05 §11.2 "Lazy chunks"). The next mount tries
// the load again.
import { useEffect, useState } from 'react';

import { createLogger } from '../lib/log';

const log = createLogger('room-push');

type RoomPushModule = typeof import('./RoomPush');

let loaded: RoomPushModule | null = null;
let loading: Promise<RoomPushModule> | null = null;

/** Loads the push chunk once (again after a failure). Tests call it up front, like the room's other chunks. */
export function loadRoomPush(): Promise<RoomPushModule> {
  loading ??= import('./RoomPush').then(
    (mod) => {
      loaded = mod;
      return mod;
    },
    (err: unknown) => {
      loading = null;
      throw err;
    },
  );
  return loading;
}

function useRoomPush(): RoomPushModule | null {
  const [mod, setMod] = useState<RoomPushModule | null>(loaded);
  useEffect(() => {
    if (mod) return undefined;
    let live = true;
    loadRoomPush().then(
      (m) => {
        if (live) setMod(m);
      },
      (err: unknown) => {
        log.warn('the push prompts could not be loaded', { error: err });
      },
    );
    return () => {
      live = false;
    };
  }, [mod]);
  return mod;
}

/** The room page's push prompts (RoomPush.tsx RoomPushPrompts), once loaded. */
export function RoomPushPrompts() {
  const mod = useRoomPush();
  return mod ? <mod.RoomPushPrompts /> : null;
}

/** The empty state's notifications card (RoomPush.tsx NotificationsCard), once loaded. */
export function RoomNotificationsCard() {
  const mod = useRoomPush();
  return mod ? <mod.NotificationsCard /> : null;
}
