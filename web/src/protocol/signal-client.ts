// SignalClient (docs/m1/01-protocol.md §16): the browser's side of the signaling protocol on one WebSocket. It owns
// the handshake (§8.2), the application-level heartbeat (§3.4), the state machine and backoff of §10.1–§10.2, the
// in-memory resume token (§10.3) and the error and close-code mapping of §12. What to resync after a welcome (§10.5)
// is 05's RoomSession, which gets every welcome through onResync; PeerConnections are 05's too (§9, §10.4).
//
// It never imports React (05 §3); 05's rooms/connection.ts creates one per tab and mirrors onState into a store.
import {
  LocalErrorCodeConnectionLost,
  LocalErrorCodeNotReady,
  LocalErrorCodeRequestTimeout,
  ProtocolError,
} from './errors';
import type {
  ClientNotificationType,
  ClientNotifications,
  ClientRequestType,
  ClientRequests,
  ServerEnvelope,
  ServerMessageType,
  ServerMessages,
} from './registry.gen';
import {
  CloseCodeNormal,
  ErrorCodeRateLimited,
  ErrorScopeConnection,
  ErrorScopeSession,
  MessageTypeError,
  MessageTypeHello,
  MessageTypeOK,
  MessageTypePing,
  MessageTypePong,
  MessageTypeServerShutdown,
  MessageTypeWelcome,
  MinVersion,
  ShutdownReasonRestart,
  Version,
  type Caps,
  type ClientInfo,
  type Feature,
  type Hello,
  type Role,
  type ServerShutdown,
  type Welcome,
} from './types.gen';

/** The states of 01 §10.1. A new client is stopped; only start() (or a reload) leaves stopped. */
export type SignalState = 'connecting' | 'handshaking' | 'ready' | 'backoff' | 'stopped';

/** What onState reports with a state. */
export interface SignalStateInfo {
  /** ready: welcome.resumed. */
  resumed?: boolean;
  /**
   * ready: welcome.serverVersion differs from this build's version and neither is a dev build (01 §6.1, §16). 05
   * reloads once (05 §16.4).
   */
  staleBuild?: boolean;
  /**
   * backoff and stopped: why the connection went down, when the server said so: its error (scope connection or
   * session, or any error answering hello), or the error its close code stands for (ProtocolError.fromCloseCode).
   * Absent for a plain drop, a connect, handshake or ping timeout, and for stop().
   */
  error?: ProtocolError;
  /** Set from a server.shutdown message until the next ready: 05 shows "Server restarting…" (05 §7.1). */
  shutdown?: ServerShutdown;
  /** backoff: the wait before the next attempt, in ms. */
  delayMs?: number;
  /**
   * backoff: the wait is a rate-limit wait (after rate_limited in scope connection, or close 4429): online, a visible
   * page and retryNow() don't shorten it (01 §10.2), so 05 disables its Retry button for delayMs.
   */
  rateLimited?: boolean;
}

export interface SignalClientOptions {
  /** new URL('/ws', location.href) with ws: or wss: (01 §3.1). */
  url: string;
  client: ClientInfo;
  role: Role;
  /** Re-read on every (re)connect (01 §8.2): a browser may gain a decoder while the page runs (§11.7). */
  caps: () => Caps;
  /** hello.features; welcome.features says which are active (01 §6.2). */
  features?: Feature[];
  /**
   * Called after every welcome, the first after start() included, resumed or not. The state is already ready, so
   * request() and notify() work; onState('ready') listeners run right after it returns. A returned promise is not
   * awaited; its rejection is logged.
   */
  onResync?: (w: Welcome) => void | Promise<void>;
  /**
   * The build version that welcome.serverVersion is compared with for staleBuild (01 §6.1). Default: this build's
   * __ISSHONI_VERSION__ (01's BUILD_VERSION). Tests set it; the app leaves it alone.
   */
  buildVersion?: string;
}

/** The requests SignalClient.request sends: every client request except hello, which the client sends itself. */
export type SignalRequestType = Exclude<ClientRequestType, typeof MessageTypeHello>;

export interface RequestOptions {
  /** Default RequestTimeoutMs (10 s, 01 §13). */
  timeoutMs?: number;
}

