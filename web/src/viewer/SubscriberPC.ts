// SubscriberPC (05 §10.1): the viewer's PeerConnection. The server is its only offerer (01 §9 rule 1), so this class
// answers offers, maps the received tracks to shares, and drives the recovery of 05 §9 / 01 §10.4 by asking the
// server for an ICE restart or a rebuild with pc.restart. It never imports React (05 §3): tracks go to the
// MediaRegistry, the connection's state to viewerStore.media.
//
// Negotiation (05 §9):
// - The RTCPeerConnection is made on the first pc.offer {pc: 'sub'}. An offer with a higher gen replaces it; offers
//   and candidates of an older gen are dropped; a repeated neg gets the stored answer again; an older neg is ignored.
// - Every offer carries `tracks` (mid → shareId, kind). The mapping is re-read on every offer, because the SFU
//   reuses transceivers for other shares (02): the registry is brought in line with it after each offer, and
//   `track` events use it too.
// - setRemoteDescription, createAnswer and setLocalDescription never interleave: one promise queue. The answer gets
//   the Opus stereo munge (01 §9 rule 6); if the browser rejects the munged answer, the plain one is used.
// - Local candidates are trickled with pc.ice; remote ones that arrive before the remote description are buffered
//   (at most 64, oldest dropped).
//
// Recovery (the sub column of 05 §9; the limits bind the client only):
// - disconnected: signal.probe() at once; after 3 s still disconnected → pc.restart {mode: 'ice'}.
// - A sub offer with a new ice-ufrag IS the ICE restart, whoever asked for it (this class, or the server's Resync()
//   after a resumed welcome): it cancels the 3 s timer and starts a 15 s timer; still not connected then →
//   pc.restart {mode: 'rebuild', reason: 'disconnected'}.
// - failed → pc.restart {mode: 'rebuild', reason: 'failed'} at once.
// - At most one ICE restart per 5 s and one rebuild per 10 s. After 5 rebuilds without connecting the state is
//   'unreachable' ("Can't reach the server's media port") and rebuilds go on every 30 s.
// - While signaling isn't ready, state changes are only recorded; the requests go out on `ready`. A resumed welcome
//   by itself asks for nothing. A welcome that is not resumed means the server has no sub PC for this connection any
//   more and gen starts at 1 again (01 §9 rule 2): the local PC is dropped.
// - sdp_invalid or bad_request about this PC, or an offer that can't be applied here: one rebuild; a second failure
//   within 60 s is the Fatal screen "Can't connect media" (01 §9 rule 8). The PC listens to the error notifications
//   in scope `pc` itself (05 §6.3); whoever owns the room session only routes pc.offer and pc.ice to it.
// - close() closes the local PC and sends nothing: room.leave closes the server side, and pc.close is for the pub
//   PC only (01 §9 rule 10).
import type { UiStore } from '../app/uiStore';
import type { Logger } from '../lib/log';
import { forceOpusStereo, iceUfrag } from '../lib/sdp';
import type { Platform } from '../platform/types';
import type { SignalClient, SignalState, SignalStateInfo } from '../protocol/signal-client';
import {
  ErrorCodeBadRequest,
  ErrorCodeRateLimited,
  ErrorCodeSDPInvalid,
  ErrorCodeStaleNegotiation,
  ErrorScopePC,
  MaxCandidateBytes,
  MessageTypeError,
  MessageTypePCAnswer,
  MessageTypePCICE,
  MessageTypePCRestart,
  PCKindSub,
  RestartModeICE,
  RestartModeRebuild,
  RestartReasonDisconnected,
  RestartReasonFailed,
  type Error as WireError,
  type ICECandidate,
  type ICEServer,
  type PCAnswer,
  type PCICE,
  type PCOffer,
  type RestartMode,
  type RestartReason,
  type TrackKind,
  type TrackRef,
} from '../protocol/types.gen';
import type { MediaRegistry } from './mediaRegistry';
import type { SubMediaState, ViewerStore } from './viewerStore';

