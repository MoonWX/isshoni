// The viewer (05 §10, §12): what the rest of the app uses from this folder.
//
// Wiring, for the room's controllers and page (05 §11):
//   const viewer = createViewer({ volume: prefs.volume, onVolume: prefs.setVolume });  // once per page
//   new RoomSession({ …, media: { createSubscriber: (deps) => viewer.createSubscriber({ ...deps, ui }) } });
//   const detach = attachViewer(signal, viewer);                           // subscribe.status → viewerStore.status
//   const unsync = attachSubscriptions(viewer, session.subscriptions);     // the desired set → subscribe.update
//   syncRoom(viewer, roomState, { userId, connectionId });                 // on every room.state; null after leaving
//   const tap = createWatchToast({ viewer, ui, selfUserId });              // room.event → "bo started sharing [Watch]"
//   <ViewerLayout viewer={viewer} localPreviews={…} empty={…} onTestConnection={…} />
//   <TapToStartPill viewer={viewer} />                                     // in the room's header (05 §10.3)
// The sub PC (05 §10.1): the session makes one through createSubscriber when the server's first sub offer arrives,
// routes pc.offer and pc.ice {pc: 'sub'} to its handleOffer and handleIce, and calls close() when the server's side
// is gone (room.leave, a room switch, a welcome that was not resumed). The viewer remembers it for the stats.
//
// What the viewer does by itself once it is wired:
// - Focus (05 §12.2, autoFocus.ts): the newest share of another person is on the stage until the user picks a
//   tile; from then on a new share only gets the toast, and auto-focus resumes when the picked share ends.
// - Audio follows focus (05 §12.3): the page's one <audio> element (audioOut.ts) plays the audible share, which is
//   the focused one unless a tile's speaker button moved the sound. Each change is one subscribe.update with every
//   share it touches (subscriptions.ts → the session's SubscriptionSync).
// - Tap to unmute (05 §10.3): when the browser refuses to play, TapToStart covers the stage, and its tap starts the
//   audio element and every refused video.
// - Watchers (05 §12.6): the eye on a tile and on the stage opens the names.
//
// Re-published shares (`replaces`, 01 §10.6): the store remembers the shares that ended, with what they had, so a
// share that comes back under a new id gets the stage, the pick and the sound back, also after a server restart,
// where the shares are gone for a few snapshots (01 §11.6). For that the session keeps calling syncRoom after every
// welcome, resumed or not, and calls viewer.store.getState().reset() only when the user leaves the room. The
// speaker button goes through the store's setAudible, which drops the remembered sound.
//
// Stats (05 §10.7): viewerStatsSources(viewer) is the sub PC for lib/stats' collector, and viewerDebugState(viewer)
// the viewer's part of window.__isshoni.state(); see lib/stats/index.ts for the wiring.
//
// Arriving with later slices, inside this folder: what feeds the layer policy beyond "mounted" (IntersectionObserver
// in useVisibility.ts, the hidden page, fullscreen, PiP), fullscreen.ts, keyboard.ts and the `?focus=` link (S56);
// the Firefox codec wait (S86).
export { SpeakerButton, StageSound, type SpeakerButtonProps, type StageSoundProps } from './AudioControls';
export { canSetVolume, createAudioOut, type AudioOut, type AudioOutDeps } from './audioOut';
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
export { desiredSubscriptions, PAGE_HIDDEN_OFF_MS, type LayerInputs } from './layerPolicy';
export { createMediaRegistry, NO_MEDIA, type MediaRegistry, type ShareMedia } from './mediaRegistry';
export { attachViewer, createViewer, syncRoom, type ViewerOptions, type ViewerServices } from './services';
export { ShareVideo, type ShareVideoProps } from './ShareVideo';
export { Stage, type StageProps } from './Stage';
export { viewerDebugState, viewerStatsSources, type ViewerDebugState } from './stats';
export { SubscriberPC, type SubscriberDeps, type SubscriberSignal } from './SubscriberPC';
export { attachSubscriptions, layerInputs, viewerWants, type SubscriptionSink } from './subscriptions';
export { TapToStart, TapToStartPill, type TapToStartPillProps, type TapToStartProps } from './TapToStart';
export { Tile, type TileProps } from './Tile';
export { markShown, useVisibility } from './useVisibility';
export { createVideoPlayback, type VideoPlayback } from './videoPlayback';
export { PHONE_LANDSCAPE_QUERY, ViewerLayout, type ViewerLayoutProps } from './ViewerLayout';
export { createWatchToast, WATCH_WAIT_MS, type WatchToastDeps } from './watchToast';
export { WatchersPopover, type WatchersPopoverProps } from './WatchersPopover';
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
  type ViewerWatcher,
} from './viewerStore';
