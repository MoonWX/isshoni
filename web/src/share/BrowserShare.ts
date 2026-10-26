// BrowserShare (05 §8 ActiveShare, §13.1): one share of this page, from share.start's answer to its end. It owns
// the picked source (the capture), drives the second half of the share state machine in shareStore, and is what
// the room session holds as its local share (RoomSession.share). No React here (05 §3).
//
//   starting ──first keyframe: the share is live in room.state──► live ⇄ reconnecting (the pub PC) ──► ended
//
// How it ends:
// - stop(): the Stop button, leaving the room, logout. share.stop, then the transceivers (a re-offer, or pc.close
//   when no share is left), then the capture tracks;
// - the capture ends by itself: the browser's "Stop sharing" bar, or the shared window closing. The same steps;
// - serverEnded(reason), from the room session (05 §13.1): the server ended it while signaling was ready. The
//   capture stops and the transceivers go, nothing is published again, and the reason decides what the user sees:
//   stopped or left → "Sharing was stopped from another tab or device"; media_timeout → failed, "no video reached
//   the server"; room_closed → nothing here (the session shows the room's end); anything else → failed;
// - an error notification in scope `share` about it (codec_not_supported) → failed, with that error;
// - a pub PC whose negotiation failed twice within a minute (01 §9 rule 8) → failed with PubNegotiationFailedError,
//   which the app turns into its "Can't connect media" screen with Reload (05 §9; shareUi.ts does, it has the
//   uiStore);
// - 60 s without a ready connection (the capture hold, 01 §10.6) → failed, "Sharing stopped: the server was
//   unreachable". Until then the capture stays, so that the share can be published again without a new pick.
//
// Across a reconnect (01 §10.5, §10.6; the room session's ShareRecovery seam, 05 §11.1):
// - a resumed welcome that kept the connection in the room: the pub PC re-sends its offer and restarts ICE by
//   itself (PublisherPC), so resync() has nothing to do;
// - a welcome after which the server has neither the share nor a pub PC (not resumed: a restarted server, a grace
//   period that ran out; or resumed, but with a room.join that gave the connection a new MediaPeer): resync()
//   publishes the share again, share.start {replaces: its id, ref: new} and its tracks on a new pub PC of gen 1;
// - the first room.state after a resumed welcome doesn't list the share (it ended while the socket was down):
//   republish() publishes it again the same way, on a new pub PC of the next gen that replaces the one the server
//   kept (PublisherPC.republish says why not on that one).
// The share gets a new id then: it reports it as a `state` event (05 §8), and viewers move their focus, tile and
// sound to it (01 §10.6). It is `reconnecting` meanwhile, `live` again once the pub PC is connected and the server
// lists it. A share.start of a re-publish whose answer the socket took goes again with the same ref when the
// connection is kept (the server answers with the share it made, 01 §8.7), and with a new one otherwise.
import { createEmitter } from '../lib/emitter';
import { LocalError } from '../lib/errors';
import type { Logger } from '../lib/log';
import type {
  ActiveShare,
  LocalEndReason,
  LocalShareState,
  PickedSource,
  ShareHint,
  ShareStatsSample,
} from '../platform/types';
import type { SignalState } from '../protocol/signal-client';
import {
  EndReasonLeft,
  EndReasonMediaTimeout,
  EndReasonRoomClosed,
  EndReasonStopped,
  MessageTypeShareStart,
  MessageTypeShareStop,
  MessageTypeShareUpdate,
  RIDHigh,
  ShareStatusStarting,
  type EndReason,
  type Preset,
  type QualityHint,
  type RoomState,
  type ShareKind,
  type ShareParams,
} from '../protocol/types.gen';
import type { ShareRecovery, ShareResyncContext } from '../rooms/RoomSession';
import { createHintDetector, hintSample, sameHint } from './hints';
import type { PublisherPC, PublisherSignal, PubMediaState } from './PublisherPC';
import { CAPTURE_HOLD_MS, isConnectionLost, newRef } from './recovery';
import type { ShareOutcome, ShareStore } from './shareStore';

