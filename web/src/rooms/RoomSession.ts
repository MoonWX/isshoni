// RoomSession (05 §11.1): the controller of "this tab is in a room". It remembers the desired room, joins it when
// signaling is ready and again after every welcome (01 §10.5), keeps roomStore equal to the server's room.state,
// announces room.events, and ties the room's media to the room: the sub PC (viewer/), the subscriptions
// (SubscriptionSync) and the local share (platform.sharing). It is app-level: it survives navigation to /account or
// /admin and ends only with leave(), a room switch, logout or the tab.
//
// It never imports React (05 §3 "Layering rule") and never imports viewer/ or share/: those reach it through the
// seams below (SessionMedia, ShareRecovery, onRoomEvent), so rooms/ builds without them.
import { createEmitter } from '../lib/emitter';
import { errorMessage } from '../lib/errorText';
import { LocalError } from '../lib/errors';
import type { Logger } from '../lib/log';
import { sleep } from '../lib/time';
import type { ActiveShare, PickedSource, Platform } from '../platform/types';
import {
  isProtocolError,
  LocalErrorCodeConnectionLost,
  LocalErrorCodeNotReady,
  LocalErrorCodeRequestTimeout,
  ProtocolError,
} from '../protocol/errors';
import type { SignalClient } from '../protocol/signal-client';
import {
  ErrorCodeInternal,
  ErrorCodeNotInRoom,
  ErrorCodeRateLimited,
  ErrorCodeRoomNotFound,
  ErrorScopeRoom,
  MessageTypeError,
  MessageTypePCICE,
  MessageTypePCOffer,
  MessageTypeRoomEvent,
  MessageTypeRoomJoin,
  MessageTypeRoomLeave,
  MessageTypeRoomState,
  MessageTypeShareStop,
  PCKindSub,
  RoomEventKindShareStopped,
  type EndReason,
  type Error as WireError,
  type PCICE,
  type PCOffer,
  type Preset,
  type RoomEvent,
  type RoomJoinResult,
  type RoomState,
  type Welcome,
} from '../protocol/types.gen';
import type { Stores } from './connection';
import { isAnnounced, roomEventMessage } from './roomEvents';
import { SubscriptionSync } from './subscriptionSync';

// ---- Seams for the media folders ----

/**
 * The part of viewer/SubscriberPC (05 §10.1) the session drives. While the session is in its room, it makes one when
 * the server's first sub offer arrives and routes pc.offer and pc.ice {pc: 'sub'} to it; at any other time (a
 * room.join on its way, no room) those messages are a MediaPeer's that is about to end, and are dropped. The session
 * closes it when the server's side is gone: leave(), a room switch, a welcome that wasn't resumed (a new one, so gen
 * starts again), a room-scope error, dispose().
 */
export interface SubscriberLike {
  handleOffer(o: PCOffer): Promise<void>;
  handleIce(i: PCICE): Promise<void>;
  /** Closes the local PC; no message (leaving the room closes the server's side). */
  close(): void;
}

/** What the session hands a sub PC controller: SubscriberPC's deps (05 §10.1) but its media registry. */
export interface SubscriberDeps {
  platform: Platform;
  signal: SignalClient;
  log: Logger;
}

/** What the session needs from the folders that hold the media. Everything is optional: rooms/ runs without them. */
export interface SessionMedia {
  /** Makes the sub PC controller: `(deps) => new SubscriberPC({...deps, registry})` (viewer/). */
  createSubscriber?: (deps: SubscriberDeps) => SubscriberLike;
}

export interface ShareResyncContext {
  /** The welcome that started this resync. */
  readonly welcome: Welcome;
  /** The room the session is in again. */
  readonly roomId: string;
  /**
   * The server kept this connection's media: the welcome was resumed and the connection was still in roomId. False
   * after a welcome that wasn't resumed, and after a resumed one that needed a room.join (joining gives the
   * connection a new MediaPeer, 01 §8.4): then the server has neither the share nor a pub PC.
   */
  readonly kept: boolean;
}

/**
 * What a local share can add to ActiveShare (05 §8) so that it survives a reconnect: the three transitions that only
 * the session can start, because only it sees every welcome, room.event and room.state. share/'s in-page ActiveShare
 * implements them (serverEnded with the publisher, resync and republish with the web recovery slice). Each is
 * optional; without one the session falls back to ending the share, which is always correct, just less kind.
 */
