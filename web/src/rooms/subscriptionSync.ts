// SubscriptionSync (05 §12.4, 01 §8.9): keeps the server's subscriptions equal to what the viewer wants. The viewer
// computes the complete desired set (viewer/layerPolicy.ts, the only place that decides what to subscribe to) and
// passes it to set(); this class diffs it against what the server acknowledged and sends the changes:
// - one subscribe.update with every changed share, so the server applies a user action atomically (the old audio
//   stops and the new one starts in the same step, 05 §12.3), at most 64 shares per message (01 §13);
// - a raise is sent at once, debounced 150 ms to merge focus changes;
// - a drop to video off alone (a tile left the viewport) waits 1 s, so scrolling doesn't flap;
// - after every welcome the full desired set is re-sent when it is non-empty (05 §11.1), in chunks of 64;
// - shareIds the server ignored (the share ended) are dropped from the local state.
// RoomSession owns one (session.subscriptions) and calls resend() and clear(); it never imports React.
import { sleep } from '../lib/time';
import type { Logger } from '../lib/log';
import { isProtocolError, LocalErrorCodeConnectionLost, LocalErrorCodeNotReady } from '../protocol/errors';
import type { SignalClient } from '../protocol/signal-client';
import {
  AudioStateOff,
  ErrorCodeInternal,
  ErrorCodeNotInRoom,
  ErrorCodeRateLimited,
  MaxSubs,
  MessageTypeSubscribeUpdate,
  VideoLayerOff,
  type AudioState,
  type SubscribeResult,
  type SubscriptionWant,
  type VideoLayer,
} from '../protocol/types.gen';

/** Raises (and everything that isn't a lone video-off) go out this long after the last change (05 §18). */
export const SUBSCRIBE_DEBOUNCE_MS = 150;
/** A share whose only change is video → off waits this long (05 §18). */
export const SUBSCRIBE_OFF_DELAY_MS = 1_000;
/** The wait before the one retry of a rate-limited subscribe.update that names no retryAfterMs. */
const RATE_LIMIT_FALLBACK_MS = 1_000;

interface Want {
  readonly video: VideoLayer;
  readonly audio: AudioState;
}

const OFF: Want = { video: VideoLayerOff, audio: AudioStateOff };

const isOff = (w: Want): boolean => w.video === VideoLayerOff && w.audio === AudioStateOff;
const same = (a: Want, b: Want): boolean => a.video === b.video && a.audio === b.audio;

type Timer = ReturnType<typeof setTimeout>;

export interface SubscriptionSyncDeps {
  /** The SignalClient (or a stand-in with its request() and state). */
  signal: Pick<SignalClient, 'request' | 'state'>;
  log: Logger;
  /**
   * subscribe.update was answered not_in_room (05 §6.3): rejoin the desired room. Resolves true once the session is
   * in it again; the full desired set is then sent, because leaving a room drops the subscriptions (01 §8.9). It is
   * called at most once per run of the send loop: a server that still answers not_in_room after the rejoin gets no
   * second one before the next trigger (a set(), a timer, a welcome).
   */
  rejoin?: () => Promise<boolean>;
}

export class SubscriptionSync {
  readonly #signal: SubscriptionSyncDeps['signal'];
  readonly #log: Logger;
  readonly #rejoin: (() => Promise<boolean>) | undefined;
  /** What the viewer wants, without the {off, off} entries (the server's default for a share it wasn't told about). */
  #desired = new Map<string, Want>();
  /** What the server holds, as far as its oks say; without {off, off} entries. */
  readonly #sent = new Map<string, Want>();
  /** Shares whose pending change is a lone video → off: when that change was first seen. */
  readonly #offSince = new Map<string, number>();
  #fastTimer: Timer | undefined;
  #slowTimer: Timer | undefined;
  /** The debounce of the fast changes ran out: they go with the next batch. */
  #fastDue = false;
  /** The next batch is the full desired set (after a welcome or a rejoin). */
  #fullPending = false;
  /** The send loop runs. */
  #running = false;
  /** Something became due while the loop was waiting for a reply. */
  #wake = false;
  /** Grows when the server-side state is void (clear(), resend()): replies to older requests are not applied. */
  #epoch = 0;
  #disposed = false;

