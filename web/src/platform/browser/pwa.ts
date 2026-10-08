// The browser's PwaProvider and the service-worker registration (05 §8, §16.2–16.4).
//
// Install (§16.3 "Install prompts"): Chrome, Edge and Android fire beforeinstallprompt; the event is kept, so the
// account menu can offer "Install app" later. iOS has no prompt: installState() says 'ios-manual' and the UI
// explains Share → Add to Home Screen.
//
// Updates (§16.2): boot step 6 registers /sw.js (production builds only). A new worker installs next to the active
// one and waits. What that means depends on the page:
// - the page runs an older build: "update ready". applyUpdate() tells the worker to take over (SKIP_WAITING) and
//   reloads the page when it has (controllerchange), so the page and its cache switch to the new shell together;
// - the page already runs the worker's build (it was reloaded after the server was updated, and navigations go to
//   the network first): there is nothing to reload. The worker is told to take over at once, and no pill shows.
// A tab whose worker was replaced from another tab is judged the same way: an old page gets the pill, a current one
// carries on.
//
// Browsers look for a new worker only on a page load, and at most once a day after that. A tab that stays open
// would learn of a new build late, so the provider polls /version.json (written by build/version-plugin.ts, served
// `no-cache`): when its `shell` hash changes, it asks the registration to update, and the pill appears as soon as the
// new worker waits. Without a service worker (an old browser, or registration failed) a changed version.json is
// "update ready" by itself, and applying it is a plain reload.
import { createLogger } from '../../lib/log';
import { shellCacheName, SKIP_WAITING, type SkipWaitingMessage } from '../../sw/contract';
import type { InstallState, PwaProvider } from '../types';
import { deviceEnv, isIOSDevice, isStandalone, type DeviceEnv } from './device';

const log = createLogger('pwa');

/** The worker's URL and scope: the root, where 04 serves it with `no-cache` (04 §9.5). */
export const SW_URL = '/sw.js';
export const SW_SCOPE = '/';

/** The build's version file (05 §17.1): `{version, protocol, shell}`. */
export const VERSION_URL = '/version.json';

/** How often an open, visible tab looks at /version.json. */
export const VERSION_POLL_MS = 15 * 60_000;
/** A tab that becomes visible looks again only when its last look is at least this old. */
export const VERSION_POLL_MIN_GAP_MS = 60_000;
/** applyUpdate() reloads anyway when the waiting worker hasn't taken over after this long. */
export const APPLY_TIMEOUT_MS = 3000;

/** The parts of dist/version.json this file reads. */
interface VersionInfo {
  readonly version: string;
  readonly shell: string;
}

function isVersionInfo(v: unknown): v is VersionInfo {
  if (typeof v !== 'object' || v === null) return false;
  const r = v as Record<string, unknown>;
  return typeof r['version'] === 'string' && typeof r['shell'] === 'string' && r['shell'] !== '';
}

export interface BrowserPwaOptions {
  /** navigator.serviceWorker, or null where there are no service workers. Default: the browser's. */
  serviceWorker?: ServiceWorkerContainer | null;
  /** The page's view of the worker's caches, or null where there is no Cache API. Default: globalThis.caches. */
  caches?: Pick<CacheStorage, 'match'> | null;
  /** The URL of the entry chunk this page runs. Default: the document's module script. */
  entryUrl?: () => string | null;
  /** Device facts; default: the real navigator and matchMedia. */
  env?: DeviceEnv;
  /** Reloads the page. Default: location.reload(). */
  reload?: () => void;
  /** This page's build version. Default: __ISSHONI_VERSION__. */
  buildVersion?: string;
}

function browserServiceWorker(): ServiceWorkerContainer | null {
  // Missing in an insecure context and in old browsers; lib.dom declares it as always there.
  return 'serviceWorker' in globalThis.navigator ? globalThis.navigator.serviceWorker : null;
}

function browserCaches(): CacheStorage | null {
  return 'caches' in globalThis ? globalThis.caches : null;
}

/** index.html has one module script: the entry chunk, whose hashed name is different in every build. */
function entryScriptUrl(): string | null {
  return globalThis.document.querySelector<HTMLScriptElement>('script[type="module"][src]')?.src ?? null;
}