export type StateListener = (state: SignalState, info: SignalStateInfo) => void;

// ---- Timings (01 §3.4, §10.1–§10.2, §13) ----

/** request(): no reply within this → request_timeout. */
export const RequestTimeoutMs = 10_000;
/** connecting: no open socket within this → backoff (a guard: a black-holed server could otherwise hang for long). */
export const ConnectTimeoutMs = 10_000;
/** handshaking: no welcome within this after the socket opened → backoff. */
export const HandshakeTimeoutMs = 10_000;
/** The ping interval when welcome.limits.pingIntervalMs is missing. */
export const DefaultPingIntervalMs = 15_000;
/** A periodic ping without a pong within this → close the socket and reconnect. */
export const PongTimeoutMs = 10_000;
/** The pong timeout of the immediate pings: probe(), a page that becomes visible, online. */
export const ProbeTimeoutMs = 3_000;
/** Backoff bounds (01 §10.2). */
export const BackoffMinMs = 500;
export const BackoffMaxMs = 10_000;
/** The backoff counter resets only after the connection has been ready this long. */
export const BackoffResetMs = 10_000;
/** The shortest wait after a connection-scope rate limit. */
export const RateLimitMinWaitMs = 30_000;

/**
 * The close code the client uses when it drops a socket to reconnect (a timeout, a connection error). Anything but
 * 1000 and 1001 keeps the server's 30 s grace (01 §4.2), so the next hello can resume; browsers accept only 1000 and
 * 3000–4999 in WebSocket.close, and the server sends no 4000.
 */
export const ReconnectCloseCode = 4000;

/**
 * The backoff delay for attempt n (n = 0 for the first retry after a drop, 01 §10.2):
 * min(10 s, 0.5 s × 2ⁿ) × U(0.8, 1.2), clamped to [0.5 s, 10 s]. random follows Math.random: [0, 1).
 */
export function backoffDelay(n: number, random: () => number = Math.random): number {
  const base = Math.min(BackoffMaxMs, BackoffMinMs * 2 ** Math.max(0, n));
  const jittered = base * (0.8 + 0.4 * random());
  return Math.min(BackoffMaxMs, Math.max(BackoffMinMs, jittered));
}

/**
 * Reports whether v is a dev build: its SemVer prerelease starts with "dev" (04 §15, like Go's version.IsDev):
 * 0.0.0-dev, 0.0.0-dev+1a2b3c4, 0.1.1-dev.1a2b3c4. Build metadata alone (1.0.0+dev) is not a prerelease.
 */
export function isDevVersion(v: string): boolean {
  const core = v.split('+', 1)[0] ?? '';
  const dash = core.indexOf('-');
  return dash >= 0 && core.slice(dash + 1).startsWith('dev');
}

/** 01 §6.1: the SPA is stale when the versions differ and neither is a dev build (nor unknown). */
export function isStaleBuild(serverVersion: string, buildVersion: string): boolean {
  return (
    serverVersion !== '' &&
    buildVersion !== '' &&
    serverVersion !== buildVersion &&
    !isDevVersion(serverVersion) &&
    !isDevVersion(buildVersion)
  );
}

// WebSocket.readyState values, as numbers so a stand-in constructor (tests) needs no statics.
const wsOpen = 1;
const wsClosing = 2;

type Timer = ReturnType<typeof setTimeout>;

/** One timer that can be armed again (which cancels the previous one) and cleared. */
class TimerSlot {
  #timer: Timer | undefined;

  set(fn: () => void, ms: number): void {
    this.clear();
    this.#timer = setTimeout(() => {
      this.#timer = undefined;
      fn();
    }, ms);
  }

  clear(): void {
    if (this.#timer !== undefined) {
      clearTimeout(this.#timer);
      this.#timer = undefined;
    }
  }
}

type Listener = (data: unknown, env: ServerEnvelope) => void;

interface PendingRequest {
  resolve: (result: unknown) => void;
  reject: (err: ProtocolError) => void;
  timer: Timer;
}

/** A parsed server message; data is {} when the message had none (01 §5). */
interface Incoming {
  type: string;
  re: string | undefined;
  data: Record<string, unknown>;
}

