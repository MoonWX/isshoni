// What the specs read from a page: window.__isshoni (05 §10.7), the debug handle that the page installs when
// sessionStorage['isshoni.debug'] is '1' (the harness sets it: fixtures.ts).
// - state(): what the page shows, as ids and states: the connection, the room, the viewer;
// - stats(): a fresh sample of the page's PeerConnections, per tile.
//
// The handle belongs to the room runtime, so it appears some time after a navigation, and it is new after each
// one. Every function here therefore waits for it first; a page that never installs it (it is not signed in, or
// it has no debug flag) fails that wait with a message that says so. A read or a wait may span a navigation: when
// the page navigates under a read, the read waits for the next page's handle and reads that.
//
// The waits poll from Node on a timer, not on the page's animation frames: a tab in the background has none.
import { errors, type Page } from '@playwright/test';

import type { DebugHandle } from '../src/lib/stats/debugHandle';
import type { AudioInSample, PCSample, StatsSample, VideoInSample } from '../src/lib/stats/summarize';
import type { PCKind, ShareKind, ShareStatus, SubscriptionStatus, VideoLayer } from '../src/protocol/types.gen';

export type { AudioInSample, PCSample, StatsSample, VideoInSample };

/**
 * What state() returns (rooms/connectStats.ts debugState, viewer/stats.ts viewerDebugState). Those modules can't
 * be imported here (they reach the page's components), so this is their shape written out; smoke.spec checks a
 * real page against it.
 */
export interface DebugState {
  readonly connection: {
    /** 01's SignalState. */
    readonly state: 'connecting' | 'handshaking' | 'ready' | 'backoff' | 'stopped';
    /** ready: the server resumed the connection (the same connectionId as before the drop). */
    readonly resumed: boolean;
    readonly connectionId: string | null;
  };
  readonly room: {
    /** The room the page wants to be in. */
    readonly roomId: string | null;
    readonly joinState: 'idle' | 'joining' | 'joined' | 'failed';
    /** The rev of the last room.state; null before the first one. */
    readonly rev: number | null;
    /** How many people the last room.state lists. */
    readonly participants: number;
  };
  readonly viewer: {
    /** The share on the stage. */
    readonly focusedShareId: string | null;
    readonly focusMode: 'auto' | 'manual';
    /** The share that is heard. */
    readonly audibleShareId: string | null;
    readonly audio: 'locked' | 'playing' | 'blocked' | 'muted';
    /** The <audio> element exists and is not paused. */
    readonly audioPlaying: boolean;
    readonly videoBlocked: boolean;
    readonly volume: number;
    readonly fullscreen: boolean;
    readonly pipShareId: string | null;
    readonly pageHidden: boolean;
    /** The state of the subscribe PeerConnection. */
    readonly media: 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'unreachable' | 'failed';
    /** The generation of the sub PC; 0 while there is none. */
    readonly subGen: number;
    /** The tiles. */
    readonly shares: readonly DebugShare[];
  };
}

export interface DebugShare {
  readonly id: string;
  readonly userId: string;
  /** A share of this user. */
  readonly own: boolean;
  /** A share of this page: its tile shows the local preview. */
  readonly local: boolean;
  readonly kind: ShareKind;
  readonly status: ShareStatus;
  /** The layers whose tracks reached the server, e.g. ["high", "low"]. */
  readonly layers: readonly VideoLayer[];
  /** How many people watch it. */
  readonly watchers: number;
  /** Its tile is on screen. */
  readonly visible: boolean;
  /** Its video is asked for and stands still. */
  readonly frozen: boolean;
  /** The last subscribe.status entry; null when the server reported nothing. */
  readonly subscription: SubscriptionStatus | null;
}

/** How long a page may take to install the handle after a navigation. */
const HANDLE_TIMEOUT_MS = 15_000;
/** A read waits at least this long for the handle, whatever time its caller has left: one answer of the page. */
const MIN_HANDLE_WAIT_MS = 1_000;
const POLL_INTERVAL_MS = 100;
const STATS_POLL_INTERVAL_MS = 250;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Whether the page has window.__isshoni within `timeout` ms. It throws when the page is closed meanwhile. */
async function handleWithin(page: Page, timeout: number): Promise<boolean> {
  try {
    await page.waitForFunction(() => window.__isshoni !== undefined, undefined, { timeout, polling: POLL_INTERVAL_MS });
    return true;
  } catch (err) {
    if (err instanceof errors.TimeoutError) return false;
    throw err;
  }
}

function noHandle(page: Page, waited: number): Error {
  return new Error(
    `${page.url()} has no window.__isshoni after ${String(waited)} ms: the page is not signed in, ` +
      'or its context lacks the debug flag (enableDebug in fixtures.ts)',
  );
}

/** Resolves when the page has window.__isshoni. */
export async function debugReady(page: Page, timeout = HANDLE_TIMEOUT_MS): Promise<void> {
  if (!(await handleWithin(page, timeout))) throw noHandle(page, timeout);
}

/**
 * A call that lost its handle: the page navigated under it (Playwright's message), or the room runtime ended
 * between the wait for the handle and the call (the message of the calls below).
 */
function handleLost(err: unknown): boolean {
  return err instanceof Error && /Execution context was destroyed|window\.__isshoni is gone/.test(err.message);
}

/**
 * Waits for the page's handle and runs `call`, which uses it in the page. When the handle goes away under the
 * call, this starts over with the next handle, as long as `timeout` ms have not passed.
 */
