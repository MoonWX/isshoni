import { afterEach, describe, expect, it, vi } from 'vitest';

import { createMemoryStorage, createWebStorage } from './storage';

/** A Web Storage whose methods throw like a blocked or full one. */
function brokenStorage(broken: { get?: boolean; set?: boolean; remove?: boolean }): Storage {
  const data = new Map<string, string>();
  const fail = (): never => {
    throw new DOMException('blocked', 'SecurityError');
  };
  return {
    get length() {
      return data.size;
    },
    clear: () => {
      data.clear();
    },
    key: (i: number) => [...data.keys()][i] ?? null,
    getItem: (k: string) => (broken.get ? fail() : (data.get(k) ?? null)),
    setItem: (k: string, v: string) => {
      if (broken.set) fail();
      data.set(k, v);
    },
    removeItem: (k: string) => {
      if (broken.remove) fail();
      data.delete(k);
    },
  };
}

describe('createWebStorage (05 §6.1)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it('reads and writes the real storage', () => {
    const s = createWebStorage('local');
    expect(s.get('isshoni.lastRoomId')).toBeNull();
    s.set('isshoni.lastRoomId', 'lounge');
    expect(localStorage.getItem('isshoni.lastRoomId')).toBe('lounge');
    expect(s.get('isshoni.lastRoomId')).toBe('lounge');
    s.remove('isshoni.lastRoomId');
    expect(localStorage.getItem('isshoni.lastRoomId')).toBeNull();
  });

  it('falls back to memory when storage is unavailable (private windows, blocked site data)', () => {
    vi.stubGlobal('localStorage', undefined);
    const s = createWebStorage('local');
    s.set('k', 'v');
    expect(s.get('k')).toBe('v');
    s.remove('k');
    expect(s.get('k')).toBeNull();
  });

  it('falls back to memory when storage throws on access', () => {
    const s = createWebStorage('local', brokenStorage({ get: true, set: true, remove: true }));
    expect(s.get('k')).toBeNull();
    s.set('k', 'v');
    expect(s.get('k')).toBe('v');
    expect(() => {
      s.remove('k');
    }).not.toThrow();
    expect(s.get('k')).toBeNull();
  });

  it('keeps a refused write (quota) in memory, over the older stored value, until a write succeeds', () => {
    const flags = { set: false };
    const storage = brokenStorage(flags);
    storage.setItem('k', 'old');
    const s = createWebStorage('local', storage);
    flags.set = true; // full
    s.set('k', 'new');
    expect(storage.getItem('k')).toBe('old');
    expect(s.get('k')).toBe('new');
    flags.set = false;
    s.set('k', 'newer');
    expect(storage.getItem('k')).toBe('newer');
    expect(s.get('k')).toBe('newer');
    // Memory no longer shadows the store: a change made elsewhere (another tab) is seen.
    storage.setItem('k', 'from another tab');
    expect(s.get('k')).toBe('from another tab');
  });

  it('has an in-memory twin for tests', () => {
    const s = createMemoryStorage({ a: '1' });
    expect(s.get('a')).toBe('1');
    s.set('b', '2');
    s.remove('a');
    expect([s.get('a'), s.get('b')]).toEqual([null, '2']);
  });
});
