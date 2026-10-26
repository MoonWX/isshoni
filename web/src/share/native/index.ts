// share/native/: the desktop app's native share UI (docs/m2/03 §10.2–§10.5): the Share button with the desktop
// ShareSheet, the first-share sheet and the Do-not-disturb tip, the share panel with its Details disclosure, the
// failures of §10.5 with what to do about them, and the note where Share would be on a PC that can't share. The share
// itself is platform/desktop's DesktopSharing and DesktopShare.
//
// A chunk of its own, which the room page loads only when platform.sharing.mode is 'native' (rooms/lazyNative.ts):
// nothing of it is in the browser's bundle or in the entry chunk (app/chunks.node.test.ts). Its texts are the lazy
// `desktop` namespace, which this module adds before any of its components renders.
//
// Wiring, in the room page (rooms/RoomPage.tsx, rooms/RoomStage.tsx):
//   <NativeShareButton onStart={startShare} elsewhere={elsewhere} />      // instead of M1's ShareButton
//   <NativeSharePanel share={share} onStop={stopShare} watchers={watchers} />   // instead of M1's SharePanel
//   <NativeShareUnavailable reason={caps.reason} />                       // canShare false
import '../../i18n/lazy/desktop';

export {
  isPickAgain,
  shareErrorText,
  shareErrorView,
  type ShareErrorAction,
  type ShareErrorView,
} from './engineErrors';
export { NativeShareButton, type NativeShareButtonProps } from './NativeShareButton';
export { NativeSharePanel, type NativeSharePanelProps } from './NativeSharePanel';
export { NativeShareUnavailable, type NativeShareUnavailableProps } from './NativeShareUnavailable';