/** Why a socket ended. With neither field it was a drop or a client-side timeout: back off. */
interface EndCause {
  /** The server's error; it decides over the close code (01 §12.2). */
  error?: ProtocolError;
  /** The error answered hello: whatever its scope, it ends the socket, and a non-retryable one stops. */
  hello?: boolean;
  closeCode?: number;
}

/**
 * The signaling client of 01 §16. One instance per tab (05 §7). Every public method is safe in every state and from
 * inside a listener; state changes are reported through onState, server messages through on.
 */
export class SignalClient {
  readonly #opts: SignalClientOptions;
  readonly #buildVersion: string;
  #state: SignalState = 'stopped';
  #welcome: Welcome | undefined;
  #ws: WebSocket | undefined;
  #helloId: string | undefined;
  #resumeToken: string | undefined;
  #nextId = 1;
  /** n of 01 §10.2. */
  #attempt = 0;
  readonly #pending = new Map<string, PendingRequest>();
  readonly #listeners = new Map<string, Set<Listener>>();
  readonly #stateListeners = new Set<StateListener>();
  /** The last server.shutdown, from its arrival until ready. */
  #shutdown: ServerShutdown | undefined;
  /** The next backoff waits exactly #shutdown.reconnectInMs. */
  #shutdownWaitPending = false;
  /** The next backoff is a rate-limit wait of this many ms. */
  #rateLimitWaitMs: number | undefined;
  /** The current backoff wait can't be skipped. */
  #backoffRateLimited = false;
  /** connecting: the connect timeout; handshaking: the handshake timeout. */
  readonly #attemptTimer = new TimerSlot();
  readonly #backoffTimer = new TimerSlot();
  readonly #pingTimer = new TimerSlot();
  readonly #pongTimer = new TimerSlot();
  /** When the pong for the ping(s) out is due (Date.now() ms); undefined when no ping is out. */
  #pongDue: number | undefined;
  /** ready: resets the backoff counter after BackoffResetMs. */
  readonly #stableTimer = new TimerSlot();
  #pageListeners = false;
  /** Stopped by pagehide: a pageshow from the bfcache starts again. */
  #suspended = false;

  constructor(opts: SignalClientOptions) {
    this.#opts = opts;
    this.#buildVersion = opts.buildVersion ?? __ISSHONI_VERSION__;
  }

  get state(): SignalState {
    return this.#state;
  }

  /** The last welcome, kept through drops until the next one; undefined before the first one after start(). */
  get welcome(): Welcome | undefined {
    return this.#welcome;
  }

  /**
   * Connects as a new connection (resumed: false) with the backoff counter at 0. No-op unless stopped. Until stop()
   * it also listens to online, visibilitychange, pagehide and pageshow.
   */
  start(): void {
    if (this.#state !== 'stopped') {
      return;
    }
    this.#suspended = false;
    this.#resumeToken = undefined;
    this.#welcome = undefined;
    this.#attempt = 0;
    this.#shutdown = undefined;
    this.#shutdownWaitPending = false;
    this.#rateLimitWaitMs = undefined;
    this.#addPageListeners();
    this.#connect();
  }

  /**
   * Closes with 1000, so the server skips the grace period (01 §4.2), and stops. Pending requests reject with
   * connection_lost. A later start() makes a new connection.
   */
  stop(): void {
    this.#suspended = false;
    this.#removePageListeners();
    this.#halt();
  }

  /** An immediate ping with a 3 s pong timeout (01 §3.4); 05 calls it when a PC becomes disconnected. Ready only. */
  probe(): void {
    if (this.#state === 'ready') {
      this.#ping(ProbeTimeoutMs);
    }
  }

  /** Skips the current backoff wait (05's Retry button), except a rate-limit wait (01 §10.2). Backoff only. */
  retryNow(): void {
    this.#skipWait();
  }

