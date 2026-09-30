import { afterEach, describe, expect, it, vi } from 'vitest';

import { createEmitter } from './emitter';

type Events = { state: string; count: number };

describe('createEmitter', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('calls listeners in order and unsubscribes', () => {
    const e = createEmitter<Events>();
    const seen: string[] = [];
    const off = e.on('state', (s) => seen.push(`a:${s}`));
    e.on('state', (s) => seen.push(`b:${s}`));
    e.emit('state', 'live');
    off();
    off(); // harmless
    e.emit('state', 'ended');
    expect(seen).toEqual(['a:live', 'b:live', 'b:ended']);
    expect(e.listenerCount('state')).toBe(1);
  });

  it('counts the same function added twice as two listeners', () => {
    const e = createEmitter<Events>();
    const fn = vi.fn();
    const off1 = e.on('count', fn);
    e.on('count', fn);
    e.emit('count', 1);
    off1();
    e.emit('count', 2);
    expect(fn.mock.calls).toEqual([[1], [1], [2]]);
  });

  it('once() runs at most once', () => {
    const e = createEmitter<Events>();
    const fn = vi.fn();
    e.once('count', fn);
    e.emit('count', 1);
    e.emit('count', 2);
    expect(fn).toHaveBeenCalledOnce();
    expect(e.listenerCount()).toBe(0);
  });

  it('uses the listeners present when emit started', () => {
    const e = createEmitter<Events>();
    const late = vi.fn();
    e.on('count', () => {
      e.on('count', late);
    });
    e.emit('count', 1);
    expect(late).not.toHaveBeenCalled();
  });

  it('reports a throwing listener without stopping the others', () => {
    const reportError = vi.fn();
    vi.stubGlobal('reportError', reportError);
    const e = createEmitter<Events>();
    const after = vi.fn();
    e.on('count', () => {
      throw new Error('boom');
    });
    e.on('count', after);
    e.emit('count', 1);
    expect(after).toHaveBeenCalledWith(1);
    expect(reportError).toHaveBeenCalledWith(expect.objectContaining({ message: 'boom' }));
  });

  it('clear() removes everything', () => {
    const e = createEmitter<Events>();
    e.on('count', vi.fn());
    e.on('state', vi.fn());
    expect(e.listenerCount()).toBe(2);
    e.clear();
    expect(e.listenerCount()).toBe(0);
  });
});