async function withHandle<T>(page: Page, timeout: number, call: () => Promise<T>): Promise<T> {
  const start = Date.now();
  const deadline = start + timeout;
  for (;;) {
    if (!(await handleWithin(page, Math.max(deadline - Date.now(), MIN_HANDLE_WAIT_MS)))) {
      throw noHandle(page, Date.now() - start);
    }
    try {
      return await call();
    } catch (err) {
      if (!handleLost(err) || Date.now() >= deadline) throw err;
    }
  }
}

/** state() of the page, once it has the handle; `timeout` is how long that may take. */
export function readState(page: Page, timeout = HANDLE_TIMEOUT_MS): Promise<DebugState> {
  return withHandle(page, timeout, async () => {
    const state = await page.evaluate(() => {
      const handle: DebugHandle | undefined = window.__isshoni;
      if (!handle) throw new Error('window.__isshoni is gone');
      return handle.state();
    });
    return state as DebugState;
  });
}

/**
 * A fresh stats() sample of the page, once it has the handle; `timeout` is how long that may take. The first call
 * loads the page's media code.
 */
export function readStats(page: Page, timeout = HANDLE_TIMEOUT_MS): Promise<StatsSample> {
  return withHandle(page, timeout, () =>
    page.evaluate(() => {
      const handle: DebugHandle | undefined = window.__isshoni;
      if (!handle) throw new Error('window.__isshoni is gone');
      return handle.stats();
    }),
  );
}

export interface WaitOptions {
  /** Default 10 s. It includes the wait for the handle. */
  timeout?: number;
  /** For the error: what was waited for. */
  message?: string;
}

/**
 * Calls `read` until `pick` returns something other than undefined, null or false, and returns that. Each read
 * gets the time it may wait for the handle: what is left of `timeout`, and no more than a page takes to install one.
 */
async function poll<S, T>(
  read: (handleTimeout: number) => Promise<S>,
  pick: (value: S) => T | undefined | null | false,
  what: string,
  timeout: number,
  interval: number,
): Promise<T> {
  const deadline = Date.now() + timeout;
  let last = 'nothing';
  for (;;) {
    let lost: unknown;
    try {
      const value = await read(Math.min(deadline - Date.now(), HANDLE_TIMEOUT_MS));
      const got = pick(value);
      if (got !== undefined && got !== null && got !== false) return got;
      last = JSON.stringify(value);
    } catch (err) {
      // A page that kept navigating for as long as the read had: the next read is of the page it becomes.
      if (!handleLost(err)) throw err;
      lost = err;
    }
    if (Date.now() >= deadline) {
      const message = `${what}: not within ${String(timeout)} ms. Last read: ${last}`;
      throw new Error(message, lost === undefined ? {} : { cause: lost });
    }
    await sleep(interval);
  }
}

/**
 * Reads state() until `pick` returns something other than undefined, null or false, and returns that:
 *   const share = await waitForState(page, (s) => s.viewer.shares.find((sh) => sh.status === 'live'));
 * The error of a wait that times out has the last state.
 */
export function waitForState<T>(
  page: Page,
  pick: (state: DebugState) => T | undefined | null | false,
  options: WaitOptions = {},
): Promise<T> {
  const { message = 'the page state', timeout = 10_000 } = options;
  return poll((handleTimeout) => readState(page, handleTimeout), pick, message, timeout, POLL_INTERVAL_MS);
}

/**
 * Reads stats() until `pick` returns something other than undefined, null or false, and returns that:
 *   await waitForStats(page, (s) => (videoOf(s, id)?.framesDecoded ?? 0) > 0);
 * The error of a wait that times out has the last sample.
 */
export function waitForStats<T>(
  page: Page,
  pick: (sample: StatsSample) => T | undefined | null | false,
  options: WaitOptions = {},
): Promise<T> {
  const { message = 'the page stats', timeout = 10_000 } = options;
  // Each read is a getStats() of the page's PeerConnections: not as often as the state.
  return poll((handleTimeout) => readStats(page, handleTimeout), pick, message, timeout, STATS_POLL_INTERVAL_MS);
}

/**
 * Waits until the page's session is in its room: signaling is ready and the join is done, in `roomId` when given.
 * Returns that state.
 */
export function waitForRoom(page: Page, roomId?: string, options: WaitOptions = {}): Promise<DebugState> {
  return waitForState(
    page,
    (s) =>
      s.connection.state === 'ready' &&
      s.room.joinState === 'joined' &&
      s.room.rev !== null &&
      (roomId === undefined || s.room.roomId === roomId) &&
      s,
    { message: `joining ${roomId ?? 'a room'}`, ...options },
  );
}

/** The video a tile receives, or undefined while its share has none in the sample. */
export function videoOf(sample: StatsSample, shareId: string): VideoInSample | undefined {
  return sample.shares[shareId]?.video;
}

/** The audio a tile receives. */
export function audioOf(sample: StatsSample, shareId: string): AudioInSample | undefined {
  return sample.shares[shareId]?.audio;
}

/** The page's PeerConnection of one kind ('sub', 'pub'): the newest generation in the sample. */
export function pcOf(sample: StatsSample, kind: PCKind): PCSample | undefined {
  return sample.pcs
    .filter((pc) => pc.pc === kind)
    .reduce<PCSample | undefined>((a, b) => (a && a.gen > b.gen ? a : b), undefined);
}
