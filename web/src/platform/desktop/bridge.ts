// NativeBridge (docs/m2/03 §7.4): what the desktop SPA needs from the native side, independent of how it travels.
// Two transports implement it: WailsBridge (the generated bindings and @wailsio/runtime's events, WA48) and HttpBridge
// (POST /__bridge/call and an EventSource on /__bridge/events, served by internal/desktop/e2ehost). DesktopPlatform
// loads one of them on demand; nothing in the entry chunk imports either.
//
// The shapes are the JSON of the bound services (docs/m2/03 §6.2, §6.3), which follow 02 §11.7 (AuthState), 05 §14.5
// (FriendsHear) and 06 §8.9 (UpdateStatus). Field names are the wire's. This file is types plus two constants (the
// event prefix and the list of fire-and-forget methods). The Go side's DTOs (internal/desktop/bridge) write the same
// JSON with plain strings where these types name the values; WailsBridge maps the generated bindings onto them.
//
// Later (M4): a RelayBridge over 01's same-user relay implements the share and audio parts over messages, so the
// interface keeps those parts free of anything Wails-specific.
import type { DeviceRef, User } from '../../protocol/api.gen';
import type { ClientOS, Preset, ServerLayerStats, ShareKind } from '../../protocol/types.gen';
import type { HelloRefusal, SessionEndCode, ShareHint, ShareStatsSample } from '../types';

/** Go's event names carry this prefix (docs/m2/03 §6.3), so they never collide with Wails' own. */
export const BridgeEventPrefix = 'isshoni:';

// ---- App ----

export interface AppInfo {
  version: string;
  os: ClientOS;
  osBuild: number;
  arch: string;
  /** core.Status().ConnectionID of C2; "" while it isn't ready. */
  publisherConnectionId: string;
  wizardDone: boolean;
  dev: boolean;
}

/** App.OpenOSSettings' fixed list (docs/m2/03 §6.2). */
export type OSPage = 'notifications' | 'dnd' | 'graphics-capture' | 'sound' | 'sound-communications' | 'taskbar';

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

/**
 * The deeplink.open payload (docs/m2/03 §6.3, §13.2): a room on the active server ("" = the SPA's own last room), or
 * a server that isn't known yet.
 */
export type PendingLink =
  { roomId: string } | { add: { origin: string; host: string; hostUnicode: string; roomId: string } };

// ---- Auth (02 §11.7) ----

export type AuthStatus = 'unlinked' | 'linking' | 'linked' | 'offline' | 'incompatible';

/** Why the state changed when the user didn't do it (02 §11.3). Shown once, then cleared by the UI. */
export type AuthNotice =
  '' | 'denied' | 'code_expired' | 'signed_out' | 'refresh_reused' | 'account_disabled' | 'save_failed';

/** 02 §13.2's compat.Check. */
export interface CompatCheck {
  verdict: 'ok' | 'update_app' | 'update_server';
  /** "", "protocol_too_new", "protocol_too_old", "no_device_flow", "min_client_version". */
  reason: string;
  serverVersion: string;
  minClientVersion: string;
}

/** The active server's link state: Auth.State() and the isshoni:auth.state event (02 §11.7). */
export interface AuthState {
  /** The server this state belongs to. DesktopPlatform ignores a state whose origin isn't its serverOrigin. */
  origin: string;
  serverName: string;
  status: AuthStatus;
  /** status linking. expiresAt: ms since the epoch. */
  link: { userCode: string; verificationUri: string; verificationUriComplete: string; expiresAt: number } | null;
  /** linked, offline */
  user: User | null;
  /** linked, offline */
  device: DeviceRef | null;
  notice: AuthNotice;
  compat: CompatCheck;
}

/** Auth.Servers(): 02's ServerEntry plus the derived id, the active mark and linked (= user != null). */
export interface ServerInfo {
  origin: string;
  name: string;
  /** RFC 3339 */
  addedAt: string;
  user?: User | null;
  device?: DeviceRef | null;
  /** The derived local id (docs/m2/03 §8.1). */
  id: string;
  active: boolean;
  linked: boolean;
}

export interface ServerProbe {
  /** The server's own public origin, which Add stores. */
  origin: string;
  name: string;
  version: string;
}

/** Auth.AccessToken's answer: the access token and its expiry in ms since the epoch. Never stored, never logged. */
export interface AccessToken {
  token: string;
  expiresAt: number;
}

