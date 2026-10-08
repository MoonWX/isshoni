// The viewer's side of the room runtime (05 §10, §12): what connects viewer/'s store and media registry to the
// session. It runs for the life of the runtime, not of the room page, so focus and audio stay right while the page
// isn't mounted (05 §11.1 "The session is app-level"). No React here (05 §3).
//
// - Every room.state the session stores goes to the viewer (syncRoom): tiles, focus, and the tracks of shares that
//   are gone. That includes the snapshots after a welcome that wasn't resumed, so re-published shares take over
//   the stage and the sound of the ones they replace (01 §10.6).
// - When the session has no snapshot anymore, the user is out of that room (leave(), a room switch, a room the
//   server closed or refused): the viewer goes back to its empty state, and forgets the shares it remembered.
// - subscribe.status reaches viewerStore.status (attachViewer).
// - "bo started sharing [Watch]" (createWatchToast) takes the session's announcement over while the user has picked
//   a tile (owner decision, 05 §24.1).
import type { UiStore } from '../app/uiStore';
import type { SignalClient } from '../protocol/signal-client';
import { attachViewer, syncRoom, type ViewerServices } from '../viewer/services';
import type { ViewerSelf } from '../viewer/viewerStore';
import { createWatchToast } from '../viewer/watchToast';
import type { RoomSession } from './RoomSession';
import type { RoomStore, RoomStoreState } from './roomStore';

export interface ConnectViewerDeps {
  viewer: ViewerServices;
  signal: Pick<SignalClient, 'on'>;
  session: Pick<RoomSession, 'onRoomEvent'>;
  room: RoomStore;
  ui: UiStore;
}

/** Nobody: for the sync that empties the viewer, which reads no ids. */
const NO_SELF: ViewerSelf = { userId: '', connectionId: '' };

/** Connects the viewer to the session. Returns the function that disconnects it. */
export function connectViewer({ viewer, signal, session, room, ui }: ConnectViewerDeps): () => void {
  const sync = (s: RoomStoreState, prev: RoomStoreState): void => {
    // Only a new snapshot. The connection's ids change with a welcome, before the room.state that follows it; the
    // snapshot of before is not read again with them (its shares belong to the old connection id).
    if (s.state === prev.state) return;
    if (s.state === null) {
      syncRoom(viewer, null, NO_SELF);
      viewer.store.getState().reset();
      return;
    }
    // Both ids are set by the welcome that came before any room.state.
    syncRoom(viewer, s.state, { userId: s.userId ?? '', connectionId: s.connectionId ?? '' });
  };

  const offs = [
    room.subscribe(sync),
    attachViewer(signal, viewer),
    session.onRoomEvent(createWatchToast({ viewer, ui, selfUserId: () => room.getState().userId })),
  ];
  return () => {
    for (const off of offs.splice(0)) off();
  };
}
