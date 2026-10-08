// PublisherPC (05 §13.4, §13.6, §9): the one pub PC of this connection, made for the first share. The client is its
// only offerer (01 §9 rule 1). It never imports React (05 §3): BrowserSharing routes pc.answer, pc.ice, pc.restart
// and the error notifications in scope `pc` to it, and reads its state through on('state').
//
// A share's m-sections (05 §13.4):
// - video: the track's contentHint from the preset, then addTransceiver(sendonly) with complete sendEncodings in
//   ascending order (`q`, then `f`; encodings.ts), setCodecPreferences with ShareParams.codec first (codecPrefs.ts);
//   if the browser rejects simulcast, one encoding (`f`);
// - audio, when the stream has a track: sendonly with sendEncodings [{maxBitrate: audioBitrate}], Opus only;
// - one setParameters on the video sender for the preset's degradationPreference, before the offer.
// Afterwards setParameters is used only for changes (applyParams, a preset change, a resized source: checked every
// 2 s), always starting from getParameters() and matching encodings by rid.
//
// Negotiation (01 §9, 05 §9):
// - Every offer carries gen, neg and `tracks` for each m-section that carries a share. gen is 1 for the first PC and
//   grows with every PC after it (a rebuild, or a new PC after close()); neg counts the offers of a gen from 1.
// - At most one offer is outstanding. What changes meanwhile is folded into one follow-up offer, made when the
//   answer came. An answer of another gen, or whose neg isn't the outstanding one, is ignored.
// - createOffer, setLocalDescription, setRemoteDescription and setParameters never interleave: one promise queue.
// - Local candidates are trickled with pc.ice; remote ones (the M1 server sends none) are buffered until the remote
//   description is set, at most 64. addIceCandidate waits in the same queue, and like every other operation on the
//   PC it is given up when the PC is closed or replaced meanwhile.
//
// Recovery (the pub column of 05 §9, 01 §10.4):
// - disconnected: signal.probe() at once; still disconnected after 3 s → restartIce() and a new offer, same gen.
// - An ICE restart that isn't connected after 15 s, a failed PC, or the server's pc.restart {pub, rebuild} → rebuild:
//   a new PC, gen + 1, the same tracks and shareIds.
// - At most one ICE restart per 5 s and one rebuild per 10 s; after 5 rebuilds without connecting the state is
//   'unreachable' and rebuilds are 30 s apart.
// - While signaling isn't ready, changes are only recorded. On `ready` after a resumed welcome the outstanding offer
//   goes out again (same neg), unsent candidates follow, and a PC that isn't connected restarts ICE: a disconnected
//   one, and one that is still connecting although its offer was answered. (One whose offer is outstanding waits for
//   the answer to the offer that just went out again; a failed one is rebuilt.) After a welcome that was not resumed
//   the server has no pub PC and gen starts at 1 again, whether a PC exists here at that moment or not: the local PC
//   is dropped without a message, and the shares stay known here, parked, until they are removed or published again
//   (the recovery slice).
// - sdp_invalid or bad_request about this PC, or an offer or answer that can't be applied here: one rebuild; a second
//   failure within 60 s ends it: state 'failed' (01 §9 rule 8), and whoever owns the shares ends them.
// - close() closes the PC on purpose and says so: pc.close {pc: 'pub', gen} (01 §9 rule 10).
import { createEmitter } from '../lib/emitter';
import { LocalError } from '../lib/errors';
import type { Logger } from '../lib/log';
import type { Platform, SignalClientLike } from '../platform/types';
import type { SignalClient, SignalState, SignalStateInfo } from '../protocol/signal-client';
import {
  ErrorCodeBadRequest,
  ErrorCodeRateLimited,
  ErrorCodeSDPInvalid,
  ErrorCodeStaleNegotiation,
  ErrorScopePC,
  MaxCandidateBytes,
  MessageTypePCClose,
  MessageTypePCICE,
  MessageTypePCOffer,
  PCKindPub,
  RestartModeRebuild,
  TrackKindAudio,
  TrackKindVideo,
  type CodecKey,
  type Error as WireError,
  type ICECandidate,
  type ICEServer,
  type PCAnswer,
  type PCClose,
  type PCICE,
  type PCOffer,
  type PCRestart,
  type Preset,
  type ShareParams,
  type TrackRef,
} from '../protocol/types.gen';
import { audioCodecPreferences, videoCodecPreferences } from './codecPrefs';
import { applyEncodings, buildSendEncodings, buildSingleEncoding, needsRescale, sourceSize } from './encodings';
import { applyVideoContentHint, presetHints } from './presets';

/** ICE `disconnected` for this long → ICE restart (01 §10.4). */
export const DISCONNECTED_GRACE_MS = 3_000;
/** An ICE restart that isn't connected this long after it started → rebuild. */
export const ICE_RESTART_TIMEOUT_MS = 15_000;
/** At most one ICE restart per this long. */
export const ICE_RESTART_SPACING_MS = 5_000;
/** At most one rebuild per this long … */
export const REBUILD_SPACING_MS = 10_000;
/** … and per this long once MAX_FAST_REBUILDS of them didn't connect. */
export const REBUILD_SLOW_SPACING_MS = 30_000;
/** This many rebuilds without reaching `connected` → state 'unreachable'. */
export const MAX_FAST_REBUILDS = 5;
/** A second negotiation failure within this long after the first ends the PC (01 §9 rule 8). */
export const NEGOTIATION_FAILURE_WINDOW_MS = 60_000;
/** Candidates kept until they can be applied or sent; the oldest go first (05 §9). */
export const MAX_BUFFERED_CANDIDATES = 64;
/** The source size is re-checked this often (05 §18). */
export const SIZE_CHECK_MS = 2_000;
/** The wait before repeating an offer the server rate-limited without saying how long. */
const DEFAULT_RATE_LIMIT_WAIT_MS = 1_000;