  constructor(deps: SubscriptionSyncDeps) {
    this.#signal = deps.signal;
    this.#log = deps.log;
    this.#rejoin = deps.rejoin;
  }

  /** The desired set as last given to set(), minus what the server ignored; {off, off} entries left out. */
  get desired(): SubscriptionWant[] {
    return [...this.#desired].map(([shareId, w]) => ({ shareId, video: w.video, audio: w.audio }));
  }

  /**
   * The complete desired set: every share not listed is {off, off}. Changes against what the server holds are sent
   * as described at the top of this file; a set equal to the last one changes nothing (and doesn't restart the
   * debounce).
   */
  set(wants: readonly SubscriptionWant[]): void {
    if (this.#disposed) return;
    const next = new Map<string, Want>();
    for (const w of wants) {
      const want: Want = { video: w.video, audio: w.audio };
      if (!isOff(want)) next.set(w.shareId, want);
    }
    if (sameWants(next, this.#desired)) return;
    this.#desired = next;
    this.#schedule(true);
  }

  /**
   * After a welcome, once the session is in its room again (05 §11.1): sends the full desired set when it is
   * non-empty. resumed false means the server kept nothing (a new connection): only what is wanted now goes out;
   * resumed true also turns off what the server still holds and the viewer no longer wants.
   */
  resend(resumed: boolean): void {
    if (this.#disposed) return;
    this.#epoch++;
    this.#clearTimers();
    this.#offSince.clear();
    this.#fastDue = false;
    if (!resumed) this.#sent.clear();
    this.#fullPending = true;
    this.#pump();
  }

  /** The session left its room (leave(), a room switch, a room-scope error): nothing is wanted or held anymore. */
  clear(): void {
    this.#epoch++;
    this.#clearTimers();
    this.#desired = new Map();
    this.#sent.clear();
    this.#offSince.clear();
    this.#fastDue = false;
    this.#fullPending = false;
  }

  dispose(): void {
    this.clear();
    this.#disposed = true;
  }

  // ---- Scheduling ----

  /**
   * Arms the timers for the changes pending now. restartDebounce: the desired set just changed, so the 150 ms start
   * again; without it a debounce that is already running keeps its time.
   */
  #schedule(restartDebounce: boolean): void {
    const now = Date.now();
    let fast = false;
    let slowDue: number | undefined;
    const stillOff = new Set<string>();
    for (const [shareId, want] of this.#changes()) {
      if (this.#isLoneVideoOff(shareId, want)) {
        const since = this.#offSince.get(shareId) ?? now;
        this.#offSince.set(shareId, since);
        stillOff.add(shareId);
        slowDue = Math.min(slowDue ?? Infinity, since + SUBSCRIBE_OFF_DELAY_MS);
      } else {
        fast = true;
      }
    }
    for (const shareId of [...this.#offSince.keys()]) {
      if (!stillOff.has(shareId)) this.#offSince.delete(shareId);
    }
    if (!fast) {
      clearTimeout(this.#fastTimer);
      this.#fastTimer = undefined;
      this.#fastDue = false;
    } else if (restartDebounce || (this.#fastTimer === undefined && !this.#fastDue)) {
      clearTimeout(this.#fastTimer);
      this.#fastDue = false;
      this.#fastTimer = setTimeout(() => {
        this.#fastTimer = undefined;
        this.#fastDue = true;
        this.#pump();
      }, SUBSCRIBE_DEBOUNCE_MS);
    }
    this.#armSlow(slowDue, now);
  }

  #armSlow(due: number | undefined, now: number): void {
    clearTimeout(this.#slowTimer);
    this.#slowTimer = undefined;
    if (due === undefined) return;
    this.#slowTimer = setTimeout(
      () => {
        this.#slowTimer = undefined;
        this.#pump();
      },
      Math.max(0, due - now),
    );
  }

  #clearTimers(): void {
    clearTimeout(this.#fastTimer);
    clearTimeout(this.#slowTimer);
    this.#fastTimer = undefined;
    this.#slowTimer = undefined;
  }

  /** Every share whose desired value differs from what the server holds, with the desired value. */
  #changes(): Map<string, Want> {
    const out = new Map<string, Want>();
    for (const [shareId, want] of this.#desired) {
      if (!same(want, this.#sent.get(shareId) ?? OFF)) out.set(shareId, want);
    }
    for (const shareId of this.#sent.keys()) {
      if (!this.#desired.has(shareId)) out.set(shareId, OFF);
    }
    return out;
  }

  /** The change of a tile that left the viewport: the video goes off and nothing else changes. */
  #isLoneVideoOff(shareId: string, want: Want): boolean {
    const held = this.#sent.get(shareId);
    return (
      held !== undefined && want.video === VideoLayerOff && held.video !== VideoLayerOff && want.audio === held.audio
    );
  }

  // ---- Sending ----

  #pump(): void {
    if (this.#running) {
      this.#wake = true; // the loop looks again before it ends
      return;
    }
    this.#running = true;
    void this.#run();
  }

  async #run(): Promise<void> {
    /** How the loop ended: nothing left to send, a failed batch, or no connection to send on. */
    let end: 'idle' | 'failed' | 'down' = 'down';
    /**
     * This run already rejoined for a not_in_room. "Rejoin and retry once" (05 §6.3): a second not_in_room fails the
     * batch, or a server that keeps saying it would get a rejoin and a full set per reply, in a tight loop.
     */
    let rejoined = false;
    try {
      for (;;) {
        this.#wake = false;
        if (this.#disposed || this.#signal.state !== 'ready') return; // the next welcome re-sends everything
        const full = this.#fullPending;
        const batch = this.#nextBatch();
        if (batch.length === 0) {
          end = 'idle';
          return;
        }
        const sent = await this.#send(batch, !rejoined);
        if (sent === 'rejoined') rejoined = true;
        if (sent !== 'failed') continue;
        if (full && this.#alive()) this.#fullPending = true; // the next trigger sends the full set again
        // A failed batch waits for the next trigger (set(), a timer, a welcome): retrying by itself could loop.
        if (!this.#woken()) {
          end = 'failed';
          return;
        }
      }
    } catch (err) {
      end = 'failed';
      this.#log.error('subscription sync failed', { error: err });
    } finally {
      this.#running = false;
      if (this.#alive()) {
        // A reply can leave changes that no timer covers (a want that went back while its request was out).
        if (end === 'idle') this.#schedule(false);
        else if (end === 'failed') this.#rearmSlow();
      }
    }
  }

  /** Something became due while the loop waited for a reply (asked after the await). */
  #woken(): boolean {
    return this.#wake;
  }

  /** Not disposed (asked after an await, when dispose() may have run). */
  #alive(): boolean {
    return !this.#disposed;
  }

  /** The shares to send now: the full set after a welcome, else the changes that are due. */
  #nextBatch(): SubscriptionWant[] {
    const batch: SubscriptionWant[] = [];
    if (this.#fullPending) {
      this.#fullPending = false;
      this.#fastDue = false;
      this.#offSince.clear();
      for (const [shareId, w] of this.#desired) batch.push({ shareId, video: w.video, audio: w.audio });
      for (const shareId of this.#sent.keys()) {
        if (!this.#desired.has(shareId)) batch.push({ shareId, ...OFF });
      }
      return batch;
    }
    const now = Date.now();
    const fastDue = this.#fastDue;
    this.#fastDue = false;
    for (const [shareId, w] of this.#changes()) {
      const since = this.#isLoneVideoOff(shareId, w) ? this.#offSince.get(shareId) : undefined;
      const due = since !== undefined ? now - since >= SUBSCRIBE_OFF_DELAY_MS : fastDue;
      if (due) batch.push({ shareId, video: w.video, audio: w.audio });
    }
    return batch;
  }

  /** After a failed batch: the lone video-offs that weren't due yet still need their timer. */
  #rearmSlow(): void {
    let due: number | undefined;
    for (const [shareId, since] of this.#offSince) {
      const want = this.#desired.get(shareId) ?? OFF;
      if (this.#isLoneVideoOff(shareId, want)) due = Math.min(due ?? Infinity, since + SUBSCRIBE_OFF_DELAY_MS);
      else this.#offSince.delete(shareId);
    }
    const now = Date.now();
    // Only the ones still in the future: a due one that failed waits for the next trigger.
    this.#armSlow(due !== undefined && due > now ? due : undefined, now);
  }

  /**
   * Sends a batch in chunks of at most 64 and applies each ok. failed: a chunk failed. rejoined: a chunk was
   * answered not_in_room and the session joined its room again (only with allowRejoin), so the full set is due.
   */
  async #send(batch: readonly SubscriptionWant[], allowRejoin: boolean): Promise<'sent' | 'rejoined' | 'failed'> {
    const epoch = this.#epoch;
    for (let i = 0; i < batch.length; i += MaxSubs) {
      const chunk = batch.slice(i, i + MaxSubs);
      const result = await this.#request(chunk, epoch, allowRejoin);
      if (result === 'rejoined') {
        // Leaving a room dropped every subscription: start over with the full set (unless a clear() or a welcome
        // already started over meanwhile).
        if (epoch === this.#epoch) {
          this.#sent.clear();
          this.#fullPending = true;
        }
        return 'rejoined';
      }
      if (epoch !== this.#epoch) return 'sent'; // cleared or re-sent meanwhile: the reply is about another state
      if (result === undefined) return 'failed';
      const ignored = new Set(result.ignored);
      for (const w of chunk) {
        if (ignored.has(w.shareId)) {
          // The share ended (01 §8.9): nothing to want anymore.
          this.#desired.delete(w.shareId);
          this.#sent.delete(w.shareId);
          this.#offSince.delete(w.shareId);
        } else if (isOff(w)) {
          this.#sent.delete(w.shareId);
        } else {
          this.#sent.set(w.shareId, { video: w.video, audio: w.audio });
        }
      }
    }
    return 'sent';
  }

  /**
   * One subscribe.update with the client actions of 05 §6.3 (scope request): rate_limited and internal are retried
   * once, not_in_room rejoins when allowRejoin says so. undefined: it failed (a lost connection is not logged; the
   * next welcome re-sends).
   */
  async #request(
    subs: SubscriptionWant[],
    epoch: number,
    allowRejoin: boolean,
  ): Promise<SubscribeResult | 'rejoined' | undefined> {
    for (let attempt = 0; ; attempt++) {
      // A retry after a wait: the room may be another one by now (clear()), and these shares not its own.
      if (epoch !== this.#epoch) return undefined;
      try {
        return await this.#signal.request(MessageTypeSubscribeUpdate, { subs });
      } catch (err) {
        if (!isProtocolError(err)) throw err;
        if (err.local && (err.code === LocalErrorCodeConnectionLost || err.code === LocalErrorCodeNotReady)) {
          return undefined;
        }
        if (err.code === ErrorCodeNotInRoom && this.#rejoin !== undefined && attempt === 0 && allowRejoin) {
          if (await this.#rejoin()) return 'rejoined';
          return undefined;
        }
        if (attempt === 0 && err.code === ErrorCodeRateLimited) {
          await sleep(err.retryAfterMs ?? RATE_LIMIT_FALLBACK_MS);
          continue;
        }
        if (attempt === 0 && err.code === ErrorCodeInternal) continue;
        this.#log.warn('subscribe.update failed', { code: err.code, shares: subs.length });
        return undefined;
      }
    }
  }
}

function sameWants(a: ReadonlyMap<string, Want>, b: ReadonlyMap<string, Want>): boolean {
  if (a.size !== b.size) return false;
  for (const [shareId, want] of a) {
    const other = b.get(shareId);
    if (other === undefined || !same(want, other)) return false;
  }
  return true;
}
