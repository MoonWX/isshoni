import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { DeviceEnv } from './device';
import {
  APPLY_TIMEOUT_MS,
  BrowserPwa,
  createPwaProvider,
  SW_URL,
  VERSION_POLL_MIN_GAP_MS,
  VERSION_POLL_MS,
  type BrowserPwaOptions,
} from './pwa';

// ---- fakes ----

class FakeWorker extends EventTarget {
  readonly messages: unknown[] = [];
  constructor(public state: ServiceWorkerState) {
    super();
  }
  postMessage(message: unknown): void {
    this.messages.push(message);
  }
  setState(state: ServiceWorkerState): void {
    this.state = state;
    this.dispatchEvent(new Event('statechange'));
  }
}

class FakeRegistration extends EventTarget {
  installing: FakeWorker | null = null;
  waiting: FakeWorker | null = null;
  active: FakeWorker | null = null;
  updateCalls = 0;
  /** What update() does; default: nothing new on the server. */
  onUpdate: () => void | Promise<void> = () => undefined;

  async update(): Promise<void> {
    this.updateCalls++;
    await this.onUpdate();
  }

  /** The browser found a new worker script: it starts installing. */
  startInstall(): FakeWorker {
    const worker = new FakeWorker('installing');
    this.installing = worker;
    this.dispatchEvent(new Event('updatefound'));
    return worker;
  }

  /** The installing worker has cached its shell and now waits (or, with nothing active, will activate). */
  finishInstall(): FakeWorker {
    const worker = this.installing;
    if (!worker) throw new Error('nothing is installing');
    this.installing = null;
    this.waiting = worker;
    worker.setState('installed');
    return worker;
  }
}

class FakeContainer extends EventTarget {
  controller: FakeWorker | null = null;
  readonly registerCalls: { url: string | URL; options: RegistrationOptions | undefined }[] = [];
  registerError: Error | null = null;

  constructor(readonly registration = new FakeRegistration()) {
    super();
  }

  register(url: string | URL, options?: RegistrationOptions): Promise<FakeRegistration> {
    this.registerCalls.push({ url, options });
    return this.registerError ? Promise.reject(this.registerError) : Promise.resolve(this.registration);
  }

  /** A worker took control of the page (clients.claim() after activation). */
  takeControl(worker: FakeWorker): void {
    if (this.registration.waiting === worker) this.registration.waiting = null;
    this.registration.active = worker;
    this.controller = worker;
    worker.setState('activated');
    this.dispatchEvent(new Event('controllerchange'));
  }
}

/** A page that an active worker controls, as on every visit after the first. */
function controlledContainer(): FakeContainer {
  const container = new FakeContainer();
  const active = new FakeWorker('activated');
  container.registration.active = active;
  container.controller = active;
  return container;
}

const desktop: DeviceEnv = {
  ua: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  platform: 'MacIntel',
  maxTouchPoints: 0,
  matches: () => false,
};

// Two builds of the SPA. The page under test runs build A unless a test says otherwise.
const BUILD = '0.3.0';
const A = { shell: 'aaaaaaaaaaaa', entry: `${location.origin}/assets/index-AAAA.js` };
const B = { shell: 'bbbbbbbbbbbb', entry: `${location.origin}/assets/index-BBBB.js` };

/** /version.json as the server has it now; null answers 404. */
let versionFile: unknown = null;
const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>();
/** The shell caches the workers filled: cache name → URLs. */
const shellCaches = new Map<string, string[]>();
const cacheMatch = vi.fn((request: RequestInfo | URL, options?: MultiCacheQueryOptions) => {
  const urls = shellCaches.get(options?.cacheName ?? '') ?? [];
  return Promise.resolve(urls.includes(request as string) ? new Response('cached') : undefined);
});

/** The server now has build B. */
function deployB(version = BUILD): void {
  versionFile = { version, protocol: 1, shell: B.shell };
}

/**
 * The browser fetched build B's worker: it installs (precaching B's shell) and waits. With `entry` set to A's, B is a
 * rebuild that left the JavaScript as it was (only CSS, index.html or a public shell file changed).
 */
function workerBWaits(container: FakeContainer, entry = B.entry): FakeWorker {
  deployB();
  container.registration.startInstall();
  shellCaches.set(`isshoni-shell-${B.shell}`, [`${location.origin}/`, entry]);
  return container.registration.finishInstall();
}