/**
 * What the pub PC needs from signaling: ShareContext.signal's structural type (05 §8), and, when the object is 01's
 * SignalClient (in the app it is), its state and welcome. Without them the PC takes signaling as always ready and
 * the ICE servers as none.
 */
export type PublisherSignal = SignalClientLike & Partial<Pick<SignalClient, 'state' | 'welcome' | 'onState'>>;

export interface PublisherPCDeps {
  /** Peer connections come from the platform, so tests inject fakes (05 §8). */
  platform: Pick<Platform, 'createPeerConnection'>;
  /** ShareContext.signal: 01's SignalClient, by its structural type. */
  signal: PublisherSignal;
  log: Logger;
  /** What the sender can encode: RTCRtpSender.getCapabilities(kind).codecs by default. Tests pass their own. */
  capabilities?: (kind: 'audio' | 'video') => readonly RTCRtpCodec[];
}

/**
 * The pub PC's state: idle (no PC), connecting (the first time), connected, reconnecting (it was connected before),
 * unreachable (5 rebuilds didn't connect: "Can't reach the server's media port"), failed (negotiation failed twice
 * within a minute; only close() leaves it).
 */
export type PubMediaState = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'unreachable' | 'failed';

interface PublisherEvents extends Record<string, unknown> {
  state: PubMediaState;
}

/** A share's outbound stats: the reports of its video and audio senders (null: no such sender). */
export interface ShareSenderStats {
  readonly video: RTCStatsReport | null;
  readonly audio: RTCStatsReport | null;
}

type Timer = ReturnType<typeof setTimeout>;

/** One timer that can be armed again (which cancels the previous one) and cleared. */
class TimerSlot {
  #timer: Timer | undefined;

  get active(): boolean {
    return this.#timer !== undefined;
  }

  set(fn: () => void, ms: number): void {
    this.clear();
    this.#timer = setTimeout(() => {
      this.#timer = undefined;
      fn();
    }, ms);
  }

  clear(): void {
    if (this.#timer !== undefined) {
      clearTimeout(this.#timer);
      this.#timer = undefined;
    }
  }
}

/** The PC a step ran on was closed or replaced meanwhile: the step's result belongs to nobody. */
class PcGoneError extends Error {
  override readonly name = 'PcGoneError';
}

/** The PC's connectivity, from connectionState and iceConnectionState: the worse of the two counts (05 §9). */
type Health = 'pending' | 'connected' | 'disconnected' | 'failed';

function healthOf(pc: RTCPeerConnection): Health {
  const c = pc.connectionState;
  const i = pc.iceConnectionState;
  if (c === 'failed' || i === 'failed') return 'failed';
  if (c === 'disconnected' || i === 'disconnected') return 'disconnected';
  return c === 'connected' ? 'connected' : 'pending';
}

/** One published share: what it sends, how, and its transceivers on the current PC (null while it has none). */
interface PublishedShare {
  readonly shareId: string;
  readonly stream: MediaStream;
  readonly video: MediaStreamTrack;
  readonly audio: MediaStreamTrack | null;
  params: ShareParams;
  preset: Preset;
  /** The profile the codec preferences put first: ShareParams.codec, then the latest hint's (01 §8.10). */
  codec: CodecKey;
  videoTx: RTCRtpTransceiver | null;
  audioTx: RTCRtpTransceiver | null;
}

interface BufferedCandidate {
  gen: number;
  /** null: end of candidates. */
  candidate: RTCIceCandidateInit | null;
}

function toRtcIceServers(servers: readonly ICEServer[] | undefined): RTCIceServer[] {
  return (servers ?? []).map((s) => ({
    urls: [...s.urls],
    ...(s.username !== undefined ? { username: s.username } : {}),
    ...(s.credential !== undefined ? { credential: s.credential } : {}),
  }));
}

/** A wire candidate as addIceCandidate wants it. With max-bundle every candidate belongs to the first m-section. */
function toCandidateInit(c: ICECandidate): RTCIceCandidateInit {
  const init: RTCIceCandidateInit = { candidate: c.candidate };
  if (c.sdpMid !== undefined) init.sdpMid = c.sdpMid;
  if (c.sdpMLineIndex !== undefined) init.sdpMLineIndex = c.sdpMLineIndex;
  if (c.sdpMid === undefined && c.sdpMLineIndex === undefined) init.sdpMLineIndex = 0;
  if (c.usernameFragment !== undefined) init.usernameFragment = c.usernameFragment;
  return init;
}

/** A local candidate for pc.ice: the JSON shape of RTCIceCandidateInit without its nulls (01 §8.8). */
function toWireCandidate(c: RTCIceCandidate): ICECandidate {
  const out: ICECandidate = { candidate: c.candidate };
  if (c.sdpMid != null) out.sdpMid = c.sdpMid;
  if (c.sdpMLineIndex != null) out.sdpMLineIndex = c.sdpMLineIndex;
  if (c.usernameFragment != null) out.usernameFragment = c.usernameFragment;
  return out;
}

/** RTCRtpSender.getCapabilities(kind).codecs of this browser; [] where the API is missing or throws. */
function browserCapabilities(kind: 'audio' | 'video'): readonly RTCRtpCodec[] {
  const sender = (globalThis as { RTCRtpSender?: { getCapabilities?: (kind: string) => RTCRtpCapabilities | null } })
    .RTCRtpSender;
  try {
    return sender?.getCapabilities?.(kind)?.codecs ?? [];
  } catch {
    return [];
  }
}

/** Stops a transceiver for good; where stop() is missing, it at least stops sending. */
function stopTransceiver(tx: RTCRtpTransceiver | null): void {
  if (tx === null) return;
  try {
    if (typeof tx.stop === 'function') {
      tx.stop();
      return;
    }
    tx.direction = 'inactive';
    void tx.sender.replaceTrack(null).catch(() => undefined);
  } catch {
    // Already stopped, or its PC is closed.
  }
}

