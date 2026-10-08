// rooms/: the tab's signaling connection and room session (05 §7, §11), and the room's pages.
//
// This is the folder's entry module: app/router.tsx loads the page components RootRedirect and RoomPage from it by
// name (the main chunk), and renders PageUnavailable until they are exported here.
export {
  connectionBanner,
  createConnection,
  createConnectionStore,
  nextBannerChange,
  RECONNECTING_AFTER_MS,
  RELOADED_FOR_KEY,
  UNREACHABLE_AFTER_MS,
  type ConnectionBannerKind,
  type ConnectionBannerState,
  type ConnectionOptions,
  type ConnectionState,
  type ConnectionStore,
  type Stores,
} from './connection';
export { ConnectionBanner, useConnectionBanner, type ConnectionBannerProps } from './ConnectionBanner';
export { roomPath, useConnection, useRoom, useRoomRuntime, useRoomSession } from './hooks';
export { isAnnounced, roomEventMessage } from './roomEvents';
export {
  RoomSession,
  type RoomEventTap,
  type RoomSessionDeps,
  type SessionMedia,
  type ShareRecovery,
  type ShareResyncContext,
  type SubscriberDeps,
  type SubscriberLike,
} from './RoomSession';
export {
  createRoomStore,
  participantOf,
  sharesOfConnection,
  type JoinState,
  type RoomRedirect,
  type RoomStore,
  type RoomStoreState,
} from './roomStore';
export { createRoomRuntime, getRoomRuntime, type RoomRuntime, type RoomRuntimeOptions } from './runtime';
export {
  SUBSCRIBE_DEBOUNCE_MS,
  SUBSCRIBE_OFF_DELAY_MS,
  SubscriptionSync,
  type SubscriptionSyncDeps,
} from './subscriptionSync';