const made: BrowserPwa[] = [];
function makePwa(opts: BrowserPwaOptions & { container?: FakeContainer | null } = {}) {
  const reload = vi.fn();
  const { container, ...rest } = opts;
  const pwa = new BrowserPwa({
    serviceWorker: container === undefined ? null : (container as unknown as ServiceWorkerContainer | null),
    caches: { match: cacheMatch },
    entryUrl: () => A.entry,
    env: desktop,
    reload,
    buildVersion: BUILD,
    ...rest,
  });
  made.push(pwa);
  const ready = vi.fn();
  pwa.onUpdateReady(ready);
  return { pwa, reload, ready };
}

function setVisibility(state: DocumentVisibilityState): void {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state });
  document.dispatchEvent(new Event('visibilitychange'));
}

/** Lets promise chains (fetch → json → cache lookup) run to their end under fake timers. */
const flush = () => vi.advanceTimersByTimeAsync(0);

beforeEach(() => {
  vi.useFakeTimers();
  versionFile = { version: BUILD, protocol: 1, shell: A.shell };
  shellCaches.clear();
  shellCaches.set(`isshoni-shell-${A.shell}`, [`${location.origin}/`, A.entry]);
  cacheMatch.mockClear();
  fetchMock.mockReset();
  fetchMock.mockImplementation(() =>
    Promise.resolve(
      versionFile === null ? new Response('not found', { status: 404 }) : Response.json(versionFile, { status: 200 }),
    ),
  );
  vi.stubGlobal('fetch', fetchMock);
  setVisibility('visible');
});

afterEach(() => {
  for (const pwa of made.splice(0)) pwa.dispose();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  Reflect.deleteProperty(document, 'visibilityState');
});

// ---- registration ----

describe('registering the service worker (05 §16.2)', () => {
  it('registers /sw.js at the root scope, never from the HTTP cache, and only once', async () => {
    const container = new FakeContainer();
    const { pwa } = makePwa({ container });
    await pwa.register();
    await pwa.register();
    expect(SW_URL).toBe('/sw.js');
    expect(container.registerCalls).toEqual([{ url: '/sw.js', options: { scope: '/', updateViaCache: 'none' } }]);
  });

  it('does nothing where there are no service workers, and still resolves', async () => {
    const { pwa } = makePwa({ container: null });
    await expect(pwa.register()).resolves.toBeUndefined();
  });

  it('rejects when the browser refuses the registration, and still watches /version.json', async () => {
    const container = new FakeContainer();
    container.registerError = new DOMException('The operation is insecure.', 'SecurityError');
    const { pwa } = makePwa({ container });
    await expect(pwa.register()).rejects.toThrow('The operation is insecure.');
    await flush();
    expect(fetchMock).toHaveBeenCalledOnce();
  });
});

// ---- update ready ----

