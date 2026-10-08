// The viewer (05 §10, §12): what the rest of the app uses from this folder.
//
// Wiring, for the room's controllers and page (05 §11):
//   const viewer = createViewer({ volume: prefs.volume });                 // once per page
//   const detach = attachViewer(signal, viewer);                           // subscribe.status → viewerStore.status
//   syncRoom(viewer, roomState, { userId, connectionId });                 // on every room.state; null after leaving
//   const tap = createWatchToast({ viewer, ui, selfUserId });              // room.event → "bo started sharing [Watch]"
//   <ViewerLayout viewer={viewer} localPreviews={…} empty={…} onTestConnection={…} />
// and the session's sub PC (05 §10.1): it makes one when the server's first sub offer arrives, routes pc.offer and
// pc.ice {pc: 'sub'} to handleOffer and handleIce, and calls close() when the server's side is gone (room.leave, a
// room switch, a welcome that was not resumed):
//   new SubscriberPC({ platform, signal, log, registry: viewer.registry, store: viewer.store, ui })
//
// Re-published shares (`replaces`, 01 §10.6): the store remembers the shares that ended, with what they had, so a
// share that comes back under a new id gets the stage, the pick and the sound back, also after a server restart,
// where the shares are gone for a few snapshots (01 §11.6). For that the session keeps calling syncRoom after every
// welcome, resumed or not, and calls viewer.store.getState().reset() only when the user leaves the room. The
// speaker button (S47) goes through the store's setAudible, which drops the remembered sound.
//
// Declared now, filled in by later slices ("interfaces first", docs/m1/README.md §4):
// - audioOut.ts: createAudioOut() throws NotImplementedError until S47 (the one <audio> element, tap to unmute);
// - layerPolicy.ts: desiredSubscriptions() throws NotImplementedError until S56.
// Arriving with their slices, inside this folder: TapToStart, WatchersPopover and what gives the session's
// SubscriptionSync its desired set (S47), fullscreen.ts, keyboard.ts and useVisibility.ts (S56), the Firefox codec
// wait (S86).
export { createAudioOut, type AudioOut, type AudioOutDeps } from './audioOut';
export {
  autoFocusTarget,
  focusReducer,
  initialFocusState,
  type FocusEvent,
  type FocusMode,
  type FocusShare,
  type FocusState,
} from './autoFocus';
export { ViewerContext, useShareMedia, useViewer, useViewerServices } from './context';
export { desiredSubscriptions, type LayerInputs } from './layerPolicy';
export { createMediaRegistry, NO_MEDIA, type MediaRegistry, type ShareMedia } from './mediaRegistry';
export { attachViewer, createViewer, syncRoom, type ViewerServices } from './services';
export { ShareVideo, type ShareVideoProps } from './ShareVideo';
export { Stage, type StageProps } from './Stage';
export { SubscriberPC, type SubscriberDeps, type SubscriberSignal } from './SubscriberPC';
export { Tile, type TileProps } from './Tile';
export { PHONE_LANDSCAPE_QUERY, ViewerLayout, type ViewerLayoutProps } from './ViewerLayout';
export { createWatchToast, WATCH_WAIT_MS, type WatchToastDeps } from './watchToast';
export {
  createViewerStore,
  type AudioUnlockState,
  type RoomSnapshot,
  type SubMediaState,
  type ViewerActions,
  type ViewerData,
  type ViewerSelf,
  type ViewerShare,
  type ViewerState,
  type ViewerStore,
  type ViewerStoreOptions,
} from './viewerStore';
