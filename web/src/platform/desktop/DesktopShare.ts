// DesktopShare (docs/m2/03 §10.2): the desktop app's native share as an ActiveShare. Go publishes it on the app's own
// connection (C2, §9) through the share controller (internal/desktop/share); this object mirrors the controller's view,
// the share.state event (ShareView), and asks the bound Share service for every change. One object per share: it
// stays the same when C2 publishes the share again with `replaces` (its shareId changes, and it emits `state` then),
// and it ends once, on the controller's final view (ended or failed).
//
// It also implements 05 §11.1's ShareRecovery, so the room session keeps it across this page's own reconnects: resync
// and republish resolve at once (C2 recovers on its own, 01 §10), and serverEnded only asks Go for its view again, since
// C2 reports the authoritative end.
//
// Its `hint` comes straight from ShareView.hint: native shares use no HintDetector (01 applies the web's 6 s on /
// 10 s off rule in its rate control, and native shares keep their resolution, 01 D11).
//
// When the page's share store follows it (DesktopSharing passes the store; the M1 UI shows one share, 05 §13.1), it
// reports to the store as BrowserShare does: publishing, advance, report and finish.
//
// It lives in DesktopSharing's chunk, which loads on demand (lazySharing.ts): never in the entry chunk.
import { createEmitter } from '../../lib/emitter';
import type { Logger } from '../../lib/log';
import type { Preset, ShareKind, ShareParams } from '../../protocol/types.gen';
import type { ShareRecovery } from '../../rooms/RoomSession';
import type { ShareStore } from '../../share/shareStore';
import type { ActiveShare, LocalEndReason, LocalShareState, ShareHint, ShareStatsSample } from '../types';
import type { NativeBridge, ShareEndReason, ShareNotice, ShareView, ShareViewState } from './bridge';
import { BridgeError } from './errors';

/** The controller's states that end a share (docs/m2/03 §6.3). */
export function isFinalView(state: ShareViewState): boolean {
  return state === 'ended' || state === 'failed';
}

/**
 * A view of a share that exists (docs/m2/03 §10.1): starting, live, paused, reconnecting, unreachable or stopping.
 * `picking` is the controller's picker, not a share.
 */
export function isActiveView(state: ShareViewState): boolean {
  return state !== 'picking' && !isFinalView(state);
}

/**
 * ShareView.state → ActiveShare.state (docs/m2/03 §7.1, §10.1): unreachable is a reconnecting share whose media port
 * can't be reached (the store's `unreachable`), and stopping keeps the state it had (the store shows `stopping`).
 */
export function localStateOf(state: ShareViewState, before: LocalShareState): LocalShareState {
  switch (state) {
    case 'picking':
    case 'starting':
      return 'starting';
    case 'live':
      return 'live';
    case 'paused':
      return 'paused';
    case 'reconnecting':
    case 'unreachable':
      return 'reconnecting';
    case 'stopping':
      return before;
    case 'ended':
    case 'failed':
      return 'ended';
  }
}

/**
 * ShareView.endReason → the `ended` event's LocalEndReason (docs/m2/03 §7.1, §10.1): the user's own stops (the panel,
 * the hotkey, the tray, the pill) are `user`; a stop from another device of the user is `server` (the store's notice
 * says "stopped from another tab or device"); a failed share is `error`.
 */
export function localEndOf(view: Pick<ShareView, 'state' | 'endReason'> | null): LocalEndReason {
  if (view?.state === 'failed') return 'error';
  const reason: ShareEndReason | null = view?.endReason ?? null;
  switch (reason) {
    case 'source-gone':
      return 'source-gone';
    case 'elsewhere':
    case 'server':
      return 'server';
    case 'server-unreachable':
      return 'server-unreachable';
    case 'unlinked':
      return 'unlinked';
    case 'system':
      return 'system';
    case 'error':
      return 'error';
    case 'user':
    case 'hotkey':
    case 'tray':
    case null:
      return 'user';
  }
}

/** The error of a failed view as the SPA shows it (docs/m2/03 §6.4, §10.5); null when the view didn't fail. */
export function viewError(view: Pick<ShareView, 'state' | 'error'> | null): BridgeError | null {
  if (view?.state !== 'failed') return null;
  return new BridgeError(view.error?.code ?? 'internal', view.error?.params ?? {});
}

/**
 * The params that mark a server_unreachable error as the end of a live share whose 30 s hold ran out (01's core), not
 * a server that never answered: the panel says "Your share stopped…" for it (share/native/engineErrors.ts).
 */
export const SHARE_HOLD_ENDED = Object.freeze({ ended: 'server-unreachable' });

