// The browser's NotificationsProvider: Web Push on this device (05 §8, §16.3). BrowserPlatform calls
// createBrowserNotifications() once.
//
// support() (05 §16.3), from the device first, then the server:
// - `needs-install`: iOS or iPadOS (the UA, or MacIntel with touch) in a browser tab, not the Home Screen app;
// - `unsupported`: no service worker (in this build or browser), no Push API or no Notification API; or a server
//   without push (`GET /api/v1/info` has no `push`), where the Home Screen app wouldn't help either;
// - `denied`: Notification.permission is 'denied';
// - `supported`: everything is there.
//
// The device decides alone where it can: a browser that can't do push asks the server nothing. A server that
// couldn't be asked counts as one with push, and so does one that hasn't answered within INFO_WAIT_MS: support()
// never rejects and never waits longer than that, because BrowserPlatform.capabilities() waits for it (and the
// stage's controls wait for capabilities()). enable() then asks again and fails with the real reason. The answer of
// /info is kept for INFO_MAX_AGE_MS.
//
// - enable(), from a click: Notification.requestPermission() first, in the click's turn (Safari only asks within
//   the user activation); then the VAPID key from a fresh /info (`push.vapidPublicKey`), pushManager.subscribe()
//   ({userVisibleOnly: true}) and POST /api/v1/push/subscriptions with the subscription (03 §12.4.6). A permission
//   that wasn't granted is no error: enable() resolves, status() stays 'off' and support() says 'denied' when the
//   user blocked it. A subscription the server refused is taken back, so status() and the server agree.
// - disable(): POST /api/v1/push/unsubscribe {endpoint}, then PushSubscription.unsubscribe(). A 401 (signed out)
//   doesn't keep the subscription: the server dropped it with the session (03).
// - test(): POST /api/v1/push/test (this browser's subscriptions; 1 per 10 s). A 404 means the server has no
//   subscription of this session (it went with an older session, or was evicted): the subscription is posted again
//   and the test tried once more.
// - status(): 'on' while permission is granted and the worker has a subscription. Every such call also does the
//   start-up re-post of 05 §16.3: the subscription goes to the server again (the server upserts it by endpoint and
//   binds it to this session, 03 §12.4.6), and is replaced first when its key isn't /info's (after `rotate-secrets
//   --include-vapid`). There is no once-per-page latch: a login, a logout and a revoked session are navigations
//   inside the page, and the new session needs the post as much as the first one did (03 §12.4.6 "keeps the
//   binding current after re-logins"; 05 §16.3: after a 401 in the worker, "the page re-posts after the next
//   login"). The SPA calls status() only on pages under RequireAuth, once GET /api/v1/me has succeeded (the room
//   page and the notifications page, on every mount), so logged-out pages never call the push endpoints.
//
// A click on a notification (sw/worker.ts) focuses an open window and posts {type: 'open', url} to it. The provider
// hears it: onNotificationOpen()'s latest handler navigates inside the SPA (the room page registers the router's
// navigate), else the page goes to the URL with a full navigation. The URL is checked again: this origin only.
import { createLogger } from '../../lib/log';
import {
  CodeNotFound,
  CodePushUnavailable,
  type Info,
  type PushSubscribeRequest,
  type PushUnsubscribeRequest,
} from '../../protocol/api.gen';
import { api as restApi, ApiError, isSignedOutError } from '../../protocol/rest';
import { OPEN_URL } from '../../sw/contract';
import type { NotificationsProvider, PushSupport } from '../types';
import { deviceEnv, isIOSDevice, isStandalone, type DeviceEnv } from './device';

const log = createLogger('push');

/** How long an answer of GET /api/v1/info counts for support() and the start-up re-post. enable() asks again. */
export const INFO_MAX_AGE_MS = 5 * 60_000;
/** How long support() waits for /info; a server that hasn't answered by then counts as one with push. */
export const INFO_WAIT_MS = 3_000;
/** How long enable() waits for an active service worker (a first visit installs it after the first render). */
export const READY_TIMEOUT_MS = 10_000;

