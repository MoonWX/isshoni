// Web Push in the service worker: payload → notification, and where a click on it goes (05 §16.3). A pure mapping
// without service-worker globals; worker.ts wires it to the push and notificationclick events.
//
// 04 §14.3's payload carries data, not text: the worker renders it from `__PUSH_STRINGS__`, the `push` section of
// en.json. Every push must show a notification, or iOS may drop the subscription (04), so notificationFor never
// throws and never returns an empty title.
//
// | Payload `type` | Title and body (keys below `push`) | Values |
// |---|---|---|
// | `share.started` | `shareStarted.title`, `.body`: "Alex started sharing", "in Lounge · Tap to watch" | user, room |
// | `admin.alert` | `adminAlert.<kind>.title`, `.body`: 03's kinds, 04's `transfer_threshold` | `actor`, `target` |
// | `push.test` | `test.title`, `test.body` | none |
// | anything else | `generic.title`, `generic.body` | none |
//
// `user` and `room` are the payload's user.name and room.name.
//
// An admin alert's `actor` is a username, or `cli` or `system` (03 §5's actors that aren't users), which read as
// `actor.cli` and `actor.system`. A payload whose texts can't be filled in (a missing name, a kind without texts, a
// catalog without the key) gets the generic notification instead: never an empty line, never a raw `{{name}}`.
// Every notification keeps what any payload may carry: `tag` (a newer notification with the same tag replaces the
// older one), `ts` and `url`.
//
// The templates use i18next's `{{name}}` syntax, so the page and the worker share one catalog; the worker has no
// i18next and fills in plain values only (fill() below).
import {
  PushTypeAdminAlert,
  PushTypeShareStarted,
  PushTypeTest,
  type AdminAlertKind,
  type PushType,
} from '../protocol/api.gen';

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

/** The title and body of a notification. */
export interface PushTexts {
  title: string;
  body: string;
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

/** A non-empty string field of an object, else undefined. */
function textField(value: unknown, name: string): string | undefined {
  const v = field(value, name);
  return typeof v === 'string' && v.trim() !== '' ? v : undefined;
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

/** `{{ name }}`, as i18next writes an interpolation. The push texts use no formats and no nesting. */
const PLACEHOLDER = /\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;

/**
 * Fills a template's `{{name}}` placeholders with values (as they are: a notification is plain text, nothing is
 * escaped). undefined when a placeholder has no value, or the result is blank, so that a notification never shows a
 * raw placeholder or an empty name: the caller falls back to the generic notification then.
 */
export function fill(template: string, values: Readonly<Record<string, string | undefined>>): string | undefined {
  const value = (name: string): string | undefined => {
    const v = Object.hasOwn(values, name) ? values[name] : undefined;
    return v === '' ? undefined : v;
  };
  for (const [, name = ''] of template.matchAll(PLACEHOLDER)) {
    if (value(name) === undefined) return undefined;
  }
  const out = template.replace(PLACEHOLDER, (_match, name: string) => value(name) ?? '');
  return out.trim() === '' ? undefined : out;
}

/** The title and body at `<path>.title` and `<path>.body`, filled in; undefined when either can't be. */
function textsAt(
  strings: PushStrings,
  path: string,
  values: Readonly<Record<string, string | undefined>>,
): PushTexts | undefined {
  const titleTemplate = pushString(strings, `${path}.title`);
  const bodyTemplate = pushString(strings, `${path}.body`);
  if (titleTemplate === undefined || bodyTemplate === undefined) return undefined;
  const title = fill(titleTemplate, values);
  const body = fill(bodyTemplate, values);
  return title === undefined || body === undefined ? undefined : { title, body };
}

/** An admin alert's kind as a catalog key: the kinds of 03 §7.11 and 04 §11.3 are snake_case. */
const KIND = /^[a-z][a-z0-9_]*$/;

/** The actors of an admin alert that aren't users (03 §5), with a text each under `push.actor`. */
const SYSTEM_ACTORS: ReadonlySet<string> = new Set(['cli', 'system']);

/** Who did it, as the alert texts name them: a username as it is, `cli` and `system` by their texts. */
function actorName(actor: string | undefined, strings: PushStrings): string | undefined {
  if (actor !== undefined && SYSTEM_ACTORS.has(actor)) return pushString(strings, `actor.${actor}`) ?? actor;
  return actor;
}

/** The texts of a payload of a type this worker knows; undefined for anything else (the generic notification). */
export function pushTexts(payload: unknown, strings: PushStrings): PushTexts | undefined {
  const type = field(payload, 'type') as PushType | undefined;
  switch (type) {
    case PushTypeShareStarted:
      return textsAt(strings, 'shareStarted', {
        user: textField(field(payload, 'user'), 'name'),
        room: textField(field(payload, 'room'), 'name'),
      });
    case PushTypeAdminAlert: {
      const kind = textField(payload, 'kind') as AdminAlertKind | undefined;
      if (kind === undefined || !KIND.test(kind)) return undefined;
      return textsAt(strings, `adminAlert.${kind}`, {
        actor: actorName(textField(payload, 'actor'), strings),
        target: textField(payload, 'target'),
      });
    }
    case PushTypeTest:
      return textsAt(strings, 'test', {});
    default:
      return undefined;
  }
}

/**
 * The notification for a push payload (the parsed JSON of 04 §14.3's `api.PushPayload`, or anything else when the
 * message had no readable body): the texts of its type with the generic notification's tag, time, URL, icon and
 * badge; the generic notification when the type is unknown or its texts can't be filled in. Never throws.
 */
export function notificationFor(payload: unknown, strings: PushStrings): PushNotification {
  const generic = genericNotification(payload, strings);
  let texts: PushTexts | undefined;
  try {
    texts = pushTexts(payload, strings);
  } catch {
    texts = undefined; // a payload that throws on a read (a getter, a proxy): the generic notification
  }
  if (texts === undefined) return generic;
  return { title: texts.title, options: { ...generic.options, body: texts.body } };
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
