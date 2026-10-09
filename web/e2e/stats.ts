// What the specs read from a page: window.__isshoni (05 §10.7), the debug handle that the page installs when
// sessionStorage['isshoni.debug'] is '1' (the harness sets it: fixtures.ts).
// - state(): what the page shows, as ids and states: the connection, the room, the viewer;
// - stats(): a fresh sample of the page's PeerConnections, per tile.
//
// The handle belongs to the room runtime, so it appears some time after a navigation, and it is new after each
// one. Every function here therefore waits for it first; a page that never installs it (it is not signed in, or
// it has no debug flag) fails that wait with a message that says so.
//
// The waits poll from Node on a timer, not on the page's animation frames: a tab in the background has none.
import type { Page } from '@playwright/test';

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
const POLL_INTERVAL_MS = 100;
const STATS_POLL_INTERVAL_MS = 250;

/** Resolves when the page has window.__isshoni. */
export async function debugReady(page: Page, timeout = HANDLE_TIMEOUT_MS): Promise<void> {
  try {
    await page.waitForFunction(() => window.__isshoni !== undefined, undefined, { timeout, polling: POLL_INTERVAL_MS });
  } catch (err) {
    throw new Error(
      `${page.url()} has no window.__isshoni after ${String(timeout)} ms: the page is not signed in, ` +
        'or its context lacks the debug flag (enableDebug in fixtures.ts)',
      { cause: err },
    );
  }
}

/** state() of the page, once it has the handle. */
export async function readState(page: Page): Promise<DebugState> {
  await debugReady(page);
  const state = await page.evaluate(() => {
    const handle: DebugHandle | undefined = window.__isshoni;
    if (!handle) throw new Error('window.__isshoni is gone');
    return handle.state();
  });
  return state as DebugState;
}

/** A fresh stats() sample of the page, once it has the handle. The first call loads the page's media code. */
export async function readStats(page: Page): Promise<StatsSample> {
  await debugReady(page);
  return page.evaluate(() => {
    const handle: DebugHandle | undefined = window.__isshoni;
    if (!handle) throw new Error('window.__isshoni is gone');
    return handle.stats();
  });
}

export interface WaitOptions {
  /** Default 10 s. */
  timeout?: number;
  /** For the error: what was waited for. */
  message?: string;
}

/** Calls `read` until `pick` returns something other than undefined, null or false, and returns that. */
async function poll<S, T>(
  read: () => Promise<S>,
  pick: (value: S) => T | undefined | null | false,
  what: string,
  timeout: number,
  interval: number,
): Promise<T> {
  const deadline = Date.now() + timeout;
  for (;;) {
    const value = await read();
    const got = pick(value);
    if (got !== undefined && got !== null && got !== false) return got;
    if (Date.now() >= deadline) {
      throw new Error(`${what}: not within ${String(timeout)} ms. Last read: ${JSON.stringify(value)}`);
    }
    await new Promise((resolve) => setTimeout(resolve, interval));
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
  return poll(() => readState(page), pick, message, timeout, POLL_INTERVAL_MS);
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
  return poll(() => readStats(page), pick, message, timeout, STATS_POLL_INTERVAL_MS);
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