/** ICE `disconnected` for this long → ask for an ICE restart (01 §10.4). */
export const DISCONNECTED_GRACE_MS = 3_000;
/** An ICE restart that isn't connected this long after its offer → ask for a rebuild. */
export const ICE_RESTART_TIMEOUT_MS = 15_000;
/** At most one ICE-restart request per this long. */
export const ICE_RESTART_SPACING_MS = 5_000;
/** At most one rebuild request per this long … */
export const REBUILD_SPACING_MS = 10_000;
/** … and per this long once MAX_FAST_REBUILDS of them didn't connect. */
export const REBUILD_SLOW_SPACING_MS = 30_000;
/** This many rebuilds without reaching `connected` → state 'unreachable'. */
export const MAX_FAST_REBUILDS = 5;
/** A second negotiation failure within this long after the first is fatal (01 §9 rule 8). */
export const NEGOTIATION_FAILURE_WINDOW_MS = 60_000;
/** Remote candidates kept until the remote description is set; the oldest go first (05 §9). */
export const MAX_BUFFERED_CANDIDATES = 64;
/** The wait before repeating a request the server rate-limited without saying how long. */
const DEFAULT_RATE_LIMIT_WAIT_MS = 1_000;

/** The part of 01's SignalClient this class uses; tests may pass a stand-in. */
export type SubscriberSignal = Pick<SignalClient, 'state' | 'welcome' | 'notify' | 'probe' | 'onState' | 'on'>;

export interface SubscriberDeps {
  platform: Pick<Platform, 'createPeerConnection'>;
  signal: SubscriberSignal;
  registry: MediaRegistry;
  log: Logger;
  /** Gets the PC's state as viewerStore.media (05 §6.1). */
  store?: ViewerStore;
  /** Shows the Fatal screen "Can't connect media" after two negotiation failures within 60 s. */
  ui?: UiStore;
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

/** The PC's connectivity, from connectionState and iceConnectionState: the worse of the two counts (05 §9). */
type Health = 'pending' | 'connected' | 'disconnected' | 'failed';

function healthOf(pc: RTCPeerConnection): Health {
  const c = pc.connectionState;
  const i = pc.iceConnectionState;
  if (c === 'failed' || i === 'failed') return 'failed';
  if (c === 'disconnected' || i === 'disconnected') return 'disconnected';
  return c === 'connected' ? 'connected' : 'pending';
}

/** What still has to be asked of the server. */
interface Wants {
  /** An ICE restart, with its reason. */
  ice: RestartReason | null;
  /** A rebuild, with its reason. It outranks an ICE restart. */
  rebuild: RestartReason | null;
  /** The rebuild is wanted whatever the ICE state: a negotiation failure, or a caller's explicit request. */
  forced: boolean;
}

const noWants = (): Wants => ({ ice: null, rebuild: null, forced: false });

interface SentRestart {
  gen: number;
  mode: RestartMode;
  reason: RestartReason;
  forced: boolean;
}

interface BufferedCandidate {
  gen: number;
  /** null: end of candidates. */
  candidate: RTCIceCandidateInit | null;
}

const boundKey = (ref: Pick<TrackRef, 'shareId' | 'kind'>): string => `${ref.kind} ${ref.shareId}`;

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

export class SubscriberPC {
  readonly #platform: Pick<Platform, 'createPeerConnection'>;
  readonly #signal: SubscriberSignal;
  readonly #registry: MediaRegistry;
  readonly #log: Logger;
  readonly #store: ViewerStore | undefined;
  readonly #ui: UiStore | undefined;

  #pc: RTCPeerConnection | null = null;
  /** Removes the listeners of #pc. */
  #offPc: (() => void) | null = null;
  /** Stops listening to the signaling state and to error notifications; set while a PC exists. */
  #offSignal: (() => void) | null = null;
  #gen = 0;
  /** The newest offer of this gen that was taken. */
  #neg = 0;
  /** The answer to offer #neg, once made: resent for a repeated neg (a safe replay after a resume). */
  #lastAnswer: PCAnswer | null = null;
  /** Whether #lastAnswer went out (false: signaling was down; it goes out on `ready`). */
  #answerSent = false;
  /** mid → share and kind, from the newest offer. */
  #tracks = new Map<string, TrackRef>();
  /** The registry entries this PC set, by boundKey. */
  readonly #bound = new Map<string, Pick<TrackRef, 'shareId' | 'kind'>>();
  /** The a=ice-ufrag of the remote description. */
  #remoteUfrag: string | null = null;
  #remoteCandidates: BufferedCandidate[] = [];
  /** Local candidates that couldn't be sent yet. */
  #localCandidates: PCICE[] = [];
  /** setRemoteDescription, createAnswer, setLocalDescription and addIceCandidate run one after another. */
  #queue: Promise<void> = Promise.resolve();
  /** Grows on close(): work that was queued or in flight before it is dropped. */
  #epoch = 0;

