// BrowserSharing (05 §8, §13): the in-page SharingProvider, the browser's `platform.sharing`. BrowserPlatform gets
// it from platform/browser/displayMedia.ts (createBrowserSharing), which passes in what only platform/ may touch:
// the picker (getDisplayMedia) and the RTCPeerConnection factory.
//
// - pick() opens the picker and classifies the result (displayMedia.ts, classify.ts, the fake-display seam).
// - start() publishes the picked source in the room (05 §13.4): share.start {kind, preset, audio, ref} → ShareParams,
//   then the share's tracks go onto the pub PC, which offers them (PublisherPC), and the caller gets the ActiveShare
//   (BrowserShare). The share is `starting` then, and `live` once room.state lists it as live.
//
// One pub PC per connection (05 §13.6), made for the first share and kept for the connection's lifetime with its
// generation counter: this class keeps one PublisherPC per signaling client and routes to it what the server sends
// about the pub PC (pc.answer, pc.ice, pc.restart, errors in scope `pc`), and to the shares what is about them
// (room.state, quality.hint, errors in scope `share`). The room session routes nothing of this: it only drives the
// ActiveShare it gets from start() (05 §11.1).
import { LocalError } from '../lib/errors';
import { createLogger, type Logger } from '../lib/log';
import type {
  ActiveShare,
  PickedSource,
  Platform,
  ShareContext,
  SharingProvider,
  SignalClientLike,
} from '../platform/types';
import { ProtocolError } from '../protocol/errors';
import {
  ErrorScopePC,
  ErrorScopeShare,
  MessageTypeError,
  MessageTypePCAnswer,
  MessageTypePCICE,
  MessageTypePCRestart,
  MessageTypeQualityHint,
  MessageTypeRoomState,
  MessageTypeShareStart,
  MessageTypeShareStop,
  PCKindPub,
  type Preset,
} from '../protocol/types.gen';
import { BrowserShare } from './BrowserShare';
import { canSendH264 } from './codecPrefs';
import { PublisherPC, type PublisherPCDeps } from './PublisherPC';
import { ShareCancelledError, shareStore, type ShareStore } from './shareStore';

export interface BrowserSharingDeps {
  /**
   * Opens the browser's picker and classifies the pick; null when the user cancels. It must call getDisplayMedia
   * before its first await: pick() runs inside the Share click (transient activation, 05 §13.1).
   */
  capture(opts: { preset: Preset }): Promise<PickedSource | null>;
  /** For the pub PC (05 §13.6): the platform's createPeerConnection. */
  platform: Pick<Platform, 'createPeerConnection'>;
  /** The share state machine the shares report to. Default: the page's shareStore. */
  store?: ShareStore;
  /** What the sender can encode. Default: RTCRtpSender.getCapabilities(kind).codecs. Tests pass their own. */
  capabilities?: PublisherPCDeps['capabilities'];
  log?: Logger;
}

/** What belongs to one signaling client: its pub PC and the shares on it. */
interface Link {
  readonly publisher: PublisherPC;
  readonly shares: Map<string, BrowserShare>;
}

/** Whether a share has ended (asked through a function: its state changes behind an await). */
function isOver(share: BrowserShare): boolean {
  return share.state === 'ended';
}

/** Whether a capture track is over: stopped, or its source ended (the browser's "Stop sharing"). */
function hasEnded(track: MediaStreamTrack): boolean {
  return track.readyState === 'ended';
}

