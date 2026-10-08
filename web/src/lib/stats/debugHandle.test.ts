// window.__isshoni (05 §10.7, §20): there only with the tab's debug flag, read-only, and no more than stats(),
// state() and dropSocket().
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createMemoryStorage } from '../../platform/browser/storage';
import { NotImplementedError } from '../errors';
import { DEBUG_FLAG_KEY, debugEnabled, installDebugHandle, type DebugHandle } from './debugHandle';
import type { StatsSample } from './summarize';

const SAMPLE: StatsSample = { at: 1, intervalMs: 0, pcs: [], shares: {}, outbound: [] };

function storage(flag?: string) {
  const s = createMemoryStorage();
  if (flag !== undefined) s.set(DEBUG_FLAG_KEY, flag);
  return s;
}

const deps = (flag?: string) => ({
  storage: storage(flag),
  stats: vi.fn(() => Promise.resolve(SAMPLE)),
  state: vi.fn(() => ({ viewer: { focusedShareId: 's_a' } })),
});

let uninstall: (() => void) | undefined;

afterEach(() => {
  uninstall?.();
  uninstall = undefined;
  Reflect.deleteProperty(window, '__isshoni');
});

describe('installDebugHandle', () => {
  it('does nothing without the flag', () => {
    for (const flag of [undefined, '', '0', 'true']) {
      const d = deps(flag);
      expect(debugEnabled(d.storage)).toBe(false);
      uninstall = installDebugHandle(d);
      expect(window.__isshoni).toBeUndefined();
      expect('__isshoni' in window).toBe(false);
    }
  });

  it("puts stats(), state() and dropSocket() on window when sessionStorage['isshoni.debug'] is '1'", async () => {
    const d = deps('1');
    const dropSocket = vi.fn();
    expect(DEBUG_FLAG_KEY).toBe('isshoni.debug');
    expect(debugEnabled(d.storage)).toBe(true);
    uninstall = installDebugHandle({ ...d, dropSocket });

    const handle = window.__isshoni;
    expect(Object.keys(handle ?? {}).sort()).toEqual(['dropSocket', 'state', 'stats']);
    await expect(handle?.stats()).resolves.toBe(SAMPLE);
    expect(handle?.state()).toEqual({ viewer: { focusedShareId: 's_a' } });
    handle?.dropSocket();
    expect(dropSocket).toHaveBeenCalledTimes(1);
    // Asked each time: nothing is cached.
    await handle?.stats();
    expect(d.stats).toHaveBeenCalledTimes(2);
  });

  it('is read-only: the handle is frozen and the property can’t be assigned', () => {
    uninstall = installDebugHandle(deps('1'));
    const handle = window.__isshoni;
    expect(Object.isFrozen(handle)).toBe(true);
    expect(() => {
      'use strict';
      window.__isshoni = { stats: () => Promise.resolve(SAMPLE), state: () => null, dropSocket: () => undefined };
    }).toThrow(TypeError);
    expect(window.__isshoni).toBe(handle);
    expect(Object.keys(window)).not.toContain('__isshoni');
  });

  it('says so when no way to drop the socket was given', () => {
    uninstall = installDebugHandle(deps('1'));
    expect(() => window.__isshoni?.dropSocket()).toThrow(NotImplementedError);
  });

  it('goes where it is told, and the returned function removes it', () => {
    const target: { __isshoni?: DebugHandle } = {};
    const remove = installDebugHandle({ ...deps('1'), target });
    expect(target.__isshoni).toBeDefined();
    expect(window.__isshoni).toBeUndefined();
    remove();
    expect('__isshoni' in target).toBe(false);
    remove(); // harmless twice
  });
});
