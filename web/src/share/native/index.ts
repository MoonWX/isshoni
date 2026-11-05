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
//   <NativeShareUnavailable reason={caps.reason} />                       // canShare false: the empty state's note
//   <NativeShareDisabled reason={caps.reason} label="Share" />            // canShare false: the header's Share
//
// And once for the whole desktop app (its layout, every page): <MicPromptCorner />, the mic prompts in the window's
// corner while no panel or sheet shows them (03 §10.3); and <WindowNotice />, the close-to-tray notice at the top of the
// window (03 §11.6).
//
// Hand-over (WA80, open): no layout mounts these two yet, so for now the panel mounts <WindowNotice /> (a share
// stopped from the tray while the window is hidden leaves no panel, and then no notice). The integrator mounts both in
// the desktop layout and removes the panel's <WindowNotice />, and binds what the bridge lacks: App.WindowNotice and
// App.WindowNoticeShown (internal/desktop/bridge), the 'window.notice' event in BridgeEvents and NativeBridge.app in
// platform/desktop/bridge.ts, and ShareView's lockPrompt for the panel's "Resume sharing?" bar. WA84 maps
// app.App.Hotkeys to Settings.Hotkeys (bridge settings.go). Until then the notice is dead code in the real app.
import '../../i18n/lazy/desktop';

export {
  isPickAgain,
  shareErrorText,
  shareErrorView,
  shareFailureText,
  type ShareErrorAction,
  type ShareErrorView,
} from './engineErrors';
export { NativeShareButton, type NativeShareButtonProps } from './NativeShareButton';
export { MicPromptCorner } from './MicPromptCorner';
export { NativeSharePanel, type NativeSharePanelProps } from './NativeSharePanel';
export { WindowNotice, type WindowNoticeProps } from './WindowNotice';
export {
  NativeShareDisabled,
  NativeShareUnavailable,
  type NativeShareDisabledProps,
  type NativeShareUnavailableProps,
} from './NativeShareUnavailable';
