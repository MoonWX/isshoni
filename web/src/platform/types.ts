// The Platform adapter (05 §8): everything the SPA needs from where it runs. BrowserPlatform is M1's implementation;
// the desktop app (M2) bundles this same SPA with DesktopPlatform (platform/desktop/, docs/m2/03 §7). Outside
// platform/, nothing reads cookies or location.origin, builds /api URLs by hand, or constructs RTCPeerConnection or
// WebSocket URLs.
//
// Wire types come from 01's types.gen.ts. The interfaces are declared in full ("interfaces first", README §4). M2's
// changes (docs/m2/03 §7.1) are all additive for BrowserPlatform, which implements the new optional members as
// absent.
import type { ClientNotifications, ClientRequests, ServerEnvelope, ServerMessages } from '../protocol/registry.gen';
import type { HelloAuthProvider } from '../protocol/signal-client';
import type {
  Caps,
  ClientInfo,
  ClientOS,
  MessageTypeHello,
  Preset,
  ShareKind,
  ShareParams,
} from '../protocol/types.gen';
import type { AuthState, NativeBridge } from './desktop/bridge';

export type PlatformKind = 'browser' | 'desktop' | 'mobile';
export type PushSupport = 'supported' | 'needs-install' | 'denied' | 'unsupported';
export type InstallState = 'prompt' | 'ios-manual' | 'installed' | 'none';

export interface PlatformCapabilities {
  /** 01 detectCaps(): decode/encode CodecKeys, simulcast, displayCapture. */
  caps: Caps;
  /** sharing !== null and usable on this device. */
  canShare: boolean;
  fullscreen: 'element' | 'video-only' | 'none';
  /** false on iOS in M1 (plan: PiP unreliable). */
  pip: boolean;
  wakeLock: boolean;
  push: PushSupport;
  install: InstallState;
}

/** Both stores wrap Web Storage in try/catch and fall back to memory (private windows, blocked site data). */
export interface KeyValueStore {
  get(k: string): string | null;
  set(k: string, v: string): void;
  remove(k: string): void;
}

export interface WakeLockHandle {
  release(): Promise<void>;
}

/**
 * What the VersionMismatch screen offers (05 §16.4): the browser reloads; the desktop app (M2) updates itself or opens
 * the current room in the system browser (docs/m2/03 §16.3). The desktop has no reload: its SPA is the app's own
 * build, which a reload can't change (no stale-build reload, docs/m2/02 §11.8).
 */
export interface VersionActions {
  reload?: () => void;
  openInBrowser?: () => void;
  /** Checks for an app update and installs a ready one; resolves with what happened, for the screen's text. */
  updateApp?: () => Promise<AppUpdateOutcome>;
}

/** What VersionActions.updateApp did (docs/m2/03 §16.3). */
export type AppUpdateOutcome =
  /** Updates.Install() ran: the app quits within 5 s and the installer starts it again. */
  | { kind: 'installing' }
  /** "No newer version is available yet", with a link to serverOrigin + '/download'. */
  | { kind: 'up-to-date' }
  /** A build run from a folder (update error not_installed): notesUrl was opened in the browser. */
  | { kind: 'not-installed'; notesUrl: string }
  /** Any other update error (desktop.update.error.<code>), or the code of a bridge error. */
  | { kind: 'error'; code: string };

/**
 * Gives the hello.auth of the next hello (docs/m2/03 §7.1): the SignalClient awaits it before every hello and, once
 * after a session-scope unauthenticated, calls it with forceRefresh (docs/m2/02 §11.8). undefined: no token, and the
 * hello goes without auth. Declared with the SignalClient, whose option it is.
 */
export type { HelloAuthProvider };