/** BrowserPlatform's PwaProvider. One per page: createPwaProvider() and registerServiceWorker() share it. */
export class BrowserPwa implements PwaProvider {
  readonly #sw: ServiceWorkerContainer | null;
  readonly #caches: Pick<CacheStorage, 'match'> | null;
  readonly #entryUrl: () => string | null;
  readonly #env: DeviceEnv;
  readonly #reload: () => void;
  readonly #buildVersion: string;
  readonly #cleanup: (() => void)[] = [];

  #installPrompt: BeforeInstallPromptEvent | null = null;
  #installed = false;

  #registering: Promise<void> | null = null;
  #registration: ServiceWorkerRegistration | null = null;
  /** Whether a worker controlled this page before the latest controllerchange. */
  #controlled = false;
  /** Waiting workers that were already judged (or are being judged). */
  readonly #judged = new WeakSet<ServiceWorker>();
  #ready = false;
  readonly #readyListeners = new Set<() => void>();
  #applying = false;
  #reloaded = false;

  /** The `shell` of the last /version.json poll; null before the first. */
  #shell: string | null = null;
  /** A poll showed a build that isn't this page's. */
  #newBuild = false;
  #checking = false;
  #lastCheck = 0;

  constructor(opts: BrowserPwaOptions = {}) {
    this.#sw = opts.serviceWorker === undefined ? browserServiceWorker() : opts.serviceWorker;
    this.#caches = opts.caches === undefined ? browserCaches() : opts.caches;
    this.#entryUrl = opts.entryUrl ?? entryScriptUrl;
    this.#env = opts.env ?? deviceEnv();
    this.#reload =
      opts.reload ??
      (() => {
        globalThis.location.reload();
      });
    this.#buildVersion = opts.buildVersion ?? __ISSHONI_VERSION__;

    // The browser fires beforeinstallprompt once, early: listen from the start (boot step 2).
    const onPrompt = (event: BeforeInstallPromptEvent): void => {
      event.preventDefault(); // no browser mini-infobar: the app offers "Install app" itself (05 §16.3)
      this.#installPrompt = event;
    };
    const onInstalled = (): void => {
      this.#installPrompt = null;
      this.#installed = true;
    };
    globalThis.addEventListener('beforeinstallprompt', onPrompt);
    globalThis.addEventListener('appinstalled', onInstalled);
    this.#cleanup.push(() => {
      globalThis.removeEventListener('beforeinstallprompt', onPrompt);
      globalThis.removeEventListener('appinstalled', onInstalled);
    });
  }

  // ---- install ----

  installState(): InstallState {
    if (this.#installed || isStandalone(this.#env)) return 'installed';
    if (this.#installPrompt) return 'prompt';
    if (isIOSDevice(this.#env)) return 'ios-manual';
    return 'none';
  }

  /** Shows the browser's install dialog; call it from a click. 'unavailable' when no prompt was kept. */
  async promptInstall(): Promise<'accepted' | 'dismissed' | 'unavailable'> {
    const event = this.#installPrompt;
    if (!event) return 'unavailable';
    this.#installPrompt = null; // an event prompts once; the browser fires a new one if it may ask again
    try {
      await event.prompt();
      return (await event.userChoice).outcome;
    } catch (err) {
      log.warn('the install prompt failed', { error: err });
      return 'unavailable';
    }
  }

  // ---- updates ----

  /**
   * Calls fn once an update is ready; a listener added after that is called too (in a microtask), so it doesn't
   * matter whether the shell mounted before or after the worker was found waiting.
   */
  onUpdateReady(fn: () => void): () => void {
    let subscribed = true;
    const listener = (): void => {
      if (subscribed) fn();
    };
    this.#readyListeners.add(listener);
    if (this.#ready) queueMicrotask(listener);
    return () => {
      subscribed = false;
      this.#readyListeners.delete(listener);
    };
  }

  /**
   * Switches to the new build: a waiting worker is told to take over and the page reloads when it controls the page
   * (or after APPLY_TIMEOUT_MS, whichever is first). Without a waiting worker it reloads at once: the server always
   * serves the current SPA, and navigations go to the network first.
   */
  applyUpdate(): void {
    const waiting = this.#registration?.waiting;
    if (!waiting) {
      this.#reloadOnce();
      return;
    }
    if (this.#applying) return;
    this.#applying = true;
    waiting.postMessage({ type: SKIP_WAITING } satisfies SkipWaitingMessage);
    const timer = setTimeout(() => {
      this.#reloadOnce();
    }, APPLY_TIMEOUT_MS);
    this.#cleanup.push(() => {
      clearTimeout(timer);
    });
  }

  /**
   * Registers the service worker and starts watching for updates. Boot calls it once, after the first render, in
   * production builds only (05 §4 step 6); later calls return the first call's promise. It rejects when the browser
   * refuses the registration; the /version.json poll runs either way.
   */
  register(): Promise<void> {
    return (this.#registering ??= this.#register());
  }

  /** Stops the poll and removes every listener (tests; a page never disposes its provider). */
  dispose(): void {
    for (const undo of this.#cleanup.splice(0)) undo();
    this.#readyListeners.clear();
  }

  async #register(): Promise<void> {
    try {
      const sw = this.#sw;
      if (!sw) return;
      this.#controlled = sw.controller !== null;
      const onControllerChange = (): void => {
        this.#onControllerChange();
      };
      sw.addEventListener('controllerchange', onControllerChange);
      this.#cleanup.push(() => {
        sw.removeEventListener('controllerchange', onControllerChange);
      });
      const registration = await sw.register(SW_URL, { scope: SW_SCOPE, updateViaCache: 'none' });
      this.#registration = registration;
      this.#watch(registration);
    } finally {
      this.#startVersionChecks();
    }
  }

  /** Follows a registration's new workers until one waits. */
  #watch(registration: ServiceWorkerRegistration): void {
    const track = (worker: ServiceWorker | null): void => {
      if (!worker) return;
      const onState = (): void => {
        if (worker.state === 'installed') this.#noteWaiting(registration);
      };
      worker.addEventListener('statechange', onState);
      this.#cleanup.push(() => {
        worker.removeEventListener('statechange', onState);
      });
    };
    const onUpdateFound = (): void => {
      track(registration.installing);
    };
    registration.addEventListener('updatefound', onUpdateFound);
    this.#cleanup.push(() => {
      registration.removeEventListener('updatefound', onUpdateFound);
    });
    track(registration.installing);
    this.#noteWaiting(registration);
  }

  /**
   * A worker waits. Behind an active one, that is a new build (the very first worker also passes through
   * "installed", but with nothing active before it: the first install, not an update). An old page gets the pill;
   * a page that already runs the worker's build lets it take over without a reload.
   */
  #noteWaiting(registration: ServiceWorkerRegistration): void {
    const waiting = registration.waiting;
    if (!waiting || !registration.active || this.#ready || this.#judged.has(waiting)) return;
    this.#judged.add(waiting);
    void this.#pageIsCurrent().then((current) => {
      if (registration.waiting !== waiting) return; // it took over, or a newer one replaced it, meanwhile
      if (!current) {
        this.#markReady();
        return;
      }
      log.info('the page is current: the new worker takes over without a reload');
      waiting.postMessage({ type: SKIP_WAITING } satisfies SkipWaitingMessage);
    });
  }

  #onControllerChange(): void {
    if (this.#applying) {
      this.#reloadOnce();
      return;
    }
    // Not asked for by this tab's pill. The first worker claiming the page changes nothing. A worker replacing
    // another one was let in by another tab, or by this page because it is current; an old page now runs the old
    // shell under the new worker and needs the reload.
    const replaced = this.#controlled;
    this.#controlled = true;
    if (!replaced || this.#ready) return;
    void this.#pageIsCurrent().then((current) => {
      if (!current) this.#markReady();
    });
  }

  /**
   * Whether this page already runs the build that the server has now: a worker of that build has precached its shell
   * into the cache named after /version.json's `shell`, and the page's entry chunk (whose hashed name changes with
   * every build) is in it. False whenever that can't be shown (offline, no Cache API, another build's chunk): the
   * pill and a reload are always a safe answer.
   */
  async #pageIsCurrent(): Promise<boolean> {
    const caches = this.#caches;
    const entry = this.#entryUrl();
    if (!caches || !entry) return false;
    const file = await this.#fetchVersion();
    if (!file) return false;
    try {
      const cached = await caches.match(entry, { cacheName: shellCacheName(file.shell), ignoreVary: true });
      return cached !== undefined;
    } catch {
      return false;
    }
  }

  #markReady(): void {
    if (this.#ready) return;
    this.#ready = true;
    log.info('update ready');
    for (const listener of [...this.#readyListeners]) listener();
  }

  #reloadOnce(): void {
    if (this.#reloaded) return;
    this.#reloaded = true;
    this.#reload();
  }

  // ---- /version.json ----

  #startVersionChecks(): void {
    const check = (): void => {
      void this.#checkVersion();
    };
    const timer = setInterval(() => {
      if (globalThis.document.visibilityState !== 'hidden') check();
    }, VERSION_POLL_MS);
    // An installed app stays in memory for days: look again when it comes back to the front.
    const onVisibility = (): void => {
      if (globalThis.document.visibilityState !== 'visible') return;
      if (Date.now() - this.#lastCheck >= VERSION_POLL_MIN_GAP_MS) check();
    };
    globalThis.document.addEventListener('visibilitychange', onVisibility);
    this.#cleanup.push(() => {
      clearInterval(timer);
      globalThis.document.removeEventListener('visibilitychange', onVisibility);
    });
    check();
  }

  /**
   * Reads /version.json and, when it shows a new build, starts the update. The first read compares the file's
   * `version` with this page's (a page started from a stale cache differs at once); later reads compare `shell`
   * with the previous read, which also catches a rebuild under the same version.
   */
  async #checkVersion(): Promise<void> {
    if (this.#checking || this.#ready) return;
    this.#checking = true;
    this.#lastCheck = Date.now();
    try {
      const file = await this.#fetchVersion();
      if (!file) return;
      const known = this.#shell;
      this.#shell = file.shell;
      if (known === null ? file.version !== this.#buildVersion : file.shell !== known) this.#newBuild = true;
      if (this.#newBuild) await this.#fetchUpdate();
    } finally {
      this.#checking = false;
    }
  }

  async #fetchVersion(): Promise<VersionInfo | null> {
    try {
      // globalThis.fetch is looked up per call, so test interceptors apply. no-store: never a cached copy.
      const response = await globalThis.fetch(new URL(VERSION_URL, globalThis.location.origin), { cache: 'no-store' });
      if (!response.ok) return null;
      const body: unknown = await response.json();
      return isVersionInfo(body) ? body : null;
    } catch {
      return null; // offline, or not JSON (a dev server without a build): the next check tries again
    }
  }

  /**
   * A new build is on the server. With a registration, the browser fetches the new worker; the pill appears once it
   * has installed the new shell and waits (#watch). A failed update is tried again at the next check. Without a
   * registration there is nothing to wait for.
   */
  async #fetchUpdate(): Promise<void> {
    const registration = this.#registration;
    if (!registration) {
      this.#markReady();
      return;
    }
    try {
      await registration.update();
    } catch (err) {
      log.warn('the service worker update failed', { error: err });
      return;
    }
    this.#noteWaiting(registration);
  }
}

let shared: BrowserPwa | null = null;

function sharedPwa(): BrowserPwa {
  return (shared ??= new BrowserPwa());
}

/** The page's PwaProvider (BrowserPlatform calls this once per platform; every call returns the same provider). */
export function createPwaProvider(): PwaProvider {
  return sharedPwa();
}

/**
 * Registers the service worker (05 §16.2): `navigator.serviceWorker.register('/sw.js', {scope: '/', updateViaCache:
 * 'none'})`, and starts the update checks. Boot step 6 calls it once, after the first render, when the browser is
 * idle, and only in production builds: there is no /sw.js on the Vite dev server.
 */
export function registerServiceWorker(): Promise<void> {
  return sharedPwa().register();
}