export interface ShareRecovery {
  /**
   * After a welcome, once the session is in its room again (01 §10.5).
   * ctx.kept: re-send a pending pub offer (same neg), ICE-restart a pub PC that isn't connected, retry a share.start
   * that failed with connection_lost (same ref).
   * Not kept: the server has neither the share nor a pub PC: share.start {replaces, ref: new}, then a new pub PC
   * with gen 1 (01 §10.6); shareId changes.
   * Without it: when the server didn't keep the media, the session stops the share.
   */
  resync(ctx: ShareResyncContext): Promise<void>;
  /**
   * The first room.state after a resumed welcome doesn't list this share: publish it again with replaces
   * (01 §10.5). Without it the session stops the share.
   */
  republish(): Promise<void>;
  /**
   * The server ended this share while signaling was ready (05 §13.1): room.event share.stopped with its reason, or
   * (reason undefined) a newer room.state without it. Stop the capture, clean up the pub PC and go by the reason;
   * never publish it again. Without it the session calls stop().
   */
  serverEnded(reason: EndReason | undefined): void | Promise<void>;
}

type SessionShare = ActiveShare & Partial<ShareRecovery>;

/**
 * Sees every room.event of the session's room before it is announced. Returning true takes the announcement over
 * (the viewer does that for "bo started sharing [Watch]", 05 §12.2); anything else leaves the default toast.
 */
export type RoomEventTap = (e: RoomEvent) => boolean | undefined;

interface SessionEvents extends Record<string, unknown> {
  /** The local share changed: started, or gone (null). */
  share: ActiveShare | null;
}

export interface RoomSessionDeps {
  platform: Platform;
  signal: SignalClient;
  stores: Stores;
  log: Logger;
  media?: SessionMedia;
}

interface JoinOp {
  readonly roomId: string;
  /** Resolves true when this join put the session into roomId. */
  done: Promise<boolean>;
}

interface JoinWaiter {
  readonly roomId: string;
  resolve(): void;
  reject(err: unknown): void;
}

/** The wait before the one retry of a rate-limited request that names no retryAfterMs. */
const RATE_LIMIT_FALLBACK_MS = 1_000;

export class RoomSession {
  /** The server's subscriptions follow the set the viewer gives it (05 §12.4). */
  readonly subscriptions: SubscriptionSync;

  readonly #platform: Platform;
  readonly #signal: SignalClient;
  readonly #stores: Stores;
  readonly #log: Logger;
  readonly #media: SessionMedia;
  readonly #emitter = createEmitter<SessionEvents>();
  readonly #taps = new Set<RoomEventTap>();
  readonly #offs: (() => void)[] = [];

  /** The desired room: the "memory" of 01 §10.5. */
  #desired: string | null = null;
  /**
   * The room the session is in: the server has this connection in it, as far as the client knows, and its MediaPeer
   * there is this page's (not #staleRoom's).
   */
  #joined: string | null = null;
  /**
   * The room whose media the page ended (leave(), a switch to another room) while the server may still have this
   * connection in it, with the MediaPeer of before: its sub PC at its old gen and its subscriptions, through the
   * grace period too (01 §10.3). Joining that room again would keep that MediaPeer, because a room.join for the room
   * the connection is already in only answers ok (01 §8.4); so a room.leave goes first (#runJoin). Null once the
   * server answered a room.leave or a room.join, or a welcome says the connection isn't in that room anymore.
   */
  #staleRoom: string | null = null;
  /** The room.join on its way. */
  #joinOp: JoinOp | null = null;
  #waiters: JoinWaiter[] = [];
  /**
   * No room was ever asked for, so a welcome may pick one (isshoni.lastRoomId, then the default room). False from
   * the first join(), and it stays false after leave() and after the server took the room away.
   */
  #mayPickRoom = true;
  /** rev of the stored snapshot; undefined after every welcome and every join (01 §8.5). */
  #lastRev: number | undefined;
  #resyncSeq = 0;
  #resyncing = false;
  /** A resumed welcome came: the next room.state is compared with the local share (01 §10.5). */
  #reconcilePending = false;
  #subscriber: SubscriberLike | null = null;
  #share: SessionShare | null = null;
  #shareOff: (() => void) | null = null;
  /** The local share's id as a room.state listed it: only then can a snapshot without it mean "ended". */
  #shareSeenId: string | null = null;
  /** platform.sharing.start() runs: its share may be in a room.state before the session knows its id. */
  #startingShare = false;
  /** The share the page is stopping itself: its share.stopped is not "the server ended it". */
  #stoppingShareId: string | null = null;
  #disposed = false;