export interface Platform {
  readonly kind: PlatformKind;
  /** → hello.client */
  readonly client: ClientInfo;
  /** browser: 'full' when sharing is possible; desktop SPA: 'viewer' (M2). */
  readonly role: 'full' | 'viewer';
  /** browser: location.origin; desktop: the active server from bridge.js, '' before the first server is added. */
  readonly serverOrigin: string;
  /** Synchronous 01 detectCaps(). */
  capsNow(): Caps;
  capabilities(): Promise<PlatformCapabilities>;
  /** A request to the server's API; path is resolved against serverOrigin and carries the platform's credentials. */
  apiFetch(path: `/api/${string}`, init?: RequestInit): Promise<Response>;
  /**
   * url = new URL('/ws', serverOrigin) with ws(s):. auth: the desktop's bearer provider for hello.auth (01 D2,
   * docs/m2/03 §7.1); the browser gives none (its cookie authenticates the upgrade). Was `auth?: HelloAuth` (unused).
   */
  signaling(): { url: string; auth?: HelloAuthProvider };
  createPeerConnection(config: RTCConfiguration): RTCPeerConnection;
  /** null: can't share here (phones in M1). */
  readonly sharing: SharingProvider | null;
  readonly notifications: NotificationsProvider;
  /** browser only */
  readonly pwa: PwaProvider | null;
  requestWakeLock(): Promise<WakeLockHandle | null>;
  /** A chat/social app's built-in browser (known UA token), else null. Chooses help text only. */
  inAppBrowser(): string | null;
  openExternal(url: string): void;
  readonly storage: { local: KeyValueStore; session: KeyValueStore };
  versionActions(): VersionActions;
  /**
   * M2: the desktop app's features; null (or absent) in the browser. Optional, so that the M1 test platforms (test/,
   * rooms/, viewer/, account/) stay valid: read it as `platform.desktop ?? null`. docs/m2/03 §7.1 makes it required
   * once every test platform sets `desktop: null` (WA32's blocked items).
   */
  readonly desktop?: DesktopFeatures | null;
}

// ---- desktop (M2, docs/m2/03 §7.1) ----

/** The steps of the desktop's first-run wizard (docs/m2/03 §8.2). */
export type WizardStep = 'link' | 'audio-permission' | 'sound' | 'voice';

/** The codes C1 hands to Auth.SessionEnded (docs/m2/02 §11.7). */
export type SessionEndCode = 'unauthenticated' | 'session_revoked' | 'account_disabled';

/** A hello that the server refused for its version (docs/m2/02 §11.7, §13.2). */
export interface HelloRefusal {
  code: 'client_outdated' | 'protocol_unsupported';
  params?: Record<string, unknown>;
}

/**
 * What the room runtime does after this page's SignalClient (C1) stopped (docs/m2/03 §7.3, 02 §11.8):
 * - redial: Auth.SessionEnded says the server is still linked: start C1 again (once);
 * - relink: it isn't; the relink page follows auth.state (docs/m2/03 §8.3);
 * - version: Auth.HelloRefused's state drives 02 §13.3's screen (docs/m2/03 §16.3);
 * - wait: the bridge answered not_found (this page's server isn't active any more): stay stopped, the window reloads;
 * - none: not a desktop case; M1's handling applies.
 */
export type SignalStopAction = 'redial' | 'relink' | 'version' | 'wait' | 'none';

export interface DesktopFeatures {
  readonly app: { version: string; os: ClientOS; osBuild: number };
  /** The active server from bridge.js; null before the first server is added. */
  readonly server: { id: string; origin: string; name: string } | null;
  /** The native side (docs/m2/03 §7.4). A call made before the transport has loaded waits for it. */
  readonly bridge: NativeBridge;
  /** Windows: ['link', 'sound', 'voice']; later (M3) macOS adds 'audio-permission'. */
  wizardSteps(): WizardStep[];
  /** Auth.SessionEnded with this page's serverOrigin (docs/m2/02 §11.8). */
  sessionEnded(code: SessionEndCode): Promise<AuthState>;
  /** Auth.HelloRefused with this page's serverOrigin (docs/m2/02 §11.8). */
  helloRefused(e: HelloRefusal): Promise<AuthState>;
  /**
   * C1 stopped with err (its SignalStateInfo.error): calls sessionEnded for session_revoked, account_disabled and
   * unauthenticated (the SignalClient has already made its one forced refresh), helloRefused for client_outdated and
   * protocol_unsupported, and says what the room runtime does next (SignalStopAction).
   */
  signalStopped(err: { code: string; params?: Readonly<Record<string, unknown>> }): Promise<SignalStopAction>;
}

// ---- sharing ----

/**
 * The part of 01's SignalClient (protocol/signal-client.ts, S20) that sharing uses, as a structural type: SignalClient
 * satisfies it, and platform/ doesn't depend on the class. The signatures are SignalClient's own (01 §16): request()
 * never sends hello (the client does), and a listener gets the envelope of its own message type.
 * ShareContext.signal stays this structural type (05 §8); types.test.ts checks at compile time that SignalClient
 * fits it.
 */
export interface SignalClientLike {
  request<K extends Exclude<keyof ClientRequests, typeof MessageTypeHello>>(
    type: K,
    data: ClientRequests[K]['data'],
    opts?: { timeoutMs?: number },
  ): Promise<ClientRequests[K]['result']>;
  notify<K extends keyof ClientNotifications>(type: K, data: ClientNotifications[K]): boolean;
  on<K extends keyof ServerMessages>(
    type: K,
    fn: (data: ServerMessages[K], env: ServerEnvelope<K>) => void,
  ): () => void;
  probe(): void;
}

