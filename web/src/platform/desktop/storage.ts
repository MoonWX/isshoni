// The desktop's Web Storage per server (docs/m2/03 §14.3): storage.local prefixes every key with
// `isshoni:<serverId>:` (the derived id of §8.1, from bridge.js), so isshoni.lastRoomId, the volume and the dismissed
// hints (05 §6.1) are per server; with no active server the prefix is `isshoni:_:`. At boot the keys of servers that
// servers.json no longer lists are removed. storage.session stays unprefixed (one tab).
//
// No token ever goes into storage (docs/m2/03 §5.8): tokens.ts keeps it in a closure.
import type { KeyValueStore } from '../types';

/** The id part of the prefix when no server is active. */
export const NoServerId = '_';

const PREFIXED_KEY = /^isshoni:([^:]+):/;

/** `isshoni:<serverId>:`, or `isshoni:_:` without an active server. */
export function serverStoragePrefix(serverId: string | null): string {
  return `isshoni:${serverId ?? NoServerId}:`;
}

/** A KeyValueStore that puts prefix before every key of inner. */
export function createPrefixedStorage(inner: KeyValueStore, prefix: string): KeyValueStore {
  return {
    get: (k) => inner.get(prefix + k),
    set: (k, v) => {
      inner.set(prefix + k, v);
    },
    remove: (k) => {
      inner.remove(prefix + k);
    },
  };
}

/**
 * The prefixed keys among keys that belong to no known server: their id is neither in serverIds, nor the active
 * server's, nor `_`. Keys without the prefix are not this code's and are kept.
 */
export function staleServerKeys(
  keys: readonly string[],
  serverIds: readonly string[],
  active: string | null,
): string[] {
  const known = new Set([...serverIds, active ?? NoServerId, NoServerId]);
  return keys.filter((k) => {
    const id = PREFIXED_KEY.exec(k)?.[1];
    return id !== undefined && !known.has(id);
  });
}