/** 03's endpoints (03 §12.4.6). */
export const PUSH_SUBSCRIPTIONS_PATH = '/api/v1/push/subscriptions';
export const PUSH_UNSUBSCRIBE_PATH = '/api/v1/push/unsubscribe';
export const PUSH_TEST_PATH = '/api/v1/push/test';

/** Why enable() couldn't subscribe this browser, when no server answer says why (that is an ApiError). */
export type PushSetupFailure =
  /** No service worker, Push API or Notification API: support() said so already. */
  | 'unsupported'
  /** The service worker didn't become active within READY_TIMEOUT_MS. */
  | 'no_worker'
  /** /info's VAPID key isn't base64url. */
  | 'bad_key'
  /** The browser's push service refused (no push service, push turned off in the browser, …). */
  | 'subscribe_failed';

/** enable() failed on this device. The UI shows one text for all of them; `reason` is for the log. */
export class PushSetupError extends Error {
  override readonly name = 'PushSetupError';
  readonly reason: PushSetupFailure;

  constructor(reason: PushSetupFailure, options?: ErrorOptions) {
    super(`push: ${reason}`, options);
    this.reason = reason;
  }
}

/** The parts of the Notification API that the provider uses. */
export interface NotificationApi {
  readonly permission: NotificationPermission;
  requestPermission(): Promise<NotificationPermission>;
}

export interface BrowserNotificationsOptions {
  /** Device facts; default: the real navigator and matchMedia. */
  env?: DeviceEnv;
  /**
   * navigator.serviceWorker where this build registers a worker, else null. Default: the browser's in production
   * builds, null in the others (05 §16.2: the worker is registered in production only, so a dev build can't push).
   */
  serviceWorker?: ServiceWorkerContainer | null;
  /** The Notification API, or null where there is none. Default: globalThis.Notification. */
  notification?: NotificationApi | null;
  /** Whether the browser has the Push API (PushManager). Default: whether globalThis has it. */
  pushManager?: boolean;
  /** REST. Default: protocol/rest.ts api(), which boot binds to the platform. */
  api?: typeof restApi;
  /** The page's origin, for the URL of a notification click. Default: location.origin. */
  origin?: string;
  /** A full navigation, for a notification click that no page handles. Default: location.assign(). */
  assign?: (url: string) => void;
  /** Default: Date.now. */
  now?: () => number;
  /** Default: READY_TIMEOUT_MS. */
  readyTimeoutMs?: number;
  /** Default: INFO_WAIT_MS. */
  infoWaitMs?: number;
}

function browserServiceWorker(): ServiceWorkerContainer | null {
  if (!import.meta.env.PROD) return null;
  return 'serviceWorker' in globalThis.navigator ? globalThis.navigator.serviceWorker : null;
}

function browserNotification(): NotificationApi | null {
  return 'Notification' in globalThis ? globalThis.Notification : null;
}

/** /info's VAPID public key, or null when the server has no push (03 §12.4.1: `push` is absent then). */
function vapidKey(info: Info | null | undefined): string | null {
  const key: unknown = info?.push?.vapidPublicKey;
  return typeof key === 'string' && key !== '' ? key : null;
}

/** Decodes base64url (with or without padding); null when it isn't. */
export function base64UrlBytes(s: string): Uint8Array<ArrayBuffer> | null {
  if (!/^[A-Za-z0-9_-]*={0,2}$/.test(s)) return null;
  const b64 = s.replace(/=+$/, '').replace(/-/g, '+').replace(/_/g, '/');
  if (b64.length % 4 === 1) return null;
  try {
    const bin = atob(b64 + '='.repeat((4 - (b64.length % 4)) % 4));
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  } catch {
    return null;
  }
}

/**
 * Whether a subscription was made with this key. A browser that doesn't report the key (options.applicationServerKey
 * null) is taken at its word: the subscription is kept, not replaced on every start.
 */
export function hasKey(sub: PushSubscription, key: Uint8Array): boolean {
  const current = sub.options.applicationServerKey;
  if (current === null) return true;
  const bytes = new Uint8Array(current);
  return bytes.length === key.length && bytes.every((b, i) => b === key[i]);
}