describe('update ready: a new worker waits behind the active one, and the page is older', () => {
  it.each<[string, BrowserPwaOptions]>([
    ['', {}],
    [', also where the page cannot tell which build it runs', { caches: null }],
  ])('is not ready on the first install: the first worker has nothing to replace%s', async (_name, opts) => {
    const container = new FakeContainer();
    const { pwa, reload, ready } = makePwa({ container, ...opts });
    await pwa.register();
    const first = container.registration.startInstall();
    container.registration.finishInstall();
    await flush();
    container.takeControl(first); // activate → clients.claim()
    await flush();
    expect(ready).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    expect(first.messages).toEqual([]);
  });

  it('is ready when a worker of a newer build already waits at registration', async () => {
    const container = controlledContainer();
    const waiting = workerBWaits(container);
    const { pwa, ready } = makePwa({ container });
    await pwa.register();
    await flush();
    expect(ready).toHaveBeenCalledOnce();
    expect(waiting.messages).toEqual([]); // it waits for the user's Reload
  });

  it('is ready when a new worker finishes installing, also one that was installing at registration', async () => {
    const found = controlledContainer();
    const a = makePwa({ container: found });
    await a.pwa.register();
    await flush();
    deployB();
    found.registration.startInstall();
    await flush();
    expect(a.ready).not.toHaveBeenCalled(); // the new shell is still downloading
    workerBWaits(found);
    await flush();
    expect(a.ready).toHaveBeenCalledOnce();

    const midInstall = controlledContainer();
    midInstall.registration.installing = new FakeWorker('installing');
    const b = makePwa({ container: midInstall });
    await b.pwa.register();
    midInstall.registration.finishInstall();
    await flush();
    expect(b.ready).toHaveBeenCalledOnce();
  });

  it('is ready on a page no worker controls (a hard reload) when one waits behind an active worker', async () => {
    const container = controlledContainer();
    container.controller = null;
    const { pwa, ready } = makePwa({ container });
    await pwa.register();
    workerBWaits(container);
    await flush();
    expect(ready).toHaveBeenCalledOnce();
  });

  it('tells every listener once, a late listener too, and none after it unsubscribed', async () => {
    const container = controlledContainer();
    const { pwa, ready } = makePwa({ container });
    const gone = vi.fn();
    pwa.onUpdateReady(gone)();
    await pwa.register();
    workerBWaits(container);
    await flush();
    // A second new worker changes nothing: the update is ready already.
    workerBWaits(container);
    await flush();
    expect(ready).toHaveBeenCalledOnce();
    expect(gone).not.toHaveBeenCalled();

    const late = vi.fn();
    const lateGone = vi.fn();
    pwa.onUpdateReady(late);
    pwa.onUpdateReady(lateGone)();
    expect(late).not.toHaveBeenCalled(); // in a microtask, not inside onUpdateReady
    await flush();
    expect(late).toHaveBeenCalledOnce();
    expect(lateGone).not.toHaveBeenCalled();
  });

  it('is ready in an old tab whose worker another tab replaced, without reloading it', async () => {
    const container = controlledContainer();
    const { pwa, reload, ready } = makePwa({ container });
    await pwa.register();
    const waiting = workerBWaits(container);
    container.takeControl(waiting); // another tab's Reload
    await flush();
    expect(ready).toHaveBeenCalledOnce();
    expect(reload).not.toHaveBeenCalled(); // this tab didn't ask: no reload under the user's hands
  });

  it.each<[string, BrowserPwaOptions, boolean]>([
    ['there is no Cache API', { caches: null }, false],
    ['the page has no entry script', { entryUrl: () => null }, false],
    ['/version.json cannot be read', {}, true],
    [
      'the cache lookup fails',
      { caches: { match: () => Promise.reject(new DOMException('no', 'SecurityError')) } },
      false,
    ],
  ])('shows the pill when it cannot tell which build the page runs: %s', async (_name, opts, offline) => {
    const container = controlledContainer();
    // The page runs build B here: with the lookup working, the worker would be let in silently.
    const { pwa, ready } = makePwa({ container, entryUrl: () => B.entry, ...opts });
    await pwa.register();
    await flush();
    if (offline) fetchMock.mockImplementation(() => Promise.reject(new TypeError('Failed to fetch')));
    const waiting = workerBWaits(container);
    await flush();
    expect(ready).toHaveBeenCalledOnce();
    expect(waiting.messages).toEqual([]);
  });
});

