// Signaling wiring (05 §7): one 01 SignalClient per tab, mirrored into connectionStore for the UI, with the
// reactions of 05 §7.1 that aren't a banner: where a stopped connection sends the user (the login page, a Fatal
// screen, VersionMismatch) and the one reload for a stale build (05 §16.4). The banner itself is ConnectionBanner,
// which renders connectionBanner() of this store.
//
// 01 owns the client's behaviour (heartbeat, backoff, resume, close codes; 01 §3.4, §10, §16); nothing here repeats
// it. No React in this file (05 §3 "Layering rule").
import type { QueryClient } from '@tanstack/react-query';
import { createStore, type StoreApi } from 'zustand/vanilla';

import { clearMe } from '../app/me';
import type { PrefsStore } from '../app/prefs';
import type { UiStore } from '../app/uiStore';
import { i18n } from '../i18n';
import { errorMessage } from '../lib/errorText';
import { createLogger, type Logger } from '../lib/log';
import type { Platform } from '../platform/types';
import type { ProtocolError } from '../protocol/errors';
import { SignalClient, type SignalState, type SignalStateInfo } from '../protocol/signal-client';
import {
  ErrorCodeAccountDisabled,
  ErrorCodeClientOutdated,
  ErrorCodeProtocolUnsupported,
  ErrorCodeReplaced,
  ErrorCodeTooManyConnections,
  ErrorScopeSession,
  MinVersion,
  ShutdownReasonRestart,
  type ServerShutdown,
  type Welcome,
} from '../protocol/types.gen';
import type { RoomSession } from './RoomSession';
import type { RoomStore } from './roomStore';

// ---- Timings (05 §18) and keys ----

/** "Reconnecting…" shows once the connection has been down this long, so short blips show nothing (01 §10.2). */
export const RECONNECTING_AFTER_MS = 2_000;
/** After this long the banner reads "Can't reach the server" and offers Retry now. */
export const UNREACHABLE_AFTER_MS = 30_000;
/**
 * sessionStorage key of the reload guard (05 §16.4): the server version this tab already reloaded for. One reload
 * per server version and tab, so a reload that doesn't help can't loop.
 */
export const RELOADED_FOR_KEY = 'isshoni.reloadedFor';

// ---- connectionStore (05 §6.1) ----

export interface ConnectionState {
  /** 01's SignalState; stopped before the first start(). */
  readonly state: SignalState;
  /** The last welcome: server version, limits, features, the user, the default room. null until the first one. */
  readonly welcome: Welcome | null;
  /** ready: the connection was resumed (welcome.resumed). */
  readonly resumed: boolean;
  /** ready: the server runs another version than this build (01 §6.1). */
  readonly staleBuild: boolean;
  /**
   * Since when the connection is down (Date.now()): the first backoff after the last ready (or after start()).
   * It spans the attempts in between (connecting, handshaking); ready and stopped clear it. The banner's 2 s and
   * 30 s count from here.
   */
  readonly downSince: number | null;
  /** stopped: the server's error (or the one its close code stands for); null after stop() and before start(). */
  readonly stopReason: ProtocolError | null;
  /**
   * The server announced its shutdown: 01's server.shutdown, or a REST call answered 503 server_shutdown
   * (serverRestarting()). Kept until the next ready.
   */
  readonly shutdown: ServerShutdown | null;
  /** backoff: when the next attempt starts (Date.now()). */
  readonly retryAt: number | null;
  /** backoff: a rate-limit wait, which Retry now can't shorten (01 §10.2). */
  readonly rateLimited: boolean;

  /** Mirrors one SignalClient state change (createConnection passes onState here). */
  signalChanged(state: SignalState, info: SignalStateInfo): void;
  /** Every welcome, before its ready state reaches signalChanged. */
  welcomed(w: Welcome): void;
  /**
   * A REST call was answered 503 server_shutdown (05 §7.1) while the connection is down: the outage is a restart,
   * also when the socket dropped before it heard server.shutdown. Ignored while ready (the socket hears it itself)
   * and while stopped.
   */
  serverRestarting(): void;
}

export type ConnectionStore = StoreApi<ConnectionState>;

