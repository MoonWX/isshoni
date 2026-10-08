// The service worker's event handlers (05 §16.2, §16.3). Its only jobs are caching the app shell for an offline
// start and handling push. sw.ts calls startWorker() with the real global scope and the build-time constants; the
// tests call it with a fake scope.
//
// | Event | What happens |
// |---|---|
// | install | precache the shell into isshoni-shell-<version>. No automatic skipWaiting: a new worker waits |
// | activate | delete the shell caches of other versions; clients.claim() |
// | fetch | routes.ts decides; shell.ts answers navigations, hashed assets, icons and the manifest |
// | message {type: 'SKIP_WAITING'} | skipWaiting(): a page asked (the UpdatePill's Reload, or a page of this build) |
// | push | show a notification, always (push.ts) |
// | notificationclick | close it; focus an open window and tell it to navigate, else open one |
// | pushsubscriptionchange | subscribe again with the same key and post the new subscription |
import { OPEN_URL, SKIP_WAITING, type OpenUrlMessage } from './contract';
import { clickUrl, genericNotification, notificationFor, type PushNotification } from './push';
import { dropOldCaches, handleFetch, precache, type ShellConfig } from './shell';

/** The build-time constants of src/types/sw-globals.d.ts, as sw.ts passes them. */
export interface WorkerConfig {
  /** `__SHELL__` */
  readonly shell: readonly string[];
  /** `__SHELL_VERSION__` */
  readonly shellVersion: string;
  /** `__PUSH_STRINGS__` */
  readonly pushStrings: PushStrings;
}

/** 03's endpoint that stores a push subscription (POST, the body is PushSubscription.toJSON()). */
export const PUSH_SUBSCRIPTIONS_PATH = '/api/v1/push/subscriptions';

const noop = (): void => undefined;

function messageType(data: unknown): unknown {
  return typeof data === 'object' && data !== null ? (data as { type?: unknown }).type : undefined;
}

/** The notification for a push message. Never throws: an unreadable body still gets the generic notification. */
function render(data: PushMessageData | null, strings: PushStrings): PushNotification {
  let payload: unknown = null;
  try {
    payload = data ? data.json() : null;
  } catch {
    // Not JSON: the generic notification below.
  }
  try {
    return notificationFor(payload, strings);
  } catch {
    return genericNotification(payload, strings);
  }
}

/** Brings the app to the front at url: an open window is focused and told to navigate, else a new one opens. */
async function openApp(clients: Clients, url: string): Promise<void> {
  const windows = await clients.matchAll({ type: 'window', includeUncontrolled: true });
  const target = windows.find((w) => w.visibilityState === 'visible') ?? windows[0];
  if (target) {
    try {
      await target.focus();
      target.postMessage({ type: OPEN_URL, url } satisfies OpenUrlMessage);
      return;
    } catch {
      // The browser refused to focus it: open a window instead.
    }
  }
  await clients.openWindow(url);
}

/**
 * The browser replaced or dropped the push subscription: subscribe again with the same VAPID key and give the server
 * the new one. The answer is not read: after a 401 (logged out), and after any failure here, the page posts its
 * subscription again at its next start (05 §16.3). Content-Type is what 03 §7.5's CSRF rule asks of every unsafe
 * request.
 */
async function resubscribe(scope: ServiceWorkerGlobalScope, event: PushSubscriptionChangeEvent): Promise<void> {
  let subscription = event.newSubscription;
  if (!subscription) {
    const key = event.oldSubscription?.options.applicationServerKey;
    if (!key) return;
    subscription = await scope.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
  }
  await scope.fetch(PUSH_SUBSCRIPTIONS_PATH, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(subscription.toJSON()),
  });
}

/** Registers every handler on scope. Call it once, synchronously, while the worker script first runs. */
export function startWorker(scope: ServiceWorkerGlobalScope, config: WorkerConfig): void {
  const origin = scope.location.origin;
  const shell: ShellConfig = {
    version: config.shellVersion,
    urls: config.shell,
    origin,
    caches: scope.caches,
    fetch: (request) => scope.fetch(request),
  };

  scope.addEventListener('install', (event) => {
    event.waitUntil(precache(shell));
  });

  scope.addEventListener('activate', (event) => {
    event.waitUntil(dropOldCaches(shell).then(() => scope.clients.claim()));
  });

  scope.addEventListener('fetch', (event) => {
    const response = handleFetch(shell, event.request, (task) => {
      event.waitUntil(task);
    });
    if (response) event.respondWith(response);
  });

  scope.addEventListener('message', (event) => {
    if (messageType(event.data) === SKIP_WAITING) event.waitUntil(scope.skipWaiting());
  });

  scope.addEventListener('push', (event) => {
    const { title, options } = render(event.data, config.pushStrings);
    event.waitUntil(scope.registration.showNotification(title, options));
  });

  scope.addEventListener('notificationclick', (event) => {
    event.notification.close();
    const data = event.notification.data as { url?: unknown } | null | undefined;
    event.waitUntil(openApp(scope.clients, clickUrl(data?.url, origin)));
  });

  scope.addEventListener('pushsubscriptionchange', (event) => {
    event.waitUntil(resubscribe(scope, event).catch(noop));
  });
}