/** The sender's stats are sampled this often for the hints (05 §18). */
export const STATS_SAMPLE_MS = 2_000;
/**
 * After the server ended a share with `stopped`, an error about it may still be on its way (the hub ends a share
 * without usable H.264 with `stopped` and then sends codec_not_supported, 01 §9 rule 7): the "stopped from another
 * tab or device" notice waits this long for it.
 */
export const END_NOTICE_GRACE_MS = 300;

/**
 * Why a share ended that nobody stopped: the server ended it (05 §13.1), or the server was out of reach for longer
 * than the capture hold (01 §10.6). The share panel has a text for each kind.
 */
export class ShareEndedError extends Error {
  override readonly name = 'ShareEndedError';
  /**
   * `media_timeout`: no video reached the server. `server`: any other reason, or none (room.state lost it).
   * `unreachable`: no ready connection for CAPTURE_HOLD_MS.
   */
  readonly kind: 'media_timeout' | 'server' | 'unreachable';
  /** The reason on the wire, when there was one. */
  readonly reason: string | undefined;

  constructor(kind: 'media_timeout' | 'server' | 'unreachable', reason?: string) {
    super(
      kind === 'unreachable'
        ? 'the server was unreachable for too long'
        : `the server ended the share (${reason ?? 'no reason'})`,
    );
    this.kind = kind;
    this.reason = reason;
  }
}

/**
 * The pub PC gave up: its negotiation failed twice within a minute (sdp_invalid or bad_request from the server, or an
 * offer or answer that couldn't be applied here: 01 §9 rule 8). That is not shown as a failed share that can be
 * started again: the app shows "Can't connect media" with Reload (05 §9).
 *
 * It is a LocalError `webrtc_failed` for whoever only reads the code. A start that fails once is a plain LocalError
 * with that code, and stays a failed share.
 */
export class PubNegotiationFailedError extends LocalError {
  constructor() {
    super('webrtc_failed');
  }
}

// A type alias, not an interface: the emitter wants an index signature, which only an alias has implicitly.
type ShareEvents = {
  state: LocalShareState;
  ended: LocalEndReason;
  hint: ShareHint | null;
};

/** An in-page pick: a PickedSource whose stream the page holds (a native pick has none, docs/m2/03 §7.1). */
export type CapturedSource = PickedSource & { readonly preview: MediaStream };

/** Whether a pick carries the page's own capture (every pick of BrowserSharing does). */
export function isCaptured(src: PickedSource): src is CapturedSource {
  return src.preview !== null;
}

export interface BrowserShareDeps {
  /**
   * ShareContext.signal: 01's SignalClient in the app. The capture hold follows its state; a bare SignalClientLike
   * (tests) counts as always ready.
   */
  signal: PublisherSignal;
  publisher: PublisherPC;
  /**
   * The page's share state machine, which this share reports to; null for a share the page's machine doesn't follow
   * (it shows one share, the first: 05 §13.1).
   */
  store: ShareStore | null;
  log: Logger;
  /** The picked source, with its stream; the share's from here on, released when it ends. */
  src: CapturedSource;
  /** The room it is published in. */
  roomId: string;
  /** share.start's answer. */
  params: ShareParams;
  preset: Preset;
  /** Called once, when everything of the share is gone. */
  onEnded: (share: BrowserShare) => void;
}

type Stat = Readonly<Record<string, unknown>>;

const num = (v: unknown): number | undefined => (typeof v === 'number' && Number.isFinite(v) ? v : undefined);
const str = (v: unknown): string | undefined => (typeof v === 'string' && v !== '' ? v : undefined);

/** The outbound-rtp entries of one kind in a sender's stats report. */
function outboundRtp(report: RTCStatsReport | null, kind: 'audio' | 'video'): Stat[] {
  const out: Stat[] = [];
  if (report === null) return out;
  for (const value of report.values()) {
    const s = value as Stat;
    if (s['type'] === 'outbound-rtp' && (s['kind'] ?? s['mediaType']) === kind) out.push(s);
  }
  return out;
}

function limitOf(v: unknown): 'none' | 'bandwidth' | 'cpu' | 'other' | undefined {
  return v === 'none' || v === 'bandwidth' || v === 'cpu' || v === 'other' ? v : undefined;
}

export class BrowserShare implements ActiveShare, ShareRecovery {
  readonly kind: ShareKind;