/** 03's body: PushSubscription.toJSON() trimmed to the endpoint and the two keys; null when one is missing. */
export function subscribeRequest(sub: PushSubscription): PushSubscribeRequest | null {
  const json = sub.toJSON();
  const p256dh = json.keys?.['p256dh'];
  const auth = json.keys?.['auth'];
  if (!json.endpoint || !p256dh || !auth) return null;
  return { endpoint: json.endpoint, keys: { p256dh, auth } };
}

/**
 * What a click on a notification asks a page to open: path, query and fragment of a URL of this origin, or null.
 * The worker checked it already (sw/push.ts clickUrl); a message is data all the same, and a page navigates only
 * within its own origin (05 §20).
 */
export function openRequest(data: unknown, origin: string): string | null {
  if (typeof data !== 'object' || data === null) return null;
  const { type, url } = data as { type?: unknown; url?: unknown };
  if (type !== OPEN_URL || typeof url !== 'string' || url === '') return null;
  let target: URL;
  try {
    target = new URL(url, origin);
  } catch {
    return null;
  }
  // Two slashes would make the path a protocol-relative URL to another site; no SPA route starts so.
  if (target.origin !== origin || target.pathname.startsWith('//')) return null;
  return target.pathname + target.search + target.hash;
}

/** Navigates the SPA to a path of this origin. */
export type NotificationOpenHandler = (url: string) => void;

const openHandlers: NotificationOpenHandler[] = [];

/**
 * Makes fn the way a notification click opens its URL in this page (the latest registration wins; the one before
 * is used again when it unregisters). The room page registers the router's navigate while it is mounted; without
 * any, the page goes to the URL with a full navigation. Returns the unregister function.
 */
export function onNotificationOpen(fn: NotificationOpenHandler): () => void {
  openHandlers.push(fn);
  return () => {
    const i = openHandlers.lastIndexOf(fn);
    if (i >= 0) openHandlers.splice(i, 1);
  };
}

/** BrowserPlatform's NotificationsProvider. */
export class BrowserNotifications implements NotificationsProvider {
  readonly #env: DeviceEnv;
  readonly #sw: ServiceWorkerContainer | null;
  readonly #notification: NotificationApi | null;
  readonly #pushManager: boolean;
  readonly #api: typeof restApi;
  readonly #origin: string;
  readonly #assign: (url: string) => void;
  readonly #now: () => number;
  readonly #readyTimeoutMs: number;
  readonly #infoWaitMs: number;
  readonly #cleanup: (() => void)[] = [];

  /** The latest answer of /info: the VAPID key, or null for a server without push. */
  #info: { at: number; key: string | null } | null = null;
  #infoLoad: Promise<string | null> | null = null;
  /** enable(), disable() and status() run one at a time: none of them sees another's half-done subscription. */
  #queue: Promise<unknown> = Promise.resolve();