  #health: Health = 'pending';
  #state: SubMediaState = 'idle';
  #everConnected = false;
  #wants: Wants = noWants();
  #lastSent: SentRestart | null = null;
  /** Rebuild requests since the PC was last connected. */
  #rebuilds = 0;
  #iceCooling = false;
  #rebuildCooling = false;
  /** A negotiation failure happened less than 60 s ago. */
  #strike = false;
  /** Two negotiation failures within 60 s: nothing more is tried; only a reload helps. */
  #fatal = false;
  readonly #disconnectTimer = new TimerSlot();
  readonly #iceRestartTimer = new TimerSlot();
  readonly #iceCooldown = new TimerSlot();
  readonly #rebuildCooldown = new TimerSlot();
  readonly #strikeTimer = new TimerSlot();
  readonly #rateLimitTimer = new TimerSlot();

  constructor(deps: SubscriberDeps) {
    this.#platform = deps.platform;
    this.#signal = deps.signal;
    this.#registry = deps.registry;
    this.#log = deps.log;
    this.#store = deps.store;
    this.#ui = deps.ui;
  }

  /** The generation of the current PC; 0 while there is none. */
  get gen(): number {
    return this.#gen;
  }

  /** The PC's state, as it is written to viewerStore.media. */
  get state(): SubMediaState {
    return this.#state;
  }

  /**
   * Takes a pc.offer {pc: 'sub'}: queued behind the work in flight. A higher gen replaces the PC; a repeated neg
   * resends the stored answer; a new ice-ufrag counts as the ICE restart (05 §9). It never rejects: failures are
   * logged and handled as a negotiation failure.
   */
  handleOffer(o: PCOffer): Promise<void> {
    return this.#enqueue(() => this.#applyOffer(o));
  }