// ---- Share (docs/m2/03 §6.2, §6.3) ----

export interface ShareCaps {
  canShare: boolean;
  reason?: string;
  encoders: string[];
  borderless: boolean;
  osBuild: number;
  /** media.AudioCapsFromFlags' Backend (README §3 item 20). */
  audioBackend: 'process-loopback' | 'endpoint' | 'none';
}

/**
 * A source picked in the OS picker, held by Go until Start or Release, at most 300 s from the pick (docs/m2/03 §10.1,
 * 04's pick-token life), so a friend who reads the first-share sheet or follows the DND tip into Windows Settings
 * comes back to a pick that still works. (The picker itself closes after 120 s without a choice: Share.Pick() then
 * answers null.)
 */
export interface Picked {
  pickId: string;
  kind: ShareKind;
  /** The picker's display name; shown on this PC only, never sent. */
  label: string;
  /** Windows draws its yellow capture border. */
  border: boolean;
  confirm: ('first-share' | 'dnd')[];
}

export interface StartRequest {
  pickId: string;
  roomId: string;
  preset: Preset;
  withAudio: boolean;
}

export type ShareViewState =
  'picking' | 'starting' | 'live' | 'paused' | 'reconnecting' | 'unreachable' | 'stopping' | 'ended' | 'failed';

export type ShareEndReason =
  | 'user'
  | 'hotkey'
  | 'tray'
  | 'source-gone'
  | 'elsewhere'
  | 'server'
  | 'server-unreachable'
  | 'unlinked'
  | 'system'
  | 'error';

/** A bridge error as JSON (docs/m2/03 §6.4). */
export interface BridgeErrorJSON {
  code: string;
  params?: Record<string, unknown>;
}

export interface ShareLayerView {
  rid: string;
  width: number;
  height: number;
  fps: number;
  targetKbps: number;
  sentKbps: number;
  captureToSendMs: { p50: number; p95: number };
}

/** The native share as the SPA sees it (docs/m2/03 §6.3): Share.Current() and the isshoni:share.state event. */
export interface ShareView {
  state: ShareViewState;
  shareId: string;
  connectionId: string;
  roomId: string;
  kind: ShareKind;
  label: string;
  preset: Preset;
  withAudio: boolean;
  audioOn: boolean;
  border: boolean;
  pill: boolean;
  minimized: boolean;
  watching: number;
  /** RFC 3339, null until live. */
  liveSince: string | null;
  hint: ShareHint | null;
  encoder: string;
  hw: boolean;
  layers: ShareLayerView[];
  /**
   * 01's view of this connection's layers (protocol.ServerLayerStats, the server's stats message) while
   * Share.WatchServerStats is on, else null.
   */
  server: { layers: ServerLayerStats[] } | null;
  endReason: ShareEndReason | null;
  error: BridgeErrorJSON | null;
}

/** share.notice: transient notices from the engine (docs/m2/03 §10.5). */
export interface ShareNotice {
  code: string;
  params?: Record<string, unknown>;
}

// ---- Audio (05 §14.5) ----

export type Decision = 'keep-out' | 'include' | 'auto'; // setAppDecision ('auto' = forget the rule)
export type PromptAnswer = 'keep-out' | 'include'; // decide; dismissing is dismissPrompt
// No audio-mode type or method: M2 has no "All sound" mode (docs/m2/03 §6.2, README §3 item 15).

export interface AppRow {
  id: string;
  /** local only, never sent anywhere */
  name: string;
  reason:
    | 'builtin'
    | 'user'
    | 'mic'
    | 'viewer'
    | 'isshoni'
    | 'launcher'
    | 'tree'
    | 'pid-reuse'
    | 'limit'
    | 'capture-failed'
    | '';
  /** 'tree', 'pid-reuse': the kept-out app's name */
  detail?: string;
  playing: boolean;
  usesMic: boolean;
  browser: boolean;
  shared: boolean;
  control: 'locked' | 'toggle';
  decision: Decision;
  warning?: 'shared-uses-mic' | 'shared-kept-out' | 'included-uses-mic';
}

export interface Prompt {
  id: string;
  appId: string;
  name: string;
  kind: 'mic' | 'browser-mic';
  since: number;
}