  /**
   * Sends a request and resolves with the payload of its ok. It never retries; it rejects with a ProtocolError: the
   * server's error, or connection_lost (not ready, or the socket went away before the reply), request_timeout (no
   * reply within opts.timeoutMs, default 10 s) or not_ready (stopped).
   */
  request<K extends SignalRequestType>(
    type: K,
    data: ClientRequests[K]['data'],
    opts?: RequestOptions,
  ): Promise<ClientRequests[K]['result']> {
    if (this.#state !== 'ready') {
      const code = this.#state === 'stopped' ? LocalErrorCodeNotReady : LocalErrorCodeConnectionLost;
      return Promise.reject(ProtocolError.local(code));
    }
    const id = this.#newId();
    return new Promise<ClientRequests[K]['result']>((resolve, reject) => {
      const timer = setTimeout(() => {
        if (this.#pending.delete(id)) {
          reject(ProtocolError.local(LocalErrorCodeRequestTimeout));
        }
      }, opts?.timeoutMs ?? RequestTimeoutMs);
      this.#pending.set(id, {
        resolve: (result) => {
          resolve(result as ClientRequests[K]['result']);
        },
        reject,
        timer,
      });
      if (!this.#send({ type, id, data })) {
        this.#settle(id)?.reject(ProtocolError.local(LocalErrorCodeConnectionLost));
      }
    });
  }

  /** Sends a notification; false (and nothing sent) unless ready. */
  notify<K extends ClientNotificationType>(type: K, data: ClientNotifications[K]): boolean {
    return this.#state === 'ready' && this.#send({ type, data });
  }

  /**
   * Listens to one server message type. Replies go to request() and never here (ok, welcome, error with a re), and
   * neither do errors in scope connection or session, which onState reports. Message types this build doesn't know
   * are dropped (01 §8.13). A throwing listener is logged and doesn't disturb the others. Returns the unsubscribe
   * function.
   */
  on<K extends ServerMessageType>(type: K, fn: (data: ServerMessages[K], env: ServerEnvelope<K>) => void): () => void {
    let set = this.#listeners.get(type);
    if (set === undefined) {
      set = new Set();
      this.#listeners.set(type, set);
    }
    const listener = fn as Listener;
    set.add(listener);
    return () => {
      this.#listeners.get(type)?.delete(listener);
    };
  }

  /** Listens to state changes (see SignalStateInfo). Returns the unsubscribe function. */
  onState(fn: StateListener): () => void {
    this.#stateListeners.add(fn);
    return () => {
      this.#stateListeners.delete(fn);
    };
  }

  // ---- The connection ----

  #connect(): void {
    this.#backoffTimer.clear();
    this.#backoffRateLimited = false;
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.#opts.url);
    } catch (err) {
      // A malformed URL or a blocked port: a failed attempt like any other.
      report('SignalClient: cannot create the WebSocket', err);
      this.#enterBackoff({});
      return;
    }
    this.#ws = ws;
    this.#helloId = undefined;
    ws.onopen = () => {
      this.#onOpen(ws);
    };
    ws.onmessage = (ev: MessageEvent) => {
      if (ws === this.#ws) {
        this.#onMessage(ws, ev.data);
      }
    };
    ws.onclose = (ev: CloseEvent) => {
      if (ws === this.#ws) {
        this.#end({ closeCode: ev.code });
      }
    };
    ws.onerror = null; // a close event follows every error event
    this.#attemptTimer.set(() => {
      if (ws === this.#ws && this.#state === 'connecting') {
        this.#end({});
      }
    }, ConnectTimeoutMs);
    this.#state = 'connecting';
    this.#emit('connecting', this.#downInfo());
  }

  #onOpen(ws: WebSocket): void {
    if (ws !== this.#ws || this.#state !== 'connecting') {
      return;
    }
    const hello: Hello = {
      protocol: Version,
      minProtocol: MinVersion,
      features: [...(this.#opts.features ?? [])],
      client: this.#opts.client,
      role: this.#opts.role,
      caps: this.#caps(),
    };
    if (this.#resumeToken !== undefined) {
      hello.resumeToken = this.#resumeToken;
    }
    this.#helloId = this.#newId();
    this.#state = 'handshaking';
    if (!this.#send({ type: MessageTypeHello, id: this.#helloId, data: hello })) {
      this.#end({});
      return;
    }
    this.#attemptTimer.set(() => {
      if (ws === this.#ws && this.#state === 'handshaking') {
        this.#end({});
      }
    }, HandshakeTimeoutMs);
    this.#emit('handshaking', this.#downInfo());
  }

  #onMessage(ws: WebSocket, raw: unknown): void {
    const msg = parseIncoming(raw);
    if (msg === undefined) {
      report('SignalClient: dropped a malformed server message', undefined);
      return;
    }
    switch (msg.type) {
      case MessageTypeWelcome:
        if (this.#state === 'handshaking' && msg.re === this.#helloId) {
          this.#onWelcome(ws, msg.data);
        }
        return;
      case MessageTypeOK:
        if (msg.re !== undefined) {
          this.#settle(msg.re)?.resolve(msg.data);
        }
        return;
      case MessageTypeError:
        this.#onError(msg);
        return;
      case MessageTypePong:
        this.#pongTimer.clear();
        this.#pongDue = undefined;
        break;
      case MessageTypeServerShutdown:
        this.#onShutdown(msg.data);
        break;
    }
    this.#dispatch(msg);
  }

  #onWelcome(ws: WebSocket, data: Record<string, unknown>): void {
    if (typeof data['connectionId'] !== 'string' || typeof data['resumeToken'] !== 'string') {
      report('SignalClient: dropped a malformed welcome', undefined);
      this.#end({});
      return;
    }
    const w = data as unknown as Welcome;
    this.#attemptTimer.clear();
    this.#welcome = w;
    this.#resumeToken = w.resumeToken !== '' ? w.resumeToken : undefined;
    this.#helloId = undefined;
    this.#shutdown = undefined;
    this.#shutdownWaitPending = false;
    this.#rateLimitWaitMs = undefined;
    this.#state = 'ready';
    this.#stableTimer.set(() => {
      this.#attempt = 0;
    }, BackoffResetMs);
    this.#schedulePing();
    const serverVersion = data['serverVersion'];
    const info: SignalStateInfo = {
      resumed: data['resumed'] === true,
      staleBuild: isStaleBuild(typeof serverVersion === 'string' ? serverVersion : '', this.#buildVersion),
    };
    this.#resync(w);
    if (ws !== this.#ws || this.state !== 'ready') {
      return; // onResync stopped the client, or the socket went away meanwhile
    }
    this.#emit('ready', info);
  }

  #resync(w: Welcome): void {
    const onResync = this.#opts.onResync;
    if (onResync === undefined) {
      return;
    }
    try {
      const done = onResync(w);
      if (done instanceof Promise) {
        done.catch((err: unknown) => {
          report('SignalClient: onResync failed', err);
        });
      }
    } catch (err) {
      report('SignalClient: onResync failed', err);
    }
  }

  #onError(msg: Incoming): void {
    const err = ProtocolError.fromWire(msg.data);
    if (msg.re !== undefined && msg.re === this.#helloId) {
      this.#end({ error: err, hello: true });
      return;
    }
    const pending = msg.re !== undefined ? this.#settle(msg.re) : undefined;
    pending?.reject(err);
    if (err.scope === ErrorScopeConnection || err.scope === ErrorScopeSession) {
      // The server closes the socket next (01 §12.1). The error decides, so act now.
      this.#end({ error: err });
      return;
    }
    if (msg.re === undefined) {
      this.#dispatch(msg);
    }
  }

  #onShutdown(data: Record<string, unknown>): void {
    const ms = data['reconnectInMs'];
    const valid = typeof ms === 'number' && Number.isFinite(ms) && ms >= 0;
    const reason = data['reason'];
    this.#shutdown = {
      reason: typeof reason === 'string' ? (reason as ServerShutdown['reason']) : ShutdownReasonRestart,
      reconnectInMs: valid ? ms : 0,
    };
    this.#shutdownWaitPending = valid;
  }

  /**
   * The socket is done: pick backoff or stopped (01 §10.1; the server's error decides, the close code is the fallback,
   * §12.2), drop the socket and fail the pending requests.
   */
  #end(cause: EndCause): void {
    let error = cause.error;
    if (error === undefined && cause.closeCode !== undefined) {
      error = ProtocolError.fromCloseCode(cause.closeCode);
    }
    let stop = false;
    if (error !== undefined) {
      if (error.scope === ErrorScopeSession) {
        stop = true;
      } else if (error.scope === ErrorScopeConnection || cause.hello === true) {
        stop = !error.retryable;
      }
      if (error.code === ErrorCodeRateLimited && error.scope === ErrorScopeConnection) {
        this.#rateLimitWaitMs = Math.max(RateLimitMinWaitMs, error.retryAfterMs ?? 0);
      }
    }
    this.#dropSocket(ReconnectCloseCode);
    const info: SignalStateInfo = error !== undefined ? { error } : {};
    if (stop) {
      this.#removePageListeners();
      this.#resumeToken = undefined;
      this.#state = 'stopped';
      this.#emit('stopped', info);
      return;
    }
    this.#enterBackoff(info);
  }

  #enterBackoff(info: SignalStateInfo): void {
    let delay: number;
    let rateLimited = false;
    if (this.#rateLimitWaitMs !== undefined) {
      delay = this.#rateLimitWaitMs;
      rateLimited = true;
      this.#rateLimitWaitMs = undefined;
      this.#attempt++;
    } else if (this.#shutdownWaitPending && this.#shutdown !== undefined) {
      // 01 §10.2: the first delay after server.shutdown is exactly reconnectInMs, then the normal sequence.
      delay = this.#shutdown.reconnectInMs;
      this.#shutdownWaitPending = false;
    } else {
      delay = backoffDelay(this.#attempt);
      this.#attempt++;
    }
    this.#backoffRateLimited = rateLimited;
    this.#backoffTimer.set(() => {
      if (this.#state === 'backoff') {
        this.#connect();
      }
    }, delay);
    this.#state = 'backoff';
    this.#emit('backoff', { ...this.#downInfo(), ...info, delayMs: delay, rateLimited });
  }

  /** stop() and pagehide: close with 1000 (the server skips grace), fail the pending requests, stopped. */
  #halt(): void {
    if (this.#state === 'stopped') {
      return;
    }
    this.#dropSocket(CloseCodeNormal);
    this.#backoffTimer.clear();
    this.#resumeToken = undefined;
    this.#state = 'stopped';
    this.#emit('stopped', {});
  }

  /** Detaches and closes the current socket, stops its timers and rejects the pending requests. */
  #dropSocket(closeCode: number): void {
    const ws = this.#ws;
    this.#ws = undefined;
    this.#helloId = undefined;
    this.#attemptTimer.clear();
    this.#pingTimer.clear();
    this.#pongTimer.clear();
    this.#pongDue = undefined;
    this.#stableTimer.clear();
    if (ws !== undefined) {
      ws.onopen = null;
      ws.onmessage = null;
      ws.onclose = null;
      ws.onerror = null;
      if (ws.readyState < wsClosing) {
        try {
          ws.close(closeCode);
        } catch (err) {
          report('SignalClient: close failed', err);
        }
      }
    }
    const pending = [...this.#pending.values()];
    this.#pending.clear();
    for (const p of pending) {
      clearTimeout(p.timer);
      p.reject(ProtocolError.local(LocalErrorCodeConnectionLost));
    }
  }

  // ---- Heartbeat (01 §3.4) ----

  #schedulePing(): void {
    const limits = this.#welcome?.limits as Partial<Welcome['limits']> | undefined;
    const interval = limits?.pingIntervalMs;
    const ms = typeof interval === 'number' && interval > 0 ? interval : DefaultPingIntervalMs;
    this.#pingTimer.set(() => {
      if (this.#state === 'ready') {
        this.#ping(PongTimeoutMs);
        this.#schedulePing();
      }
    }, ms);
  }

  /** Sends a ping; without a pong by the earliest deadline of the pings out, drops the socket and reconnects. */
  #ping(timeoutMs: number): void {
    if (!this.#send({ type: MessageTypePing, data: { t: Date.now() } })) {
      return;
    }
    const due = Date.now() + timeoutMs;
    if (this.#pongDue !== undefined && this.#pongDue <= due) {
      return;
    }
    this.#pongDue = due;
    this.#pongTimer.set(() => {
      this.#pongDue = undefined;
      if (this.#state === 'ready') {
        this.#end({});
      }
    }, timeoutMs);
  }

  // ---- Page events (01 §3.4, §10.2, §16) ----

  readonly #onOnline = (): void => {
    this.#wake();
  };

  readonly #onVisibility = (): void => {
    if (document.visibilityState === 'visible') {
      this.#wake();
    }
  };

  /** Closes with 1000 so the server skips grace; pagehide, not beforeunload, which breaks the bfcache. */
  readonly #onPageHide = (): void => {
    if (this.#state !== 'stopped') {
      this.#halt();
      this.#suspended = true;
    }
  };

  /** Back from the bfcache: a new connection (resumed: false). */
  readonly #onPageShow = (ev: PageTransitionEvent): void => {
    if (ev.persisted && this.#suspended) {
      this.start();
    }
  };

  /** online or a visible page: a ping at once when ready; in backoff, skip the wait. */
  #wake(): void {
    if (this.#state === 'ready') {
      this.#ping(ProbeTimeoutMs);
    } else {
      this.#skipWait();
    }
  }

  #skipWait(): void {
    if (this.#state === 'backoff' && !this.#backoffRateLimited) {
      this.#connect();
    }
  }

  #addPageListeners(): void {
    if (this.#pageListeners || typeof window === 'undefined') {
      return;
    }
    this.#pageListeners = true;
    window.addEventListener('online', this.#onOnline);
    window.addEventListener('pagehide', this.#onPageHide);
    window.addEventListener('pageshow', this.#onPageShow);
    document.addEventListener('visibilitychange', this.#onVisibility);
  }

  #removePageListeners(): void {
    if (!this.#pageListeners) {
      return;
    }
    this.#pageListeners = false;
    window.removeEventListener('online', this.#onOnline);
    window.removeEventListener('pagehide', this.#onPageHide);
    window.removeEventListener('pageshow', this.#onPageShow);
    document.removeEventListener('visibilitychange', this.#onVisibility);
  }

  // ---- Helpers ----

  #newId(): string {
    return String(this.#nextId++);
  }

  #caps(): Caps {
    try {
      return this.#opts.caps();
    } catch (err) {
      report('SignalClient: caps() failed', err);
      return { decode: [] };
    }
  }

  #send(msg: { type: string; id?: string; data: unknown }): boolean {
    const ws = this.#ws;
    if (ws?.readyState !== wsOpen) {
      return false;
    }
    try {
      ws.send(JSON.stringify(msg));
      return true;
    } catch (err) {
      report('SignalClient: send failed', err);
      return false;
    }
  }

  /** Takes a pending request out and stops its timer. */
  #settle(id: string): PendingRequest | undefined {
    const p = this.#pending.get(id);
    if (p !== undefined) {
      this.#pending.delete(id);
      clearTimeout(p.timer);
    }
    return p;
  }

  #dispatch(msg: Incoming): void {
    const set = this.#listeners.get(msg.type);
    if (set === undefined || set.size === 0) {
      return;
    }
    const env = { type: msg.type, data: msg.data } as ServerEnvelope;
    for (const fn of [...set]) {
      try {
        fn(msg.data, env);
      } catch (err) {
        report(`SignalClient: a ${msg.type} listener failed`, err);
      }
    }
  }

  /** The info of the states between two connections: the pending server.shutdown. */
  #downInfo(): SignalStateInfo {
    return this.#shutdown !== undefined ? { shutdown: this.#shutdown } : {};
  }

  #emit(state: SignalState, info: SignalStateInfo): void {
    for (const fn of [...this.#stateListeners]) {
      try {
        fn(state, info);
      } catch (err) {
        report('SignalClient: a state listener failed', err);
      }
    }
  }
}

/** Parses a server frame; undefined for anything that isn't an envelope (binary, not JSON, no type, bad data). */
function parseIncoming(raw: unknown): Incoming | undefined {
  if (typeof raw !== 'string') {
    return undefined;
  }
  let v: unknown;
  try {
    v = JSON.parse(raw);
  } catch {
    return undefined;
  }
  if (!isRecord(v) || typeof v['type'] !== 'string') {
    return undefined;
  }
  const re = v['re'];
  const data = v['data'] ?? {};
  if (!isRecord(data)) {
    return undefined;
  }
  return { type: v['type'], re: typeof re === 'string' ? re : undefined, data };
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** The client's only log output: problems that are bugs somewhere (a listener, the server), never user errors. */
function report(what: string, err: unknown): void {
  console.error(what, err);
}