/**
 * What the share panel says about an end that wasn't the user's (docs/m2/03 §10.1, §10.5), as the store's error: a
 * failed share's error; "The window you shared was closed" (source-gone); "Your share stopped: isshoni couldn't reach
 * the server for 30 seconds" when 01's 30 s hold ran out (server-unreachable, server_unreachable with
 * SHARE_HOLD_ENDED). null for the others: the user's own stops, a stop from another device (the store's notice), the
 * server's end, unlinked (the relink page follows) and the system's (Go's toast says why).
 */
export function endError(view: Pick<ShareView, 'state' | 'error' | 'endReason'> | null): BridgeError | null {
  const failed = viewError(view);
  if (failed !== null) return failed;
  switch (view?.endReason) {
    case 'source-gone':
      return new BridgeError('engine.source_gone');
    case 'server-unreachable':
      return new BridgeError('server_unreachable', SHARE_HOLD_ENDED);
    default:
      return null;
  }
}

function sameHint(a: ShareHint | null, b: ShareHint | null): boolean {
  if (a === null || b === null) return a === b;
  if (a.kind !== b.kind) return false;
  return a.kind !== 'upload-limited' || (b.kind === 'upload-limited' && a.approxHeight === b.approxHeight);
}

// A type alias, not an interface: the emitter wants an index signature, which only an alias has implicitly.
type Events = {
  state: LocalShareState;
  ended: LocalEndReason;
  hint: ShareHint | null;
  /** Every new view (the native panel's bar and Details disclosure read it). */
  view: ShareView;
  /** share.notice: encoder_fallback (docs/m2/03 §10.5). */
  notice: ShareNotice;
};

export interface DesktopShareDeps {
  bridge: NativeBridge;
  /** The first view of the share (an active one). */
  view: ShareView;
  /** The page's share store, which this share reports to; null when the store follows another share. */
  store: ShareStore | null;
  /** Asks Go for its view again (DesktopSharing's pull of Share.Current()). */
  refresh: () => void;
  log: Logger;
}

export class DesktopShare implements ActiveShare, ShareRecovery {
  readonly kind: ShareKind;
  /** The engine captures in Go: the page has no stream of its own (docs/m2/03 §10.4). */
  readonly preview = null;
  /** C2 holds the share's ShareParams (docs/m2/03 §9). */
  readonly params: ShareParams | null = null;

  readonly #bridge: NativeBridge;
  readonly #store: ShareStore | null;
  readonly #refresh: () => void;
  readonly #log: Logger;
  readonly #emitter = createEmitter<Events>();
  #view: ShareView;
  #state: LocalShareState;
  #ended = false;

  constructor(deps: DesktopShareDeps) {
    this.#bridge = deps.bridge;
    this.#refresh = deps.refresh;
    this.#log = deps.log;
    this.#view = deps.view;
    this.kind = deps.view.kind;
    this.#state = localStateOf(deps.view.state, 'starting');
    const { view } = deps;
    const followed = deps.store?.getState().publishing({
      picked: { kind: view.kind, audioScope: view.withAudio ? 'system' : 'none', warning: null, label: view.label },
      preset: view.preset,
      withAudio: view.withAudio,
      params: null,
    });
    this.#store = followed === true ? (deps.store ?? null) : null;
    this.#report(view);
  }

  /** The share's id on the server; '' until C2's share.start was answered. It changes with `replaces`. */
  get shareId(): string {
    return this.#view.shareId;
  }

  /** The room C2 publishes the share in (docs/m2/03 §9: the page's room follows it after a reload). */
  get roomId(): string {
    return this.#view.roomId;
  }

  get state(): LocalShareState {
    return this.#state;
  }

  /** The controller's latest view of this share (docs/m2/03 §6.3). */
  get view(): ShareView {
    return this.#view;
  }

  /** Whether the share has ended (its last view was final). */
  get ended(): boolean {
    return this.#ended;
  }

  on(ev: 'state', fn: (s: LocalShareState) => void): () => void;
  on(ev: 'ended', fn: (r: LocalEndReason) => void): () => void;
  on(ev: 'hint', fn: (h: ShareHint | null) => void): () => void;
  on(ev: 'view', fn: (v: ShareView) => void): () => void;
  on(ev: 'notice', fn: (n: ShareNotice) => void): () => void;
  on<K extends keyof Events>(ev: K, fn: (value: Events[K]) => void): () => void {
    return this.#emitter.on(ev, fn);
  }

  // ---- DesktopSharing's side ----

