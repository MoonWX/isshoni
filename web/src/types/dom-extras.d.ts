// Browser APIs that the SPA feature-detects but TypeScript's lib.dom does not declare yet (05 §2). Every member is
// optional or an optional method: code must check that it exists before using it. When lib.dom gains one of these
// with the same type, delete it here (a different type is a compile error, which is the reminder).
//
// This file is a script (no imports or exports), so the interfaces merge into lib.dom's globals.

// ---- Screen capture (05 §13.2) ----

interface DisplayMediaStreamOptions {
  /** Chrome/Edge: offer "Also share system audio" for a whole-screen pick. */
  systemAudio?: 'include' | 'exclude';
  /** Chrome/Edge: per-window audio (Windows 11, macOS 14.2+). */
  windowAudio?: 'exclude' | 'window' | 'system';
  /** Chrome/Edge: whether the picker lists the current tab (isshoni never wants its own tab). */
  selfBrowserSurface?: 'include' | 'exclude';
  /** Chrome/Edge: the "Share this tab instead" button. */
  surfaceSwitching?: 'include' | 'exclude';
  /** Chrome/Edge: whether the picker offers whole screens. */
  monitorTypeSurfaces?: 'include' | 'exclude';
  /** Chrome/Edge: pre-select the current tab. */
  preferCurrentTab?: boolean;
}

interface MediaTrackConstraintSet {
  /** Keep this document's own audio playback out of a tab/system audio capture. */
  restrictOwnAudio?: ConstrainBoolean;
  /** Mute the captured tab's local playback while it is captured. */
  suppressLocalAudioPlayback?: ConstrainBoolean;
}

interface MediaTrackSupportedConstraints {
  restrictOwnAudio?: boolean;
  suppressLocalAudioPlayback?: boolean;
}

interface MediaTrackSettings {
  restrictOwnAudio?: boolean;
  suppressLocalAudioPlayback?: boolean;
}

// ---- Client hints and iOS (05 §8, §16.3) ----

interface NavigatorUABrandVersion {
  readonly brand: string;
  readonly version: string;
}

/** User-Agent Client Hints (Chromium only). Used for `hello.client` and help text, never to gate features. */
interface NavigatorUAData {
  readonly brands: readonly NavigatorUABrandVersion[];
  readonly mobile: boolean;
  readonly platform: string;
  getHighEntropyValues(hints: string[]): Promise<Record<string, unknown>>;
}

interface Navigator {
  /** Chromium only. */
  readonly userAgentData?: NavigatorUAData;
  /** iOS Safari: true inside a Home Screen app. */
  readonly standalone?: boolean;
}

// ---- Fullscreen on iPhone (05 §12.5) ----

interface HTMLVideoElement {
  /** iPhone Safari: native fullscreen player for this video (element fullscreen is not available). */
  webkitEnterFullscreen?(): void;
  webkitExitFullscreen?(): void;
  readonly webkitSupportsFullscreen?: boolean;
  readonly webkitDisplayingFullscreen?: boolean;
}

// ---- WebRTC stats (05 §10.7) ----

interface RTCInboundRtpStreamStats {
  /** Chrome exposes it only to pages with an active capture. */
  powerEfficientDecoder?: boolean;
}

interface RTCOutboundRtpStreamStats {
  encoderImplementation?: string;
  powerEfficientEncoder?: boolean;
}

// ---- Install prompt (05 §16.3) ----

/** Chrome/Edge/Android: fired when the page can be installed; kept to show "Install app" later. */
interface BeforeInstallPromptEvent extends Event {
  readonly platforms: readonly string[];
  readonly userChoice: Promise<{ outcome: 'accepted' | 'dismissed'; platform: string }>;
  prompt(): Promise<void>;
}

interface WindowEventMap {
  beforeinstallprompt: BeforeInstallPromptEvent;
}
