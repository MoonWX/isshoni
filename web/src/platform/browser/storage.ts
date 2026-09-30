// platform.storage (05 §6.1, §8): Web Storage behind try/catch, with an in-memory fallback. Private windows, blocked
// site data and full quotas make localStorage/sessionStorage throw (sometimes on access, sometimes on write); the
// page must keep working, so a failed write is kept in memory for the rest of the page's life and reads see it.
//
// The only file allowed to touch localStorage and sessionStorage (eslint no-restricted-globals). Keys are full names
// ("isshoni.lastRoomId", which 01 §10.5 reads).
import type { KeyValueStore } from '../types';

export type WebStorageKind = 'local' | 'session';

function webStorage(kind: WebStorageKind): Storage | null {
  try {
    const s = kind === 'local' ? globalThis.localStorage : globalThis.sessionStorage;
    // Some browsers only throw on first use.
    s.getItem('isshoni.probe');
    return s;
  } catch {
    return null;
  }
}

/**
 * A KeyValueStore over localStorage or sessionStorage. A value that storage refused lives in memory and wins over
 * the (older) stored one until a later write of that key succeeds.
 */
export function createWebStorage(kind: WebStorageKind, storage: Storage | null = webStorage(kind)): KeyValueStore {
  const memory = new Map<string, string>();
  return {
    get(k) {
      const kept = memory.get(k);
      if (kept !== undefined) return kept;
      if (!storage) return null;
      try {
        return storage.getItem(k);
      } catch {
        return null;
      }
    },
    set(k, v) {
      if (storage) {
        try {
          storage.setItem(k, v);
          memory.delete(k);
          return;
        } catch {
          // quota or policy: keep it for this page
        }
      }
      memory.set(k, v);
    },
    remove(k) {
      memory.delete(k);
      if (storage) {
        try {
          storage.removeItem(k);
        } catch {
          // nothing stored that we could remove
        }
      }
    },
  };
}

/** An in-memory KeyValueStore (tests, and platforms without Web Storage). */
export function createMemoryStorage(initial?: Record<string, string>): KeyValueStore {
  const memory = new Map<string, string>(Object.entries(initial ?? {}));
  return {
    get: (k) => memory.get(k) ?? null,
    set: (k, v) => {
      memory.set(k, v);
    },
    remove: (k) => {
      memory.delete(k);
    },
  };
}
