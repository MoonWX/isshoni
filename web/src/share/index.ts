// The web sharer (05 §13): what the rest of the app uses from this folder.
//
// Wiring, for the room page (05 §11.2). The provider itself needs none: BrowserPlatform makes it
// (platform.sharing), and the room session publishes through it (RoomSession.startShare).
//   const share = session.share;                                  // and session.on('share', …) for changes
//   <ShareButton
//     onStart={(src, opts) => session.startShare(src, opts)}
//     elsewhere={other && { onStop: () => signal.request('share.stop', { shareId: other.id }) }}
//   />                                                             // other = findShareElsewhere(room.state…)
//   <SharePanel
//     share={share}
//     onStop={() => session.stopShare()}
//     watchers={shareWatchers(roomState, share?.shareId)}
//     onTestConnection={…}
//   />
//   <ViewerLayout localPreviews={share ? { [share.shareId]: share.preview } : {}} … />   // viewer/
//
// Who does what:
// - shareStore: the share state machine of the page (05 §13.1). The Share buttons drive its first half (picking,
//   the whole-screen warning), BrowserSharing and its BrowserShare the second (starting, live, reconnecting,
//   stopping, failed); the panel is a view of it.
// - BrowserSharing.start: share.start, then the share's tracks on the pub PC (PublisherPC: transceivers with
//   complete encodings, codec preferences, gen and neg, recovery). It routes what the server sends about the pub PC
//   and about its shares itself; the room session only starts and stops the share and tells it what only the
//   session sees (the session's ShareRecovery seam): that the server ended it (BrowserShare.serverEnded), and that a
//   reconnect needs it published again (resync and republish: share.start {replaces}, 01 §10.6).
// - recovery.ts: the 60 s capture hold, and share.start retried with the same ref across a resumed drop.
// - uiStore.sharing ("no Reload in the update pill while sharing", 05 §16.2), the toast of a share that was stopped
//   from another tab, the app's "Can't connect media" screen when the pub PC's negotiation failed twice within
//   a minute (05 §9), and "You're live" and "Sharing stopped" for screen readers (05 §16.6) follow the share state
//   through linkShareUi, which every Share button and panel sets up when it mounts. It keeps working after the room
//   page is left.
// - stats.ts: the page's pub PC as a source of lib/stats' collector (05 §10.7), which PublisherPC lists while it has
//   a PC: `sources: () => [...viewerStatsSources(viewer), ...shareStatsSources()]`.
export {
  BrowserShare,
  END_NOTICE_GRACE_MS,
  PubNegotiationFailedError,
  ShareEndedError,
  STATS_SAMPLE_MS,
  type BrowserShareDeps,
} from './BrowserShare';
export { BrowserSharing, type BrowserSharingDeps } from './BrowserSharing';
export { audioCodecPreferences, canSendH264, H264_PROFILE_ORDER, videoCodecPreferences } from './codecPrefs';
export { findShareElsewhere } from './elsewhere';
export {
  applyEncodings,
  buildSendEncodings,
  buildSingleEncoding,
  needsRescale,
  scaleDownBy,
  sourceSize,
  type SourceSize,
} from './encodings';
export { createHintDetector, roundHintHeight, type HintDetector, type HintSample } from './hints';
export { LevelMeter, type LevelMeterProps } from './LevelMeter';
export { noAudioNote } from './notes';
export { applyVideoContentHint, PRESET_HINTS, presetHints, PRESETS, type PresetHints } from './presets';
export { kindText, presetText } from './presetText';
export { PublisherPC, type PublisherPCDeps, type PublisherSignal, type PubMediaState } from './PublisherPC';
export { CAPTURE_HOLD_MS } from './recovery';
export { ScreenAudioWarning, type ScreenAudioWarningProps } from './ScreenAudioWarning';
export { ShareButton, ShareUnavailableNote, type ShareButtonProps } from './ShareButton';
export { SharePanel, type SharePanelProps } from './SharePanel';
export { ShareSheet, type ShareElsewhere, type ShareSheetProps } from './ShareSheet';
export { addShareStatsPublisher, shareStatsSources, type ShareStatsPublisher } from './stats';
export {
  createShareStore,
  isSharingPhase,
  ShareCancelledError,
  shareStore,
  type PickedInfo,
  type ShareFlowOutcome,
  type ShareNotice,
  type ShareOutcome,
  type SharePhase,
  type ShareState,
  type ShareStore,
  type StartShare,
} from './shareStore';
export { linkShareUi, shareErrorMessage } from './shareUi';
export { shareWatchers, type ShareWatcher } from './watchers';
