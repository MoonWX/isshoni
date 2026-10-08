// The Platform adapter (05 §8): everything the SPA needs from where it runs. BrowserPlatform is the only M1
// implementation; the desktop app (M2/M3) bundles this same SPA with a DesktopPlatform. Outside platform/, nothing
// reads cookies or location.origin, builds /api URLs by hand, or constructs RTCPeerConnection or WebSocket URLs.
//
// Wire types come from 01's types.gen.ts. The interfaces are declared in full ("interfaces first", README §4); the
// browser's push provider is a stub until its slice lands (S77).
import type { ClientNotifications, ClientRequests, ServerEnvelope, ServerMessages } from '../protocol/registry.gen';
import type {
  Caps,
  ClientInfo,
  HelloAuth,
  MessageTypeHello,
  Preset,
  ShareKind,
  ShareParams,
} from '../protocol/types.gen';

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

/** What the VersionMismatch screen offers (05 §16.4): the browser reloads; the desktop app (M2) updates itself. */
export interface VersionActions {
  reload?: () => void;
  openInBrowser?: () => void;
  updateApp?: () => void;
}

export interface Platform {
  readonly kind: PlatformKind;
  /** → hello.client */
  readonly client: ClientInfo;
  /** browser: 'full' when sharing is possible; desktop SPA: 'viewer' (M2). */
  readonly role: 'full' | 'viewer';
  /** browser: location.origin; desktop: the linked server (M2). */
  readonly serverOrigin: string;
  /** Synchronous 01 detectCaps(). */
  capsNow(): Caps;
  capabilities(): Promise<PlatformCapabilities>;
  /** A request to the server's API; path is resolved against serverOrigin and carries the platform's credentials. */
  apiFetch(path: `/api/${string}`, init?: RequestInit): Promise<Response>;
  /** url = new URL('/ws', serverOrigin) with ws(s):. auth: later (M2), a bearer in hello.auth (01 D2). */
  signaling(): { url: string; auth?: HelloAuth };
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
  audioScope: 'window' | 'tab' | 'system' | 'none';
  warning: 'screen-with-system-audio' | 'no-audio' | null;
  /** Owned by the provider. */
  preview: MediaStream;
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

export type LocalShareState = 'starting' | 'live' | 'paused' /* later (M2) */ | 'reconnecting' | 'ended';
export type LocalEndReason = 'user' | 'browser-stopped' | 'server' | 'server-unreachable' | 'error';
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
  /** later (M2, feature share.pause); M1 rejects with not_supported. */
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