/** share.start's `ref` (01 §8.7): a fresh random id per start, at most 32 characters. */
function newRef(): string {
  const bytes = new Uint8Array(12);
  globalThis.crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
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

export class BrowserSharing implements SharingProvider {
  readonly mode = 'in-page' as const;
  readonly #deps: BrowserSharingDeps;
  readonly #store: ShareStore;
  readonly #log: Logger;
  readonly #capabilities: NonNullable<PublisherPCDeps['capabilities']>;
  /** Keyed by the signaling client: a page has one (05 §7), tests have one each. */
  readonly #links = new WeakMap<SignalClientLike, Link>();

  constructor(deps: BrowserSharingDeps) {
    this.#deps = deps;
    this.#store = deps.store ?? shareStore;
    this.#log = deps.log ?? createLogger('share');
    this.#capabilities = deps.capabilities ?? browserCapabilities;
  }

  /** Call synchronously from the click handler. null = cancelled; a failed capture rejects with a LocalError. */
  pick(opts: { preset: Preset }): Promise<PickedSource | null> {
    return this.#deps.capture(opts);
  }

  /**
   * Publishes a picked source in ctx's room (05 §13.4) and resolves with its ActiveShare once the share exists on
   * the server and its tracks are offered on the pub PC. Without opts.withAudio the source's audio tracks are
   * stopped and left out.
   *
   * It rejects:
   * - before anything is sent, with LocalError `capture_failed` (no live video track) or `h264_unavailable` (this
   *   browser has no H.264 encoder);
   * - with the server's ProtocolError when share.start is refused (share_limit, not_in_room, forbidden, …);
   * - once the server has the share, with a LocalError when the tracks can't be put on the pub PC, with the
   *   server's error when it reports one about the share meanwhile (codec_not_supported), and with
   *   ShareCancelledError when the capture ended meanwhile (the browser's "Stop sharing"). The share is stopped on
   *   the server again in each case.
   * On a rejection the source is the caller's to release (releasing it twice is harmless).
   */
  async start(
    src: PickedSource,
    opts: { preset: Preset; withAudio: boolean },
    ctx: ShareContext,
  ): Promise<ActiveShare> {
    const { signal, roomId } = ctx;
    const stream = src.preview;
    const [video] = stream.getVideoTracks();
    if (video === undefined || hasEnded(video)) throw new LocalError('capture_failed');
    // 05 §13.4 step 8, before the server makes a share that could never be published.
    if (!canSendH264(this.#capabilities('video'))) throw new LocalError('h264_unavailable');
    if (!opts.withAudio) {
      for (const track of stream.getAudioTracks()) {
        track.stop();
        stream.removeTrack(track);
      }
    }
    const withAudio = stream.getAudioTracks().some((t) => !hasEnded(t));

    // 01 §8.7. Only what was picked, by kind: a window title never leaves the page (05 §20).
    const params = await signal.request(MessageTypeShareStart, {
      kind: src.kind,
      preset: opts.preset,
      audio: withAudio,
      ref: newRef(),
    });
    const { shareId } = params;
    this.#log.info('share.start answered', {
      kind: src.kind,
      preset: opts.preset,
      audio: withAudio,
      codec: params.codec,
    });

    const link = this.#linkFor(signal);
    // The page's state machine follows one share; a second one at the same time runs without it.
    const followed = this.#store.getState().publishing({
      picked: { kind: src.kind, audioScope: src.audioScope, warning: src.warning },
      preset: opts.preset,
      withAudio,
      params,
    });
    const store = followed ? this.#store : null;
    const share = new BrowserShare({
      signal,
      publisher: link.publisher,
      store,
      log: this.#log,
      src,
      roomId,
      params,
      preset: opts.preset,
      onEnded: (ended) => {
        if (link.shares.get(shareId) === ended) link.shares.delete(shareId);
      },
    });
    // Routed from now on: an error about the share may come before its offer is even made.
    link.shares.set(shareId, share);
    try {
      // "Stop sharing" in the browser's own bar while share.start was on its way.
      if (hasEnded(video)) throw new ShareCancelledError();
      await link.publisher.addShare(shareId, stream, params, opts.preset);
      // An error in scope `share` ended it while its tracks were being added: that error is the failure.
      if (isOver(share)) throw share.error instanceof Error ? share.error : new LocalError('webrtc_failed');
    } catch (err) {
      link.shares.delete(shareId);
      if (!isOver(share)) {
        // The server has the share (starting): end it there. share.stop is idempotent (01 §8.7).
        signal.request(MessageTypeShareStop, { shareId }).catch((stopErr: unknown) => {
          this.#log.warn('share.stop after a failed start failed', { err: stopErr });
        });
        store?.getState().finish(err instanceof ShareCancelledError ? {} : { error: err });
      }
      this.#log.warn('the share could not be published', { err });
      throw err;
    }
    share.begin();
    // The capture ended while its tracks were being added: begin() found that out and stopped the share.
    if (isOver(share)) throw new ShareCancelledError();
    return share;
  }

  /** The pub PC of a signaling client and the listeners that feed it, made on the client's first share. */
  #linkFor(signal: SignalClientLike): Link {
    const existing = this.#links.get(signal);
    if (existing) return existing;
    const publisher = new PublisherPC({
      platform: this.#deps.platform,
      signal,
      log: this.#log.child('pub-pc'),
      capabilities: this.#capabilities,
    });
    const link: Link = { publisher, shares: new Map() };
    this.#links.set(signal, link);
    // Kept for the client's lifetime, like the PC's generation counter: without a share they find nothing to do.
    signal.on(MessageTypePCAnswer, (a) => {
      if (a.pc === PCKindPub) void publisher.handleAnswer(a);
    });
    signal.on(MessageTypePCICE, (i) => {
      if (i.pc === PCKindPub) void publisher.handleIce(i);
    });
    signal.on(MessageTypePCRestart, (r) => {
      if (r.pc === PCKindPub) void publisher.handleRestart(r);
    });
    signal.on(MessageTypeRoomState, (s) => {
      for (const share of [...link.shares.values()]) share.roomState(s);
    });
    signal.on(MessageTypeQualityHint, (h) => {
      link.shares.get(h.shareId)?.qualityHint(h);
    });
    signal.on(MessageTypeError, (e) => {
      if (e.scope === ErrorScopePC) {
        publisher.handleError(e);
      } else if (e.scope === ErrorScopeShare && e.shareId !== undefined) {
        const share = link.shares.get(e.shareId);
        if (share) {
          this.#log.warn('the server reported an error about the share', { code: e.code });
          share.shareError(ProtocolError.fromWire(e));
        }
      }
    });
    return link;
  }
}
