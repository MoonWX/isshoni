// PublisherPC (05 §13.6, §9): the one pub PC of this connection, created on the first share. The client is the
// offerer; every offer carries `gen`, `neg` and `tracks` for each m-section that carries a share (01 §9).
//
// STUB until S46 writes this file: the class is declared as 05 §13.6 gives it ("interfaces first",
// docs/m1/README.md §4), so BrowserSharing and the room session have a fixed shape to build on. S46 adds the
// behaviour (transceivers, complete ascending sendEncodings, codec preferences, the gen/neg rules, quality.hint,
// rebuilds); until then every method rejects with NotImplementedError, and close() throws it.
import { NotImplementedError } from '../lib/errors';
import type { Logger } from '../lib/log';
import type { Platform, SignalClientLike } from '../platform/types';
import type { PCAnswer, PCICE, PCRestart, Preset, ShareParams } from '../protocol/types.gen';

export interface PublisherPCDeps {
  /** Peer connections come from the platform, so tests inject fakes (05 §8). */
  platform: Pick<Platform, 'createPeerConnection'>;
  /** ShareContext.signal: 01's SignalClient, by its structural type. */
  signal: SignalClientLike;
  log: Logger;
}

function notImplemented(member: string): Promise<never> {
  return Promise.reject(new NotImplementedError(`PublisherPC.${member}`, 'S46'));
}

export class PublisherPC {
  protected readonly deps: PublisherPCDeps;

  constructor(deps: PublisherPCDeps) {
    this.deps = deps;
  }

  /** The generation of the current pub PC: 1 for the first, + 1 on every rebuild (01 §9, 05 §11.1). */
  get gen(): number {
    return 1;
  }

  /** Adds a share's tracks (video, and audio when the stream has it) and offers them. */
  readonly addShare: (shareId: string, stream: MediaStream, params: ShareParams, preset: Preset) => Promise<void> =
    () => notImplemented('addShare');

  /** Stops the share's transceivers; re-offers, or sends pc.close when none are left. */
  readonly removeShare: (shareId: string) => Promise<void> = () => notImplemented('removeShare');

  /** encodings → setParameters (matched by rid); a codec or audioBitrate change → re-offer. */
  readonly applyParams: (shareId: string, p: Partial<ShareParams>) => Promise<void> = () =>
    notImplemented('applyParams');

  // later (M2): setPaused(shareId: string, paused: boolean): Promise<void>;

  readonly handleAnswer: (a: PCAnswer) => Promise<void> = () => notImplemented('handleAnswer');

  readonly handleIce: (i: PCICE) => Promise<void> = () => notImplemented('handleIce');

  /** The server asks: 'ice' → restartIce(); 'rebuild' → rebuild(). */
  readonly handleRestart: (r: PCRestart) => Promise<void> = () => notImplemented('handleRestart');

  /** gen + 1, same tracks and shareIds, a new offer. */
  readonly rebuild: () => Promise<void> = () => notImplemented('rebuild');

  /** Closes the PC on purpose and sends pc.close {pc: 'pub', gen}. */
  close(): void {
    throw new NotImplementedError('PublisherPC.close', 'S46');
  }
}