export interface PickedSource {
  /** 'screen' | 'window' | 'tab' (01); never a window title. */
  kind: ShareKind;
  /** native (M2): 'system' (everything but the exclusions) or 'none'. */
  audioScope: 'window' | 'tab' | 'system' | 'none';
  /** native (M2): null (voice apps are kept out). */
  warning: 'screen-with-system-audio' | 'no-audio' | null;
  /**
   * Owned by the provider. docs/m2/03 §7.1 makes it `MediaStream | null` (null for a native pick). Not done yet:
   * share/'s readers (BrowserSharing, BrowserShare, shareStore, SharePanel) and rooms/RoomPage assume a stream, and no
   * slice's files cover both them and this one, so the change waits for the integrator to assign it (WA32's blocked
   * items).
   */
  preview: MediaStream;
  /** M2, native: the picker's name for the source; shown on this PC only, never sent. */
  label?: string;
  /** M2, native: one-time sheets to show before going live, in order (docs/m2/03 §10.2). */
  confirm?: ('first-share' | 'dnd')[];
  /** Stop tracks if the user backs out at the warning. */
  release(): void;
}

export interface ShareContext {
  signal: SignalClientLike;
  roomId: string;
}

export interface ShareStatsSample {
  /** performance.now() */
  at: number;
  layers: {
    rid: string;
    width?: number;
    height?: number;
    fps?: number;
    kbps: number;
    limit?: 'none' | 'bandwidth' | 'cpu' | 'other';
    encoder?: string;
    hw?: boolean;
  }[];
  audioKbps: number;
}

/** 'paused': a native share on a server with share.pause (docs/m2/03 §7.1). */
export type LocalShareState = 'starting' | 'live' | 'paused' | 'reconnecting' | 'ended';
export type LocalEndReason =
  | 'user'
  | 'browser-stopped'
  | 'server'
  | 'server-unreachable'
  | 'error'
  // M2, native (docs/m2/03 §7.1):
  | 'source-gone'
  | 'system'
  | 'unlinked';
export type ShareHint = { kind: 'upload-limited'; approxHeight: number } | { kind: 'cpu-limited' };

export interface ActiveShare {
  /** Changes on re-publish (`replaces`, 01 §10.6); watch 'state'. */
  readonly shareId: string;
  readonly kind: ShareKind;
  readonly preview: MediaStream | null;
  readonly state: LocalShareState;
  /** Latest from share.start / share.update / quality.hint. */
  readonly params: ShareParams | null;
  on(ev: 'state', fn: (s: LocalShareState) => void): () => void;
  on(ev: 'ended', fn: (r: LocalEndReason) => void): () => void;
  on(ev: 'hint', fn: (h: ShareHint | null) => void): () => void;
  setPreset(p: Preset): Promise<void>;
  /** M2: a native share on a server with share.pause (docs/m2/03 §7.1); the browser rejects with not_supported. */
  setPaused(paused: boolean): Promise<void>;
  setAudioEnabled(on: boolean): Promise<void>;
  stop(): Promise<void>;
  stats(): Promise<ShareStatsSample | null>;
}

export interface SharingProvider {
  /** browser: 'in-page'; desktop app and Linux agent: 'native' (M2/M4). */
  readonly mode: 'in-page' | 'native';
  /** Call synchronously from the click handler (transient user activation). null = cancelled. */
  pick(opts: { preset: Preset }): Promise<PickedSource | null>;
  start(src: PickedSource, opts: { preset: Preset; withAudio: boolean }, ctx: ShareContext): Promise<ActiveShare>;
  /** M2, native: the share the app publishes now (e.g. after the page reloaded), or null. */
  current?(): ActiveShare | null;
  /** M2: connections whose shares are this page's local shares beyond the page's own (desktop: C2). */
  localConnectionIds?(): string[];
  /** M2, native: share.stop for another connection's share of this user (the desktop SPA's role is viewer). */
  stopOther?(roomId: string, shareId: string): Promise<void>;
}

// ---- notifications and PWA ----

export interface NotificationsProvider {
  support(): Promise<PushSupport>;
  status(): Promise<'on' | 'off'>;
  /** From a user gesture; throws ApiError or a local error. */
  enable(): Promise<void>;
  disable(): Promise<void>;
  test(): Promise<void>;
}

export interface PwaProvider {
  installState(): InstallState;
  promptInstall(): Promise<'accepted' | 'dismissed' | 'unavailable'>;
  onUpdateReady(fn: () => void): () => void;
  /** postMessage SKIP_WAITING, reload on controllerchange. */
  applyUpdate(): void;
}