export class PublisherPC {
  readonly #platform: Pick<Platform, 'createPeerConnection'>;
  readonly #signal: PublisherSignal;
  readonly #log: Logger;
  readonly #capabilities: (kind: 'audio' | 'video') => readonly RTCRtpCodec[];
  readonly #emitter = createEmitter<PublisherEvents>();
  /** The shares of this connection, in the order they were added (05 §13.6). */
  readonly #shares = new Map<string, PublishedShare>();

  #pc: RTCPeerConnection | null = null;
  /** Removes the listeners of #pc. */
  #offPc: (() => void) | null = null;
  /** Rejects when #pc is closed or replaced, so a step in flight on it ends (see #step). */
  #gone: { promise: Promise<never>; reject: (err: PcGoneError) => void } | null = null;
  #gen = 1;
  /** A PC of generation #gen exists or existed: the next one is #gen + 1. */
  #made = false;
  /** The number of the newest offer of this gen. */
  #neg = 0;
  /** The outstanding offer: sent (or waiting for signaling) and not answered. */
  #pending: PCOffer | null = null;
  /** Something changed while an offer was outstanding: one follow-up offer when its answer came. */
  #dirty = false;
  /** An offer was made since the last close(): the server may have a pub PC of this connection. */
  #announced = false;
  /** A pc.close that couldn't be sent; it goes out on the next resumed `ready`. */
  #unsentClose: PCClose | null = null;
  #remoteCandidates: BufferedCandidate[] = [];
  /** Local candidates that couldn't be sent yet. */
  #localCandidates: PCICE[] = [];
  /** Everything that touches the PC's descriptions or a sender's parameters runs one after another. */
  #queue: Promise<void> = Promise.resolve();

  #health: Health = 'pending';
  #state: PubMediaState = 'idle';
  #everConnected = false;
  #wantsIce = false;
  #wantsRebuild = false;
  /** The wanted rebuild is the server's request (its side of the PC failed): this side connecting doesn't settle it. */
  #rebuildAsked = false;
  /** Rebuilds since the PC was last connected. */
  #rebuilds = 0;
  #iceCooling = false;
  #rebuildCooling = false;
  /** A negotiation failure happened less than 60 s ago. */
  #strike = false;
  /** Two negotiation failures within 60 s: nothing more is tried until close(). */
  #fatal = false;
  readonly #disconnectTimer = new TimerSlot();
  readonly #iceRestartTimer = new TimerSlot();
  readonly #iceCooldown = new TimerSlot();
  readonly #rebuildCooldown = new TimerSlot();
  readonly #strikeTimer = new TimerSlot();
  readonly #rateLimitTimer = new TimerSlot();
  #sizeTimer: ReturnType<typeof setInterval> | undefined;

  constructor(deps: PublisherPCDeps) {
    this.#platform = deps.platform;
    this.#signal = deps.signal;
    this.#log = deps.log;
    this.#capabilities = deps.capabilities ?? browserCapabilities;
    // For the object's lifetime, like the listeners of whoever routes the server's messages here (BrowserSharing's
    // link): a welcome that was not resumed starts gen at 1 again, also while no PC exists (01 §9 rule 2).
    this.#signal.onState?.(this.#onSignalState);
  }

  /** The generation of the current pub PC: 1 for the first, + 1 for every PC after it (01 §9, 05 §11.1). */
  get gen(): number {
    return this.#gen;
  }

  get state(): PubMediaState {
    return this.#state;
  }

  /** The ids of the shares this PC publishes (or will, once it has a PC again), oldest first. */
  get shareIds(): string[] {
    return [...this.#shares.keys()];
  }

  /** Listens to the PC's state. Returns the unsubscribe function. */
  on(event: 'state', fn: (state: PubMediaState) => void): () => void {
    return this.#emitter.on(event, fn);
  }

  /**
   * Adds a share's tracks (video, and audio when the stream has an audio track) and offers them: 05 §13.4 steps 2 to
   * 6. It makes the PC when there is none. It resolves once the share's m-sections are in an offer that went out, or
   * are waiting for the outstanding offer's answer (they are in the follow-up offer then).
   *
   * It rejects, with nothing of the share left on the PC, with LocalError `h264_unavailable` when this browser has
   * no H.264 encoder, `capture_failed` when the stream has no video track, and `webrtc_failed` for anything else.
   */
  addShare(shareId: string, stream: MediaStream, params: ShareParams, preset: Preset): Promise<void> {
    return this.#enqueue(async () => {
      if (this.#shares.has(shareId)) throw new Error(`PublisherPC: share ${shareId} is already published`);
      if (this.#fatal) throw new LocalError('webrtc_failed');
      const [video] = stream.getVideoTracks();
      if (video === undefined) throw new LocalError('capture_failed');
      // Step 8, before a PC is made: no H.264 encoder at all.
      videoCodecPreferences(this.#capabilities('video'), params.codec);
      const [audio] = stream.getAudioTracks();
      const share: PublishedShare = {
        shareId,
        stream,
        video,
        audio: audio ?? null,
        params,
        preset,
        codec: params.codec,
        videoTx: null,
        audioTx: null,
      };
      const pc = this.#pc ?? this.#createPc();
      this.#shares.set(shareId, share);
      try {
        await this.#attach(pc, share);
        await this.#negotiate(pc);
      } catch (err) {
        await this.#dropFailedShare(pc, share);
        throw err instanceof LocalError ? err : new LocalError('webrtc_failed', { cause: err });
      }
    });
  }

  /**
   * Stops the share's transceivers; then re-offers, or, when no share is left, closes the PC and sends pc.close
   * (01 §8.7). The capture tracks are the caller's to stop. An unknown share is a no-op.
   */
  removeShare(shareId: string): Promise<void> {
    return this.#enqueue(async () => {
      const share = this.#shares.get(shareId);
      if (share === undefined) return;
      this.#shares.delete(shareId);
      this.#detach(share);
      if (this.#shares.size === 0) {
        this.close();
        return;
      }
      if (this.#pc) await this.#renegotiate(this.#pc);
    });
  }

  /**
   * Applies new ShareParams, or the part of them a quality.hint carries (01 §8.10):
   * - encodings: setParameters on the video sender, each entry matched by rid (active, maxBitrate, maxFramerate,
   *   scaleResolutionDownBy from maxPixels);
   * - codec: when it differs from the one last applied, setCodecPreferences with that profile first and a re-offer
   *   (same gen, neg + 1);
   * - audioBitrate: when it changed, the audio sender's cap and a re-offer, so that the answer carries the new Opus
   *   maxaveragebitrate.
   * shareId is not a parameter to apply and is ignored. An unknown share is a no-op (it ended meanwhile).
   */
  applyParams(shareId: string, p: Partial<ShareParams>): Promise<void> {
    return this.#enqueue(async () => {
      const share = this.#shares.get(shareId);
      if (share === undefined) return;
      let reoffer = false;
      if (p.encodings !== undefined) {
        share.params = { ...share.params, encodings: p.encodings };
        await this.#setEncodings(share);
      }
      if (p.audioBitrate !== undefined && p.audioBitrate !== share.params.audioBitrate) {
        share.params = { ...share.params, audioBitrate: p.audioBitrate };
        if (share.audioTx !== null) {
          await this.#setAudioCap(share);
          reoffer = true;
        }
      }
      if (p.codec !== undefined && p.codec !== '') {
        share.params = { ...share.params, codec: p.codec };
        // A hint is sent again after every resume: the profile last applied needs no re-offer.
        if (p.codec !== share.codec) {
          share.codec = p.codec;
          if (share.videoTx !== null) {
            this.#preferVideoCodecs(share.videoTx, share.codec);
            reoffer = true;
          }
        }
      }
      if (reoffer && this.#pc) await this.#renegotiate(this.#pc);
    });
  }