export interface Banner {
  code:
    | 'exclusion-unavailable'
    | 'windows-too-old'
    | 'no-output-device'
    | 'reconnecting-sound'
    | 'sound-off'
    | 'capture-trouble'
    | 'viewer-muted'
    | 'viewer-unknown-browser';
  level: 'error' | 'warning' | 'info';
  params?: Record<string, string | number>;
}

export interface Tip {
  code: 'ducking';
}

/** What friends hear (05 §14.5): Audio.FriendsHear() and the isshoni:audio.report event. */
export interface FriendsHear {
  seq: number;
  live: boolean;
  mode: 'exclude-apps' | 'off';
  engine: 'include-set' | 'exclude-self' | 'endpoint' | 'none';
  muted: boolean;
  paused: boolean;
  keepOutMicUsers: boolean;
  heard: AppRow[];
  keptOut: AppRow[];
  notCaptured: AppRow[];
  prompts: Prompt[];
  banners: Banner[];
  tips: Tip[];
  voiceApps: { id: string; name: string; running: boolean }[];
  alert: { voiceAppsHeard: boolean; questions: number };
}

export interface Levels {
  seq: number;
  mixPeakDb: number;
  mixRmsDb: number;
  apps: Record<string, { peakDb: number; source: 'captured' | 'activity' }>;
}

/** One remembered per-app decision (05 §14.2). */
export interface AppRule {
  id: string;
  name: string;
  decision: 'keep-out' | 'include';
}

/** 01's media.SoundCheckResult. */
export interface SoundCheckResult {
  captured: boolean;
  levelDb: number;
  ms: number;
  backend: 'process-loopback' | 'endpoint' | 'none';
  reason?: string;
}

// ---- Settings (docs/m2/03 §14.2) ----

export interface HotkeyCombos {
  pause: string;
  stop: string;
}

export interface SettingsView {
  schema: number;
  serverPrefs: Record<string, { preset: Preset }>;
  wizardDone: boolean;
  window: { x: number; y: number; w: number; h: number; maximized: boolean };
  closeToTray: boolean;
  autostart: boolean;
  hideFromCapture: boolean;
  notifications: boolean;
  hotkeys: HotkeyCombos;
  pill: { mode: 'auto' | 'always'; positions: Record<string, { x: number; y: number }> };
  updates: { check: boolean; channel: 'stable' | 'beta'; policyOff: boolean };
  tips: { firstShareShown: boolean; dndShown: boolean; trayNoticeShown: boolean };
  log: { level: LogLevel };
}

/** Every field optional; only these are accepted (docs/m2/03 §14.2). */
export interface SettingsPatch {
  closeToTray?: boolean;
  autostart?: boolean;
  hideFromCapture?: boolean;
  notifications?: boolean;
  hotkeys?: Partial<HotkeyCombos>;
  pill?: { mode?: 'auto' | 'always' };
  updates?: { check?: boolean; channel?: 'stable' | 'beta' };
  tips?: Partial<SettingsView['tips']>;
  log?: { level?: LogLevel };
  serverPrefs?: Record<string, { preset?: Preset }>;
}

export interface HotkeyStatus {
  id: string;
  combo: string;
  state: 'registered' | 'idle' | 'taken' | 'invalid';
  /** The combination types a character in the current keyboard layout (docs/m2/03 §11.2). */
  typesChar?: boolean;
}

// ---- Updates (06 §8.9) ----

export type UpdateState = 'disabled' | 'off' | 'idle' | 'checking' | 'up_to_date' | 'downloading' | 'ready' | 'error';

export interface UpdateStatus {
  state: UpdateState;
  current: string;
  available: string;
  required: boolean;
  notesUrl: string;
  /** 0..1 while downloading */
  progress: number;
  /** RFC 3339; the zero time before the first check */
  checkedAt: string;
  /** 06 §8.10; "" unless error (or disabled: "no_keys", "not_release") */
  errorCode: string;
  policyOff: boolean;
}

// ---- Events (docs/m2/03 §6.3; the names here are without the isshoni: prefix) ----

export interface BridgeEvents {
  /** null: there is no share (the controller's Current() is null: before any, or a pick in progress was settled). */
  'share.state': ShareView | null;
  /** `at` is set by the transport on receipt (performance.now()). */
  'share.stats': ShareStatsSample;
  'share.notice': ShareNotice;
  'audio.report': FriendsHear;
  'audio.levels': Levels;
  'audio.prompt': Prompt;
  'audio.alert': Banner;
  'auth.state': AuthState;
  'deeplink.open': PendingLink;
  'window.state': { visible: boolean; focused: boolean; fullscreen: boolean };
  'settings.changed': SettingsView;
  'update.state': UpdateStatus;
}

