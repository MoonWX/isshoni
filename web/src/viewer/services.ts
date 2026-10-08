// The viewer's objects and how the rest of the app connects them (no React here, 05 §3):
// - createViewer() makes the store, the media registry and the page's one <audio> element, once per page; the
//   element plays the audible share by itself (audio follows focus, 05 §12.3);
// - syncRoom() gives them each room.state (RoomSession calls it, so focus and audio stay right while the room page
//   is not mounted, 05 §11.1);
// - attachViewer() gives the store the server's subscribe.status messages;
// - attachSubscriptions() (subscriptions.ts) gives the session's SubscriptionSync the desired set;
// - viewer.createSubscriber() makes the session's sub PC (05 §10.1, §11.1): it is SessionMedia.createSubscriber.
//   The session routes pc.offer and pc.ice {pc: 'sub'} to the PC's handleOffer and handleIce, and calls close()
//   when the server's side is gone. The viewer remembers the PC for the stats (stats.ts).
//
// This module is in the app's initial bundle (the room runtime makes the viewer before any page shows), and
// SubscriberPC is not: its class comes with the room's media chunk (rooms/media.ts), so createSubscriber takes it
// from the caller, and it is only a type here.
import type { Logger } from '../lib/log';
import type { SignalClient } from '../protocol/signal-client';
import { MessageTypeSubscribeStatus } from '../protocol/types.gen';
import { createAudioOut, type AudioOut } from './audioOut';
import { createMediaRegistry, type MediaRegistry } from './mediaRegistry';
import type { SubscriberDeps, SubscriberPC } from './SubscriberPC';
import { createVideoPlayback, type VideoPlayback } from './videoPlayback';
import {
  createViewerStore,
  type RoomSnapshot,
  type ViewerSelf,
  type ViewerStore,
  type ViewerStoreOptions,
} from './viewerStore';

/** The SubscriberPC class, as createSubscriber takes it. */
export type SubscriberClass = new (deps: SubscriberDeps) => SubscriberPC;

export interface ViewerServices {
  readonly store: ViewerStore;
  readonly registry: MediaRegistry;
  /** The page's one <audio> element (05 §10.2). It follows viewerStore.audibleShareId and .volume by itself. */
  readonly audio: AudioOut;
  /** The tiles' <video> elements: which of them the browser refused to play (05 §10.3). */
  readonly videos: VideoPlayback;
  /** The sub PC that createSubscriber() made last; null before the first. */
  readonly subscriber: SubscriberPC | null;
  /**
   * Makes the session's sub PC, with the viewer's registry and store: SessionMedia.createSubscriber (05 §10.1),
   * `(deps) => viewer.createSubscriber({ ...deps, ui }, SubscriberPC)`. `ui` shows the Fatal screen "Can't connect
   * media". The class is the second argument, so that this module doesn't import it.
   */
  createSubscriber(deps: Omit<SubscriberDeps, 'registry' | 'store'>, Subscriber: SubscriberClass): SubscriberPC;
  /**
   * The tap of TapToStart (05 §10.3): audio.play() and play() on every refused video, synchronously, so call it
   * inside the user gesture. It picks nothing: the unmute tap is not a manual focus (05 §12.2).
   */
  unlock(): void;
  /** Stops the audio from following the store and removes its element. Tests only: the page keeps its viewer. */
  dispose(): void;
}

export interface ViewerOptions extends ViewerStoreOptions {
  /** The user changed the volume: prefsStore.setVolume (05 §10.3). */
  onVolume?: (volume: number) => void;
  /** Where the <audio> element is appended; default document.body. */
  audioParent?: HTMLElement;
  log?: Logger;
}

export function createViewer(opts: ViewerOptions = {}): ViewerServices {
  const store = createViewerStore(opts);
  const registry = createMediaRegistry();
  const audio = createAudioOut({
    store,
    ...(opts.audioParent ? { parent: opts.audioParent } : {}),
    ...(opts.log ? { log: opts.log } : {}),
  });
  const videos = createVideoPlayback({ store });

  // Audio follows focus (05 §12.3): the element plays the audio track of the audible share, whichever comes
  // first, the choice or the track.
  let offTrack: (() => void) | null = null;
  const playAudible = (): void => {
    const shareId = store.getState().audibleShareId;
    audio.setTrack(shareId === null ? null : (registry.get(shareId).audio ?? null));
  };
  const followAudible = (): void => {
    offTrack?.();
    const shareId = store.getState().audibleShareId;
    offTrack = shareId === null ? null : registry.subscribe(shareId, playAudible);
    playAudible();
  };
  const offStore = store.subscribe((s, prev) => {
    if (s.audibleShareId !== prev.audibleShareId) followAudible();
    if (s.volume !== prev.volume) {
      audio.setVolume(s.volume);
      opts.onVolume?.(s.volume);
    }
  });
  followAudible();

  let subscriber: SubscriberPC | null = null;

  return {
    store,
    registry,
    audio,
    videos,
    get subscriber() {
      return subscriber;
    },
    createSubscriber(deps, Subscriber) {
      subscriber = new Subscriber({ ...deps, registry, store });
      return subscriber;
    },
    unlock() {
      // Both synchronously: the gesture covers every play() made before the handler returns.
      void audio.unlock().catch(() => undefined); // still refused: the state stays `blocked`
      videos.retry();
    },
    dispose() {
      offStore();
      offTrack?.();
      offTrack = null;
      audio.dispose();
    },
  };
}

/**
 * Takes a room.state snapshot (01 §8.5), or null when the page is in no room: the store updates its tiles and the
 * focus (05 §12.2), and the registry drops the tracks of shares that are gone (05 §10.1).
 */
export function syncRoom(
  viewer: Pick<ViewerServices, 'store' | 'registry'>,
  room: RoomSnapshot | null,
  self: ViewerSelf,
): void {
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