export function createConnectionStore(now: () => number = Date.now): ConnectionStore {
  return createStore<ConnectionState>()((set, get) => ({
    state: 'stopped',
    welcome: null,
    resumed: false,
    staleBuild: false,
    downSince: null,
    stopReason: null,
    shutdown: null,
    retryAt: null,
    rateLimited: false,

    signalChanged(state, info) {
      switch (state) {
        case 'ready':
          set({
            state,
            resumed: info.resumed === true,
            staleBuild: info.staleBuild === true,
            downSince: null,
            stopReason: null,
            shutdown: null,
            retryAt: null,
            rateLimited: false,
          });
          return;
        case 'backoff': {
          const at = now();
          set({
            state,
            downSince: get().downSince ?? at,
            stopReason: null,
            shutdown: info.shutdown ?? get().shutdown,
            retryAt: at + (info.delayMs ?? 0),
            rateLimited: info.rateLimited === true,
          });
          return;
        }
        case 'connecting':
        case 'handshaking':
          set({
            state,
            // start() makes a new connection: the last one's welcome (its user, its limits) is history.
            ...(get().state === 'stopped' ? { welcome: null, resumed: false, staleBuild: false } : {}),
            stopReason: null,
            shutdown: info.shutdown ?? get().shutdown,
            retryAt: null,
            rateLimited: false,
          });
          return;
        case 'stopped':
          set({
            state,
            resumed: false,
            downSince: null,
            stopReason: info.error ?? null,
            shutdown: null,
            retryAt: null,
            rateLimited: false,
          });
          return;
      }
    },
    welcomed(welcome) {
      set({ welcome });
    },
    serverRestarting() {
      const { state, shutdown } = get();
      if (state === 'ready' || state === 'stopped' || shutdown !== null) return;
      set({ shutdown: { reason: ShutdownReasonRestart, reconnectInMs: 0 } });
    },
  }));
}

// ---- The banner of 05 §7.1 ----

/**
 * reconnecting: "Reconnecting…"; unreachable: "Can't reach the server. Retrying… [Retry now] [Test my connection]";
 * restarting: "Server restarting…" (no error styling).
 */
export type ConnectionBannerKind = 'reconnecting' | 'unreachable' | 'restarting';

export interface ConnectionBannerState {
  readonly kind: ConnectionBannerKind;
  /**
   * unreachable, during a rate-limit wait: the whole seconds left, shown on the disabled Retry button (retryNow()
   * has no effect then). null otherwise.
   */
  readonly retryInSec: number | null;
}

type BannerInput = Pick<ConnectionState, 'state' | 'downSince' | 'shutdown' | 'retryAt' | 'rateLimited'>;

function downFor(s: BannerInput, now: number): number | null {
  if (s.state === 'ready' || s.state === 'stopped' || s.downSince === null) return null;
  return Math.max(0, now - s.downSince);
}

/**
 * What the banner shows at `now` (05 §7.1): nothing while ready, stopped (screens and the login page take over), on
 * the first connect, and for the first 2 s of an outage; then "Reconnecting…"; after 30 s "Can't reach the server".
 * After a server.shutdown it reads "Server restarting…" at once. A server that isn't back after 30 s is treated as
 * unreachable, because only that banner has the Retry and Test buttons.
 */
export function connectionBanner(s: BannerInput, now: number): ConnectionBannerState | null {
  const down = downFor(s, now);
  if (down === null) return null;
  if (down >= UNREACHABLE_AFTER_MS) {
    const waiting = s.state === 'backoff' && s.rateLimited && s.retryAt !== null && s.retryAt > now;
    return { kind: 'unreachable', retryInSec: waiting ? Math.ceil((s.retryAt - now) / 1000) : null };
  }
  if (s.shutdown !== null) return { kind: 'restarting', retryInSec: null };
  if (down >= RECONNECTING_AFTER_MS) return { kind: 'reconnecting', retryInSec: null };
  return null;
}

/** In how many ms connectionBanner()'s result can change by time alone; null when only a state change changes it. */
export function nextBannerChange(s: BannerInput, now: number): number | null {
  const down = downFor(s, now);
  if (down === null) return null;
  if (down < RECONNECTING_AFTER_MS && s.shutdown === null) return RECONNECTING_AFTER_MS - down;
  if (down < UNREACHABLE_AFTER_MS) return UNREACHABLE_AFTER_MS - down;
  if (s.state === 'backoff' && s.rateLimited && s.retryAt !== null && s.retryAt > now) {
    // The countdown: the next whole second.
    return (s.retryAt - now) % 1000 || 1000;
  }
  return null;
}

// ---- Stores and createConnection (05 §7) ----

/** The stores the room's controllers write, and the session that createConnection resyncs. */
export interface Stores {
  readonly connection: ConnectionStore;
  readonly room: RoomStore;
  readonly ui: UiStore;
  readonly prefs: PrefsStore;
  /** The room session, once it exists (the runtime sets it): every welcome goes to its resync() (05 §11.1). */
  session?: RoomSession;
}

export interface ConnectionOptions {
  /**
   * The REST cache: a connection stopped for its session (unauthenticated, session_revoked) clears ['me'], which
   * sends guarded routes to /login?next=… (app/guards.tsx). Without it only the stores change.
   */
  queryClient?: QueryClient;
  log?: Logger;
  /** Passed to the SignalClient: the build version compared with welcome.serverVersion. Tests set it. */
  buildVersion?: string;
}

/**
 * Creates the tab's SignalClient (05 §7) and wires it: its states into stores.connection, every welcome into the
 * session's resync, and the reactions of 05 §7.1 and §16.4. It doesn't start the client; the first room page does
 * (rooms/runtime.ts).
 */