export type BridgeEventName = keyof BridgeEvents;

// ---- The bridge ----

/**
 * Every method returns what the bound Go method returns; a failing one rejects with a BridgeError (errors.ts) whose
 * code is one of docs/m2/03 §6.4. The void methods are fire-and-forget: their failures are logged, not thrown.
 */
export interface NativeBridge {
  app: {
    info(): Promise<AppInfo>;
    openExternal(url: string): Promise<void>;
    openOSSettings(p: OSPage): Promise<void>;
    quit(stopShare: boolean): Promise<void>;
    log(l: LogLevel, msg: string, attrs?: Record<string, string>): void;
    openLogsFolder(): Promise<void>;
    openNotices(): Promise<void>;
    ownConnections(ids: string[]): void;
    pendingLink(): Promise<PendingLink | null>;
    linkHandled(): Promise<void>;
  };
  auth: {
    state(): Promise<AuthState>;
    servers(): Promise<ServerInfo[]>;
    probe(address: string): Promise<ServerProbe>;
    addServer(origin: string): Promise<void>;
    switchServer(origin: string): Promise<void>;
    removeServer(origin: string): Promise<void>;
    link(): Promise<void>;
    cancelLink(): Promise<void>;
    linkWithPassword(username: string, password: string): Promise<AuthState>;
    unlink(): Promise<{ confirmed: boolean }>;
    recheck(): Promise<AuthState>;
    /** null: not linked, or origin isn't the active server's (docs/m2/03 §5.8). */
    accessToken(origin: string, force: boolean): Promise<AccessToken | null>;
    /** Rejects with not_found when origin isn't the active server's. */
    sessionEnded(origin: string, code: SessionEndCode): Promise<AuthState>;
    /** Rejects with not_found when origin isn't the active server's. */
    helloRefused(origin: string, e: HelloRefusal): Promise<AuthState>;
  };
  share: {
    capabilities(): Promise<ShareCaps>;
    pick(): Promise<Picked | null>;
    release(pickId: string): void;
    start(r: StartRequest): Promise<ShareView>;
    current(): Promise<ShareView | null>;
    stop(): Promise<void>;
    stopOther(roomId: string, shareId: string): Promise<void>;
    setPaused(p: boolean): Promise<void>;
    setPreset(p: Preset): Promise<void>;
    setAudioEnabled(on: boolean): Promise<void>;
    /** `at` is set by the transport on receipt (performance.now()). */
    stats(): Promise<ShareStatsSample | null>;
    watchServerStats(on: boolean): void;
  };
  audio: {
    friendsHear(): Promise<FriendsHear>;
    watch(on: boolean): void;
    watchLevels(on: boolean): void;
    setAppDecision(appId: string, d: Decision): Promise<void>;
    setKeepOutMicUsers(on: boolean): Promise<void>;
    decide(promptId: string, d: PromptAnswer): Promise<void>;
    dismissPrompt(promptId: string): Promise<void>;
    dismissTip(code: string): Promise<void>;
    appRules(): Promise<AppRule[]>;
    soundCheck(): Promise<SoundCheckResult>;
  };
  settings: {
    get(): Promise<SettingsView>;
    update(p: SettingsPatch): Promise<SettingsView>;
    hotkeys(): Promise<HotkeyStatus[]>;
    testHotkey(combo: string): Promise<HotkeyStatus>;
  };
  window: { setFullscreen(on: boolean): Promise<void> };
  updates: { status(): Promise<UpdateStatus>; check(): Promise<UpdateStatus>; install(): Promise<void> };
  /** Listens to a Go event (without its isshoni: prefix); returns the unsubscribe function. */
  on<E extends BridgeEventName>(e: E, fn: (p: BridgeEvents[E]) => void): () => void;
}

/** The fire-and-forget methods, as `group.method`: they return nothing, and a transport logs their failures. */
export const VoidMethods: ReadonlySet<string> = new Set([
  'app.log',
  'app.ownConnections',
  'share.release',
  'share.watchServerStats',
  'audio.watch',
  'audio.watchLevels',
]);
