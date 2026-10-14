// What the sharer needs to outlive a lost connection (01 §10.5, §10.6; 05 §11.1, §13.6). No React here (05 §3).
//
// - The capture hold: a share keeps its capture tracks for CAPTURE_HOLD_MS without a ready connection, so that a
//   server restart or a network switch doesn't make the user pick again; then it ends with "Sharing stopped: the
//   server was unreachable" (BrowserShare).
// - share.start across a drop: a share.start whose answer the socket took with it is sent again with the same `ref`
//   once the connection is resumed into the same room. The server answers a repeated ref with the share it made, if
//   it made one (01 §8.7), so nothing is made twice (requestShareStart; BrowserSharing.start uses it, and
//   BrowserShare keeps the ref of a re-publish that was cut off the same way).
import type { Logger } from '../lib/log';
import { isProtocolError, LocalErrorCodeConnectionLost } from '../protocol/errors';
import { MessageTypeShareStart, type ShareParams, type ShareStart } from '../protocol/types.gen';
import type { PublisherSignal } from './PublisherPC';

/**
 * A share keeps its capture this long without a ready connection, then ends (01 §10.6: 60 s for the web sharer).
 * The same bound limits the wait of a share.start for a resumed connection.
 */
export const CAPTURE_HOLD_MS = 60_000;

/** share.start's `ref` (01 §8.7): a fresh random id, at most 32 characters, unique per connection. */
export function newRef(): string {
  const bytes = new Uint8Array(12);
  globalThis.crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

/** The request failed because the socket went away (or wasn't there): 01's local `connection_lost`. */
export function isConnectionLost(err: unknown): boolean {
  return isProtocolError(err) && err.local && err.code === LocalErrorCodeConnectionLost;
}

/**
 * Waits for the next `ready` of a client that is down, and says whether the server kept the connection in roomId:
 * the welcome was resumed and names that room (01 §10.3, §10.5). False for a new connection, a welcome in another
 * room or none, a client that stops or has stopped, no `ready` within limitMs, an `abort` that fires (or has fired)
 * first, and a signal that can't tell (no onState: the structural ShareContext.signal of tests), or one that is ready
 * already (there is no drop to wait out).
 */
export function resumedInto(
  signal: PublisherSignal,
  roomId: string,
  limitMs: number,
  abort?: AbortSignal,
): Promise<boolean> {
  const { state } = signal;
  if (
    signal.onState === undefined ||
    state === undefined ||
    state === 'ready' ||
    state === 'stopped' ||
    abort?.aborted === true
  ) {
    return Promise.resolve(false);
  }
  return new Promise<boolean>((resolve) => {
    let settled = false;
    // None runs before all are set: the listeners are called on the next state change or abort, the timer later.
    const settle = (kept: boolean): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      off?.();
      abort?.removeEventListener('abort', aborted);
      resolve(kept);
    };
    const aborted = (): void => {
      settle(false);
    };
    const timer = setTimeout(() => {
      settle(false);
    }, limitMs);
    const off = signal.onState?.((state, info) => {
      if (state === 'ready') settle(info.resumed === true && signal.welcome?.roomId === roomId);
      else if (state === 'stopped') settle(false);
    });
    abort?.addEventListener('abort', aborted, { once: true });
  });
}

/**
 * share.start (01 §8.7) in roomId, sent again with the same data, its `ref` included, when the socket went away
 * before the answer and the next welcome resumes the connection in that room within CAPTURE_HOLD_MS (01 §10.5:
 * "retry share.start with the same ref"). Any other outcome rejects with the request's error: the server's, or
 * connection_lost when the connection wasn't kept (a new connection has no such ref, and no room to start it in
 * before the room session has joined again), or when `abort` fired (the capture ended: nothing is left to start).
 */
export async function requestShareStart(
  signal: PublisherSignal,
  roomId: string,
  data: ShareStart,
  log: Logger,
  abort?: AbortSignal,
): Promise<ShareParams> {
  for (;;) {
    try {
      return await signal.request(MessageTypeShareStart, data);
    } catch (err) {
      if (!isConnectionLost(err)) throw err;
      log.info('share.start lost its connection: it goes again if the connection is resumed');
      if (!(await resumedInto(signal, roomId, CAPTURE_HOLD_MS, abort))) throw err;
    }
  }
}