export function createConnection(platform: Platform, stores: Stores, opts: ConnectionOptions = {}): SignalClient {
  const log = opts.log ?? createLogger('signal');
  const { url } = platform.signaling(); // auth: later (M2), a bearer in hello.auth (01 D2)
  const client = new SignalClient({
    url,
    client: platform.client, // 01 ClientInfo {kind: 'web', version, os, browser}
    role: platform.role, // 'full' if the platform can share, else 'viewer' (01 §6.3)
    caps: () => platform.capsNow(), // 01 detectCaps(), re-read on every (re)connect
    features: [], // later: 'share.pause' (M2), 'layer.mid' (M5)
    onResync: (w) => {
      stores.connection.getState().welcomed(w);
      return stores.session?.resync(w); // RoomSession, 05 §11.1
    },
    ...(opts.buildVersion !== undefined ? { buildVersion: opts.buildVersion } : {}),
  });

  const { ui } = stores;

  /** unauthenticated, session_revoked and session-scope codes this build doesn't know: the login page (01 §12.3). */
  const signedOut = (error: ProtocolError): void => {
    ui.getState().toast({ kind: 'info', message: errorMessage(error) });
    if (opts.queryClient) clearMe(opts.queryClient);
  };

  /** protocol_unsupported (and client_outdated): the VersionMismatch screen (05 §16.4). */
  const versionMismatch = (error: ProtocolError): void => {
    const serverVersion = error.params['serverVersion'];
    const serverMax = error.params['serverMax'];
    const version = typeof serverVersion === 'string' && serverVersion !== '' ? serverVersion : undefined;
    // The screen's only action is Reload, so seeing it twice for one server version means a reload didn't help.
    const session = platform.storage.session;
    const marker = version ?? 'unknown';
    const stillStale = session.get(RELOADED_FOR_KEY) === marker;
    session.set(RELOADED_FOR_KEY, marker);
    ui.getState().showScreen({
      kind: 'versionMismatch',
      serverVersion: version,
      // The server's newest protocol is older than the oldest this page speaks: only the admin can fix that.
      serverOlder: typeof serverMax === 'number' && serverMax < MinVersion,
      stillStale,
    });
  };

  const stopped = (error: ProtocolError | undefined): void => {
    if (error === undefined) return; // stop() or pagehide: on purpose
    log.warn('signaling stopped', { code: error.code, scope: error.scope, closeCode: error.closeCode });
    switch (error.code) {
      case ErrorCodeReplaced:
        return; // another socket took this connection over
      case ErrorCodeAccountDisabled:
        ui.getState().showScreen({ kind: 'fatal', reason: 'account_disabled' });
        return;
      case ErrorCodeTooManyConnections:
        ui.getState().showScreen({ kind: 'fatal', reason: 'too_many_connections' });
        return;
      case ErrorCodeProtocolUnsupported:
      case ErrorCodeClientOutdated:
        versionMismatch(error);
        return;
    }
    if (error.scope === ErrorScopeSession) {
      signedOut(error);
      return;
    }
    // bad_message, and every other code or stop-type close code (01 §12.3): "Something went wrong" with Reload.
    ui.getState().showScreen({ kind: 'fatal', reason: 'generic', code: error.code });
  };

  /**
   * ready with staleBuild (05 §16.4): the shell is stale (a cached PWA; the server always serves a matching SPA).
   * Reload once per server version. A plain reload is enough to update: the service worker answers navigations
   * from the network first (05 §16.2), and the browser checks for a new worker on that navigation.
   */
  const staleBuild = (serverVersion: string): void => {
    const session = platform.storage.session;
    if (session.get(RELOADED_FOR_KEY) === serverVersion) {
      // The reload didn't help. The handshake worked, so the app keeps running rather than blocking the user.
      log.warn('still a stale build after reloading', { serverVersion, buildVersion: platform.client.version });
      return;
    }
    if (stores.session?.share != null) {
      // Not while sharing: a reload would end the share. The UpdatePill offers it instead.
      ui.getState().setUpdateReady(true);
      return;
    }
    const reload = platform.versionActions().reload;
    if (reload === undefined) return;
    session.set(RELOADED_FOR_KEY, serverVersion);
    log.info('stale build: reloading', { serverVersion });
    reload();
  };

  client.onState((state, info) => {
    const before = stores.connection.getState();
    const hadBanner = connectionBanner(before, Date.now()) !== null;
    before.signalChanged(state, info);
    if (state === 'stopped') {
      stopped(info.error);
    } else if (state === 'ready') {
      // The banner is a live region, but its text just disappears: say that the outage is over (05 §16.6).
      if (hadBanner) ui.getState().announce(i18n.t('room.connection.reconnected'));
      if (info.staleBuild === true) staleBuild(client.welcome?.serverVersion ?? '');
    }
  });
  return client;
}
