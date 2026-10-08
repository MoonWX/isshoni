// What a page and the service worker agree on (05 §16.2, §16.3): the messages between them and the name of the shell
// cache. The worker (sw/worker.ts, sw/shell.ts) and the page (platform/browser/) both import this file, so it uses
// nothing from the DOM or the worker libraries.

/** Page → waiting worker: activate now. The page that asked reloads on controllerchange. */
export const SKIP_WAITING = 'SKIP_WAITING';

export interface SkipWaitingMessage {
  readonly type: typeof SKIP_WAITING;
}

/** Worker → page, after a click on a notification: navigate to url (same-origin path, query and fragment). */
export const OPEN_URL = 'open';

export interface OpenUrlMessage {
  readonly type: typeof OPEN_URL;
  readonly url: string;
}

/** Every shell cache is named CACHE_PREFIX + its shell version; a worker deletes the others when it activates. */
export const CACHE_PREFIX = 'isshoni-shell-';

/**
 * The cache a worker fills at install with its shell: `/`, the entry chunk with its static imports and CSS, and the
 * public shell files. version is the worker's `__SHELL_VERSION__`, the same value as `shell` in /version.json.
 */
export function shellCacheName(version: string): string {
  return CACHE_PREFIX + version;
}
