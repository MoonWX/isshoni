// Time helpers: waiting, and dates and durations formatted with Intl in the UI language (05 §16.5). REST and signaling
// timestamps are RFC 3339 UTC strings with milliseconds ("2026-10-01T12:00:00.000Z", 01 §5, 03 §3.3).

/**
 * Resolves after ms milliseconds, or rejects with the signal's reason (an AbortError DOMException by default) when
 * the signal aborts first.
 */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(abortReason(signal));
      return;
    }
    const onAbort = (): void => {
      clearTimeout(timer);
      reject(abortReason(signal));
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

function abortReason(signal: AbortSignal | undefined): Error {
  const reason: unknown = signal?.reason;
  return reason instanceof Error ? reason : new DOMException('The operation was aborted.', 'AbortError');
}

/** Runs fn when the browser is idle (requestIdleCallback), else after a short timeout. Returns a cancel function. */
export function whenIdle(fn: () => void, timeoutMs = 2000): () => void {
  if (typeof globalThis.requestIdleCallback === 'function') {
    const id = globalThis.requestIdleCallback(fn, { timeout: timeoutMs });
    return () => {
      globalThis.cancelIdleCallback(id);
    };
  }
  const id = setTimeout(fn, 1);
  return () => {
    clearTimeout(id);
  };
}

/** Parses a wire timestamp; undefined for an empty or invalid string. */
export function parseWireTime(s: string | undefined): Date | undefined {
  if (!s) return undefined;
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? undefined : d;
}

/** Whole seconds for a wait in milliseconds, at least 1 (for "Try again in 3 s"). */
export function secondsFromMs(ms: number): number {
  return Math.max(1, Math.ceil(ms / 1000));
}

/** "Oct 1, 2026, 2:00 PM" in the given language. */
export function formatDateTime(date: Date, lang: string): string {
  return new Intl.DateTimeFormat(lang, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

const RELATIVE_UNITS: readonly [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 365 * 24 * 3600],
  ['month', 30 * 24 * 3600],
  ['week', 7 * 24 * 3600],
  ['day', 24 * 3600],
  ['hour', 3600],
  ['minute', 60],
  ['second', 1],
];

/** "3 minutes ago", "in 2 days", "now": the largest whole unit, in the given language. */
export function formatRelativeTime(date: Date, lang: string, now: Date = new Date()): string {
  const seconds = Math.round((date.getTime() - now.getTime()) / 1000);
  const fmt = new Intl.RelativeTimeFormat(lang, { numeric: 'auto' });
  for (const [unit, size] of RELATIVE_UNITS) {
    if (Math.abs(seconds) >= size || unit === 'second') {
      return fmt.format(Math.trunc(seconds / size), unit);
    }
  }
  return fmt.format(0, 'second');
}
