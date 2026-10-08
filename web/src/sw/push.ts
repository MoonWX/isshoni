// Web Push in the service worker: payload → notification, and where a click on it goes (05 §16.3). A pure mapping
// without service-worker globals; worker.ts wires it to the push and notificationclick events.
//
// 04 §14.3's payload carries data, not text: the worker renders it from `__PUSH_STRINGS__`, the `push` section of
// en.json. Every push must show a notification, or iOS may drop the subscription (04), so notificationFor never
// throws and never returns an empty title.
//
// S38 (the PWA shell) declares this interface and renders every payload as the generic notification. S77 (Web Push)
// adds the texts per type (`share.started`, `admin.alert` by kind, `push.test`) here and in en.json; the generic
// notification stays the answer for a type this worker doesn't know and for a payload it can't read.

/** The icon and the Android status-bar badge of every notification (public/icons/). */
export const NOTIFICATION_ICON = '/icons/icon-192.png';
export const NOTIFICATION_BADGE = '/icons/badge-72.png';

/** Where a click goes when the payload has no usable URL. */
export const DEFAULT_CLICK_URL = '/';

/** The title when the catalog has none: the app's name, which is the same in every language. */
const FALLBACK_TITLE = 'isshoni';

/** What notificationclick reads back from Notification.data. */
export interface PushNotificationData {
  /** The payload's `url` as it came; clickUrl() checks it at click time. */
  url: string;
}

/** NotificationOptions as the worker sets them. `timestamp` isn't in TypeScript's DOM library yet. */
export interface PushNotificationOptions extends NotificationOptions {
  body: string;
  icon: string;
  badge: string;
  /** When the event happened (the payload's `ts`, unix milliseconds). */
  timestamp?: number;
  data: PushNotificationData;
}

/** The arguments of ServiceWorkerRegistration.showNotification(). */
export interface PushNotification {
  title: string;
  options: PushNotificationOptions;
}

/** The catalog string at a dotted path below `push` ("generic.title"), or undefined when it isn't a string. */
export function pushString(strings: PushStrings, path: string): string | undefined {
  // The catalog is build-time data, but a notification must show whatever it holds: no lookup here may throw.
  let node: unknown = strings;
  for (const key of path.split('.')) {
    if (typeof node !== 'object' || node === null || !Object.hasOwn(node, key)) return undefined;
    node = (node as Record<string, unknown>)[key];
  }
  return typeof node === 'string' && node !== '' ? node : undefined;
}

function field(payload: unknown, name: string): unknown {
  return typeof payload === 'object' && payload !== null ? (payload as Record<string, unknown>)[name] : undefined;
}

/**
 * The generic notification: "isshoni" with a line that says to open the app. It keeps what any payload may carry:
 * `tag` (a newer notification with the same tag replaces the older one), `ts` and `url`.
 */
export function genericNotification(payload: unknown, strings: PushStrings): PushNotification {
  const tag = field(payload, 'tag');
  const ts = field(payload, 'ts');
  const url = field(payload, 'url');
  return {
    title: pushString(strings, 'generic.title') ?? FALLBACK_TITLE,
    options: {
      body: pushString(strings, 'generic.body') ?? '',
      icon: NOTIFICATION_ICON,
      badge: NOTIFICATION_BADGE,
      ...(typeof tag === 'string' && tag !== '' ? { tag } : {}),
      ...(typeof ts === 'number' && Number.isFinite(ts) && ts > 0 ? { timestamp: ts } : {}),
      data: { url: typeof url === 'string' && url !== '' ? url : DEFAULT_CLICK_URL },
    },
  };
}

/**
 * The notification for a push payload (the parsed JSON of 04 §14.3's `api.PushPayload`, or anything else when the
 * message had no readable body).
 *
 * Until S77 adds the texts per type, every payload gets the generic notification.
 */
export function notificationFor(payload: unknown, strings: PushStrings): PushNotification {
  return genericNotification(payload, strings);
}

/**
 * The URL a notification click opens: the payload's `url` when it resolves to the worker's own origin, as path,
 * query and fragment; else `/`. A push message is data from the network: it must never send the user to another
 * site (05 §20).
 */
export function clickUrl(url: unknown, origin: string): string {
  if (typeof url !== 'string' || url === '') return DEFAULT_CLICK_URL;
  let target: URL;
  try {
    target = new URL(url, origin);
  } catch {
    return DEFAULT_CLICK_URL;
  }
  if (target.origin !== origin) return DEFAULT_CLICK_URL;
  // "https://this.host//other.host/" is same-origin, but its path alone would resolve as "//other.host/": a
  // protocol-relative URL to another site. No SPA route starts with two slashes.
  if (target.pathname.startsWith('//')) return DEFAULT_CLICK_URL;
  return target.pathname + target.search + target.hash;
}