  constructor(deps: RoomSessionDeps) {
    this.#platform = deps.platform;
    this.#signal = deps.signal;
    this.#stores = deps.stores;
    this.#log = deps.log;
    this.#media = deps.media ?? {};
    this.subscriptions = new SubscriptionSync({
      signal: deps.signal,
      log: deps.log.child('subscriptions'),
      rejoin: () => this.#rejoin(),
    });
    const signal = deps.signal;
    this.#offs.push(
      signal.on(MessageTypeRoomState, (s) => {
        this.#onRoomState(s);
      }),
      signal.on(MessageTypeRoomEvent, (e) => {
        this.#onRoomEvent(e);
      }),
      signal.on(MessageTypeError, (e) => {
        this.#onError(e);
      }),
      signal.on(MessageTypePCOffer, (o) => {
        if (o.pc === PCKindSub) this.#toSubscriber((sub) => sub.handleOffer(o));
      }),
      signal.on(MessageTypePCICE, (i) => {
        if (i.pc === PCKindSub) this.#toSubscriber((sub) => sub.handleIce(i));
      }),
      signal.onState((state) => {
        // Stopped: the connection is gone for good, and with it the server's memory of this tab's room.
        if (state === 'stopped') {
          this.#joined = null;
          this.#staleRoom = null;
          this.#joinOp = null;
        }
      }),
    );
  }

  /** The desired room, null when there is none. */
  get roomId(): string | null {
    return this.#desired;
  }

  /** The share this tab publishes, null when it shares nothing. The M1 UI starts at most one (01 §8.7). */
  get share(): ActiveShare | null {
    return this.#share;
  }

  /** Listens to the local share starting and ending. Returns the unsubscribe function. */
  on(event: 'share', fn: (share: ActiveShare | null) => void): () => void {
    return this.#emitter.on(event, fn);
  }

  /** Adds a RoomEventTap. Returns the function that removes it. */
  onRoomEvent(tap: RoomEventTap): () => void {
    this.#taps.add(tap);
    return () => {
      this.#taps.delete(tap);
    };
  }

  // ---- join, leave ----

  /**
   * Makes roomId the desired room, before anything else. If signaling is ready it sends room.join; otherwise the
   * next resync joins. Joining another room ends this tab's share and subscriptions in the old one (01 §8.4).
   *
   * Resolves once the session is in roomId, or once roomId is no longer wanted (a later join() or leave()). Rejects
   * with the server's ProtocolError when it refuses the room (room_full, forbidden, room_not_found, …); the same
   * error is in roomStore (joinState failed, joinError), so a caller may ignore the promise but must catch it. A
   * lost connection is never an error here: the join happens after the next welcome.
   */
  join(roomId: string): Promise<void> {
    if (this.#disposed) return Promise.reject(new Error('RoomSession: join() after dispose()'));
    const previous = this.#desired;
    this.#desired = roomId;
    this.#mayPickRoom = false;
    const room = this.#stores.room;
    if (previous !== roomId) {
      this.#settleOthers(roomId);
      if (previous !== null) {
        this.#markStale();
        this.#dropMedia();
      }
      this.#lastRev = undefined;
      room.setState({ roomId, joinState: 'joining', joinError: null, room: null, state: null, redirect: null });
    } else if (this.#joined === roomId && room.getState().joinState === 'joined') {
      return Promise.resolve();
    } else {
      room.setState({ joinState: 'joining', joinError: null });
    }
    const done = new Promise<void>((resolve, reject) => {
      this.#waiters.push({ roomId, resolve, reject });
    });
    if (this.#signal.state === 'ready') void this.#joinNow(roomId);
    return done;
  }

  /**
   * Leaves the room and clears the desired room: stops the local share (share.stop; share/ sends pc.close {pub} when
   * it was the last one), sends room.leave, closes the sub PC locally. Offline it only cleans up; the server ends
   * the rest when the grace period runs out, or the next resumed welcome sends the room.leave (also when the same
   * room is wanted again by then: its join leaves first).
   */
  async leave(): Promise<void> {
    const onServer = this.#joined !== null || this.#joinOp !== null;
    this.#markStale();
    this.#desired = null;
    this.#mayPickRoom = false;
    this.#joined = null;
    this.#joinOp = null;
    this.#lastRev = undefined;
    this.#settleOthers(null);
    this.#closeSubscriber();
    this.subscriptions.clear();
    this.#reconcilePending = false;
    const share = this.#detachShare();
    this.#stores.room.setState({
      roomId: null,
      joinState: 'idle',
      joinError: null,
      room: null,
      state: null,
      redirect: null,
    });
    if (share !== null) await this.#stop(share);
    // A join() during the await owns the room now: a room.leave after its room.join would undo it.
    if (this.roomId !== null || this.#disposed) return;
    if (onServer && this.#signal.state === 'ready') await this.#leaveOnServer();
  }

  /**
   * 01 §10.5, called by the SignalClient for every welcome (the first after start() included) with the state
   * already ready; it isn't awaited. It joins the desired room first; with none, and only when no room was ever
   * asked for, it falls back to isshoni.lastRoomId, then welcome.defaultRoomId. Then the media follows: the local
   * share (ShareRecovery.resync), the full subscription set, and after a resumed welcome the comparison of the
   * next room.state with the local share.
   */
  async resync(w: Welcome): Promise<void> {
    if (this.#disposed) return;
    const seq = ++this.#resyncSeq;
    this.#resyncing = true;
    try {
      this.#lastRev = undefined; // rev starts again with every welcome
      this.#reconcilePending = false;
      this.#joinOp = null; // a join on the old socket ended with connection_lost
      const room = this.#stores.room;
      room.setState({ connectionId: w.connectionId, userId: w.user.id });
      if (w.resumed) {
        this.#joined = w.roomId !== undefined && w.roomId !== '' ? w.roomId : null;
        // The welcome says where the server has the connection: the media of any other room is gone there.
        if (this.#staleRoom !== this.#joined) this.#staleRoom = null;
      } else {
        // A new connection: the server has neither PC. Discard the sub PC, so the next one starts at gen 1; the
        // pub PC goes with the share's own resync.
        this.#joined = null;
        this.#staleRoom = null;
        this.#closeSubscriber();
      }

      let roomId = this.#desired;
      if (roomId === null && this.#mayPickRoom) {
        const last = this.#stores.prefs.getState().lastRoomId;
        roomId = last ?? (w.defaultRoomId !== '' ? w.defaultRoomId : null);
        if (roomId !== null) {
          this.#desired = roomId;
          this.#mayPickRoom = false;
          room.setState({ roomId, joinState: 'joining', joinError: null, room: null, state: null, redirect: null });
        }
      }
      if (roomId === null) {
        // No room is wanted. A resumed connection may still be in the one leave() couldn't leave while offline.
        if (this.#joined !== null) {
          this.#joined = null;
          await this.#leaveOnServer();
        }
        return;
      }

      // The server kept the connection in roomId, but the page ended that room's media while the socket was down
      // (leave() and the same room again, or a switch away and back): the server's MediaPeer has no page side
      // anymore. The session is not in the room then, and its join leaves first (#runJoin).
      if (this.#staleRoom === roomId) this.#joined = null;
      // Whether the server still has this connection's media: its MediaPeer lives as long as the connection stays
      // in the room (01 §8.4).
      const kept = this.#joined === roomId;
      if (kept) {
        // Resumed into the wanted room: a join() made while the connection was down is done.
        room.setState({ joinState: 'joined', joinError: null });
        this.#settle(roomId);
      } else {
        room.setState({ joinState: 'joining', joinError: null });
        if (!(await this.#joinNow(roomId))) return;
      }
      if (this.#isStale(seq, roomId)) return;

      await this.#resyncShare({ welcome: w, roomId, kept });
      if (this.#isStale(seq, roomId)) return;
      this.subscriptions.resend(kept);
      if (kept) {
        // Compare the local share with the server's (01 §10.5): with the snapshot that followed this welcome if it
        // is already here, else with the next one.
        this.#reconcilePending = true;
        const snapshot = this.#snapshotSinceWelcome();
        if (snapshot !== null) this.#checkShare(snapshot);
      }
    } finally {
      if (seq === this.#resyncSeq) this.#resyncing = false;
    }
  }

  // ---- The local share ----

  /**
   * Starts publishing a picked source in the session's room (05 §13.1 "starting") through platform.sharing. It
   * rejects with the provider's error (a ProtocolError such as share_limit, a LocalError, …): the share sheet shows
   * it. not_in_room rejoins the desired room and tries once more (05 §6.3).
   */
  async startShare(picked: PickedSource, opts: { preset: Preset; withAudio: boolean }): Promise<void> {
    const sharing = this.#platform.sharing;
    if (sharing === null) throw new LocalError('capture_failed');
    if (this.#share !== null || this.#startingShare) {
      throw new Error('RoomSession: this tab already shares (the M1 UI starts one share, 01 §8.7)');
    }
    const roomId = this.#desired;
    if (roomId === null || this.#joined !== roomId) throw ProtocolError.local(LocalErrorCodeNotReady);
    this.#startingShare = true;
    try {
      let share: ActiveShare;
      try {
        share = await sharing.start(picked, opts, { signal: this.#signal, roomId });
      } catch (err) {
        if (!isProtocolError(err) || err.code !== ErrorCodeNotInRoom || !(await this.#rejoin())) throw err;
        share = await sharing.start(picked, opts, { signal: this.#signal, roomId });
      }
      if (this.#disposed || this.#desired !== roomId) {
        // The session left this room while the share was starting.
        await this.#stop(share);
        throw ProtocolError.local(LocalErrorCodeNotReady);
      }
      this.#attachShare(share);
    } finally {
      this.#startingShare = false;
    }
  }

  /** Stops the local share (the Stop button): ActiveShare.stop() sends share.stop and cleans up the pub PC. */
  async stopShare(): Promise<void> {
    const share = this.#share;
    if (share === null) return;
    this.#stoppingShareId = share.shareId;
    try {
      await share.stop();
    } finally {
      if (this.#stoppingShareId === share.shareId) this.#stoppingShareId = null;
      // A provider that doesn't report 'ended' for its own stop() still ends here.
      if (this.#share === share) this.#detachShare();
    }
  }

  /** The end of the session (logout, tests): drops every listener and timer, closes the sub PC, stops the share. */
  dispose(): void {
    if (this.#disposed) return;
    this.#disposed = true;
    for (const off of this.#offs.splice(0)) off();
    this.#taps.clear();
    this.#desired = null;
    this.#joined = null;
    this.#joinOp = null;
    this.#settleOthers(null);
    this.#closeSubscriber();
    this.subscriptions.dispose();
    const share = this.#detachShare();
    if (share !== null) void this.#stop(share);
    this.#emitter.clear();
  }

  // ---- Joining ----

  /** Sends room.join for roomId unless one is on its way; the result is shared. */
  #joinNow(roomId: string): Promise<boolean> {
    const current = this.#joinOp;
    if (current?.roomId === roomId) return current.done;
    // Joining gives the connection a new MediaPeer (01 §8.4): a sub PC from before has no server side anymore, and
    // the next one starts at gen 1.
    this.#closeSubscriber();
    const op: JoinOp = { roomId, done: Promise.resolve(false) };
    this.#joinOp = op;
    op.done = this.#runJoin(op);
    return op.done;
  }

  /** Still the join the session waits for: not replaced by another join(), leave(), a welcome or dispose(). */
  #isCurrent(op: JoinOp): boolean {
    return !this.#disposed && this.#joinOp === op && this.#desired === op.roomId;
  }

  async #runJoin(op: JoinOp): Promise<boolean> {
    const { roomId } = op;
    try {
      if (this.#staleRoom === roomId) {
        // The server may still have the connection in this room, with the MediaPeer whose page side is gone, and a
        // room.join alone would keep it (01 §8.4). Out first, so that the join makes a new one; a room.leave that
        // fails fails the join. (When leave()'s own room.leave isn't answered yet this is a second one, which the
        // server answers ok all the same.) Until the join's ok the session is in no room: whatever the old
        // MediaPeer still sends is not for the next sub PC.
        this.#joined = null;
        await this.#requestLeave();
        if (!this.#isCurrent(op)) return false;
      }
      const result = await this.#requestJoin(op);
      if (result === null || !this.#isCurrent(op)) return false;
      this.#joined = roomId;
      this.#staleRoom = null; // joining closed the MediaPeer of the room before (01 §8.4)
      this.#stores.room.setState({ joinState: 'joined', joinError: null, room: result.room });
      this.#stores.prefs.getState().setLastRoomId(roomId);
      this.#settle(roomId);
      return true;
    } catch (err) {
      if (this.#isCurrent(op)) this.#joinFailed(roomId, err);
      return false;
    } finally {
      if (this.#joinOp === op) this.#joinOp = null;
    }
  }

  /**
   * room.join with the client actions of 05 §6.3: rate_limited is retried once after retryAfterMs, internal and a
   * timeout once at once. null: the join was replaced while it waited.
   */
  async #requestJoin(op: JoinOp): Promise<RoomJoinResult | null> {
    for (let attempt = 0; ; attempt++) {
      if (!this.#isCurrent(op)) return null;
      try {
        return await this.#signal.request(MessageTypeRoomJoin, { roomId: op.roomId });
      } catch (err) {
        if (!isProtocolError(err) || attempt > 0) throw err;
        if (err.code === ErrorCodeRateLimited && !err.local) {
          await sleep(err.retryAfterMs ?? RATE_LIMIT_FALLBACK_MS);
        } else if (err.code !== ErrorCodeInternal && !(err.local && err.code === LocalErrorCodeRequestTimeout)) {
          throw err;
        }
      }
    }
  }

  #joinFailed(roomId: string, err: unknown): void {
    if (isProtocolError(err) && err.local && err.code !== LocalErrorCodeRequestTimeout) {
      // connection_lost (or stopped): not an error. The room stays wanted, and the next resync joins it.
      return;
    }
    // A refused join leaves the connection where it was (01 §8.4). The session already left that room's UI and
    // media when it switched, so it leaves it on the server too, rather than stay there as a ghost.
    if (this.#joined !== null) {
      this.#joined = null;
      void this.#leaveOnServer();
    }
    if (!isProtocolError(err)) {
      this.#log.error('room.join failed', { error: err });
      this.#stores.room.setState({ joinState: 'failed', joinError: null });
      this.#settle(roomId, err);
      return;
    }
    this.#log.warn('room.join refused', { code: err.code });
    if (err.code === ErrorCodeRoomNotFound && this.#loseRoom(roomId, err)) return;
    this.#stores.room.setState({ joinState: 'failed', joinError: err });
    // After its retry, an internal error is shown with its reference (05 §6.3). The others are the room page's.
    if (err.code === ErrorCodeInternal) this.#stores.ui.getState().toast({ kind: 'error', message: errorMessage(err) });
    this.#settle(roomId, err);
  }

  /** subscribe.update or share.start got not_in_room (05 §6.3): join the desired room again. */
  async #rejoin(): Promise<boolean> {
    const roomId = this.#desired;
    if (roomId === null) return false;
    this.#joined = null;
    this.#stores.room.setState({ joinState: 'joining', joinError: null });
    return this.#joinNow(roomId);
  }

  /** The page is about to end the media of the room it is in: the server's MediaPeer may outlive it (#staleRoom). */
  #markStale(): void {
    if (this.#joined !== null) this.#staleRoom = this.#joined;
  }

  /** room.leave. Its ok says the server has the connection in no room, so no MediaPeer of before is left. */
  async #requestLeave(): Promise<void> {
    await this.#signal.request(MessageTypeRoomLeave, {});
    this.#staleRoom = null;
  }

  /** Sends room.leave; the caller has already forgotten the room. A failure is only logged. */
  async #leaveOnServer(): Promise<void> {
    try {
      await this.#requestLeave();
    } catch (err) {
      // The server ends the membership by itself when the connection is gone.
      if (!(isProtocolError(err) && err.local)) this.#log.warn('room.leave failed', { error: err });
    }
  }

  /**
   * The server took the session out of roomId, or says the room doesn't exist: leave the room UI, tell the user, and
   * send the UI to the default room (05 §6.3). false: there is no other room to go to (the default room itself).
   */
  #loseRoom(roomId: string, err: ProtocolError): boolean {
    const fallback = this.#signal.welcome?.defaultRoomId;
    if (fallback === undefined || fallback === '' || fallback === roomId) return false;
    this.#desired = null;
    this.#joined = null;
    this.#joinOp = null;
    this.#lastRev = undefined;
    this.#dropMedia();
    const prefs = this.#stores.prefs.getState();
    if (prefs.lastRoomId === roomId) prefs.setLastRoomId(null);
    this.#stores.room.setState({
      roomId: null,
      joinState: 'idle',
      joinError: null,
      room: null,
      state: null,
      redirect: { roomId: fallback, code: err.code },
    });
    this.#stores.ui.getState().toast({ kind: 'info', message: errorMessage(err) });
    this.#settle(roomId, err);
    return true;
  }

  /** The room.state stored since the last welcome (or join), null when none came yet. */
  #snapshotSinceWelcome(): RoomState | null {
    return this.#lastRev === undefined ? null : this.#stores.room.getState().state;
  }

  #isStale(seq: number, roomId: string): boolean {
    return this.#disposed || seq !== this.#resyncSeq || this.#desired !== roomId;
  }

  /** Settles the join() promises of roomId: resolved, or rejected with err. */
  #settle(roomId: string, err?: unknown): void {
    const waiters = this.#waiters.filter((w) => w.roomId === roomId);
    this.#waiters = this.#waiters.filter((w) => w.roomId !== roomId);
    for (const w of waiters) {
      if (err === undefined) w.resolve();
      else w.reject(err);
    }
  }

  /** Resolves the join() promises of every room but keep: those rooms aren't wanted anymore. */
  #settleOthers(keep: string | null): void {
    const others = this.#waiters.filter((w) => w.roomId !== keep);
    this.#waiters = this.#waiters.filter((w) => w.roomId === keep);
    for (const w of others) w.resolve();
  }

  // ---- Server messages ----

  #onRoomState(s: RoomState): void {
    if (s.roomId !== this.#desired) return; // a late snapshot of a room the session left
    if (this.#lastRev !== undefined && s.rev <= this.#lastRev) return;
    this.#lastRev = s.rev;
    this.#stores.room.setState({ state: s });
    this.#checkShare(s);
  }

  /** Compares a new snapshot with the local share (01 §10.5 after a resumed welcome; else 05 §13.1's fallback). */
  #checkShare(s: RoomState): void {
    if (this.#signal.state !== 'ready' || this.#startingShare) return;
    const share = this.#share;
    const listed = share !== null && s.shares.some((x) => x.id === share.shareId);
    if (share !== null && listed) this.#shareSeenId = share.shareId;

    if (this.#reconcilePending) {
      this.#reconcilePending = false;
      const own = this.#stores.room.getState().connectionId;
      const localId = share?.shareId;
      // A server share of this connection that the page no longer has (its share.stop was lost with the socket).
      for (const x of s.shares) {
        if (x.connectionId === own && x.id !== localId) this.#stopStray(x.id);
      }
      // A local share the server no longer has (it timed out while the socket was down): publish it again.
      if (share !== null && !listed && share.state !== 'ended') void this.#republish(share);
      return;
    }

    if (share === null || listed || this.#resyncing) return;
    if (this.#shareSeenId === share.shareId && this.#stoppingShareId !== share.shareId) {
      this.#serverEnded(share, undefined);
    }
  }

  #onRoomEvent(e: RoomEvent): void {
    if (e.roomId !== this.#desired) return;
    const share = this.#share;
    if (e.kind === RoomEventKindShareStopped && share !== null && e.shareId === share.shareId) {
      // The own share: never a toast, but it drives the share's state machine (05 §13.1).
      if (this.#stoppingShareId !== share.shareId) this.#serverEnded(share, e.reason);
      return;
    }
    for (const tap of [...this.#taps]) {
      try {
        if (tap(e) === true) return;
      } catch (err) {
        this.#log.error('a room.event tap failed', { error: err });
      }
    }
    if (!isAnnounced(e, this.#stores.room.getState().userId)) return;
    const message = roomEventMessage(e);
    if (message !== null) this.#stores.ui.getState().toast({ kind: 'info', message });
  }

  /** Error notifications (no re). The session acts on scope room; the PCs and the share panel own theirs. */
  #onError(data: WireError): void {
    const err = ProtocolError.fromWire(data);
    if (err.scope !== ErrorScopeRoom) return;
    const roomId = this.#desired;
    if (roomId === null || (err.roomId !== undefined && err.roomId !== roomId)) return;
    this.#log.warn('the server took the session out of its room', { code: err.code });
    if (this.#loseRoom(roomId, err)) return;
    // The default room itself: nowhere to send the UI. Show it as a failed join.
    this.#joined = null;
    this.#dropMedia();
    this.#stores.room.setState({ joinState: 'failed', joinError: err, state: null });
    this.#settle(roomId, err);
  }

  // ---- Media ----

  #toSubscriber(handle: (sub: SubscriberLike) => Promise<void>): void {
    // Only once the session is in the room it wants. While a room.join is on its way, and after a resumed welcome
    // that still has another room, the server's sub PC is the one that join ends (01 §8.4): a sub PC made from its
    // messages would live on into the new room with the old gen and neg. The new MediaPeer sends its first offer
    // after the join's ok.
    if (this.#joined === null || this.#joined !== this.#desired) return;
    this.#subscriber ??=
      this.#media.createSubscriber?.({
        platform: this.#platform,
        signal: this.#signal,
        log: this.#log.child('sub-pc'),
      }) ?? null;
    const sub = this.#subscriber;
    if (sub === null) return;
    handle(sub).catch((err: unknown) => {
      this.#log.error('the sub PC failed to handle a message', { error: err });
    });
  }

  #closeSubscriber(): void {
    const sub = this.#subscriber;
    this.#subscriber = null;
    try {
      sub?.close();
    } catch (err) {
      this.#log.error('closing the sub PC failed', { error: err });
    }
  }

  /** The room's media ends with the room: the sub PC, the subscriptions, the local share. */
  #dropMedia(): void {
    this.#closeSubscriber();
    this.subscriptions.clear();
    this.#reconcilePending = false;
    const share = this.#detachShare();
    if (share !== null) void this.#stop(share);
  }

  #attachShare(share: ActiveShare): void {
    this.#share = share;
    this.#shareSeenId = null;
    this.#shareOff = share.on('ended', () => {
      if (this.#share === share) this.#detachShare();
    });
    this.#emitter.emit('share', share);
  }

  /** Forgets the local share (it ended, or is about to be ended) and returns it. */
  #detachShare(): SessionShare | null {
    const share = this.#share;
    if (share === null) return null;
    this.#shareOff?.();
    this.#shareOff = null;
    this.#share = null;
    this.#shareSeenId = null;
    if (!this.#disposed) this.#emitter.emit('share', null);
    return share;
  }

  /** stop() of a share the session already let go of; a failure is only logged. */
  async #stop(share: ActiveShare): Promise<void> {
    try {
      await share.stop();
    } catch (err) {
      this.#log.warn('stopping the share failed', { error: err });
    }
  }

  #serverEnded(share: SessionShare, reason: EndReason | undefined): void {
    this.#log.info('the server ended the local share', { reason: reason ?? 'room.state' });
    this.#detachShare();
    if (typeof share.serverEnded !== 'function') {
      void this.#stop(share);
      return;
    }
    try {
      const done = share.serverEnded(reason);
      if (done instanceof Promise) {
        done.catch((err: unknown) => {
          this.#log.error('share.serverEnded failed', { error: err });
        });
      }
    } catch (err) {
      this.#log.error('share.serverEnded failed', { error: err });
    }
  }

  /** The share's part of 01 §10.5, after the session is in its room again. */
  async #resyncShare(ctx: ShareResyncContext): Promise<void> {
    const share = this.#share;
    if (share === null || share.state === 'ended') return;
    if (typeof share.resync === 'function') {
      try {
        await share.resync(ctx);
      } catch (err) {
        this.#log.warn('the share could not follow the reconnect', { error: err });
      }
      return;
    }
    if (!ctx.kept) {
      // The server has no such share anymore, and this one can't be published again (no ShareRecovery.resync).
      this.#log.warn('the server lost the share and it has no resync(): stopping it');
      this.#detachShare();
      await this.#stop(share);
    }
  }

  async #republish(share: SessionShare): Promise<void> {
    if (typeof share.republish !== 'function') {
      this.#log.warn('the server lost the share and it has no republish(): stopping it');
      if (this.#share === share) this.#detachShare();
      await this.#stop(share);
      return;
    }
    try {
      await share.republish();
    } catch (err) {
      this.#log.warn('publishing the share again failed', { error: err });
    }
  }

  #stopStray(shareId: string): void {
    this.#log.info('stopping a share of this connection that the page no longer has');
    this.#signal.request(MessageTypeShareStop, { shareId }).catch((err: unknown) => {
      if (!(isProtocolError(err) && err.local && err.code === LocalErrorCodeConnectionLost)) {
        this.#log.warn('share.stop of a stray share failed', { error: err });
      }
    });
  }
}