describe('a page that already runs the new build (reloaded after the server was updated)', () => {
  it('lets the waiting worker take over at once: no pill, no reload', async () => {
    const container = controlledContainer(); // still the old worker
    const { pwa, reload, ready } = makePwa({ container, entryUrl: () => B.entry });
    await pwa.register();
    await flush();
    const waiting = workerBWaits(container);
    await flush();
    expect(waiting.messages).toEqual([{ type: 'SKIP_WAITING' }]);
    expect(cacheMatch).toHaveBeenLastCalledWith(B.entry, { cacheName: `isshoni-shell-${B.shell}`, ignoreVary: true });

    container.takeControl(waiting); // it activates and claims this page
    await flush();
    expect(ready).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
  });

  it('does not judge the page again when the worker it let in takes over', async () => {
    const container = controlledContainer();
    const { pwa, reload, ready } = makePwa({ container, entryUrl: () => B.entry });
    await pwa.register();
    await flush();
    const waiting = workerBWaits(container);
    await flush();
    expect(waiting.messages).toEqual([{ type: 'SKIP_WAITING' }]);

    // The server was redeployed a moment ago: a read of /version.json may well fail right now.
    const reads = fetchMock.mock.calls.length;
    fetchMock.mockImplementation(() => Promise.reject(new TypeError('Failed to fetch')));
    container.takeControl(waiting);
    await flush();
    expect(ready).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    expect(fetchMock).toHaveBeenCalledTimes(reads);
  });

  it('stays quiet when another tab lets the worker in', async () => {
    const container = controlledContainer();
    const { pwa, reload, ready } = makePwa({ container, entryUrl: () => B.entry });
    await pwa.register();
    await flush();
    const waiting = workerBWaits(container);
    container.takeControl(waiting); // before this tab had judged it
    await flush();
    expect(ready).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    expect(waiting.messages).toEqual([]);
  });

  it('judges a waiting worker once', async () => {
    const container = controlledContainer();
    container.registration.onUpdate = () => {
      workerBWaits(container);
    };
    const { pwa } = makePwa({ container, entryUrl: () => B.entry, buildVersion: '0.2.0' });
    await pwa.register(); // the first read sees another version → update() → the worker waits → judged twice over
    await flush();
    expect(container.registration.updateCalls).toBe(1);
    expect(container.registration.waiting?.messages).toEqual([{ type: 'SKIP_WAITING' }]);
  });
});

// ---- applying ----

describe('applyUpdate', () => {
  it('tells the waiting worker to take over and reloads once when it controls the page', async () => {
    const container = controlledContainer();
    const { pwa, reload } = makePwa({ container });
    await pwa.register();
    const waiting = workerBWaits(container);
    await flush();

    pwa.applyUpdate();
    pwa.applyUpdate(); // a second click
    expect(waiting.messages).toEqual([{ type: 'SKIP_WAITING' }]);
    expect(reload).not.toHaveBeenCalled();

    container.takeControl(waiting);
    expect(reload).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(APPLY_TIMEOUT_MS);
    container.dispatchEvent(new Event('controllerchange'));
    expect(reload).toHaveBeenCalledOnce();
  });

  it('reloads anyway when the worker has not taken over after 3 s', async () => {
    const container = controlledContainer();
    const { pwa, reload } = makePwa({ container });
    await pwa.register();
    workerBWaits(container);
    await flush();
    pwa.applyUpdate();
    await vi.advanceTimersByTimeAsync(APPLY_TIMEOUT_MS - 1);
    expect(reload).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(reload).toHaveBeenCalledOnce();
  });

  it('reloads at once without a waiting worker: the server always serves the current SPA', async () => {
    const none = makePwa({ container: null });
    none.pwa.applyUpdate();
    expect(none.reload).toHaveBeenCalledOnce();

    const container = controlledContainer();
    const registered = makePwa({ container });
    await registered.pwa.register();
    registered.pwa.applyUpdate();
    expect(registered.reload).toHaveBeenCalledOnce();
  });
});

// ---- /version.json ----

