// Fakes for the service-worker tests: CacheStorage and a ServiceWorkerGlobalScope that records what the worker does.
// Only what src/sw/ uses is implemented. Nothing here is bundled into sw.js.

export const ORIGIN = 'https://isshoni.test';

function keyOf(request: RequestInfo | URL): string {
  if (typeof request === 'string') return new URL(request, ORIGIN).href;
  return request instanceof URL ? request.href : request.url;
}

export class FakeCache {
  readonly entries = new Map<string, Response>();
  /** The requests of every addAll call, in order. */
  readonly added: Request[] = [];
  /** Makes addAll and put reject, as a missing shell file or a full disk would. */
  failWrites: Error | null = null;

  addAll(requests: Iterable<RequestInfo>): Promise<void> {
    if (this.failWrites) return Promise.reject(this.failWrites);
    for (const request of requests) {
      if (typeof request === 'string') throw new Error('FakeCache.addAll expects Request objects');
      this.added.push(request);
      this.entries.set(request.url, new Response(`precached ${new URL(request.url).pathname}`));
    }
    return Promise.resolve();
  }

  match(request: RequestInfo | URL): Promise<Response | undefined> {
    return Promise.resolve(this.entries.get(keyOf(request))?.clone());
  }

  put(request: RequestInfo | URL, response: Response): Promise<void> {
    if (this.failWrites) return Promise.reject(this.failWrites);
    this.entries.set(keyOf(request), response);
    return Promise.resolve();
  }
}

export class FakeCacheStorage {
  readonly caches = new Map<string, FakeCache>();

  open(name: string): Promise<Cache> {
    let cache = this.caches.get(name);
    if (!cache) {
      cache = new FakeCache();
      this.caches.set(name, cache);
    }
    return Promise.resolve(cache as unknown as Cache);
  }

  keys(): Promise<string[]> {
    return Promise.resolve([...this.caches.keys()]);
  }

  delete(name: string): Promise<boolean> {
    return Promise.resolve(this.caches.delete(name));
  }

  /** The cache with this name; it must exist. */
  get(name: string): FakeCache {
    const cache = this.caches.get(name);
    if (!cache) throw new Error(`no cache named ${name}; there are: ${[...this.caches.keys()].join(', ')}`);
    return cache;
  }

  asCacheStorage(): CacheStorage {
    return this as unknown as CacheStorage;
  }
}

/**
 * A request as the fetch event delivers it. A plain object, because the Request constructor refuses mode
 * `navigate`; the worker only reads url, mode and method and hands the request on.
 */
export function fakeRequest(path: string, init: { mode?: string; method?: string } = {}): Request {
  return {
    url: new URL(path, ORIGIN).href,
    mode: init.mode ?? 'no-cors',
    method: init.method ?? 'GET',
  } as unknown as Request;
}

/** Collects the promises an event handler passed to waitUntil. */
export class FakeExtendableEvent {
  readonly tasks: Promise<unknown>[] = [];

  waitUntil(task: Promise<unknown>): void {
    this.tasks.push(task);
  }

  /** Resolves when every task has settled; rejects with the first failure. */
  async settled(): Promise<void> {
    // A task may add another one while it runs.
    for (let i = 0; i < this.tasks.length; i++) await this.tasks[i];
  }
}

export class FakeFetchEvent extends FakeExtendableEvent {
  response: Promise<Response> | null = null;

  constructor(readonly request: Request) {
    super();
  }

  respondWith(response: Promise<Response>): void {
    this.response = response;
  }
}

export interface FakeWindowClient {
  visibilityState: DocumentVisibilityState;
  focus: () => Promise<unknown>;
  postMessage: (message: unknown) => void;
}

type Handler = (event: never) => void;

/** The parts of ServiceWorkerGlobalScope that worker.ts touches. */
export class FakeScope {
  readonly location = { origin: ORIGIN };
  readonly cacheStorage = new FakeCacheStorage();
  readonly caches = this.cacheStorage.asCacheStorage();
  readonly handlers = new Map<string, Handler>();

  /** fetch() calls, in order; the answer comes from `network`. */
  readonly fetched: { input: RequestInfo | URL; init: RequestInit | undefined }[] = [];
  network: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response> = () =>
    Promise.resolve(new Response('from the network'));

  skipWaitingCalls = 0;
  claimCalls = 0;
  windows: FakeWindowClient[] = [];
  readonly opened: string[] = [];
  readonly notifications: { title: string; options: NotificationOptions | undefined }[] = [];
  readonly subscribeCalls: PushSubscriptionOptionsInit[] = [];
  subscribeResult: PushSubscription | null = null;

  readonly clients = {
    claim: (): Promise<void> => {
      this.claimCalls++;
      return Promise.resolve();
    },
    matchAll: (): Promise<FakeWindowClient[]> => Promise.resolve([...this.windows]),
    openWindow: (url: string): Promise<null> => {
      this.opened.push(url);
      return Promise.resolve(null);
    },
  };

  readonly registration = {
    showNotification: (title: string, options?: NotificationOptions): Promise<void> => {
      this.notifications.push({ title, options });
      return Promise.resolve();
    },
    pushManager: {
      subscribe: (options: PushSubscriptionOptionsInit): Promise<PushSubscription> => {
        this.subscribeCalls.push(options);
        return this.subscribeResult
          ? Promise.resolve(this.subscribeResult)
          : Promise.reject(new Error('FakeScope: no subscribeResult set'));
      },
    },
  };

  addEventListener(type: string, handler: Handler): void {
    if (this.handlers.has(type)) throw new Error(`a second ${type} handler`);
    this.handlers.set(type, handler);
  }

  fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    this.fetched.push({ input, init });
    return this.network(input, init);
  }

  skipWaiting(): Promise<void> {
    this.skipWaitingCalls++;
    return Promise.resolve();
  }

  /** Calls the handler of an event type with the given event. */
  dispatch(type: string, event: unknown): void {
    const handler = this.handlers.get(type);
    if (!handler) throw new Error(`no ${type} handler registered`);
    handler(event as never);
  }

  asScope(): ServiceWorkerGlobalScope {
    return this as unknown as ServiceWorkerGlobalScope;
  }
}
