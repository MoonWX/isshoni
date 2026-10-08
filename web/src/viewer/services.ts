// The viewer's objects and how the rest of the app connects them (no React here, 05 §3):
// - createViewer() makes the store and the media registry, once per page;
// - syncRoom() gives them each room.state (RoomSession calls it, so focus and audio stay right while the room page
//   is not mounted, 05 §11.1);
// - attachViewer() gives the store the server's subscribe.status messages.
// The sub PC is the session's to make and to feed (05 §10.1, §11.1): `new SubscriberPC({platform, signal, log,
// registry: viewer.registry, store: viewer.store, ui})`, with pc.offer and pc.ice {pc: 'sub'} routed to its
// handleOffer and handleIce, and close() when the server's side is gone.
import type { SignalClient } from '../protocol/signal-client';
import { MessageTypeSubscribeStatus } from '../protocol/types.gen';
import { createMediaRegistry, type MediaRegistry } from './mediaRegistry';
import {
  createViewerStore,
  type RoomSnapshot,
  type ViewerSelf,
  type ViewerStore,
  type ViewerStoreOptions,
} from './viewerStore';

export interface ViewerServices {
  readonly store: ViewerStore;
  readonly registry: MediaRegistry;
}

export function createViewer(opts: ViewerStoreOptions = {}): ViewerServices {
  return { store: createViewerStore(opts), registry: createMediaRegistry() };
}

/**
 * Takes a room.state snapshot (01 §8.5), or null when the page is in no room: the store updates its tiles and the
 * focus (05 §12.2), and the registry drops the tracks of shares that are gone (05 §10.1).
 */
export function syncRoom(viewer: ViewerServices, room: RoomSnapshot | null, self: ViewerSelf): void {
  viewer.store.getState().syncRoom(room, self);
  viewer.registry.retain(room?.shares.map((s) => s.id) ?? []);
}

/**
 * Listens to subscribe.status (01 §8.9) for the viewer: what the server forwards per share goes to
 * viewerStore.status, which the tiles show (05 §10.4). Returns the function that stops it.
 */
export function attachViewer(signal: Pick<SignalClient, 'on'>, viewer: Pick<ViewerServices, 'store'>): () => void {
  return signal.on(MessageTypeSubscribeStatus, (status) => {
    viewer.store.getState().applyStatus(status.subs);
  });
}