  constructor(opts: BrowserNotificationsOptions = {}) {
    this.#env = opts.env ?? deviceEnv();
    this.#sw = opts.serviceWorker === undefined ? browserServiceWorker() : opts.serviceWorker;
    this.#notification = opts.notification === undefined ? browserNotification() : opts.notification;
    this.#pushManager = opts.pushManager ?? 'PushManager' in globalThis;
    this.#api = opts.api ?? restApi;
    this.#origin = opts.origin ?? globalThis.location.origin;
    this.#assign =
      opts.assign ??
      ((url) => {
        globalThis.location.assign(url);
      });
    this.#now = opts.now ?? Date.now;
    this.#readyTimeoutMs = opts.readyTimeoutMs ?? READY_TIMEOUT_MS;
    this.#infoWaitMs = opts.infoWaitMs ?? INFO_WAIT_MS;

    const sw = this.#sw;
    if (sw) {
      const onMessage = (event: MessageEvent): void => {
        const url = openRequest(event.data, this.#origin);
        if (url === null) return;
        const handler = openHandlers.at(-1);
        if (handler) handler(url);
        else this.#assign(url);
      };
      sw.addEventListener('message', onMessage);
      this.#cleanup.push(() => {
        sw.removeEventListener('message', onMessage);
      });
    }
  }

  async support(): Promise<PushSupport> {
    const ios = isIOSDevice(this.#env) && !isStandalone(this.#env);
    if (!ios && !this.#browserCan()) return 'unsupported';
    let key: string | null | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const late = new Promise<undefined>((resolve) => {
      timer = setTimeout(() => {
        log.debug('GET /api/v1/info is slow; push counts as there');
        resolve(undefined);
      }, this.#infoWaitMs);
    });
    try {
      // The read goes on after a timeout: its answer fills the cache for the next call and the re-post.
      key = await Promise.race([this.#serverKey(INFO_MAX_AGE_MS), late]);
    } catch (err) {
      log.debug('GET /api/v1/info failed; push counts as there', { error: err });
      key = undefined;
    } finally {
      clearTimeout(timer);
    }
    if (key === null) return 'unsupported';
    if (ios) return 'needs-install';
    if (this.#notification?.permission === 'denied') return 'denied';
    return 'supported';
  }

  status(): Promise<'on' | 'off'> {
    return this.#serial(() => this.#status());
  }

  enable(): Promise<void> {
    const notification = this.#notification;
    if (!notification || !this.#browserCan()) return Promise.reject(new PushSetupError('unsupported'));
    // In the click's own turn: Safari shows the prompt only within the user activation.
    let permission: Promise<NotificationPermission>;
    try {
      permission = Promise.resolve(notification.requestPermission());
    } catch (err) {
      return Promise.reject(err instanceof Error ? err : new PushSetupError('unsupported'));
    }
    return this.#serial(async () => {
      if ((await permission) !== 'granted') return;
      const key = await this.#serverKey(0);
      if (key === null) throw new ApiError({ status: 503, code: CodePushUnavailable });
      const bytes = base64UrlBytes(key);
      if (bytes === null) throw new PushSetupError('bad_key');
      const registration = await this.#ready();
      const sub = await this.#subscribe(registration, bytes);
      try {
        await this.#post(sub);
      } catch (err) {
        await sub.unsubscribe().catch(() => false);
        throw err;
      }
    });
  }

  disable(): Promise<void> {
    return this.#serial(async () => {
      const sub = await this.#subscription();
      if (!sub) return;
      try {
        await this.#api('POST', PUSH_UNSUBSCRIBE_PATH, { endpoint: sub.endpoint } satisfies PushUnsubscribeRequest);
      } catch (err) {
        if (!isSignedOutError(err)) throw err;
      }
      await sub.unsubscribe();
    });
  }

  async test(): Promise<void> {
    try {
      await this.#api('POST', PUSH_TEST_PATH);
    } catch (err) {
      // 404: the server has no subscription of this session (03 §12.4.6). The 404 comes before the rate limit, so
      // the second try after the re-post isn't refused for being the second.
      if (!(err instanceof ApiError && err.code === CodeNotFound)) throw err;
      if ((await this.status()) !== 'on') throw err;
      await this.#api('POST', PUSH_TEST_PATH);
    }
  }

  /** Stops listening for notification clicks (tests; a page keeps its provider). */
  dispose(): void {
    for (const undo of this.#cleanup.splice(0)) undo();
  }

  // ---- internals ----

  #browserCan(): boolean {
    return this.#sw !== null && this.#pushManager && this.#notification !== null;
  }

  #serial<T>(task: () => Promise<T>): Promise<T> {
    const run = this.#queue.then(task, task);
    this.#queue = run.catch(() => undefined);
    return run;
  }

  /** /info's VAPID key (null: the server has no push), from an answer at most maxAgeMs old. Rejects as api() does. */
  #serverKey(maxAgeMs: number): Promise<string | null> {
    const cached = this.#info;
    if (cached !== null && this.#now() - cached.at < maxAgeMs) return Promise.resolve(cached.key);
    this.#infoLoad ??= this.#api<Info>('GET', '/api/v1/info')
      .then((info) => {
        const key = vapidKey(info);
        this.#info = { at: this.#now(), key };
        return key;
      })
      .finally(() => {
        this.#infoLoad = null;
      });
    return this.#infoLoad;
  }

  /** The worker's registration if there is one now; null without waiting for one. */
  async #registration(): Promise<ServiceWorkerRegistration | null> {
    if (!this.#sw) return null;
    try {
      return (await this.#sw.getRegistration()) ?? null;
    } catch {
      return null;
    }
  }

  async #subscription(): Promise<PushSubscription | null> {
    const registration = await this.#registration();
    if (!registration) return null;
    try {
      return await registration.pushManager.getSubscription();
    } catch {
      return null;
    }
  }

  /** The active worker's registration; PushSetupError('no_worker') when none is active in time. */
  async #ready(): Promise<ServiceWorkerRegistration> {
    const sw = this.#sw;
    if (!sw) throw new PushSetupError('unsupported');
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeout = new Promise<null>((resolve) => {
      timer = setTimeout(() => {
        resolve(null);
      }, this.#readyTimeoutMs);
    });
    try {
      const registration = await Promise.race([sw.ready, timeout]);
      if (!registration) throw new PushSetupError('no_worker');
      return registration;
    } finally {
      clearTimeout(timer);
    }
  }

  /**
   * This browser's subscription for the key: the one it has when it was made with the key, else a new one (an
   * old one with another key is dropped first: a browser keeps one subscription per worker).
   */
  async #subscribe(registration: ServiceWorkerRegistration, key: Uint8Array<ArrayBuffer>): Promise<PushSubscription> {
    try {
      const existing = await registration.pushManager.getSubscription();
      if (existing && hasKey(existing, key)) return existing;
      if (existing) await existing.unsubscribe();
      return await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
    } catch (err) {
      throw new PushSetupError('subscribe_failed', { cause: err });
    }
  }

  /** POST /api/v1/push/subscriptions. The server upserts by endpoint, so posting again is harmless. */
  async #post(sub: PushSubscription): Promise<void> {
    const body = subscribeRequest(sub);
    if (body === null) throw new PushSetupError('subscribe_failed');
    await this.#api('POST', PUSH_SUBSCRIPTIONS_PATH, body);
  }

  async #status(): Promise<'on' | 'off'> {
    if (!this.#browserCan() || this.#notification?.permission !== 'granted') return 'off';
    let sub = await this.#subscription();
    if (sub) sub = await this.#resync(sub);
    return sub ? 'on' : 'off';
  }

  /**
   * The start-up re-post (05 §16.3): post the subscription again, after replacing it when /info has another key.
   * The key needs to be only as fresh as one app start, so support()'s answer of a moment ago does. A failure is
   * logged and tried again at the next status(); the subscription stays. Returns the subscription the browser has
   * afterwards (null when a replacement failed half-way).
   */
  async #resync(sub: PushSubscription): Promise<PushSubscription | null> {
    let key: string | null;
    try {
      key = await this.#serverKey(INFO_MAX_AGE_MS);
    } catch (err) {
      log.info('the push subscription was not re-posted: /info failed', { error: err });
      return sub;
    }
    // A server without push keeps the browser's subscription: it may be turned on again.
    if (key === null) return sub;
    let current: PushSubscription | null = sub;
    try {
      const bytes = base64UrlBytes(key);
      if (bytes !== null && !hasKey(sub, bytes)) {
        log.info('the server has a new push key: subscribing again');
        current = null;
        const registration = await this.#registration();
        if (registration) current = await this.#subscribe(registration, bytes);
        if (!current) return null;
      }
      await this.#post(current);
    } catch (err) {
      if (isSignedOutError(err)) return current; // logged out meanwhile: the next sign-in's page posts it
      log.warn('the push subscription was not re-posted', { error: err });
    }
    return current;
  }
}

/** The browser's NotificationsProvider (BrowserPlatform calls this once). */
export function createBrowserNotifications(opts: BrowserNotificationsOptions = {}): NotificationsProvider {
  return new BrowserNotifications(opts);
}