describe('/version.json: a tab that stays open learns of a new build', () => {
  it('reads it at registration and every 15 minutes, without the HTTP cache', async () => {
    const { pwa } = makePwa({ container: controlledContainer() });
    await pwa.register();
    await flush();
    expect(fetchMock).toHaveBeenCalledOnce();
    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(url instanceof URL ? url.href : url).toBe(`${location.origin}/version.json`);
    expect(init).toEqual({ cache: 'no-store' });
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('asks the registration to update when the shell changes; ready once the new worker waits', async () => {
    const container = controlledContainer();
    const { pwa, ready } = makePwa({ container });
    await pwa.register();
    await flush();
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS); // unchanged
    expect(container.registration.updateCalls).toBe(0);

    deployB();
    container.registration.onUpdate = () => {
      container.registration.startInstall();
    };
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(container.registration.updateCalls).toBe(1);
    expect(ready).not.toHaveBeenCalled(); // the new shell is still downloading
    workerBWaits(container);
    await flush();
    expect(ready).toHaveBeenCalledOnce();

    // Ready: no more polls.
    const reads = fetchMock.mock.calls.length;
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS * 2);
    expect(fetchMock).toHaveBeenCalledTimes(reads);
  });

  it('is ready when update() leaves a worker waiting by the time it resolves', async () => {
    const container = controlledContainer();
    const { pwa, ready } = makePwa({ container });
    await pwa.register();
    await flush();
    deployB('0.3.1');
    container.registration.onUpdate = () => {
      container.registration.waiting = new FakeWorker('installed');
    };
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(ready).toHaveBeenCalledOnce();
  });

  it('sees at its first read that the page is older than the server (a page from a stale cache)', async () => {
    const container = controlledContainer();
    container.registration.onUpdate = () => {
      container.registration.startInstall();
    };
    deployB('0.4.0');
    const { pwa } = makePwa({ container });
    await pwa.register();
    await flush();
    expect(container.registration.updateCalls).toBe(1);
  });

  it('tries the update again at the next read when it failed', async () => {
    const container = controlledContainer();
    const { pwa, ready } = makePwa({ container });
    await pwa.register();
    await flush();
    deployB();
    container.registration.onUpdate = () => Promise.reject(new TypeError('Failed to fetch /sw.js'));
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(container.registration.updateCalls).toBe(1);
    expect(ready).not.toHaveBeenCalled();

    container.registration.onUpdate = () => {
      workerBWaits(container);
    };
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS); // the same shell as the last read: still a new build
    expect(container.registration.updateCalls).toBe(2);
    expect(ready).toHaveBeenCalledOnce();
  });

  it('stops asking once the worker it fetched was let in (a rebuild that left the page’s JavaScript as it was)', async () => {
    const container = controlledContainer();
    const { pwa, reload, ready } = makePwa({ container }); // the page runs A's entry chunk
    await pwa.register();
    await flush();
    deployB();
    container.registration.onUpdate = () => {
      workerBWaits(container, A.entry);
    };
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(container.registration.updateCalls).toBe(1);
    const waiting = container.registration.waiting;
    if (!waiting) throw new Error('no worker waits');
    expect(waiting.messages).toEqual([{ type: 'SKIP_WAITING' }]);
    container.takeControl(waiting);

    container.registration.onUpdate = () => undefined;
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS * 3); // the same version.json three more times
    expect(container.registration.updateCalls).toBe(1);
    expect(ready).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
  });

  it('asks for nothing when the new shell’s worker was let in before a read saw the new shell', async () => {
    const container = controlledContainer();
    const { pwa, ready } = makePwa({ container });
    await pwa.register();
    await flush();
    // The browser found the worker itself (a page load in another tab), not this tab's read of /version.json.
    const waiting = workerBWaits(container, A.entry);
    await flush();
    expect(waiting.messages).toEqual([{ type: 'SKIP_WAITING' }]);
    container.takeControl(waiting);

    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS * 3); // the first of these reads sees the new shell
    expect(container.registration.updateCalls).toBe(0);
    expect(ready).not.toHaveBeenCalled();
  });

  it('without a service worker, a changed version.json is the update, and applying it reloads', async () => {
    for (const container of [null, Object.assign(new FakeContainer(), { registerError: new Error('refused') })]) {
      versionFile = { version: BUILD, protocol: 1, shell: A.shell };
      const { pwa, reload, ready } = makePwa({ container });
      await pwa.register().catch(() => undefined);
      await flush();
      await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
      expect(ready).not.toHaveBeenCalled();

      deployB();
      await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
      expect(ready).toHaveBeenCalledOnce();
      pwa.applyUpdate();
      expect(reload).toHaveBeenCalledOnce();
      pwa.dispose();
    }
  });

  it.each([
    ['a 404 (no build, a dev server)', null],
    ['HTML instead of JSON', '<!doctype html>'],
    ['JSON of another shape', { version: BUILD }],
    ['an empty shell', { version: BUILD, shell: '' }],
    ['null', 'null'],
  ])('ignores %s', async (_name, body) => {
    const { pwa, ready } = makePwa({ container: null });
    await pwa.register();
    await flush();
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        body === null
          ? new Response('not found', { status: 404 })
          : new Response(typeof body === 'string' ? body : JSON.stringify(body), { status: 200 }),
      ),
    );
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(ready).not.toHaveBeenCalled();
  });

  it('ignores a failed read and compares the next one with the last good one', async () => {
    const { pwa, ready } = makePwa({ container: null });
    await pwa.register();
    await flush();
    fetchMock.mockImplementationOnce(() => Promise.reject(new TypeError('Failed to fetch')));
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(ready).not.toHaveBeenCalled();
    deployB();
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
    expect(ready).toHaveBeenCalledOnce();
  });

  it('does not read while the tab is hidden, and reads when it comes back after a minute or more', async () => {
    const { pwa } = makePwa({ container: null });
    await pwa.register();
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    setVisibility('hidden');
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS * 3);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    setVisibility('visible');
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // Flipping back and forth right away doesn't read again.
    setVisibility('hidden');
    setVisibility('visible');
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MIN_GAP_MS - 1);
    setVisibility('hidden');
    setVisibility('visible');
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1);
    setVisibility('hidden');
    setVisibility('visible');
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('stops reading once disposed', async () => {
    const { pwa } = makePwa({ container: null });
    await pwa.register();
    await flush();
    pwa.dispose();
    await vi.advanceTimersByTimeAsync(VERSION_POLL_MS * 2);
    setVisibility('visible');
    await flush();
    expect(fetchMock).toHaveBeenCalledOnce();
  });
});