  /**
   * A preset change while live (05 §13.5): the part only the browser can set, the video track's contentHint and the
   * sender's degradationPreference. The numbers come with the new ShareParams, through applyParams.
   */
  setPreset(shareId: string, preset: Preset): Promise<void> {
    return this.#enqueue(async () => {
      const share = this.#shares.get(shareId);
      if (share === undefined) return;
      share.preset = preset;
      applyVideoContentHint(share.video, preset);
      if (share.videoTx !== null) await this.#setDegradation(share.videoTx.sender, preset);
    });
  }

  // later (M2): setPaused(shareId: string, paused: boolean): Promise<void>;

  /**
   * Takes a pc.answer {pc: 'pub'}. Only the answer to the outstanding offer is applied; any other gen or neg is
   * ignored (01 §9 rule 3). It never rejects: an answer that can't be applied is a negotiation failure.
   */
  handleAnswer(a: PCAnswer): Promise<void> {
    return this.#enqueue(async () => {
      const pc = this.#pc;
      const pending = this.#pending;
      if (a.pc !== PCKindPub || !pc || !pending || a.gen !== this.#gen || a.neg !== pending.neg) {
        this.#log.debug('ignored a pub answer that is not for the outstanding offer', { gen: a.gen, neg: a.neg });
        return;
      }
      try {
        await this.#step(pc, () => pc.setRemoteDescription({ type: 'answer', sdp: a.sdp }));
        this.#pending = null;
        await this.#flushCandidates(pc);
      } catch (err) {
        if (err instanceof PcGoneError) return;
        this.#pending = null;
        this.#log.error('the pub answer could not be applied', { err, gen: this.#gen, neg: a.neg });
        this.#negotiationFailed();
        return;
      }
      if (this.#dirty) await this.#renegotiate(pc);
    });
  }

  /** Takes a pc.ice {pc: 'pub'}: added once the remote description of its gen is set, buffered until then. */
  handleIce(i: PCICE): Promise<void> {
    return this.#enqueue(async () => {
      if (i.pc !== PCKindPub || i.gen < this.#gen) return;
      const candidate = i.candidate ? toCandidateInit(i.candidate) : null;
      const pc = this.#pc;
      if (pc && i.gen === this.#gen && pc.remoteDescription) {
        await this.#addCandidate(pc, candidate);
        return;
      }
      this.#remoteCandidates.push({ gen: i.gen, candidate });
      if (this.#remoteCandidates.length > MAX_BUFFERED_CANDIDATES) {
        this.#remoteCandidates.splice(0, this.#remoteCandidates.length - MAX_BUFFERED_CANDIDATES);
      }
    });
  }

  /**
   * The server asks (01 §10.4): 'rebuild' → a new PC with gen + 1; 'ice' → restartIce() and a new offer. A request
   * for an older gen is ignored. The client's spacing applies: the work may start when its cooldown is over.
   */
  handleRestart(r: PCRestart): Promise<void> {
    if (r.pc !== PCKindPub || r.gen < this.#gen || !this.#pc || this.#fatal) return Promise.resolve();
    this.#log.info('the server asked for a pub PC restart', { mode: r.mode, reason: r.reason, gen: r.gen });
    if (r.mode === RestartModeRebuild) {
      this.#wantsRebuild = true;
      this.#rebuildAsked = true;
    } else {
      this.#wantsIce = true;
    }
    this.#pump();
    return this.#queue;
  }

  /**
   * Takes an error notification in scope `pc` about the pub PC (05 §6.3). One that names the outstanding offer
   * clears it. sdp_invalid and bad_request → rebuild once, 'failed' the second time within 60 s; stale_negotiation →
   * ignored; rate_limited → the offer goes out again after retryAfterMs; others are logged.
   */
  handleError(e: WireError): void {
    if (e.scope !== ErrorScopePC || e.pc !== PCKindPub || !this.#pc || this.#fatal) return;
    if (e.gen !== undefined && e.gen < this.#gen) return;
    const pending = this.#pending;
    const aboutPending = pending !== null && (e.neg === undefined || e.neg === pending.neg);
    switch (e.code) {
      case ErrorCodeSDPInvalid:
      case ErrorCodeBadRequest:
        this.#log.warn('the server rejected the pub offer', { code: e.code, gen: this.#gen, neg: e.neg });
        if (aboutPending) this.#pending = null;
        this.#negotiationFailed();
        break;
      case ErrorCodeStaleNegotiation:
        break;
      case ErrorCodeRateLimited:
        if (aboutPending) {
          this.#rateLimitTimer.set(() => {
            this.#resendPending();
          }, e.retryAfterMs ?? DEFAULT_RATE_LIMIT_WAIT_MS);
        }
        break;
      default:
        this.#log.warn('pub PC error', { code: e.code, gen: this.#gen });
    }
  }

  /** gen + 1, the same tracks and shareIds, a new offer (01 §10.4). A no-op without a share. */
  rebuild(): Promise<void> {
    this.#startRebuild();
    return this.#queue;
  }

  /** The outbound stats of one share's senders; null entries where it has no such sender (now). */
  async shareStats(shareId: string): Promise<ShareSenderStats> {
    const share = this.#shares.get(shareId);
    const read = async (tx: RTCRtpTransceiver | null | undefined): Promise<RTCStatsReport | null> => {
      if (!tx) return null;
      try {
        return await tx.sender.getStats();
      } catch {
        return null;
      }
    };
    const [video, audio] = await Promise.all([read(share?.videoTx), read(share?.audioTx)]);
    return { video, audio };
  }

  /**
   * The height a share's full layer is meant to encode: the source's height over that layer's
   * scaleResolutionDownBy. undefined while the source size or the sender is unknown.
   */
  targetHeight(shareId: string): number | undefined {
    const share = this.#shares.get(shareId);
    const size = share ? sourceSize(share.video) : null;
    if (!share?.videoTx || size === null) return undefined;
    const encodings = share.videoTx.sender.getParameters().encodings;
    const scale = Math.min(...encodings.map((e) => e.scaleResolutionDownBy ?? 1));
    return Number.isFinite(scale) && scale > 0 ? Math.round(size.height / scale) : undefined;
  }

  /**
   * Closes the PC on purpose and sends pc.close {pc: 'pub', gen} (the server ends every share of it, 01 §8.7).
   * Everything about it is forgotten, the shares too; the capture tracks are their owners' to stop. The object
   * stays usable: the next addShare makes a new PC with the next gen.
   */
  close(): void {
    // Also for a rebuilt PC that made no offer yet: the server still has the one before it, and a pc.close of a
    // newer gen closes that too (02 §5.3).
    const closing: PCClose | null = this.#pc && this.#announced ? { pc: PCKindPub, gen: this.#gen } : null;
    this.#announced = false;
    this.#shares.clear();
    this.#teardownPc();
    this.#resetRecovery();
    if (closing !== null) {
      if (this.#signal.notify(MessageTypePCClose, closing)) this.#log.info('pub PC closed', { gen: closing.gen });
      else this.#unsentClose = closing;
    }
    this.#updateState();
  }

  // ---- The queue ----

  /** Runs task after the work in flight; the returned promise is the task's, and a failure doesn't stop the queue. */
  #enqueue<T>(task: () => Promise<T>): Promise<T> {
    const run = this.#queue.then(task);
    this.#queue = run.then(
      () => undefined,
      () => undefined,
    );
    return run;
  }

  /** Runs internal work in the queue: a failure is logged, never thrown. */
  #background(what: string, task: () => Promise<void>): void {
    void this.#enqueue(task).catch((err: unknown) => {
      if (!(err instanceof PcGoneError)) this.#log.error(`pub PC: ${what} failed`, { err });
    });
  }

  /**
   * Waits for one operation on pc. It rejects with PcGoneError when pc is closed or replaced meanwhile: an operation
   * on a closed RTCPeerConnection may never settle, and the queue must not wait behind it.
   */
  async #step<T>(pc: RTCPeerConnection, op: () => Promise<T>): Promise<T> {
    const gone = this.#gone;
    if (this.#pc !== pc || gone === null) throw new PcGoneError();
    const value = await Promise.race([op(), gone.promise]);
    if (this.#pc !== pc) throw new PcGoneError();
    return value;
  }

  // ---- The PC ----

  #createPc(): RTCPeerConnection {
    if (this.#made) this.#gen++;
    this.#made = true;
    const pc = this.#platform.createPeerConnection({
      iceServers: toRtcIceServers(this.#signal.welcome?.iceServers),
      bundlePolicy: 'max-bundle',
      rtcpMuxPolicy: 'require',
    });
    const onCandidate = (ev: RTCPeerConnectionIceEvent): void => {
      if (this.#pc === pc) this.#onLocalCandidate(ev.candidate);
    };
    const onState = (): void => {
      if (this.#pc === pc) this.#onPcState(pc);
    };
    pc.addEventListener('icecandidate', onCandidate);
    pc.addEventListener('connectionstatechange', onState);
    pc.addEventListener('iceconnectionstatechange', onState);
    this.#offPc = () => {
      pc.removeEventListener('icecandidate', onCandidate);
      pc.removeEventListener('connectionstatechange', onState);
      pc.removeEventListener('iceconnectionstatechange', onState);
    };
    let reject!: (err: PcGoneError) => void;
    const promise = new Promise<never>((_, rej) => {
      reject = rej;
    });
    // Nobody may be waiting when the PC goes: that is not an unhandled rejection.
    promise.catch(() => undefined);
    this.#gone = { promise, reject };
    this.#pc = pc;
    // A pc.close that never went out is about a PC this one replaces (a higher gen does that, 01 §9 rule 2).
    this.#unsentClose = null;
    this.#log.info('pub PC created', { gen: this.#gen });
    this.#updateState();
    return pc;
  }

  /** Closes the PC locally and drops what belongs to it; the shares stay, without transceivers. */
  #teardownPc(): void {
    const pc = this.#pc;
    this.#pc = null;
    this.#offPc?.();
    this.#offPc = null;
    this.#gone?.reject(new PcGoneError());
    this.#gone = null;
    this.#disconnectTimer.clear();
    this.#iceRestartTimer.clear();
    this.#rateLimitTimer.clear();
    this.#neg = 0;
    this.#pending = null;
    this.#dirty = false;
    this.#remoteCandidates = [];
    this.#localCandidates = [];
    this.#wantsIce = false;
    this.#wantsRebuild = false;
    this.#rebuildAsked = false;
    this.#health = 'pending';
    for (const share of this.#shares.values()) {
      share.videoTx = null;
      share.audioTx = null;
    }
    if (!pc) return;
    try {
      pc.close();
    } catch (err) {
      this.#log.warn('closing the pub PC failed', { err });
    }
  }

  /** What outlives a PC but not a close(): the recovery bookkeeping and the size check. */
  #resetRecovery(): void {
    for (const t of [this.#iceCooldown, this.#rebuildCooldown, this.#strikeTimer]) t.clear();
    this.#everConnected = false;
    this.#rebuilds = 0;
    this.#iceCooling = false;
    this.#rebuildCooling = false;
    this.#strike = false;
    this.#fatal = false;
    if (this.#sizeTimer !== undefined) clearInterval(this.#sizeTimer);
    this.#sizeTimer = undefined;
  }

  // ---- A share's transceivers ----

  /** 05 §13.4 steps 2 to 5 for one share on pc. */
  async #attach(pc: RTCPeerConnection, share: PublishedShare): Promise<void> {
    applyVideoContentHint(share.video, share.preset);
    const size = sourceSize(share.video);
    let videoTx: RTCRtpTransceiver;
    try {
      videoTx = pc.addTransceiver(share.video, {
        direction: 'sendonly',
        streams: [share.stream],
        sendEncodings: buildSendEncodings(share.params.encodings, size),
      });
    } catch (err) {
      if (this.#pc !== pc) throw new PcGoneError();
      this.#log.warn('simulcast was rejected: publishing one layer', { err });
      videoTx = pc.addTransceiver(share.video, {
        direction: 'sendonly',
        streams: [share.stream],
        sendEncodings: buildSingleEncoding(share.params.encodings, size),
      });
    }
    share.videoTx = videoTx;
    this.#preferVideoCodecs(videoTx, share.codec);
    if (share.audio !== null) {
      // A cap only: the answer's maxaveragebitrate is what the browser encodes at (02 §8.4).
      const audioTx = pc.addTransceiver(share.audio, {
        direction: 'sendonly',
        streams: [share.stream],
        sendEncodings: [{ maxBitrate: share.params.audioBitrate }],
      });
      share.audioTx = audioTx;
      this.#preferCodecs(audioTx, audioCodecPreferences(this.#capabilities('audio')));
    }
    await this.#setDegradation(videoTx.sender, share.preset);
    if (this.#pc !== pc) throw new PcGoneError();
    this.#watchSizes();
  }

  #detach(share: PublishedShare): void {
    stopTransceiver(share.videoTx);
    stopTransceiver(share.audioTx);
    share.videoTx = null;
    share.audioTx = null;
  }

  /** addShare failed after the share was put on pc: take it off again, and leave the PC as it was before. */
  async #dropFailedShare(pc: RTCPeerConnection, share: PublishedShare): Promise<void> {
    this.#shares.delete(share.shareId);
    if (this.#pc !== pc) return;
    this.#detach(share);
    if (this.#shares.size === 0) this.close();
    else await this.#renegotiate(pc);
  }

  #preferVideoCodecs(tx: RTCRtpTransceiver, codec: CodecKey): void {
    this.#preferCodecs(tx, videoCodecPreferences(this.#capabilities('video'), codec));
  }

  #preferCodecs(tx: RTCRtpTransceiver, codecs: RTCRtpCodec[]): void {
    if (codecs.length === 0 || typeof tx.setCodecPreferences !== 'function') return;
    try {
      tx.setCodecPreferences(codecs);
    } catch (err) {
      // The offer then lists the browser's own order; the server's answer still picks an allowed profile.
      this.#log.warn('setCodecPreferences was not accepted', { err });
    }
  }

  /** One setParameters for degradationPreference only: the encodings stay as getParameters() gave them. */
  async #setDegradation(sender: RTCRtpSender, preset: Preset): Promise<void> {
    try {
      const params = sender.getParameters();
      params.degradationPreference = presetHints(preset).degradationPreference;
      await sender.setParameters(params);
    } catch (err) {
      this.#log.info('degradationPreference was not accepted: the browser keeps its default', { err });
    }
  }

  async #setEncodings(share: PublishedShare): Promise<void> {
    const sender = share.videoTx?.sender;
    if (!sender) return;
    try {
      const params = sender.getParameters();
      if (applyEncodings(params, share.params.encodings, sourceSize(share.video))) await sender.setParameters(params);
    } catch (err) {
      this.#log.warn('the video encodings could not be set', { err });
    }
  }

  async #setAudioCap(share: PublishedShare): Promise<void> {
    const sender = share.audioTx?.sender;
    if (!sender) return;
    try {
      const params = sender.getParameters();
      for (const e of params.encodings) e.maxBitrate = share.params.audioBitrate;
      await sender.setParameters(params);
    } catch (err) {
      this.#log.warn('the audio bitrate cap could not be set', { err });
    }
  }

  /** Starts the 2 s check of the source sizes (window resizes): 05 §13.4 step 7. */
  #watchSizes(): void {
    this.#sizeTimer ??= setInterval(() => {
      for (const share of this.#shares.values()) {
        const sender = share.videoTx?.sender;
        const size = sourceSize(share.video);
        if (!sender || size === null) continue;
        let resized: boolean;
        try {
          resized = needsRescale(sender.getParameters(), share.params.encodings, size);
        } catch {
          continue;
        }
        if (resized) this.#background('rescaling a resized source', () => this.#setEncodings(share));
      }
    }, SIZE_CHECK_MS);
  }

  // ---- Negotiation ----

  /** The m-sections that carry a share now (01 §9 rule 4). */
  #trackRefs(): TrackRef[] {
    const refs: TrackRef[] = [];
    for (const share of this.#shares.values()) {
      const videoMid = share.videoTx?.mid;
      if (videoMid != null) refs.push({ mid: videoMid, shareId: share.shareId, kind: TrackKindVideo });
      const audioMid = share.audioTx?.mid;
      if (audioMid != null) refs.push({ mid: audioMid, shareId: share.shareId, kind: TrackKindAudio });
    }
    return refs;
  }

  /** Makes and sends the next offer, unless one is outstanding: then one follow-up is made when its answer came. */
  async #negotiate(pc: RTCPeerConnection): Promise<void> {
    if (this.#pending !== null) {
      this.#dirty = true;
      return;
    }
    this.#dirty = false;
    const offer = await this.#step(pc, () => pc.createOffer());
    await this.#step(pc, () => pc.setLocalDescription(offer));
    const sdp = pc.localDescription?.sdp ?? offer.sdp ?? '';
    const msg: PCOffer = { pc: PCKindPub, gen: this.#gen, neg: ++this.#neg, sdp, tracks: this.#trackRefs() };
    this.#pending = msg;
    this.#announced = true;
    // false while signaling is down: the offer stays outstanding and goes out on the next resumed `ready`.
    this.#signal.notify(MessageTypePCOffer, msg);
  }

  /** #negotiate for a change after the first offer: a failure here is a negotiation failure, never thrown. */
  async #renegotiate(pc: RTCPeerConnection): Promise<void> {
    try {
      await this.#negotiate(pc);
    } catch (err) {
      if (err instanceof PcGoneError || this.#pc !== pc) return;
      this.#log.error('the pub offer could not be made', { err, gen: this.#gen });
      this.#negotiationFailed();
    }
  }

  /** Sends the outstanding offer again, with the same neg: the server answers a repeated neg with its stored answer. */
  #resendPending(): void {
    if (this.#pending !== null && this.#pc) this.#signal.notify(MessageTypePCOffer, this.#pending);
  }

  // ---- Candidates ----

  #onLocalCandidate(candidate: RTCIceCandidate | null): void {
    let msg: PCICE;
    if (candidate !== null && candidate.candidate !== '') {
      if (new TextEncoder().encode(candidate.candidate).length > MaxCandidateBytes) return;
      msg = { pc: PCKindPub, gen: this.#gen, candidate: toWireCandidate(candidate) };
    } else {
      // null, or Firefox's empty candidate string: end of candidates. The marker is optional (01 §9 rule 5).
      msg = { pc: PCKindPub, gen: this.#gen };
    }
    if (this.#signal.notify(MessageTypePCICE, msg)) return;
    this.#localCandidates.push(msg);
    if (this.#localCandidates.length > MAX_BUFFERED_CANDIDATES) this.#localCandidates.shift();
  }

  async #flushCandidates(pc: RTCPeerConnection): Promise<void> {
    const mine = this.#remoteCandidates.filter((c) => c.gen === this.#gen);
    this.#remoteCandidates = this.#remoteCandidates.filter((c) => c.gen !== this.#gen);
    for (const c of mine) {
      if (this.#pc !== pc) return;
      await this.#addCandidate(pc, c.candidate);
    }
  }

  /** Never rejects: a candidate the PC doesn't take is logged, and one for a PC that is gone is nobody's. */
  async #addCandidate(pc: RTCPeerConnection, candidate: RTCIceCandidateInit | null): Promise<void> {
    try {
      // No argument: end of candidates.
      await this.#step(pc, () => (candidate ? pc.addIceCandidate(candidate) : pc.addIceCandidate()));
    } catch (err) {
      if (err instanceof PcGoneError || this.#pc !== pc) return;
      this.#log.warn('a remote candidate was not accepted', { err, gen: this.#gen });
    }
  }

  // ---- Recovery ----

  #ready(): boolean {
    const state = this.#signal.state;
    return state === undefined || state === 'ready';
  }

  #onPcState(pc: RTCPeerConnection): void {
    const health = healthOf(pc);
    if (health === this.#health) return;
    this.#health = health;
    this.#log.info('pub PC state', { state: health, gen: this.#gen });
    switch (health) {
      case 'connected':
        this.#disconnectTimer.clear();
        this.#iceRestartTimer.clear();
        this.#wantsIce = false;
        if (!this.#rebuildAsked) this.#wantsRebuild = false;
        this.#rebuilds = 0;
        this.#everConnected = true;
        break;
      case 'disconnected':
        // Is the socket gone too? An immediate ping finds out within 3 s (01 §3.4).
        this.#signal.probe();
        // During an ICE restart its 15 s timer decides, not another restart.
        if (!this.#iceRestartTimer.active) {
          this.#disconnectTimer.set(() => {
            this.#wantsIce = true;
            this.#pump();
          }, DISCONNECTED_GRACE_MS);
        }
        break;
      case 'failed':
        this.#disconnectTimer.clear();
        this.#iceRestartTimer.clear();
        this.#wantsRebuild = true;
        this.#pump();
        break;
      case 'pending':
        this.#disconnectTimer.clear();
        break;
    }
    this.#updateState();
  }

  /** Does what is wanted, if signaling is ready and the spacing allows; otherwise it stays wanted. */
  #pump(): void {
    if (!this.#pc || this.#fatal || !this.#ready()) return;
    if (this.#wantsRebuild) {
      if (!this.#rebuildCooling) this.#startRebuild();
      return;
    }
    if (this.#wantsIce && !this.#iceCooling) this.#startIceRestart(this.#pc);
  }

  #startIceRestart(pc: RTCPeerConnection): void {
    this.#wantsIce = false;
    this.#disconnectTimer.clear();
    this.#iceCooling = true;
    this.#iceCooldown.set(() => {
      this.#iceCooling = false;
      this.#pump();
    }, ICE_RESTART_SPACING_MS);
    this.#iceRestartTimer.set(() => {
      if (this.#health === 'connected') return;
      this.#wantsRebuild = true;
      this.#pump();
    }, ICE_RESTART_TIMEOUT_MS);
    this.#log.info('restarting ICE on the pub PC', { gen: this.#gen });
    try {
      pc.restartIce();
    } catch (err) {
      this.#log.warn('restartIce failed', { err });
    }
    this.#background('the ICE-restart offer', () => this.#renegotiate(pc));
  }

  /** Replaces the PC: gen + 1, every share attached again, a first offer. Starts the rebuild cooldown. */
  #startRebuild(): void {
    if (this.#shares.size === 0 || this.#fatal) return;
    this.#wantsRebuild = false;
    this.#wantsIce = false;
    // Rebuilds count towards 'unreachable' while the PC doesn't connect; one that replaces a connected PC starts
    // no such run.
    this.#rebuilds = this.#health === 'connected' ? 0 : this.#rebuilds + 1;
    this.#rebuildCooling = true;
    this.#rebuildCooldown.set(
      () => {
        this.#rebuildCooling = false;
        this.#pump();
      },
      this.#rebuilds >= MAX_FAST_REBUILDS ? REBUILD_SLOW_SPACING_MS : REBUILD_SPACING_MS,
    );
    this.#teardownPc();
    const pc = this.#createPc();
    this.#log.info('rebuilding the pub PC', { gen: this.#gen, rebuilds: this.#rebuilds });
    this.#background('the rebuild', async () => {
      try {
        for (const share of this.#shares.values()) await this.#attach(pc, share);
        await this.#negotiate(pc);
      } catch (err) {
        if (err instanceof PcGoneError || this.#pc !== pc) return;
        this.#log.error('the rebuilt pub PC could not be offered', { err, gen: this.#gen });
        this.#negotiationFailed();
      }
    });
    this.#updateState();
  }

  /** An offer or an answer of this PC could not be applied, here or on the server (01 §9 rule 8). */
  #negotiationFailed(): void {
    if (this.#fatal) return;
    if (this.#strike) {
      this.#fatal = true;
      this.#wantsIce = false;
      this.#wantsRebuild = false;
      for (const t of [this.#disconnectTimer, this.#iceRestartTimer, this.#rateLimitTimer]) t.clear();
      this.#log.error('pub PC negotiation failed twice within a minute: giving up', { gen: this.#gen });
      this.#updateState();
      return;
    }
    this.#strike = true;
    this.#strikeTimer.set(() => {
      this.#strike = false;
    }, NEGOTIATION_FAILURE_WINDOW_MS);
    // At once, whatever the spacing: this PC takes no further offer.
    this.#startRebuild();
  }

  readonly #onSignalState = (state: SignalState, info: SignalStateInfo): void => {
    if (state !== 'ready') return;
    if (info.resumed === false) {
      // A new connection: the server has neither the shares nor a pub PC, and gen starts at 1 again (01 §9 rule 2).
      this.#unsentClose = null;
      this.#announced = false;
      if (this.#pc) this.#log.info('the connection was not resumed: dropping the pub PC');
      this.#teardownPc();
      this.#resetRecovery();
      this.#gen = 1;
      this.#made = false;
      this.#updateState();
      return;
    }
    if (this.#unsentClose !== null && this.#signal.notify(MessageTypePCClose, this.#unsentClose)) {
      this.#unsentClose = null;
    }
    if (!this.#pc) return;
    this.#resendPending();
    const unsent = this.#localCandidates;
    this.#localCandidates = [];
    for (const msg of unsent) this.#signal.notify(MessageTypePCICE, msg);
    // 01 §10.4: after a resumed welcome, a pub PC that isn't connected restarts ICE, without the 3 s wait. That is
    // a disconnected one, and one that is still connecting although its offer was answered (the network changed
    // under its first checks: without this it would have to run into `failed` first). Not while an ICE restart is
    // under way (its 15 s timer decides), not a failed PC (it is rebuilt), and not one that waits for an answer:
    // its offer just went out again, and nothing is wrong with its ICE yet.
    const stuck = this.#health === 'pending' && this.#neg > 0 && this.#pending === null;
    if ((this.#health === 'disconnected' || stuck) && !this.#iceRestartTimer.active) this.#wantsIce = true;
    this.#pump();
  };

  #updateState(): void {
    let next: PubMediaState;
    if (!this.#pc) next = 'idle';
    else if (this.#fatal) next = 'failed';
    else if (this.#health === 'connected') next = 'connected';
    else if (this.#rebuilds >= MAX_FAST_REBUILDS) next = 'unreachable';
    else next = this.#everConnected ? 'reconnecting' : 'connecting';
    if (next === this.#state) return;
    this.#state = next;
    this.#emitter.emit('state', next);
  }
}