  /** A newer view of this share, an active one. */
  update(view: ShareView): void {
    if (this.#ended) return;
    const before = this.#view;
    this.#view = view;
    const state = localStateOf(view.state, this.#state);
    const changed = state !== this.#state || view.shareId !== before.shareId;
    this.#state = state;
    this.#report(view);
    this.#emitter.emit('view', view);
    if (changed) this.#emitter.emit('state', state);
    if (!sameHint(before.hint, view.hint)) this.#emitter.emit('hint', view.hint);
  }

  /** The share is over: its final view, or null when Go no longer reports any share. Runs once. */
  end(view: ShareView | null): void {
    if (this.#ended) return;
    this.#ended = true;
    if (view !== null) this.#view = view;
    this.#state = 'ended';
    const reason = localEndOf(view);
    const error = endError(view);
    this.#log.info('the native share ended', { reason: view?.endReason ?? 'gone' });
    if (error !== null) this.#store?.getState().finish({ error });
    else if (view?.endReason === 'elsewhere') this.#store?.getState().finish({ notice: 'elsewhere' });
    else this.#store?.getState().finish();
    if (view !== null) this.#emitter.emit('view', view);
    this.#emitter.emit('state', 'ended');
    this.#emitter.emit('ended', reason);
    this.#emitter.clear();
  }

  /** share.notice while this share runs. */
  notify(notice: ShareNotice): void {
    if (!this.#ended) this.#emitter.emit('notice', notice);
  }

  // ---- ActiveShare ----

  /** Share.SetPreset: C2 sends share.update (docs/m2/03 §10.6). */
  async setPreset(p: Preset): Promise<void> {
    await this.#bridge.share.setPreset(p);
    this.#store?.getState().report({ preset: p });
  }

  /** Share.SetPaused (docs/m2/03 §11.3): feature_disabled on a server without share.pause. */
  setPaused(paused: boolean): Promise<void> {
    return this.#bridge.share.setPaused(paused);
  }

  /** Share sound (docs/m2/03 §10.3): local only, 05's SetShareSound; a no-op for a share started without sound. */
  async setAudioEnabled(on: boolean): Promise<void> {
    if (!this.#view.withAudio) return;
    await this.#bridge.share.setAudioEnabled(on);
    this.#store?.getState().report({ soundOn: on });
  }

  /**
   * Share.Stop(): never fails, idempotent; Go runs its local steps before it answers. The final view follows as an
   * event; the pull after the answer makes sure it arrives.
   */
  async stop(): Promise<void> {
    if (this.#ended) return;
    this.#store?.getState().advance('stopping');
    try {
      await this.#bridge.share.stop();
    } catch (err) {
      this.#log.warn('Share.Stop failed', { error: err });
    }
    this.#refresh();
  }

  /** share.stats' last sample (docs/m2/03 §6.3). */
  stats(): Promise<ShareStatsSample | null> {
    return this.#bridge.share.stats();
  }

  /** The Details disclosure is open (docs/m2/03 §10.3): 01's server stats while any is. */
  watchServerStats(on: boolean): void {
    this.#bridge.share.watchServerStats(on);
  }

  // ---- ShareRecovery (05 §11.1) ----

  /** C2 recovers by itself (01 §10): nothing for this page to do. */
  resync(): Promise<void> {
    return Promise.resolve();
  }

  /** C2 publishes again by itself (01 §10.6, `replaces`): nothing for this page to do. */
  republish(): Promise<void> {
    return Promise.resolve();
  }

  /**
   * The page's connection saw the share end (a room.event, a room.state without it). C2 reports the authoritative end
   * (docs/m2/03 §10.2), so this only asks Go for the share's view again.
   */
  serverEnded(): void {
    this.#refresh();
  }

  /** The store follows what the view says. */
  #report(view: ShareView): void {
    const store = this.#store;
    if (store === null) return;
    const s = store.getState();
    switch (view.state) {
      case 'live':
      case 'paused':
        s.advance('live');
        break;
      case 'reconnecting':
      case 'unreachable':
        // Only a share that was live can be reconnecting in the store; one that is still starting stays starting.
        // One that was live before this page saw it (adopted after a reload: liveSince) was live.
        if (s.phase === 'starting' && view.liveSince !== null) s.advance('live');
        s.advance('reconnecting');
        break;
      case 'stopping':
        s.advance('stopping');
        break;
      default:
        break;
    }
    s.report({
      preset: view.preset,
      withAudio: view.withAudio,
      soundOn: view.audioOn,
      hint: view.hint,
      unreachable: view.state === 'unreachable',
    });
  }
}

/** Whether an ActiveShare is the desktop's native share (the native panel reads its view). */
export function isDesktopShare(share: ActiveShare | null | undefined): share is DesktopShare {
  return share instanceof DesktopShare;
}