  readonly #signal: PublisherSignal;
  readonly #publisher: PublisherPC;
  readonly #store: ShareStore | null;
  readonly #log: Logger;
  readonly #src: CapturedSource;
  readonly #roomId: string;
  readonly #onEnded: (share: BrowserShare) => void;
  readonly #emitter = createEmitter<ShareEvents>();
  readonly #detector = createHintDetector();
  /** Removes what begin() set up: track and publisher listeners, the unload prompt, the sampling timer. */
  readonly #offs: (() => void)[] = [];
  /** Per stream of the stats: the bytes sent and the time of the previous sample, for the bitrate. */
  readonly #sent = new Map<string, { bytes: number; at: number }>();

  /** The id the server knows the share by: share.start's, then each re-publish's (01 §10.6). */
  #shareId: string;
  #params: ShareParams | null;
  #preset: Preset;
  #state: LocalShareState = 'starting';
  #preview: MediaStream | null;
  #hint: ShareHint | null = null;
  /** Set once the share is ending; resolves when everything of it is gone. */
  #ending: Promise<void> | null = null;
  /** An error notification about this share (scope share). */
  #serverError: unknown;
  /** Ends the wait for a late error (see END_NOTICE_GRACE_MS). */
  #wake: (() => void) | null = null;
  /** The capture hold: runs while signaling isn't ready (01 §10.6). */
  #holdTimer: ReturnType<typeof setTimeout> | undefined;
  /** The re-publish under way, if any: there is one at a time. */
  #republishing: Promise<void> | null = null;
  /** The ref of a re-publish's share.start whose answer was lost with the socket (01 §10.5: retried as it was). */
  #lostRef: string | null = null;

  constructor(deps: BrowserShareDeps) {
    this.kind = deps.src.kind;
    this.#signal = deps.signal;
    this.#publisher = deps.publisher;
    this.#store = deps.store;
    this.#log = deps.log;
    this.#src = deps.src;
    this.#roomId = deps.roomId;
    this.#onEnded = deps.onEnded;
    this.#shareId = deps.params.shareId;
    this.#params = deps.params;
    this.#preset = deps.preset;
    this.#preview = deps.src.preview;
  }

  /** The share's id on the server. It changes on a re-publish with `replaces` (01 §10.6), with a `state` event. */
  get shareId(): string {
    return this.#shareId;
  }

  /** The captured stream, for the local preview; null once the share ended. */
  get preview(): MediaStream | null {
    return this.#preview;
  }

  get state(): LocalShareState {
    return this.#state;
  }

  /** The error notification the server sent about this share (scope share), if any: why it failed. */
  get error(): unknown {
    return this.#serverError;
  }

  /** The latest ShareParams: share.start's, then share.update's and what quality.hints changed. */
  get params(): ShareParams | null {
    return this.#params;
  }

  on(ev: 'state', fn: (s: LocalShareState) => void): () => void;
  on(ev: 'ended', fn: (r: LocalEndReason) => void): () => void;
  on(ev: 'hint', fn: (h: ShareHint | null) => void): () => void;
  on<K extends keyof ShareEvents>(ev: K, fn: (value: ShareEvents[K]) => void): () => void {
    return this.#emitter.on(ev, fn);
  }

