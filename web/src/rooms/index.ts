// rooms/: the tab's signaling connection and room session (05 §7, §11), and the room's pages.
//
// This is the folder's entry module: app/router.tsx loads the page components RootRedirect and RoomPage from it by
// name, as one lazy chunk like every page folder (05 §5). InRoomBar is for the app's layout, which gets it from
// the router once this folder is loaded: it shows the session on the other pages (05 §11.1).
//
// The room's media code (the stage, the Share button, the sub PC controller: rooms/media.ts) is a lazy chunk and
// is deliberately not exported from here; the page and the runtime load it themselves.
export { AccountMenu, type AccountMenuProps } from './AccountMenu';
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
export { connectViewer, type ConnectViewerDeps } from './connectViewer';
export { isRoomPath, roomPath, useConnection, useLocalShare, useRoom, useRoomRuntime, useRoomSession } from './hooks';
export { InRoomBar } from './InRoomBar';
export { PeoplePanel, type PeoplePanelProps } from './PeoplePanel';
export { isAnnounced, roomEventMessage } from './roomEvents';
export { RoomHeader, type RoomHeaderProps } from './RoomHeader';
export { RoomPage } from './RoomPage';
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
export { fetchRooms, homeRoomId, roomOf, roomsQueryOptions, useRooms } from './roomsQuery';
export {
  createRoomStore,
  participantOf,
  sharesOfConnection,
  type JoinState,
  type RoomRedirect,
  type RoomStore,
  type RoomStoreState,
} from './roomStore';
export { RoomSwitcher, type RoomSwitcherProps } from './RoomSwitcher';
export { liveShareCount, roomTitle, useDocumentTitle, type RoomTitleInput } from './roomTitle';
export { RootRedirect } from './RootRedirect';
export {
  createRoomRuntime,
  getRoomRuntime,
  peekRoomRuntime,
  type RoomRuntime,
  type RoomRuntimeOptions,
} from './runtime';
export {
  SUBSCRIBE_DEBOUNCE_MS,
  SUBSCRIBE_OFF_DELAY_MS,
  SubscriptionSync,
  type SubscriptionSyncDeps,
} from './subscriptionSync';