  /** Takes a pc.ice {pc: 'sub'}: added when the remote description of its gen is set, buffered until then. */
  handleIce(i: PCICE): Promise<void> {
    return this.#enqueue(async () => {
      if (i.pc !== PCKindSub || i.gen < this.#gen) return;
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
   * Takes an error notification in scope `pc` about the sub PC (05 §6.3): sdp_invalid and bad_request → rebuild
   * once, fatal the second time within 60 s; stale_negotiation → ignored; rate_limited → the last restart request
   * is repeated after retryAfterMs; others are logged. Errors about another PC, scope or an older gen are ignored.
   * While it has a PC, this class gets these from the SignalClient itself; nobody needs to route them.
   */
  handleError(e: WireError): void {
    if (e.scope !== ErrorScopePC || e.pc !== PCKindSub || !this.#pc || this.#fatal) return;
    if (e.gen !== undefined && e.gen < this.#gen) return;
    switch (e.code) {
      case ErrorCodeSDPInvalid:
      case ErrorCodeBadRequest:
        this.#log.warn('the server rejected the sub answer', { code: e.code, gen: this.#gen, neg: e.neg });
        this.#negotiationFailed();
        break;
      case ErrorCodeStaleNegotiation:
        break;
      case ErrorCodeRateLimited:
        this.#retryAfterRateLimit(e.retryAfterMs ?? DEFAULT_RATE_LIMIT_WAIT_MS);
        break;
      default:
        this.#log.warn('sub PC error', { code: e.code, gen: this.#gen });
    }
  }

  /**
   * Asks the server (the sub PC's offerer) for an ICE restart or a rebuild: pc.restart {pc: 'sub', gen, mode,
   * reason}. The request waits for signaling to be ready and for the spacing of 05 §9 (one ICE restart per 5 s, one
   * rebuild per 10 s); a rebuild outranks a pending ICE restart. A no-op without a PC.
   */
  requestRestart(mode: RestartMode, reason: RestartReason): void {
    if (!this.#pc || this.#fatal) return;
    if (mode === RestartModeRebuild) {
      this.#wants.rebuild = reason;
      this.#wants.forced = true;
    } else {
      this.#wants.ice = reason;
    }
    this.#pump();
  }

  getStats(): Promise<RTCStatsReport | null> {
    return this.#pc ? this.#pc.getStats() : Promise.resolve(null);
  }

  /**
   * Closes the local PC and forgets everything about it; no message is sent (room.leave closes the server side, and
   * pc.close is for the pub PC, 01 §9 rule 10). Its tracks leave the registry. The object stays usable: the next
   * sub offer, of any gen, makes a new PC (after a room switch, or a welcome that was not resumed).
   */
  close(): void {
    this.#epoch++;
    // An operation on a closed RTCPeerConnection may never settle: later work must not wait behind it.
    this.#queue = Promise.resolve();
    this.#teardownPc();
    this.#offSignal?.();
    this.#offSignal = null;
    for (const t of [this.#iceCooldown, this.#rebuildCooldown, this.#strikeTimer, this.#rateLimitTimer]) t.clear();
    this.#gen = 0;
    this.#remoteCandidates = [];
    this.#everConnected = false;
    this.#lastSent = null;
    this.#rebuilds = 0;
    this.#iceCooling = false;
    this.#rebuildCooling = false;
    this.#strike = false;
    this.#fatal = false;
    this.#updateState();
  }

  // ---- Negotiation ----

  /** Runs task after the work in flight. A failure is logged; the returned promise never rejects. */
  #enqueue(task: () => Promise<void>): Promise<void> {
    const epoch = this.#epoch;
    const run = this.#queue.then(async () => {
      if (epoch !== this.#epoch) return;
      try {
        await task();
      } catch (err) {
        this.#log.error('sub PC: unexpected failure', { err });
      }
    });
    this.#queue = run;
    return run;
  }

  async #applyOffer(o: PCOffer): Promise<void> {
    if (o.pc !== PCKindSub || this.#fatal) return;
    if (o.gen < this.#gen) {
      this.#log.debug('dropped a sub offer of an older gen', { gen: o.gen, current: this.#gen });
      return;
    }
    if (o.gen > this.#gen) this.#replacePc(o.gen);
    const pc = this.#pc;
    if (!pc) return;
    if (o.neg < this.#neg) {
      this.#log.debug('ignored a sub offer with an older neg', { gen: o.gen, neg: o.neg, current: this.#neg });
      return;
    }
    if (o.neg === this.#neg) {
      // The same offer again (the server's resend after a resume, or every 15 s without an answer): replay. Without
      // a stored answer this offer failed here, and the rebuild that was asked for is on its way.
      if (this.#lastAnswer) this.#answerSent = this.#signal.notify(MessageTypePCAnswer, this.#lastAnswer);
      return;
    }
    this.#neg = o.neg;
    this.#lastAnswer = null;
    this.#answerSent = false;
    this.#tracks = new Map(o.tracks.map((t): [string, TrackRef] => [t.mid, t]));

    const ufrag = iceUfrag(o.sdp);
    if (ufrag !== null && this.#remoteUfrag !== null && ufrag !== this.#remoteUfrag) this.#iceRestartOffered();

    try {
      await pc.setRemoteDescription({ type: 'offer', sdp: o.sdp });
      if (this.#pc !== pc) return;
      if (ufrag !== null) this.#remoteUfrag = ufrag;
      await this.#flushCandidates(pc);
      if (this.#pc !== pc) return;
      this.#bindTracks(pc);
      const answer = await pc.createAnswer();
      const sdp = await this.#setLocalAnswer(pc, answer.sdp ?? '');
      if (this.#pc !== pc) return;
      this.#lastAnswer = { pc: PCKindSub, gen: this.#gen, neg: o.neg, sdp };
      this.#answerSent = this.#signal.notify(MessageTypePCAnswer, this.#lastAnswer);
    } catch (err) {
      if (this.#pc !== pc) return; // closed or replaced while it ran: the failure is that of a PC that is gone
      this.#log.error('the sub offer could not be applied', { err, gen: this.#gen, neg: o.neg });
      this.#negotiationFailed();
    }
  }

  /** Sets the answer with the Opus stereo munge; the plain answer when the browser rejects the munged one. */
  async #setLocalAnswer(pc: RTCPeerConnection, plain: string): Promise<string> {
    const munged = forceOpusStereo(plain);
    if (munged !== plain) {
      try {
        await pc.setLocalDescription({ type: 'answer', sdp: munged });
        return pc.localDescription?.sdp ?? munged;
      } catch (err) {
        if (this.#pc !== pc) throw err;
        this.#log.warn('the stereo answer was rejected, answering without the munge', { err, gen: this.#gen });
      }
    }
    await pc.setLocalDescription({ type: 'answer', sdp: plain });
    return pc.localDescription?.sdp ?? plain;
  }

  /** Closes the current PC, if any, and makes the one of generation gen. */
  #replacePc(gen: number): void {
    this.#teardownPc();
    this.#gen = gen;
    // Candidates that came ahead of this gen's offer stay; older ones are of a PC that is gone.
    this.#remoteCandidates = this.#remoteCandidates.filter((c) => c.gen === gen);
    // The rebuild that was wanted, if any, is this PC.
    this.#wants = noWants();

    const pc = this.#platform.createPeerConnection({
      iceServers: toRtcIceServers(this.#signal.welcome?.iceServers),
      bundlePolicy: 'max-bundle',
      rtcpMuxPolicy: 'require',
    });
    const onTrack = (ev: RTCTrackEvent): void => {
      if (this.#pc === pc) this.#onTrack(ev);
    };
    const onCandidate = (ev: RTCPeerConnectionIceEvent): void => {
      if (this.#pc === pc) this.#onLocalCandidate(ev.candidate);
    };
    const onState = (): void => {
      if (this.#pc === pc) this.#onPcState(pc);
    };
    pc.addEventListener('track', onTrack);
    pc.addEventListener('icecandidate', onCandidate);
    pc.addEventListener('connectionstatechange', onState);
    pc.addEventListener('iceconnectionstatechange', onState);
    this.#offPc = () => {
      pc.removeEventListener('track', onTrack);
      pc.removeEventListener('icecandidate', onCandidate);
      pc.removeEventListener('connectionstatechange', onState);
      pc.removeEventListener('iceconnectionstatechange', onState);
    };
    this.#pc = pc;
    if (!this.#offSignal) {
      const offState = this.#signal.onState(this.#onSignalState);
      const offError = this.#signal.on(MessageTypeError, (e) => {
        this.handleError(e);
      });
      this.#offSignal = () => {
        offState();
        offError();
      };
    }
    this.#log.info('sub PC created', { gen });
    this.#updateState();
  }

  /** Closes the PC and drops what belongs to it: listeners, its timers, its tracks in the registry. */
  #teardownPc(): void {
    const pc = this.#pc;
    this.#pc = null;
    this.#offPc?.();
    this.#offPc = null;
    this.#disconnectTimer.clear();
    this.#iceRestartTimer.clear();
    this.#neg = 0;
    this.#lastAnswer = null;
    this.#answerSent = false;
    this.#tracks = new Map();
    this.#remoteUfrag = null;
    this.#localCandidates = [];
    this.#wants = noWants();
    this.#health = 'pending';
    for (const ref of this.#bound.values()) this.#registry.delete(ref.shareId, ref.kind);
    this.#bound.clear();
    if (!pc) return;
    try {
      pc.close();
    } catch (err) {
      this.#log.warn('closing the sub PC failed', { err });
    }
  }

  // ---- Tracks ----

  #onTrack(ev: RTCTrackEvent): void {
    const mid = ev.transceiver.mid;
    const ref = mid === null ? undefined : this.#tracks.get(mid);
    if (!ref || ref.kind !== ev.track.kind) {
      // The offer's `tracks` name every m-section that carries a share, with its kind (01 §9 rule 4): this is a
      // server bug.
      this.#log.warn('a track arrived on a mid without a mapping; ignored', { mid, kind: ev.track.kind });
      return;
    }
    this.#bind(ref, ev.track);
  }

  /**
   * Brings the registry in line with the newest offer's mapping: every mapped m-section's receiver track is its
   * share's track (a reused transceiver fires no `track` event when only its share changed), and what this PC set
   * for a share that is no longer mapped is dropped.
   */
  #bindTracks(pc: RTCPeerConnection): void {
    const mapped = new Map<string, { ref: TrackRef; track: MediaStreamTrack }>();
    for (const t of pc.getTransceivers()) {
      const ref = t.mid === null ? undefined : this.#tracks.get(t.mid);
      if (ref && t.receiver.track.kind === ref.kind) mapped.set(boundKey(ref), { ref, track: t.receiver.track });
    }
    for (const [key, ref] of [...this.#bound]) {
      if (mapped.has(key)) continue;
      this.#registry.delete(ref.shareId, ref.kind);
      this.#bound.delete(key);
    }
    for (const { ref, track } of mapped.values()) this.#bind(ref, track);
  }

  #bind(ref: TrackRef, track: MediaStreamTrack): void {
    const kind: TrackKind = ref.kind;
    this.#registry.set(ref.shareId, kind, track);
    this.#bound.set(boundKey(ref), { shareId: ref.shareId, kind });
  }

  // ---- Candidates ----

  #onLocalCandidate(candidate: RTCIceCandidate | null): void {
    let msg: PCICE;
    if (candidate !== null && candidate.candidate !== '') {
      if (new TextEncoder().encode(candidate.candidate).length > MaxCandidateBytes) return;
      msg = { pc: PCKindSub, gen: this.#gen, candidate: toWireCandidate(candidate) };
    } else {
      // null, or Firefox's empty candidate string: end of candidates. The marker is optional (01 §9 rule 5).
      msg = { pc: PCKindSub, gen: this.#gen };
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

  async #addCandidate(pc: RTCPeerConnection, candidate: RTCIceCandidateInit | null): Promise<void> {
    try {
      // No argument: end of candidates.
      if (candidate) await pc.addIceCandidate(candidate);
      else await pc.addIceCandidate();
    } catch (err) {
      if (this.#pc === pc) this.#log.warn('a remote candidate was not accepted', { err, gen: this.#gen });
    }
  }

  // ---- Recovery ----

  #onPcState(pc: RTCPeerConnection): void {
    const health = healthOf(pc);
    if (health === this.#health) return;
    this.#health = health;
    this.#log.info('sub PC state', { state: health, gen: this.#gen });
    switch (health) {
      case 'connected':
        this.#disconnectTimer.clear();
        this.#iceRestartTimer.clear();
        this.#wants.ice = null;
        if (!this.#wants.forced) this.#wants.rebuild = null;
        this.#rebuilds = 0;
        this.#everConnected = true;
        break;
      case 'disconnected':
        // Is the socket gone too? An immediate ping finds out within 3 s (01 §3.4).
        this.#signal.probe();
        // During an ICE restart its 15 s timer decides, not another restart.
        if (!this.#iceRestartTimer.active) {
          this.#disconnectTimer.set(() => {
            this.#wants.ice = RestartReasonDisconnected;
            this.#pump();
          }, DISCONNECTED_GRACE_MS);
        }
        break;
      case 'failed':
        this.#disconnectTimer.clear();
        this.#iceRestartTimer.clear();
        this.#wants.rebuild ??= RestartReasonFailed;
        this.#pump();
        break;
      case 'pending':
        this.#disconnectTimer.clear();
        break;
    }
    this.#updateState();
  }

  /** A sub offer with a new ice-ufrag: the ICE restart is under way, whoever asked for it. */
  #iceRestartOffered(): void {
    this.#disconnectTimer.clear();
    this.#wants.ice = null;
    this.#iceRestartTimer.set(() => {
      if (this.#health === 'connected') return;
      this.#wants.rebuild ??= RestartReasonDisconnected;
      this.#pump();
    }, ICE_RESTART_TIMEOUT_MS);
  }

  /** Sends what is wanted, if signaling is ready and the spacing allows; otherwise it stays wanted. */
  #pump(): void {
    if (!this.#pc || this.#fatal || this.#signal.state !== 'ready') return;
    const reason = this.#wants.rebuild;
    if (reason !== null) {
      if (this.#rebuildCooling) return;
      if (!this.#send(RestartModeRebuild, reason, this.#wants.forced)) return;
      this.#wants = noWants();
      this.#disconnectTimer.clear();
      this.#iceRestartTimer.clear();
      this.#rebuilds++;
      this.#rebuildCooling = true;
      const gen = this.#gen;
      this.#rebuildCooldown.set(
        () => {
          this.#rebuildCooling = false;
          // No new PC came and this one still isn't connected (a lost or refused request): ask again.
          if (this.#pc && this.#gen === gen && this.#health !== 'connected') this.#wants.rebuild ??= reason;
          this.#pump();
        },
        this.#rebuilds >= MAX_FAST_REBUILDS ? REBUILD_SLOW_SPACING_MS : REBUILD_SPACING_MS,
      );
      this.#updateState();
      return;
    }
    const iceReason = this.#wants.ice;
    if (iceReason !== null) {
      if (this.#iceCooling) return;
      if (!this.#send(RestartModeICE, iceReason, false)) return;
      this.#wants.ice = null;
      this.#iceCooling = true;
      this.#iceCooldown.set(() => {
        this.#iceCooling = false;
        this.#pump();
      }, ICE_RESTART_SPACING_MS);
    }
  }

  #send(mode: RestartMode, reason: RestartReason, forced: boolean): boolean {
    const gen = this.#gen;
    if (!this.#signal.notify(MessageTypePCRestart, { pc: PCKindSub, gen, mode, reason })) return false;
    this.#lastSent = { gen, mode, reason, forced };
    this.#log.info('asked for a sub PC restart', { mode, reason, gen });
    return true;
  }

  /** The offer or the answer of this PC could not be applied (01 §9 rule 8). */
  #negotiationFailed(): void {
    if (this.#strike) {
      this.#fatal = true;
      this.#wants = noWants();
      for (const t of [this.#disconnectTimer, this.#iceRestartTimer, this.#rateLimitTimer]) t.clear();
      this.#log.error('sub PC negotiation failed twice within a minute; giving up', { gen: this.#gen });
      this.#updateState();
      this.#ui?.getState().showScreen({ kind: 'fatal', reason: 'media' });
      return;
    }
    this.#strike = true;
    this.#strikeTimer.set(() => {
      this.#strike = false;
    }, NEGOTIATION_FAILURE_WINDOW_MS);
    this.#wants.rebuild = RestartReasonFailed;
    this.#wants.forced = true;
    this.#pump();
  }

  #retryAfterRateLimit(waitMs: number): void {
    const last = this.#lastSent;
    if (!last || last.gen !== this.#gen) return;
    this.#rateLimitTimer.set(() => {
      if (!this.#pc || this.#gen !== last.gen) return;
      if (last.mode === RestartModeRebuild) {
        if (!last.forced && this.#health === 'connected') return;
        this.#wants.rebuild ??= last.reason;
        this.#wants.forced ||= last.forced;
      } else if (this.#health === 'disconnected') {
        this.#wants.ice ??= last.reason;
      }
      this.#pump();
    }, waitMs);
  }

  readonly #onSignalState = (state: SignalState, info: SignalStateInfo): void => {
    if (state !== 'ready' || !this.#pc) return;
    if (info.resumed === false) {
      // A new connection: the server has no sub PC for it, and its first offer will be gen 1 again.
      this.#log.info('the connection was not resumed: dropping the sub PC', { gen: this.#gen });
      this.close();
      return;
    }
    const unsent = this.#localCandidates;
    this.#localCandidates = [];
    for (const msg of unsent) this.#signal.notify(MessageTypePCICE, msg);
    if (this.#lastAnswer && !this.#answerSent) {
      this.#answerSent = this.#signal.notify(MessageTypePCAnswer, this.#lastAnswer);
    }
    // What was recorded while signaling was down goes out now. A resumed welcome asks for nothing by itself: the
    // server's Resync() ICE-restarts a sub PC that isn't connected, and that offer counts as the restart.
    this.#pump();
  };

  #updateState(): void {
    let next: SubMediaState;
    if (!this.#pc) next = 'idle';
    else if (this.#fatal) next = 'failed';
    else if (this.#health === 'connected') next = 'connected';
    else if (this.#rebuilds >= MAX_FAST_REBUILDS) next = 'unreachable';
    else next = this.#everConnected ? 'reconnecting' : 'connecting';
    if (next === this.#state) return;
    this.#state = next;
    this.#store?.getState().setMedia(next);
  }
}
