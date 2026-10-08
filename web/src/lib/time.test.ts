import { afterEach, describe, expect, it, vi } from 'vitest';

import { copyText } from './clipboard';
import { formatDateTime, formatRelativeTime, parseWireTime, secondsFromMs, sleep, whenIdle } from './time';

describe('sleep', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('resolves after the delay', async () => {
    vi.useFakeTimers();
    let done = false;
    const p = sleep(1000).then(() => (done = true));
    await vi.advanceTimersByTimeAsync(999);
    expect(done).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    await p;
    expect(done).toBe(true);
  });

  it('rejects with AbortError when aborted, and at once when already aborted', async () => {
    const ctl = new AbortController();
    const p = sleep(60_000, ctl.signal);
    ctl.abort();
    await expect(p).rejects.toMatchObject({ name: 'AbortError' });
    await expect(sleep(10, ctl.signal)).rejects.toMatchObject({ name: 'AbortError' });
    const custom = new AbortController();
    custom.abort(new Error('mine'));
    await expect(sleep(10, custom.signal)).rejects.toThrow('mine');
  });
});

describe('whenIdle', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('uses requestIdleCallback when present', () => {
    const ric = vi.fn(() => 7);
    const cancel = vi.fn();
    vi.stubGlobal('requestIdleCallback', ric);
    vi.stubGlobal('cancelIdleCallback', cancel);
    const fn = vi.fn();
    const stop = whenIdle(fn, 500);
    expect(ric).toHaveBeenCalledWith(fn, { timeout: 500 });
    stop();
    expect(cancel).toHaveBeenCalledWith(7);
  });

  it('falls back to a timeout, which the cancel function clears', () => {
    vi.useFakeTimers();
    vi.stubGlobal('requestIdleCallback', undefined);
    const fn = vi.fn();
    whenIdle(fn);
    vi.advanceTimersByTime(5);
    expect(fn).toHaveBeenCalledOnce();
    const other = vi.fn();
    whenIdle(other)();
    vi.advanceTimersByTime(5);
    expect(other).not.toHaveBeenCalled();
  });
});

describe('time formatting', () => {
  it('parses wire timestamps', () => {
    expect(parseWireTime('2026-10-01T12:00:00.000Z')?.toISOString()).toBe('2026-10-01T12:00:00.000Z');
    expect(parseWireTime('')).toBeUndefined();
    expect(parseWireTime(undefined)).toBeUndefined();
    expect(parseWireTime('not a date')).toBeUndefined();
  });

  it('rounds waits up to whole seconds, at least 1', () => {
    expect(secondsFromMs(0)).toBe(1);
    expect(secondsFromMs(1001)).toBe(2);
    expect(secondsFromMs(42_000)).toBe(42);
  });

  it('formats dates and relative times with Intl', () => {
    const d = new Date('2026-10-01T12:00:00.000Z');
    expect(formatDateTime(d, 'en')).toMatch(/2026/);
    const now = new Date('2026-10-01T12:05:00.000Z');
    expect(formatRelativeTime(d, 'en', now)).toBe('5 minutes ago');
    expect(formatRelativeTime(new Date('2026-10-03T12:05:00.000Z'), 'en', now)).toBe('in 2 days');
    expect(formatRelativeTime(now, 'en', now)).toBe('now');
  });
});

describe('copyText', () => {
  afterEach(() => {
    Reflect.deleteProperty(navigator, 'clipboard');
  });

  it('writes through the Clipboard API', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    await expect(copyText('https://example.test/invite#t')).resolves.toBe(true);
    expect(writeText).toHaveBeenCalledWith('https://example.test/invite#t');
  });

  it('is false when the browser refuses or has no Clipboard API', async () => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: () => Promise.reject(new DOMException('no', 'NotAllowedError')) },
    });
    await expect(copyText('x')).resolves.toBe(false);
    Reflect.deleteProperty(navigator, 'clipboard');
    await expect(copyText('x')).resolves.toBe(false);
  });
});