  /**
   * The share's tracks are on the pub PC and its offer is made (BrowserSharing.start): from here on the share
   * watches its capture, the pub PC and its own sender stats.
   */
  begin(): void {
    if (this.#ending !== null) return;
    const browserStopped = (): void => {
      this.#log.info('the capture ended by itself: stopping the share');
      void this.#end('browser-stopped', true, {});
    };
    const videoTracks = this.#src.preview.getVideoTracks();
    for (const track of videoTracks) {
      track.addEventListener('ended', browserStopped);
      this.#offs.push(() => {
        track.removeEventListener('ended', browserStopped);
      });
    }
    this.#offs.push(
      this.#publisher.on('state', (state) => {
        this.#onPublisherState(state);
      }),
    );

    // 05 §13.6: leaving the page would end the share, so the browser asks first.
    const beforeUnload = (ev: Event): void => {
      ev.preventDefault();
    };
    if (typeof globalThis.addEventListener === 'function') {
      globalThis.addEventListener('beforeunload', beforeUnload);
      this.#offs.push(() => {
        globalThis.removeEventListener('beforeunload', beforeUnload);
      });
    }

    const timer = setInterval(() => {
      void this.#sample();
    }, STATS_SAMPLE_MS);
    this.#offs.push(() => {
      clearInterval(timer);
    });

    // The capture hold (01 §10.6): the clock runs while signaling isn't ready.
    const offSignal = this.#signal.onState?.((state) => {
      this.#followSignal(state);
    });
    this.#offs.push(() => {
      offSignal?.();
      clearTimeout(this.#holdTimer);
      this.#holdTimer = undefined;
    });
    if (this.#signal.state !== undefined) this.#followSignal(this.#signal.state);

    // "Stop sharing" may have been pressed while the share was being set up; no event comes for that any more.
    if (videoTracks.some((t) => t.readyState === 'ended')) browserStopped();
  }

  // ---- What BrowserSharing routes here ----

  /**
   * A room.state of the share's room: `starting` → `live` once the server lists the share as live (05 §13.1), and a
   * re-published share that is `reconnecting` on a pub PC that stayed connected → `live` the same way.
   */
  roomState(s: RoomState): void {
    if (this.#ending !== null || s.roomId !== this.#roomId) return;
    if (this.#state !== 'starting' && this.#state !== 'reconnecting') return;
    const info = s.shares.find((x) => x.id === this.#shareId);
    // live, stalled (it was live before), or a status this build doesn't know (01: treat it as live).
    if (info === undefined || info.status === ShareStatusStarting) return;
    const pub = this.#publisher.state;
    if (this.#state === 'starting') {
      this.#setState('live');
      if (pub === 'reconnecting' || pub === 'unreachable') this.#setState('reconnecting');
    } else if (pub === 'connected' && this.#republishing === null) {
      this.#setState('live');
    }
  }

  /**
   * A quality.hint for this share (01 §8.10): its encodings go to the sender at once (setParameters, by rid), and a
   * codec that isn't the one last applied makes the pub PC re-offer with that profile first.
   */
  qualityHint(h: QualityHint): void {
    if (this.#ending !== null || h.shareId !== this.#shareId || this.#params === null) return;
    const change: Partial<ShareParams> = {};
    if (h.encodings !== undefined && h.encodings.length > 0) change.encodings = h.encodings;
    if (h.codec !== undefined && h.codec !== '') change.codec = h.codec;
    if (Object.keys(change).length === 0) return;
    this.#params = { ...this.#params, ...change };
    this.#store?.getState().report({ params: this.#params });
    this.#publisher.applyParams(this.#shareId, change).catch((err: unknown) => {
      this.#log.warn('a quality hint could not be applied', { err, reason: h.reason });
    });
  }

  /** An error notification in scope `share` about this share (05 §6.3): the share fails with it. */
  shareError(err: unknown): void {
    this.#serverError = err;
    if (this.#ending === null) void this.#end('error', true, { error: err });
    else this.#wake?.();
  }

  // ---- ActiveShare ----

  /**
   * Changes the preset while live (05 §13.5): share.update answers with new ShareParams; the encodings and the
   * content hint apply at once, and a changed audio bitrate makes the pub PC re-offer. Rejects with the server's
   * error (the share goes on as it was).
   */
  async setPreset(p: Preset): Promise<void> {
    if (this.#over()) return;
    const params = await this.#signal.request(MessageTypeShareUpdate, { shareId: this.#shareId, preset: p });
    // The share may have ended while the server answered.
    if (this.#over()) return;
    this.#preset = p;
    this.#params = params;
    this.#store?.getState().report({ preset: p, params });
    await this.#publisher.setPreset(this.#shareId, p);
    await this.#publisher.applyParams(this.#shareId, params);
  }

  /** later (M2, feature share.pause): the M1 web sharer has no Pause (05 §13.6). */
  setPaused(): Promise<void> {
    return Promise.reject(
      new DOMException('Pausing a share arrives with the share.pause feature', 'NotSupportedError'),
    );
  }

  /** The share panel's sound switch: off sends silence, the audio m-section stays. A no-op without sound. */
  setAudioEnabled(on: boolean): Promise<void> {
    const tracks = this.#ending === null ? this.#src.preview.getAudioTracks() : [];
    if (tracks.length > 0) {
      for (const track of tracks) track.enabled = on;
      this.#store?.getState().report({ soundOn: on });
    }
    return Promise.resolve();
  }

  /**
   * Stops the share on purpose (05 §13.1 "stopping"). It never rejects: a share.stop the server didn't answer is
   * logged, and the room session stops a share that outlived it at the next resync (01 §10.5). Calling it again,
   * or after the share ended another way, waits for that same end.
   */
  stop(): Promise<void> {
    return this.#end('user', true, {});
  }

  /** One sample of this share's outbound stats (05 §8); null once it ended, or while it has no video sender. */
  async stats(): Promise<ShareStatsSample | null> {
    if (this.#ending !== null) return null;
    const { video, audio } = await this.#publisher.shareStats(this.#shareId);
    if (video === null) return null;
    const now = performance.now();
    const layers: ShareStatsSample['layers'] = [];
    for (const s of outboundRtp(video, 'video')) {
      const rid = str(s['rid']) ?? RIDHigh;
      const limit = limitOf(s['qualityLimitationReason']);
      const width = num(s['frameWidth']);
      const height = num(s['frameHeight']);
      const fps = num(s['framesPerSecond']);
      const encoder = str(s['encoderImplementation']);
      const hw = typeof s['powerEfficientEncoder'] === 'boolean' ? s['powerEfficientEncoder'] : undefined;
      layers.push({
        rid,
        kbps: this.#kbps(`video ${rid}`, s, now),
        ...(width !== undefined ? { width } : {}),
        ...(height !== undefined ? { height } : {}),
        ...(fps !== undefined ? { fps } : {}),
        ...(limit !== undefined ? { limit } : {}),
        ...(encoder !== undefined ? { encoder } : {}),
        ...(hw !== undefined ? { hw } : {}),
      });
    }
    const audioKbps = outboundRtp(audio, 'audio').reduce((sum, s) => sum + this.#kbps('audio', s, now), 0);
    return { at: now, layers, audioKbps };
  }

  // ---- For the room session (its ShareRecovery seam) ----

  /**
   * The server ended this share while signaling was ready (05 §13.1): room.event share.stopped with its reason, or
   * (undefined) a newer room.state without it. Nothing is sent for the share and it is never published again.
   */
  serverEnded(reason: EndReason | undefined): Promise<void> {
    this.#log.info('the server ended the share', { reason: reason ?? 'room.state' });
    switch (reason) {
      case EndReasonStopped:
      case EndReasonLeft:
        return this.#end('server', false, { notice: 'elsewhere' }, true);
      case EndReasonMediaTimeout:
        return this.#end('server', false, { error: new ShareEndedError('media_timeout', reason) });
      case EndReasonRoomClosed:
        // The room's end is the session's to show (05 §6.3, scope room).
        return this.#end('server', false, {});
      default:
        return this.#end('server', false, { error: new ShareEndedError('server', reason) });
    }
  }

  /**
   * After a welcome, once the session is in the share's room again (01 §10.5). ctx.kept: the server still has the
   * share's pub PC, which re-sends its offer and restarts ICE by itself (PublisherPC); a share the server lost
   * meanwhile comes back through republish(). Not kept: the server has neither the share nor a pub PC, so the share
   * is published again (01 §10.6): share.start {replaces: its id, ref: new}, then its tracks on a new pub PC of
   * gen 1. It resolves once that offer is out, or once the share has given up: it never rejects.
   */
  resync(ctx: ShareResyncContext): Promise<void> {
    if (this.#ending !== null || ctx.kept) return Promise.resolve();
    // A re-publish of the connection before is still settling: this one follows it, on what it left.
    const previous = this.#republishing;
    if (previous !== null) return previous.then(() => this.resync(ctx));
    // A share.start that was cut off on the connection before means nothing to this one.
    this.#lostRef = null;
    this.#publisher.forgetServer(ctx.welcome);
    return this.#publishAgain();
  }

  /**
   * The first room.state after a resumed welcome doesn't list the share: it ended while the socket was down. The
   * share is published again (01 §10.5): share.start {replaces: its id}, with the ref of an attempt whose answer was
   * lost if there was one, then its tracks under the new id on a new pub PC (gen + 1). Never rejects.
   */
  republish(): Promise<void> {
    if (this.#ending !== null) return Promise.resolve();
    return this.#publishAgain();
  }

  // ---- Internals ----

  /** Whether the share is ending or has ended. */
  #over(): boolean {
    return this.#ending !== null;
  }

  #setState(state: LocalShareState): void {
    if (this.#state === state || this.#state === 'ended') return;
    this.#state = state;
    if (state === 'live' || state === 'reconnecting') this.#store?.getState().advance(state);
    this.#emitter.emit('state', state);
  }

  #onPublisherState(state: PubMediaState): void {
    const store = this.#store?.getState();
    switch (state) {
      case 'connected':
        store?.report({ unreachable: false });
        if (this.#state === 'reconnecting') this.#setState('live');
        break;
      case 'unreachable':
        store?.report({ unreachable: true });
        if (this.#state === 'live') this.#setState('reconnecting');
        break;
      case 'reconnecting':
        if (this.#state === 'live') this.#setState('reconnecting');
        break;
      case 'failed':
        // Negotiation failed twice within a minute (01 §9 rule 8): nothing more is tried for this PC, and the app
        // says so on its "Can't connect media" screen (05 §9).
        void this.#end('error', true, { error: new PubNegotiationFailedError() });
        break;
      case 'idle':
      case 'connecting':
        break;
    }
  }

  /**
   * The capture hold (01 §10.6): a share whose connection isn't ready keeps its capture for CAPTURE_HOLD_MS, then
   * ends with "Sharing stopped: the server was unreachable". Every `ready` ends the wait, and the next drop starts a
   * new one.
   */
  #followSignal(state: SignalState): void {
    if (this.#ending !== null) return;
    if (state === 'ready') {
      clearTimeout(this.#holdTimer);
      this.#holdTimer = undefined;
      return;
    }
    this.#holdTimer ??= setTimeout(() => {
      this.#holdTimer = undefined;
      this.#log.info('no connection to the server for the whole capture hold: stopping the share');
      // Nothing can be sent: a resumed connection stops the server's share then (the session's reconcile), and a
      // new one never had it.
      void this.#end('server-unreachable', false, { error: new ShareEndedError('unreachable') });
    }, CAPTURE_HOLD_MS);
  }

  /** Publishes the share again under a new id (01 §10.6): one re-publish at a time, which never rejects. */
  #publishAgain(): Promise<void> {
    this.#republishing ??= this.#runPublishAgain().finally(() => {
      this.#republishing = null;
    });
    return this.#republishing;
  }

  async #runPublishAgain(): Promise<void> {
    const oldId = this.#shareId;
    const stream = this.#src.preview;
    if (!stream.getVideoTracks().some((t) => t.readyState !== 'ended')) {
      // The capture ended while the server was away: there is nothing to publish, and nothing to tell the server.
      this.#log.info('the capture ended while the server was away: the share is not published again');
      await this.#end('browser-stopped', false, {});
      return;
    }
    if (this.#state === 'live') this.#setState('reconnecting');
    const ref = this.#lostRef ?? newRef();
    let params: ShareParams;
    try {
      params = await this.#signal.request(MessageTypeShareStart, {
        kind: this.kind,
        preset: this.#preset,
        audio: stream.getAudioTracks().some((t) => t.readyState !== 'ended'),
        ref,
        replaces: oldId,
      });
    } catch (err) {
      if (this.#over()) return;
      if (isConnectionLost(err)) {
        // The server may have made the share: the next welcome that keeps the connection asks again with this ref.
        this.#lostRef = ref;
        this.#log.info('the re-publish lost its connection: it goes again after the next welcome');
        return;
      }
      this.#log.warn('the share could not be published again', { err });
      await this.#end('error', false, { error: err });
      return;
    }
    this.#lostRef = null;
    if (this.#over()) {
      // Stopped meanwhile (the Stop button, the capture): the server has the new share now, so it is stopped there.
      this.#signal.request(MessageTypeShareStop, { shareId: params.shareId }).catch((err: unknown) => {
        if (!isConnectionLost(err)) this.#log.warn('share.stop of the re-published share failed', { err });
      });
      return;
    }
    this.#log.info('the share is published again', { codec: params.codec });
    this.#shareId = params.shareId;
    this.#params = params;
    this.#store?.getState().report({ params });
    // A new id is a change of the share (05 §8: watch 'state'): the page's preview and watchers follow it.
    this.#emitter.emit('state', this.#state);
    try {
      await this.#publisher.republish(oldId, params);
    } catch (err) {
      if (this.#over()) return;
      this.#log.warn('the re-published share could not be put on the pub PC', { err });
      await this.#end('error', true, { error: err });
    }
  }

  #end(reason: LocalEndReason, tellServer: boolean, outcome: ShareOutcome, lateError = false): Promise<void> {
    if (this.#ending === null) {
      let done!: () => void;
      this.#ending = new Promise<void>((resolve) => {
        done = resolve;
      });
      void this.#finish(reason, tellServer, outcome, lateError).finally(done);
    }
    return this.#ending;
  }

  /** The end of the share, in 05 §13.1's order: share.stop, the transceivers, the capture tracks. */
  async #finish(reason: LocalEndReason, tellServer: boolean, outcome: ShareOutcome, lateError: boolean): Promise<void> {
    const shareId = this.#shareId;
    for (const off of this.#offs.splice(0)) off();
    this.#state = 'ended';
    this.#preview = null;
    if (this.#hint !== null) {
      this.#hint = null;
      this.#emitter.emit('hint', null);
    }

    // share.stop goes out first, so the server hears it before the re-offer or the pc.close (01 §8.7).
    const told = tellServer
      ? this.#signal.request(MessageTypeShareStop, { shareId }).then(
          () => undefined,
          (err: unknown) => {
            // Without a connection the server ends the share itself, or the session stops it after the resume.
            if (!isConnectionLost(err)) this.#log.warn('share.stop failed', { err });
          },
        )
      : Promise.resolve();
    const removed = this.#publisher.removeShare(shareId).catch((err: unknown) => {
      this.#log.warn('taking the share off the pub PC failed', { err });
    });
    try {
      this.#src.release();
    } catch (err) {
      this.#log.warn('releasing the capture failed', { err });
    }

    if (outcome.error === undefined) this.#store?.getState().advance('stopping');
    this.#emitter.emit('state', 'ended');
    this.#emitter.emit('ended', reason);

    await removed;
    if (lateError && this.#serverError === undefined) {
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, END_NOTICE_GRACE_MS);
        this.#wake = () => {
          clearTimeout(timer);
          resolve();
        };
      });
      this.#wake = null;
    }
    // An error the server sent about the share says more than how the share happened to end.
    const final: ShareOutcome =
      outcome.error === undefined && this.#serverError !== undefined ? { error: this.#serverError } : outcome;
    this.#store?.getState().finish(final);
    this.#onEnded(this);
    await told;
    this.#emitter.clear();
  }

  /** kbit/s of one outbound stream since the previous sample; 0 for the first one. */
  #kbps(key: string, s: Stat, now: number): number {
    const bytes = num(s['bytesSent']);
    if (bytes === undefined) return 0;
    const at = num(s['timestamp']) ?? now;
    const before = this.#sent.get(key);
    this.#sent.set(key, { bytes, at });
    if (before === undefined || at <= before.at || bytes < before.bytes) return 0;
    return Math.round(((bytes - before.bytes) * 8) / (at - before.at));
  }

  /** Feeds the hint detector (05 §13.7) and reports a hint that changed. */
  async #sample(): Promise<void> {
    if (this.#state !== 'live') return;
    let sample: ShareStatsSample | null;
    try {
      sample = await this.stats();
    } catch (err) {
      this.#log.debug('sampling the share stats failed', { err });
      return;
    }
    if (sample === null || this.#ending !== null) return;
    const hint = this.#detector.push(hintSample(sample, this.#publisher.targetHeight(this.#shareId)));
    if (sameHint(hint, this.#hint)) return;
    this.#hint = hint;
    this.#store?.getState().report({ hint });
    this.#emitter.emit('hint', hint);
  }

  /** The preset the share was started with, or last changed to. */
  get preset(): Preset {
    return this.#preset;
  }
}