// ---- install ----

describe('install state and prompt (05 §16.3)', () => {
  function installPrompt(outcome: 'accepted' | 'dismissed') {
    const event = new Event('beforeinstallprompt', { cancelable: true }) as BeforeInstallPromptEvent;
    const prompt = vi.fn(() => Promise.resolve());
    Object.assign(event, { platforms: ['web'], prompt, userChoice: Promise.resolve({ outcome, platform: 'web' }) });
    return { event, prompt };
  }

  it('is none on a desktop browser that has not offered to install', async () => {
    const { pwa } = makePwa();
    expect(pwa.installState()).toBe('none');
    await expect(pwa.promptInstall()).resolves.toBe('unavailable');
  });

  it('keeps the browser’s prompt for later and shows it once', async () => {
    const { pwa } = makePwa();
    const { event, prompt } = installPrompt('accepted');
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true); // no mini-infobar: the app offers "Install app" itself
    expect(pwa.installState()).toBe('prompt');
    expect(prompt).not.toHaveBeenCalled();

    await expect(pwa.promptInstall()).resolves.toBe('accepted');
    expect(prompt).toHaveBeenCalledOnce();
    expect(pwa.installState()).toBe('none');
    await expect(pwa.promptInstall()).resolves.toBe('unavailable');
  });

  it('reports a dismissed prompt, and a prompt that fails as unavailable', async () => {
    const { pwa } = makePwa();
    window.dispatchEvent(installPrompt('dismissed').event);
    await expect(pwa.promptInstall()).resolves.toBe('dismissed');

    const broken = installPrompt('accepted');
    broken.prompt.mockRejectedValueOnce(new DOMException('no user gesture', 'NotAllowedError'));
    window.dispatchEvent(broken.event);
    await expect(pwa.promptInstall()).resolves.toBe('unavailable');
  });

  it('is installed after appinstalled, and inside the installed app', () => {
    const tab = makePwa();
    window.dispatchEvent(installPrompt('accepted').event);
    window.dispatchEvent(new Event('appinstalled'));
    expect(tab.pwa.installState()).toBe('installed');

    const standalone = makePwa({ env: { ...desktop, matches: (q) => q === '(display-mode: standalone)' } });
    expect(standalone.pwa.installState()).toBe('installed');
  });

  it('is ios-manual on an iPhone and an iPad in a browser tab: Share → Add to Home Screen', () => {
    const iphone = makePwa({
      env: {
        ...desktop,
        ua: 'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1',
        platform: 'iPhone',
        maxTouchPoints: 5,
      },
    });
    expect(iphone.pwa.installState()).toBe('ios-manual');
    const ipad = makePwa({ env: { ...desktop, maxTouchPoints: 5 } });
    expect(ipad.pwa.installState()).toBe('ios-manual');
  });

  it('stops listening once disposed', () => {
    const { pwa } = makePwa();
    pwa.dispose();
    window.dispatchEvent(installPrompt('accepted').event);
    expect(pwa.installState()).toBe('none');
  });
});

describe('createPwaProvider', () => {
  it('gives every platform the page’s one provider', () => {
    expect(createPwaProvider()).toBe(createPwaProvider());
  });
});
