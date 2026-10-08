// "Try again in N seconds" (05 §15.1: 429 and server_busy show the wait). After a rate_limited or server_busy answer
// the form counts the seconds down and keeps its submit button off until they are over, so a friend doesn't burn
// more of the budget (03 §7.3) by clicking again.
import { useCallback, useEffect, useRef, useState } from 'react';

export interface RetryWait {
  /** Whole seconds left, 0 when no wait is running. */
  readonly secondsLeft: number;
  /** Starts (or restarts) a wait. Fractions round up; 0 or less ends it. */
  start(seconds: number): void;
}

const TICK_MS = 250;

export function useRetryWait(): RetryWait {
  const [secondsLeft, setSecondsLeft] = useState(0);
  /** performance.now() when the wait is over. */
  const until = useRef(0);

  const start = useCallback((seconds: number) => {
    const whole = Number.isFinite(seconds) ? Math.max(0, Math.ceil(seconds)) : 0;
    until.current = performance.now() + whole * 1000;
    setSecondsLeft(whole);
  }, []);

  const waiting = secondsLeft > 0;
  useEffect(() => {
    if (!waiting) return undefined;
    // The clock decides, not the number of ticks: a background tab throttles timers.
    const id = setInterval(() => {
      setSecondsLeft(Math.max(0, Math.ceil((until.current - performance.now()) / 1000)));
    }, TICK_MS);
    return () => {
      clearInterval(id);
    };
  }, [waiting]);

  return { secondsLeft, start };
}
