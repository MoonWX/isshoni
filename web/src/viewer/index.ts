// The viewer (05 §10, §12): what the rest of the app uses from this folder.
//
// Wiring, for the room's controllers and page (05 §11):
//   const viewer = createViewer({ volume: prefs.volume, onVolume: prefs.setVolume });  // once per page
//   new RoomSession({ …, media: { createSubscriber } });                   // 05 §10.1, with
//     createSubscriber = (deps) => viewer.createSubscriber({ ...deps, ui }, SubscriberPC)
//   const detach = attachViewer(signal, viewer);                           // subscribe.status → viewerStore.status
//   const unsync = attachSubscriptions(viewer, session.subscriptions);     // the desired set → subscribe.update
//   syncRoom(viewer, roomState, { userId, connectionId });                 // on every room.state; null after leaving
//   const tap = createWatchToast({ viewer, ui, selfUserId });              // room.event → "bo started sharing [Watch]"
//   const unfreeze = attachFreezeWatch(viewer, statsCollector);            // lib/stats' samples → viewerStore.frozen
//   <ViewerLayout viewer={viewer} localPreviews={…} empty={…} onTestConnection={…} roomName={…} />
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
// - Layers (05 §12.4): the stage's share is asked for in high, a tile in low while at least 10% of it is in view
//   (useVisibility.ts), and nothing has video under a fullscreen stage or once the page was hidden for 10 s
//   (page.ts, which createViewer attaches); the PiP share keeps the high layer.
// - While the room page shows the layout (ViewerLayout.tsx), each only where the platform can do it (05 §8):
//   fullscreen (F, a double click or tap on the stage, its button; fullscreen.ts), picture-in-picture (pip.ts), the
//   screen wake lock while a share is watched (wakeLock.ts), the keyboard (keyboard.ts: the tiles as one tab stop,
//   the shortcuts and their dialog), `?focus=<shareId>` from the URL (focusParam.ts: the share is waited for 5 s
//   from the room's first snapshot, viewerStore.inRoom, so a link that opens the app cold is not dropped while the
//   page joins), what plays for the system's media notification (mediaSession.ts), and the one-time hint on iOS
//   (IosHint.tsx).
// - When the page comes back (05 §12.8): iOS suspended it, and every video and the audio element are played
//   again; a refusal shows TapToStart.
//
// Re-published shares (`replaces`, 01 §10.6): the store remembers the shares that ended, with what they had, so a
// share that comes back under a new id gets the stage, the pick and the sound back, also after a server restart,
// where the shares are gone for a few snapshots (01 §11.6). For that the session keeps calling syncRoom after every
// welcome, resumed or not, and calls viewer.store.getState().reset() only when the user leaves the room. The
// speaker button goes through the store's setAudible, which drops the remembered sound.
//
// Stats (05 §10.7): viewerStatsSources(viewer) is the sub PC for lib/stats' collector, and viewerDebugState(viewer)
// the viewer's part of window.__isshoni.state(); see lib/stats/index.ts for the wiring. The collector's samples are
// also what tells a frozen tile (05 §12.6): attachFreezeWatch(viewer, collector), by whoever owns the collector.
// Until that is called no tile ever says "Waiting for video…".
//
// The layout takes the platform and the preferences from the app's context. The one thing the room page can add is
// `roomName`, the "artist" of the media notification; without it the app's name stands in.
//
// Arriving with later slices, inside this folder: the Firefox codec wait (S86). The debug overlay that Shift+D
// switches (prefsStore.debug) is the hardening slice's (05 W13).
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
export { FocusFromUrl, type FocusFromUrlProps } from './FocusFromUrl';
export { FOCUS_PARAM, FOCUS_PARAM_WAIT_MS, followFocusParam } from './focusParam';
export { attachFreezeWatch, createFreezeDetector, FREEZE_AFTER_MS, freezeWatched, type FreezeDetector } from './freeze';
export {
  createFullscreen,
  DOUBLE_TAP_MS,
  DOUBLE_TAP_PX,
  NATIVE_EXIT_RETRY_MS,
  onDoublePress,
  type FullscreenController,
  type FullscreenDeps,
  type FullscreenMode,
  type FullscreenTarget,
} from './fullscreen';
export { IOS_HINT_ID, IosHint, type IosHintProps } from './IosHint';
export {
  attachShortcuts,
  isTextField,
  moveIndex,
  moveTileFocus,
  SHORTCUT_ROWS,
  shortcutAllowed,
  shortcutFor,
  TILE_LIST,
  TILE_MAIN,
  type KeyLike,
  type MoveTo,
  type ShortcutAction,
  type ShortcutRow,
  type ShortcutsDeps,
} from './keyboard';
export { desiredSubscriptions, PAGE_HIDDEN_OFF_MS, type LayerInputs } from './layerPolicy';
export { attachMediaSession, type MediaSessionDeps } from './mediaSession';
export { createMediaRegistry, NO_MEDIA, type MediaRegistry, type ShareMedia } from './mediaRegistry';
export { attachPage, type PageDeps } from './page';
export { createPip, type PipController, type PipDeps, type PipTarget } from './pip';
export {
  attachViewer,
  createViewer,
  syncRoom,
  type SubscriberClass,
  type ViewerOptions,
  type ViewerServices,
} from './services';
export { ShareVideo, type ShareVideoProps } from './ShareVideo';
export { ShortcutsDialog, type ShortcutsDialogProps } from './ShortcutsDialog';
export { Stage, type StageProps } from './Stage';
export { StageControls, type StageControlsProps } from './StageControls';
export { viewerDebugState, viewerStatsSources, type ViewerDebugState } from './stats';
export { SubscriberPC, type SubscriberDeps, type SubscriberSignal } from './SubscriberPC';
export { attachSubscriptions, layerInputs, viewerWants, type SubscriptionSink } from './subscriptions';
export { TapToStart, TapToStartPill, type TapToStartPillProps, type TapToStartProps } from './TapToStart';
export { Tile, type TileProps } from './Tile';
export { STAGE, useCapabilities, useFullscreen, useMediaSession, usePip, useWakeLock } from './useControls';
export { markShown, showWhileVisible, useVisibility, VISIBLE_RATIO, watchVisible } from './useVisibility';
export { createVideoPlayback, type VideoPlayback } from './videoPlayback';
export { PHONE_LANDSCAPE_QUERY, ViewerLayout, type ViewerLayoutProps } from './ViewerLayout';
export { createWakeLock, isWatching, type WakeLockController, type WakeLockDeps } from './wakeLock';
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
